package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"attesttag/internal/review"
)

// The GitHub App's webhook: GitHub telling this deployment that something happened on one of the
// App's installations — the App installed, suspended or removed, a repository added to it, a pull
// request opened, a comment that may be addressed to the reviewer.
//
// It is built like the Slack events endpoint (slack_http.go), for the same reason: GitHub gives a
// delivery ten seconds and a review takes minutes, so the handler does nothing but decide whether
// the delivery is worth keeping, seal it and write it to the inbox (store_github_deliveries.go),
// and dispatchers take it from there. Unlike Slack, GitHub never retries a failed delivery by
// itself — only somebody pressing Redeliver under the App's Recent Deliveries does — so a 2xx is
// the last answer the event gets, and each one below is chosen with that in mind: a delivery we
// could not store is a 503, which leaves it listed as failed and redeliverable, never a 200 that
// loses it without a trace.
//
// The signature is the whole of the authority, and it covers the body only. The event name and the
// delivery id are headers, so anybody holding one signed body could resend it under another of
// either. Nothing here acts on a header alone: the dispatcher checks that an installation event's
// payload really is an installation record before it revokes or re-permissions anything, and what
// is to stop a resent pull-request body becoming a second review is the review lane's own dedup on
// the pull request's head (review_runs.dedupe_key), never the delivery id.
//
// What is stored is filtered at receipt. The App receives every subscribed event of every
// installation, including the many that only use it for fix jobs and Slack tools; a pull-request
// event is written down only when its installation is in an organisation's review tree and its
// repository's effective mode is not off (ReviewModeAt), so nobody else's pull requests — and no
// repository a team switched off — are ever persisted here.
//
// Nothing about a request is believed before its signature checks out, and the signature needs
// the whole body, so the body is the one cost an unauthenticated caller can impose. It is bounded
// three ways before a byte is read: by event (githubEventBodyLimit), by what the request says its
// length is, and by every body held at once across the process (githubWebhookInflight).

const (
	// GitHub caps a payload at 25 MB and does not deliver an event whose payload would be larger,
	// so this is the largest body a genuine delivery can have — and the cap for the installation
	// events only, whose record lists every repository an installation was given, which for a
	// large account runs to megabytes. No event here carries a pull request's files.
	githubWebhookBodyLimit = 25 << 20
	// The pull-request family is far smaller: GitHub cuts a pull request's, a comment's and a
	// review's body at 65,536 characters, and the rest of the payload is a few objects describing
	// the repository and the people. Two megabytes is several times the worst of it.
	githubPRBodyLimit = 2 << 20
	// The body bytes every request in this process may hold at once. Cloud Run answers twenty at a
	// time in a gigabyte; twenty unsigned bodies of the installation events' size would be half a
	// gigabyte before a signature was checked, and the garbage collector's headroom takes the rest,
	// taking Slack and the console down with it. Past this the answer is a 503, which GitHub lists
	// for redelivery; a real delivery is a few kilobytes and is almost never the one turned away.
	githubWebhookInflight = 64 << 20
	// The whole answer — reading the body, checking it, storing it — inside this, two seconds
	// short of GitHub's ten. A database having a bad moment then shows up as a 503 that GitHub
	// lists against the delivery, instead of a timeout it records with no answer at all.
	githubWebhookDeadline = 8 * time.Second
	// The database's share of that: two reads and an insert, milliseconds on any day that matters.
	githubWebhookStoreDeadline = 2 * time.Second
	// Failed signatures per address per hour, as for the Stripe webhook (billing.go). A genuine
	// delivery carries a valid signature by definition and is never counted, so only somebody
	// guessing is slowed — and a throttle that could turn GitHub away would lose events for good.
	githubBadSignaturesPerHour = 60

	// Two dispatchers per instance. What they do is milliseconds of database work — a review is
	// handed to a lane of its own rather than run here — so more would only add idle polling; two
	// means one slow delivery never holds up the next.
	githubDispatchers = 2
	// How long one dispatch may run. The lease is touched while it does, so without a ceiling a
	// dispatch that hung would hold its delivery for ever.
	githubDispatchTimeout = 2 * time.Minute
	// How long an idle dispatcher waits before asking again. Slower than the Slack inbox's second:
	// nobody is waiting on a reply in a chat, and a review that starts two seconds later than it
	// could is not one anybody notices.
	githubIdlePoll = 2 * time.Second
)

