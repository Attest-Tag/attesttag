package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const slackBodyLimit = 1 << 20

func (b *Bot) slackHTTPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /slack/events", slackWebhook(b.handleSlackEvents))
	mux.HandleFunc("POST /slack/interactions", slackWebhook(b.handleSlackInteractions))
}

// Bound the entire acknowledgement path, including body reads, without imposing
// Slack's short deadline on console document uploads sharing this HTTP server.
func slackWebhook(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
		defer cancel()
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(deadline)
		defer controller.SetReadDeadline(time.Time{})
		next(w, r.WithContext(ctx))
	}
}

// refuseSlack answers a delivery we will not act on, and says so in the log.
//
// These paths were all silent, and silence is the one answer that cannot be debugged: a message
// the bot never replied to looked exactly the same whether Slack had never called (a Request URL
// that was never set, or Socket Mode swallowing the event) or Slack had called and been turned
// away (a signing secret from a different app). Those have nothing in common but the symptom, and
// hunting the wrong one costs an afternoon. The reason goes to the log; the caller still gets the
// terse text, since the caller may not be Slack at all.
func refuseSlack(w http.ResponseWriter, r *http.Request, status int, says, why string) {
	slog.Warn("Slack delivery refused", "why", why, "path", r.URL.Path, "status", status,
		"retry", r.Header.Get("X-Slack-Retry-Num"))
	http.Error(w, says, status)
}

// Verify the exact bytes Slack signed, before JSON or form decoding. In particular,
// a legacy verification token, a retry header, or a console session grants no access.
func (b *Bot) signedSlackBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if b.cfg.SlackSigningSecret == "" {
		refuseSlack(w, r, http.StatusServiceUnavailable, "Slack delivery is not configured",
			"SLACK_SIGNING_SECRET is unset, so nothing from Slack can be verified")
		return nil, false
	}
	if !strings.HasPrefix(r.Header.Get("X-Slack-Signature"), "v0=") {
		refuseSlack(w, r, http.StatusUnauthorized, "invalid Slack signature",
			"no X-Slack-Signature header: this request did not come from Slack")
		return nil, false
	}
	verifier, err := slack.NewSecretsVerifier(r.Header, b.cfg.SlackSigningSecret)
	if err != nil {
		refuseSlack(w, r, http.StatusUnauthorized, "invalid Slack signature or timestamp",
			"unusable signature or timestamp header: "+err.Error())
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, slackBodyLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if !tooBig(w, err) {
			refuseSlack(w, r, http.StatusBadRequest, "could not read Slack payload", "body read failed: "+err.Error())
		}
		return nil, false
	}
	_, _ = verifier.Write(raw)
	if verifier.Ensure() != nil {
		refuseSlack(w, r, http.StatusUnauthorized, "invalid Slack signature",
			"the signature does not match SLACK_SIGNING_SECRET — the secret belongs to a different Slack app")
		return nil, false
	}
	return raw, true
}