// githubEventBodyLimit is the largest body each event may have, and so also the events this
// webhook takes at all: one missing here is answered without its body being read. A review thread
// event carries every comment in the thread, so it gets more room than one comment does.
var githubEventBodyLimit = map[string]int64{
	"ping":                        1 << 20,
	"installation":                githubWebhookBodyLimit,
	"installation_repositories":   githubWebhookBodyLimit,
	"pull_request":                githubPRBodyLimit,
	"issue_comment":               githubPRBodyLimit,
	"pull_request_review":         githubPRBodyLimit,
	"pull_request_review_comment": githubPRBodyLimit,
	"pull_request_review_thread":  4 * githubPRBodyLimit,
}

// githubDeliveryTouchEvery is how often a dispatcher renews its lease: a third of it, so two
// touches can fail in a row before another dispatcher may take the delivery over. A variable only
// so a test can watch a lease being lost without waiting twenty seconds; nothing else assigns it.
var githubDeliveryTouchEvery = githubDeliveryLease / 3

// routeGitHubDelivery is what a dispatcher does with a delivery it holds. A variable only so a
// test can stand in work slow enough to outlive a lease; nothing else assigns it.
var routeGitHubDelivery = (*Bot).dispatchGitHubDelivery

// githubWebhook is what a delivery is checked against. Read from the environment beside the rest
// of the App (newGitHubApp) but kept apart from it, because neither needs the other to start: an
// App with no webhook secret is one whose installations work and cannot be reviewed yet, and a
// secret with no GITHUB_APP_ID is one whose target check is skipped. The boot log says which, and
// neither is worth refusing to start Slack over.
type githubWebhook struct {
	// secrets is the current secret, then the previous one while a rotation is under way. GitHub
	// signs with one secret at a time and changing it there is one click, which no redeploy can be
	// timed to; holding both across the change is what keeps the deliveries in between.
	secrets [][]byte
	// appID is GITHUB_APP_ID, compared with the target id GitHub puts on every delivery.
	appID string
	// badSigs counts failed signatures per address. Its own limiter rather than a package one, so a
	// test's bad signatures are not still counted against the next test's address.
	badSigs *rateLimiter
	// inflight is the body bytes held right now by every request this process is answering,
	// bounded by inflightMax (githubWebhookInflight outside tests).
	inflight    atomic.Int64
	inflightMax int64
}

// hold reserves n bytes of the in-flight budget, or reports that they would not fit.
func (h *githubWebhook) hold(n int64) bool {
	for {
		cur := h.inflight.Load()
		if cur+n > h.inflightMax {
			return false
		}
		if h.inflight.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

func (h *githubWebhook) release(n int64) { h.inflight.Add(-n) }

// newGitHubWebhook reads the webhook's settings. A previous secret with no current one is ignored,
// and said so: it is the half of a rotation that is meant to be removed, and accepting it alone
// would leave a deployment verifying against a secret somebody believes retired.
func newGitHubWebhook() *githubWebhook {
	cur := strings.TrimSpace(os.Getenv("GITHUB_APP_WEBHOOK_SECRET"))
	prev := strings.TrimSpace(os.Getenv("GITHUB_APP_WEBHOOK_SECRET_PREVIOUS"))
	if cur == "" && prev != "" {
		slog.Warn("GITHUB_APP_WEBHOOK_SECRET_PREVIOUS is set without GITHUB_APP_WEBHOOK_SECRET and is ignored: " +
			"the previous secret is only accepted beside a current one")
	}
	return newGitHubWebhookFrom(cur, prev, strings.TrimSpace(os.Getenv("GITHUB_APP_ID")))
}

func newGitHubWebhookFrom(current, previous, appID string) *githubWebhook {
	h := &githubWebhook{appID: appID, badSigs: newRateLimiter(), inflightMax: githubWebhookInflight}
	if current == "" {
		return h
	}
	h.secrets = [][]byte{[]byte(current)}
	if previous != "" && previous != current {
		h.secrets = append(h.secrets, []byte(previous))
	}
	return h
}

// configured reports whether deliveries can be verified at all. A nil receiver answers false, so
// a Bot built without one — every test that never thought about GitHub — refuses deliveries.
func (h *githubWebhook) configured() bool { return h != nil && len(h.secrets) > 0 }

// verify reports whether header, an X-Hub-Signature-256 value, is the HMAC-SHA256 of body under a
// secret this deployment accepts. Every secret is tried whatever the first one said, and compared
// with hmac.Equal, so the time taken says nothing about how close a guess came.
func (h *githubWebhook) verify(body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	want, err := hex.DecodeString(hexSig)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	match := false
	for _, s := range h.secrets {
		mac := hmac.New(sha256.New, s)
		mac.Write(body)
		if hmac.Equal(mac.Sum(nil), want) {
			match = true
		}
	}
	return match
}

var (
	// GitHub's event names are lower-case words joined by underscores.
	githubEventName = regexp.MustCompile(`^[a-z_]{1,64}$`)
	// X-Hub-Signature-256 as GitHub writes it. Anything else cannot verify, so it is refused before
	// the body is read rather than after.
	githubSignatureHeader = regexp.MustCompile(`^sha256=[0-9a-f]{64}$`)
	// A delivery id is a GUID. It becomes a primary key, so anything else is refused rather than
	// stored: the header is not covered by the signature.
	githubDeliveryGUID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// githubEnvelope is the part of a delivery the receipt filter reads, and nothing more: the router
// that dispatches a delivery opens the whole payload, and adds what it needs there. Everything
// else stays sealed until then.
type githubEnvelope struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Type string `json:"type"`
	} `json:"sender"`
	// Issue is set on issue_comment, and its pull_request key only when the issue is a pull
	// request — which is how GitHub tells a comment on a pull request from one on an issue.
	Issue *struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	PullRequest *struct {
		Number int `json:"number"`
	} `json:"pull_request"`
}

func (e *githubEnvelope) prNumber() int {
	switch {
	case e.PullRequest != nil:
		return e.PullRequest.Number
	case e.Issue != nil:
		return e.Issue.Number
	}
	return 0
}

// githubReceipt decides from the event and its envelope alone — before anything is read from the
// database — whether a delivery could be worth keeping. A non-empty drop says why it is not;
// otherwise reviewed says whether it is kept only for an installation an organisation reviews.
//
// The installation's own events are always kept: they are about the binding itself, whoever
// reviews what. The pull-request family is kept for review, with one cut. A comment, a review or a
// thread change made by a bot is dropped, because the bot most likely to make one is this App
// posting its own review, and an echo of that coming back in as a command is how a reviewer ends up
// answering itself. A bot's pull_request events are kept: a rebase by a merge bot or a commit
// pushed by a workflow moves the head a review is anchored to, whoever pushed it.
// githubAppEvents are the webhook events the App has to be subscribed to for code review, by the
// event header GitHub sends and the name its App settings tick it under. Only "Pull request" brings
// reviews; without the other four a team's `@` commands and its replies in a finding's thread are
// never delivered at all, and nothing anywhere says so — so guide/configuration.md lists them, and
// TestGitHubAppEventsAreTheOnesListenedToAndInTheGuide holds the guide and githubReceipt to this list.
// The installation's own events need no subscription: GitHub sends them to every App.
var githubAppEvents = []struct{ header, label string }{
	{"pull_request", "Pull request"},
	{"issue_comment", "Issue comment"},
	{"pull_request_review", "Pull request review"},
	{"pull_request_review_comment", "Pull request review comment"},
	{"pull_request_review_thread", "Pull request review thread"},
}

func githubReceipt(event string, e *githubEnvelope) (reviewed bool, drop string) {
	switch event {
	case "installation", "installation_repositories":
		return false, ""
	case "pull_request":
		return true, ""
	case "issue_comment":
		if e.Issue == nil || len(e.Issue.PullRequest) == 0 || string(e.Issue.PullRequest) == "null" {
			return false, "a comment on an issue, not on a pull request"
		}
		fallthrough
	case "pull_request_review", "pull_request_review_comment", "pull_request_review_thread":
		if strings.EqualFold(e.Sender.Type, "Bot") {
			return false, "made by a bot"
		}
		return true, ""
	}
	return false, "not an event code review listens to"
}

// refuseGitHub answers a delivery we will not take, and says why in the log, where the reason is
// useful; the caller gets a terse line, since before the signature is checked it may not be GitHub.
func refuseGitHub(w http.ResponseWriter, r *http.Request, status int, says, why string) {
	slog.Warn("GitHub delivery refused", "why", why, "status", status, "ip", clientIP(r),
		"event", truncate(r.Header.Get("X-GitHub-Event"), 64), "delivery", truncate(r.Header.Get("X-GitHub-Delivery"), 64))
	http.Error(w, says, status)
}

// ignoreGitHub answers a signed delivery we choose not to keep. 200, because the delivery is sound
// and sending it again would get the same answer. The reason goes in the body as well as the log:
// GitHub shows the body beside the delivery under the App's Recent Deliveries, the one place the
// App's owner can see why a delivery did nothing without this deployment's logs to hand.
func ignoreGitHub(w http.ResponseWriter, why string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ignored: "+why+"\n")
}

// handleGitHubWebhook is POST /github/webhook. No session, no CSRF token, no permission: the
// authority is the signature over the exact bytes of the body, checked before anything in them is
// believed. It never calls LearnOrigin either. The public origin is learned from a person signing
// in to the console (public_origin.go), and the Host on a request anybody can send is theirs to
// choose; learning from this route would let any caller name the address the console links to.
func (b *Bot) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), githubWebhookDeadline)
	defer cancel()
	deadline, _ := ctx.Deadline()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	defer rc.SetReadDeadline(time.Time{})

	// Under the cutover freeze, a 503 rather than Slack's 200-and-drop (slack_http.go). Slack
	// disables an app that keeps failing; GitHub does not, and keeps the failure listed for
	// redelivery — so refusing is what lets these events be had back once the database is, where a
	// 200 would promise a receipt that the snapshot about to replace this database never saw.
	if b.cfg.Maintenance {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
		return
	}
	if !b.ghHook.configured() {
		refuseGitHub(w, r, http.StatusServiceUnavailable, "GitHub webhook is not configured",
			"GITHUB_APP_WEBHOOK_SECRET is unset, so no delivery can be verified")
		return
	}
	if b.store == nil || b.sealer == nil {
		http.Error(w, "GitHub delivery unavailable", http.StatusServiceUnavailable)
		return
	}
	// Everything below up to the read is decided from headers alone, so a request that could never
	// be a delivery we keep costs its headers and nothing more.
	if !githubSignatureHeader.MatchString(r.Header.Get("X-Hub-Signature-256")) {
		b.badGitHubSignature(w, r, "X-Hub-Signature-256 is missing or not in GitHub's form")
		return
	}
	event, delivery := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	if !githubEventName.MatchString(event) || !githubDeliveryGUID.MatchString(delivery) {
		refuseGitHub(w, r, http.StatusBadRequest, "missing event or delivery id",
			"X-GitHub-Event or X-GitHub-Delivery is missing or not in GitHub's form")
		return
	}
	limit, listened := githubEventBodyLimit[event]
	if !listened {
		// Nothing would be kept whatever the body said, so it is not read. The answer is the one a
		// signed delivery of the event got before: there is nothing here to authenticate.
		ignoreGitHub(w, "not an event code review listens to")
		return
	}
	// A request that says how long it is is held to that, and reserved for exactly that; one that
	// does not (chunked) is reserved for the most its event may be.
	size := limit
	if n := r.ContentLength; n > limit {
		slog.Warn("GitHub delivery refused", "why", "the body is over the cap for its event", "event", event,
			"bytes", n, "cap", limit, "ip", clientIP(r))
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return
	} else if n >= 0 {
		size = n
	}
	if !b.ghHook.hold(size) {
		slog.Warn("GitHub delivery refused", "why", "too many delivery bodies held at once", "event", event,
			"bytes", size, "ip", clientIP(r))
		w.Header().Set("Retry-After", "30")
		http.Error(w, "GitHub delivery temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer b.ghHook.release(size)
	raw, err := readGitHubBody(w, r, size)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			slog.Warn("GitHub delivery refused", "why", "the body is over the cap for its event", "event", event, "cap", limit, "ip", clientIP(r))
			tooBig(w, err)
			return
		}
		refuseGitHub(w, r, http.StatusBadRequest, "could not read the payload", "body read failed: "+err.Error())
		return
	}
	if !b.ghHook.verify(raw, r.Header.Get("X-Hub-Signature-256")) {
		b.badGitHubSignature(w, r, "X-Hub-Signature-256 matches neither GITHUB_APP_WEBHOOK_SECRET nor _PREVIOUS: the secret set at GitHub is another one, or this did not come from GitHub")
		return
	}
	// The target is the App the delivery was sent for. A webhook secret reused on a second App,
	// or on a repository's own webhook, would otherwise have that one's events read as this one's.
	if id := b.ghHook.appID; id != "" && r.Header.Get("X-GitHub-Hook-Installation-Target-ID") != id {
		refuseGitHub(w, r, http.StatusBadRequest, "delivery is for another app",
			"X-GitHub-Hook-Installation-Target-ID is "+strconv.Quote(truncate(r.Header.Get("X-GitHub-Hook-Installation-Target-ID"), 32))+", not GITHUB_APP_ID")
		return
	}
	if event == "ping" {
		// Sent when the webhook is saved at GitHub. Worth a line, because it is the proof that the
		// URL and the secret agree, and the first thing to look for when nothing else arrives.
		slog.Info("GitHub webhook ping: the URL and the secret agree", "delivery", delivery)
		ignoreGitHub(w, "ping")
		return
	}
	var env githubEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		refuseGitHub(w, r, http.StatusBadRequest, "invalid payload", "the signed body is not a JSON event: "+err.Error())
		return
	}
	reviewed, drop := githubReceipt(event, &env)
	if reviewed && b.cfg.codeReviewOff() {
		// CODE_REVIEW=off: nothing here would ever read it. The installation's own events still go
		// through, since fix jobs and the bot's GitHub tools keep their installations by them.
		drop = "code review is off on this deployment"
	}
	if drop != "" {
		slog.Debug("GitHub delivery ignored", "why", drop, "event", event, "action", env.Action, "delivery", delivery)
		ignoreGitHub(w, drop)
		return
	}
	if env.Installation.ID <= 0 {
		slog.Warn("GitHub delivery ignored", "why", "it names no installation", "event", event, "delivery", delivery)
		ignoreGitHub(w, "no installation")
		return
	}

	sctx, scancel := context.WithTimeout(ctx, githubWebhookStoreDeadline)
	defer scancel()
	ins, err := b.store.GitHubInstall(sctx, env.Installation.ID)
	if err != nil {
		refuseGitHub(w, r, http.StatusServiceUnavailable, "GitHub delivery temporarily unavailable", "could not read the installation: "+err.Error())
		return
	}
	// Never bound to an organisation by a delivery. An installation is bound by the install flow,
	// which proves the person who installed it can see it (github_app.go); a webhook proves only
	// that the App was installed somewhere, and binding on that would hand an installation to
	// whichever organisation happened to be asked first.
	if ins == nil || ins.Status == "revoked" {
		why := "this installation is not connected to an organisation here"
		if ins != nil {
			why = "this installation was disconnected here"
		}
		// Loud for the installation's own events, which are rare and mean somebody installed the App
		// without finishing in the console; quiet for the pull-request traffic that follows.
		log := slog.Debug
		if !reviewed {
			log = slog.Info
		}
		log("GitHub delivery ignored", "why", why, "installation", env.Installation.ID, "event", event, "action", env.Action, "delivery", delivery)
		ignoreGitHub(w, why)
		return
	}
	if reviewed {
		// The repository's own effective mode, not just its installation's: a team that switched
		// one repository off — for compliance, say — on an installation with every repository in it
		// expects none of its pull requests written down here, and only the mode resolved down the
		// tree says that. The installation's binding was checked above.
		mode, err := b.store.ReviewModeAt(sctx, ins.OrgID, ins.ID, env.Repository.FullName)
		if err != nil {
			refuseGitHub(w, r, http.StatusServiceUnavailable, "GitHub delivery temporarily unavailable", "could not read the review settings: "+err.Error())
			return
		}
		why := ""
		switch mode {
		case "":
			why = "this installation is not reviewed"
		case review.ModeOff:
			why = "review is off for this repository"
		}
		if why != "" {
			slog.Debug("GitHub delivery ignored", "why", why, "org", ins.OrgID, "installation", ins.ID,
				"repo", env.Repository.FullName, "event", event, "action", env.Action, "delivery", delivery)
			ignoreGitHub(w, why)
			return
		}
	}
	enc, err := b.sealer.Seal(raw)
	fresh := false
	if err == nil {
		fresh, err = b.store.enqueueGitHubDelivery(sctx, delivery, ins.OrgID, ins.ID, event, env.Action, enc)
	}
	if err != nil {
		// 503 for a full inbox as for a database error: either way GitHub lists the delivery as
		// failed, and Redeliver brings it back once the backlog has drained.
		slog.Warn("GitHub delivery not accepted", "org", ins.OrgID, "installation", ins.ID, "event", event,
			"delivery", delivery, "inbox_full", errors.Is(err, errGitHubInboxFull), "err", err)
		http.Error(w, "GitHub delivery temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !fresh {
		// A redelivery: somebody pressed Redeliver, or a second attempt of ours raced the first.
		// Answered as the first was — the work is queued or done either way.
		slog.Info("GitHub redelivery", "delivery", delivery, "event", event, "org", ins.OrgID)
	} else {
		slog.Debug("GitHub delivery accepted", "delivery", delivery, "event", event, "action", env.Action,
			"org", ins.OrgID, "repo", env.Repository.FullName, "pr", env.prNumber())
	}
	w.WriteHeader(http.StatusOK)
}