func (b *Bot) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	raw, ok := b.signedSlackBody(w, r)
	if !ok {
		return
	}
	var envelope struct {
		Type, Challenge string
		EventID         string          `json:"event_id"`
		Event           json.RawMessage `json:"event"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		refuseSlack(w, r, http.StatusBadRequest, "invalid event payload", "the signed body is not a JSON event envelope")
		return
	}
	if envelope.Type == "url_verification" {
		if envelope.Challenge == "" {
			refuseSlack(w, r, http.StatusBadRequest, "missing challenge", "url_verification without a challenge")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"challenge": envelope.Challenge})
		return
	}
	if envelope.Type != "event_callback" {
		w.WriteHeader(http.StatusOK) // e.g. app_rate_limited; no business action
		return
	}
	// The cutover freeze. This has to answer 200 and drop the event, not 503 it: Slack disables
	// event delivery to an app that fails more than about 95% of deliveries, and the only way
	// back from that is a reinstall by every connected workspace. Ten minutes of lost events is
	// a handful of messages at night; a disabled app is an outage that needs other people.
	// url_verification is answered above, so the Request URL can still be verified mid-freeze.
	if b.cfg.Maintenance {
		slog.Info("maintenance: Slack event acknowledged and dropped", "event_id", envelope.EventID)
		w.WriteHeader(http.StatusOK)
		return
	}
	var inner struct {
		Type string `json:"type"`
	}
	if envelope.EventID == "" || json.Unmarshal(envelope.Event, &inner) != nil || inner.Type == "" {
		refuseSlack(w, r, http.StatusBadRequest, "missing event or event id", "event_callback without an event id or an inner event type")
		return
	}
	// Ignore event types the router does not consume; SDKs can lag new Slack events.
	switch inner.Type {
	case "app_mention", "message", "assistant_thread_started", "app_uninstalled", "tokens_revoked":
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	ev, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
	if err != nil {
		refuseSlack(w, r, http.StatusBadRequest, "invalid event", "unparseable "+inner.Type+" event "+envelope.EventID+": "+err.Error())
		return
	}
	team := teamOf(raw, ev)
	if team == "" {
		refuseSlack(w, r, http.StatusBadRequest, "missing workspace authorization", "the event names no workspace")
		return
	}
	slog.Debug("Slack event accepted", "type", inner.Type, "team", team, "event", envelope.EventID)
	b.acceptSlackDelivery(w, r, "event", team, "event:"+team+":"+envelope.EventID, raw)
}

func (b *Bot) handleSlackInteractions(w http.ResponseWriter, r *http.Request) {
	raw, ok := b.signedSlackBody(w, r)
	if !ok {
		return
	}
	// Frozen for the same reason as an event, and answered the same way: 200, dropped, logged.
	// A button press is a write, and the database it would write to is about to be replaced.
	if b.cfg.Maintenance {
		slog.Info("maintenance: Slack interaction acknowledged and dropped")
		w.WriteHeader(http.StatusOK)
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		http.Error(w, "expected a Slack form payload", http.StatusUnsupportedMediaType)
		return
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil || len(form["payload"]) != 1 {
		http.Error(w, "invalid interaction form", http.StatusBadRequest)
		return
	}
	payload := []byte(form.Get("payload"))
	var cb slack.InteractionCallback
	if json.Unmarshal(payload, &cb) != nil || cb.Type == "" {
		http.Error(w, "invalid interaction", http.StatusBadRequest)
		return
	}
	if cb.Type != slack.InteractionTypeBlockActions {
		w.WriteHeader(http.StatusOK) // no modal validation or option providers registered
		return
	}
	if cb.Team.ID == "" || cb.User.ID == "" {
		http.Error(w, "missing interaction workspace or user", http.StatusBadRequest)
		return
	}
	// Trigger IDs identify a press, not the card: a second legitimate press must not
	// be mistaken for a network retry. Hash the signed payload only as a fallback.
	id := cb.TriggerID
	if id == "" {
		sum := sha256.Sum256(payload)
		id = hex.EncodeToString(sum[:])
	}
	b.acceptSlackDelivery(w, r, "interaction", cb.Team.ID, "interaction:"+cb.Team.ID+":"+id, payload)
}

func (b *Bot) acceptSlackDelivery(w http.ResponseWriter, r *http.Request, kind, team, key string, raw []byte) {
	// A redelivery names what Slack saw on the earlier attempt (http_timeout, connection_failed,
	// http_error, ...). It is the only trace of a failure that never reached this process.
	if n := r.Header.Get("X-Slack-Retry-Num"); n != "" {
		slog.Info("Slack redelivery", "kind", kind, "team", team, "key", key, "attempt", n, "reason", r.Header.Get("X-Slack-Retry-Reason"))
	}
	// A 2xx promises receipt. Persist first, using a shorter deadline than Slack's
	// three-second acknowledgement window. On failure Slack may retry normally.
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	if b.store == nil || b.sealer == nil {
		http.Error(w, "Slack delivery unavailable", http.StatusServiceUnavailable)
		return
	}
	installed, err := b.store.Team(ctx, team)
	if err != nil {
		refuseSlack(w, r, http.StatusServiceUnavailable, "Slack delivery unavailable", "could not read the workspace: "+err.Error())
		return
	}
	if installed == nil {
		// 200, because Slack must not retry an install we do not have — but said out loud,
		// since from the outside this is indistinguishable from the bot ignoring somebody.
		slog.Warn("Slack delivery dropped", "why", "this workspace is not installed here", "team", team, "kind", kind)
		w.WriteHeader(http.StatusOK) // never route an unknown workspace to a default tenant
		return
	}
	// A revoked workspace gets nothing queued: its token is gone, so nothing could be done
	// with the payload but hold it. The two events that say the token is gone are the
	// exception, since they are what revokes.
	if installed.Status != "active" && !revokesInstall(kind, raw) {
		w.WriteHeader(http.StatusOK)
		return
	}
	enc, err := b.sealer.Seal(raw)
	if err == nil {
		err = b.store.enqueueSlackDelivery(ctx, key, installed.OrgID, team, kind, enc)
	}
	if err != nil {
		slog.Warn("Slack delivery not accepted", "team", team, "kind", kind, "err", err)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Slack delivery temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Deliveries survive a process restart before dispatch. The lease protects the
// handoff; existing message and approval handlers retain their own idempotency.
// This is not an exactly-once guarantee for external side effects.
func (b *Bot) runSlackDeliveries(ctx context.Context) {
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				worked := b.dispatchNextSlackDelivery(ctx)
				if !worked {
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Second):
					}
				}
			}
		}()
	}
	// Delete completed receipts after the retry/dedup window; payloads are erased
	// immediately on successful dispatch, so this sweep stores only identifiers.
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
			if err := b.store.purgeSlackDeliveries(ctx); err != nil {
				slog.Warn("Slack receipt cleanup", "err", err)
			}
		}
	}
}

func (b *Bot) dispatchNextSlackDelivery(ctx context.Context) bool {
	d, err := b.store.claimSlackDelivery(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("Slack delivery claim", "err", err)
		}
		return false
	}
	if d == nil {
		return false
	}
	h := &deliveryHandoff{store: b.store, d: d}
	workCtx, cancel := context.WithTimeout(withDelivery(ctx, h), 45*time.Second)
	defer cancel()
	raw, err := b.sealer.Open(d.Payload)
	if err == nil {
		err = b.dispatchSlackPayload(workCtx, d.Kind, d.Team, raw)
	}
	// A turn started here outlives this function, so it — not the dispatcher — decides when the
	// delivery is complete. Closing the row now is what used to lose the message.
	if h.owned() {
		return true
	}
	if err != nil || workCtx.Err() != nil {
		why := "timed out"
		if err != nil {
			why = err.Error()
		}
		if d.Attempts >= slackDeliveryMaxAttempts {
			slog.Error("Slack delivery dead-lettered", "team", d.Team, "org", d.OrgID, "kind", d.Kind, "attempts", d.Attempts, "err", why)
		} else {
			slog.Warn("Slack delivery deferred", "team", d.Team, "kind", d.Kind, "attempt", d.Attempts, "err", why)
		}
		// Not ctx: during shutdown it is already cancelled, and the failure would then be
		// recorded nowhere — leaving the row holding its lease with no reason against it.
		if err := b.store.failSlackDelivery(context.WithoutCancel(ctx), d, why); err != nil {
			slog.Warn("Slack delivery failure record", "err", err)
		}
		return false // lease expires; do not turn a failed handoff into an acknowledgement
	}
	if err := b.store.finishSlackDelivery(ctx, d); err != nil {
		slog.Warn("Slack delivery completion", "err", err)
	}
	return true
}

func (b *Bot) dispatchSlackPayload(ctx context.Context, kind, team string, raw []byte) error {
	switch kind {
	case "event":
		ev, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
		if err != nil {
			return err
		}
		b.route(ctx, team, ev)
	case "interaction":
		var cb slack.InteractionCallback
		if err := json.Unmarshal(raw, &cb); err != nil {
			return err
		}
		// Action handlers create their own bounded contexts so their asynchronous
		// work survives cancellation of this handoff's context on return.
		b.interaction(ctx, team, cb)
	default:
		return errors.New("unknown Slack delivery kind")
	}
	return nil
}

// revokesInstall says whether an event is one of the two that announce the workspace's token
// is gone. Those are queued even for a workspace already marked revoked; nothing else is.
func revokesInstall(kind string, raw []byte) bool {
	if kind != "event" {
		return false
	}
	var p struct {
		Event struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	return p.Event.Type == "app_uninstalled" || p.Event.Type == "tokens_revoked"
}