// badGitHubSignature answers a request whose signature cannot be, or is not, right. Counted per
// address, as the Stripe webhook's are, and only here: a genuine delivery carries a valid
// signature by definition and is never counted, so only somebody guessing is slowed.
//
// The count is never consulted before a body is read, though that would save reading it. A
// stranger can point a repository webhook of their own at this URL with any secret they like, and
// GitHub would then deliver their failures from the same few addresses it delivers ours from;
// turning an address away up front would turn GitHub away, and lose real deliveries for an hour.
// What bounds the reading instead is githubEventBodyLimit and the in-flight budget.
func (b *Bot) badGitHubSignature(w http.ResponseWriter, r *http.Request, why string) {
	if ok, d := b.ghHook.badSigs.allow("github-bad-sig:"+clientIP(r), githubBadSignaturesPerHour, time.Hour); !ok {
		slog.Warn("GitHub delivery refused", "why", "too many bad signatures from this address", "ip", clientIP(r))
		tooMany(w, d)
		return
	}
	refuseGitHub(w, r, http.StatusUnauthorized, "invalid signature", why)
}

// readGitHubBody reads a body of at most size bytes. One whose length is declared is read into a
// buffer of exactly that length, so what is held is what was reserved; io.ReadAll's doubling would
// hold up to twice it.
func readGitHubBody(w http.ResponseWriter, r *http.Request, size int64) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, size)
	if r.ContentLength < 0 {
		return io.ReadAll(body)
	}
	raw := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(body, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ---- dispatching ----

// runGitHubDeliveries runs this instance's dispatchers until ctx ends. Not behind the leader lease,
// like the Slack inbox's: every claim takes its row under a lease of its own, so instances sharing
// the queue spread the work rather than repeat it.
func (b *Bot) runGitHubDeliveries(ctx context.Context) {
	var wg sync.WaitGroup
	for range githubDispatchers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if !b.dispatchNextGitHubDelivery(ctx) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(githubIdlePoll):
					}
				}
			}
		}()
	}
	// Receipts are kept a week, for the dedup; payloads go the moment a delivery is done.
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
			if err := b.store.purgeGitHubDeliveries(ctx); err != nil {
				slog.Warn("GitHub receipt cleanup", "err", err)
			}
		}
	}
}

// dispatchNextGitHubDelivery claims one delivery and does what it asks. It reports whether there
// was one, so an idle dispatcher waits before asking again and a busy one does not.
func (b *Bot) dispatchNextGitHubDelivery(ctx context.Context) bool {
	d, err := b.store.claimGitHubDelivery(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("GitHub delivery claim", "err", err)
		}
		return false
	}
	if d == nil {
		return false
	}
	workCtx, cancel := context.WithTimeout(ctx, githubDispatchTimeout)
	defer cancel()

	// The lease is a minute and kept alive while the work runs, so the delivery is not handed to a
	// second dispatcher halfway through. A touch that finds the lease gone means another dispatcher
	// has the delivery: the work here is stopped, and nothing is finished or failed on its behalf.
	var lost atomic.Bool
	held := make(chan struct{})
	go func() {
		defer close(held)
		t := time.NewTicker(githubDeliveryTouchEvery)
		defer t.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-t.C:
				// Not workCtx: a touch the cancellation interrupted after the row was written would
				// leave d.Lease behind the database, and the finish fenced on it would fail.
				if err := b.store.touchGitHubDelivery(context.WithoutCancel(ctx), d); errors.Is(err, errLeaseLost) {
					lost.Store(true)
					cancel()
					return
				} else if err != nil {
					slog.Warn("GitHub delivery lease", "delivery", d.ID, "err", err)
				}
			}
		}
	}()
	raw, err := b.sealer.Open(d.Payload)
	if err == nil {
		err = routeGitHubDelivery(b, workCtx, d, raw)
	}
	timedOut := errors.Is(workCtx.Err(), context.DeadlineExceeded)
	cancel()
	<-held // the holder has stopped, so d.Lease is not being written while the fenced writes read it

	if lost.Load() {
		slog.Warn("GitHub delivery lease lost; another dispatcher has it", "delivery", d.ID, "event", d.Event)
		return true
	}
	if err == nil && timedOut {
		err = errors.New("timed out")
	}
	// Not ctx for either write: during shutdown it is already cancelled, and the outcome would be
	// recorded nowhere, leaving the row to sit out its lease with no reason against it.
	wctx := context.WithoutCancel(ctx)
	if err != nil {
		if d.Attempts >= githubDeliveryMaxAttempts {
			slog.Error("GitHub delivery dead-lettered", "delivery", d.ID, "org", d.OrgID, "event", d.Event,
				"action", d.Action, "attempts", d.Attempts, "err", err)
		} else {
			slog.Warn("GitHub delivery deferred", "delivery", d.ID, "event", d.Event, "attempt", d.Attempts, "err", err)
		}
		if err := b.store.failGitHubDelivery(wctx, d, err.Error()); err != nil {
			slog.Warn("GitHub delivery failure record", "delivery", d.ID, "err", err)
		}
		return true
	}
	if err := b.store.finishGitHubDelivery(wctx, d); err != nil {
		slog.Warn("GitHub delivery completion", "delivery", d.ID, "err", err)
	}
	return true
}

// dispatchGitHubDelivery routes one opened delivery by its event. An error puts it back to be
// tried again later; nil finishes it, which is also the answer for a delivery there is nothing to
// do about — trying that one again would find nothing more to do.
func (b *Bot) dispatchGitHubDelivery(ctx context.Context, d *githubDelivery, raw []byte) error {
	switch d.Event {
	case "installation", "installation_repositories":
		return b.githubInstallationEvent(ctx, d, raw)
	case "pull_request":
		if b.review == nil {
			// A process with no review engine has nothing to start a review with. Finishing the
			// delivery keeps the inbox from filling with work nothing here will take.
			slog.Debug("pull_request delivery received; code review is not running here", "delivery", d.ID, "org", d.OrgID, "action", d.Action)
			return nil
		}
		return b.reviewPullRequestEvent(ctx, d, raw)
	case "issue_comment", "pull_request_review", "pull_request_review_comment", "pull_request_review_thread":
		if b.review == nil {
			slog.Debug("comment delivery received; code review is not running here", "delivery", d.ID, "org", d.OrgID, "event", d.Event)
			return nil
		}
		switch d.Event {
		case "issue_comment":
			// A command to the bot (review_commands.go).
			return b.reviewIssueCommentEvent(ctx, d, raw)
		case "pull_request_review_comment":
			// A reply in one of our findings' threads, queued to be answered (review_replies.go).
			return b.reviewCommentReplyEvent(ctx, d, raw)
		case "pull_request_review":
			return b.reviewSubmittedEvent(ctx, d, raw)
		default:
			return b.reviewThreadEvent(ctx, d, raw)
		}
	}
	slog.Warn("GitHub delivery of an event nothing handles", "delivery", d.ID, "event", d.Event)
	return nil
}

// githubInstallationPayload is what the installation and installation_repositories events say.
// Those two, and only those, carry the whole installation record — its app_id, its account, its
// permissions; every other event names the installation by id and nothing more.
type githubInstallationPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ghInstallation
		AppID int64 `json:"app_id"`
	} `json:"installation"`
	RepositorySelection string          `json:"repository_selection"`
	RepositoriesAdded   []githubRepoRef `json:"repositories_added"`
	RepositoriesRemoved []githubRepoRef `json:"repositories_removed"`
	Sender              struct {
		Login string `json:"login"`
	} `json:"sender"`
}

type githubRepoRef struct {
	FullName string `json:"full_name"`
}

func githubRepoNames(refs []githubRepoRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.FullName)
	}
	return out
}

// githubInstallationEvent keeps the stored installation in step with GitHub: what it was granted,
// whether it is suspended, whether it still exists. Every write names the organisation the
// delivery was accepted for, so one that has since lost the installation is simply not matched.
func (b *Bot) githubInstallationEvent(ctx context.Context, d *githubDelivery, raw []byte) error {
	var p githubInstallationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	// The event name was a header, which the signature does not cover. A real installation event
	// carries the full installation record, app_id and all; a pull-request or comment body resent
	// under this name carries only {id}, and an "action": "deleted" in one of those must not
	// revoke anything.
	if p.Installation.ID != d.InstallationID || p.Installation.AppID == 0 ||
		(b.ghHook != nil && b.ghHook.appID != "" && strconv.FormatInt(p.Installation.AppID, 10) != b.ghHook.appID) {
		slog.Warn("GitHub delivery ignored: its payload is not this App's installation record",
			"delivery", d.ID, "event", d.Event, "installation", d.InstallationID, "app_id", p.Installation.AppID)
		return nil
	}
	id, org := d.InstallationID, d.OrgID
	if d.Event == "installation_repositories" {
		slog.Info("github installation repositories changed", "org", org, "installation", id,
			"selection", p.RepositorySelection, "added", githubRepoNames(p.RepositoriesAdded),
			"removed", githubRepoNames(p.RepositoriesRemoved))
		// "Every repository" or "the ones its admin chose" is what the console labels the
		// installation with, and an owner can switch it at GitHub at any time.
		if err := b.store.SetGitHubInstallSelection(ctx, org, id, p.RepositorySelection); err != nil {
			return err
		}
		// The repositories themselves are not filed as connections here, the way
		// connectInstalledRepos does at install time: that path calls GitHub with the App's key and
		// attributes the new connections to the person who installed, and a webhook knows neither
		// who that is nor whether the App's key is configured. Review needs no connection per
		// repository — a repository the installation gains is reviewed under its connection's
		// settings at once.
		return nil
	}
	audit := func(action string) {
		b.record(ctx, AuditEvent{OrgID: org, Via: viaSystem, Action: action, ActorName: "GitHub",
			TargetKind: "github_installation", TargetID: strconv.FormatInt(id, 10), TargetName: p.Installation.Account.Login,
			Details: auditDetails(map[string]any{"sender": p.Sender.Login, "delivery": d.ID})})
	}
	switch p.Action {
	case "created", "new_permissions_accepted":
		if len(p.Installation.Permissions) == 0 {
			slog.Warn("GitHub installation delivery names no permissions", "delivery", d.ID, "installation", id)
			return nil
		}
		perms, err := json.Marshal(p.Installation.Permissions)
		if err != nil {
			return err
		}
		if err := b.store.SetGitHubInstallPermissions(ctx, org, id, string(perms)); err != nil {
			return err
		}
		if err := b.store.SetGitHubInstallSelection(ctx, org, id, p.Installation.RepositorySelection); err != nil {
			return err
		}
		// A token minted before carries the old grant, and the cache would hand it out for up to
		// an hour. Only this instance's cache is cleared; another's tokens are minted with the fixed
		// permission set of their purpose (github_token.go), so a stale one lacks nothing it asks for.
		b.proxy.forgetInstallToken(id)
		if p.Action == "new_permissions_accepted" {
			audit("github_install.permissions_accepted")
		}
		slog.Info("github installation permissions", "org", org, "installation", id, "action", p.Action, "permissions", string(perms))
	case "deleted":
		if err := b.store.RevokeGitHubInstall(ctx, org, id, "uninstalled at GitHub"); err != nil {
			return err
		}
		b.proxy.forgetInstallToken(id)
		audit("github_install.uninstalled")
		slog.Info("github installation uninstalled at GitHub", "org", org, "installation", id, "account", p.Installation.Account.Login)
	case "suspend":
		at := p.Installation.SuspendedAt
		if at == "" {
			at = time.Now().UTC().Format(time.RFC3339)
		}
		if err := b.store.SuspendGitHubInstall(ctx, org, id, at); err != nil {
			return err
		}
		b.proxy.forgetInstallToken(id)
		audit("github_install.suspended")
		slog.Info("github installation suspended at GitHub", "org", org, "installation", id)
	case "unsuspend":
		if err := b.store.UnsuspendGitHubInstall(ctx, org, id); err != nil {
			return err
		}
		b.proxy.forgetInstallToken(id)
		audit("github_install.unsuspended")
		slog.Info("github installation unsuspended at GitHub", "org", org, "installation", id)
	default:
		slog.Debug("GitHub installation delivery with nothing to do", "delivery", d.ID, "action", p.Action)
	}
	return nil
}
