package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
)

// modelEndpoints decides where a model call made for an organisation goes: the deployment's own
// endpoint, or the one the organisation brought (model_keys.go). It is the only way anything in
// this package reaches a model — TestNothingReachesTheDeploymentModelDirectly holds that — so an
// organisation's own key cannot be forgotten by one call site and spent around by it.
//
// What it answers comes from the settings cache, which already carries whether a key is stored,
// whether the plan allows it and which key it is. A client is built the first time an
// organisation's endpoint is asked for and kept until those facts change; that is also how a key
// rotated on another instance reaches this one, within the fifteen seconds the cache lives.
type modelEndpoints struct {
	cfg      Config
	platform *LLM
	store    *Store
	sealer   *Sealer
	settings *settingsCache
	// transport carries an organisation's calls; nil is orgTransport(). Tests swap in one that
	// reaches a server on loopback, which the real one refuses by design.
	transport http.RoundTripper
	// alert reaches the organisation's alert channel when its key starts failing. Wired to
	// Agent.alert at boot; nil where nobody is listening.
	alert func(ctx context.Context, orgID int64, key, text string)

	mu    sync.Mutex
	byOrg map[int64]*orgEndpoint
	noted map[int64]keyNote
}

type orgEndpoint struct {
	ref    ModelKeyRef
	client *LLM
}

// keyNote is the last outcome written for an organisation's key, so a busy endpoint writes its
// status once a minute rather than once a call.
type keyNote struct {
	at      time.Time
	problem string
}

func newModelEndpoints(cfg Config, platform *LLM, st *Store, sealer *Sealer, sc *settingsCache) *modelEndpoints {
	return &modelEndpoints{cfg: cfg, platform: platform, store: st, sealer: sealer, settings: sc,
		byOrg: map[int64]*orgEndpoint{}, noted: map[int64]keyNote{}}
}

// For returns the endpoint an organisation's model calls go to. An error is a sentence a person
// can act on (ModelKeyError), and never a reason to try the deployment's endpoint instead.
func (e *modelEndpoints) For(ctx context.Context, orgID int64) (*LLM, error) {
	k := e.settings.Get(ctx, orgID).OwnKey
	if err := k.refusal(); err != nil {
		return nil, err
	}
	if !k.Present {
		return e.platform, nil
	}
	e.mu.Lock()
	cached := e.byOrg[orgID]
	e.mu.Unlock()
	if cached != nil && cached.ref.sameEndpoint(k.Ref) {
		return cached.client, nil
	}
	row, key, err := e.store.ModelKeyRow(ctx, orgID, e.sealer)
	switch {
	case errors.Is(err, errModelKeyUnreadable):
		slog.Error("organisation model key cannot be opened", "org", orgID, "err", err)
		return nil, &ModelKeyError{Kind: keyUnreadable, Host: row.Host()}
	case err != nil:
		return nil, &ModelKeyError{Kind: keyUnavailable}
	case row == nil:
		// Removed since the settings were read, on another instance most likely. Those settings
		// are what let this call skip the deployment's limits, so it is refused once rather than
		// sent to the deployment's key past them; the next call reads afresh.
		e.settings.Invalidate(orgID)
		return nil, &ModelKeyError{Kind: keyUnavailable}
	}
	if !row.sameEndpoint(k.Ref) {
		// Saved again since the settings were read. The client is built from the row — the key and
		// the address it was saved for, from one read — and the settings catch up on the next call.
		e.settings.Invalidate(orgID)
	}
	l := e.build(orgID, *row, key)
	e.mu.Lock()
	e.byOrg[orgID] = &orgEndpoint{ref: *row, client: l}
	e.mu.Unlock()
	return l, nil
}

// Evict forgets an organisation's client, so the next call reads its endpoint afresh. The
// console calls it on every save (Bot.changed); other instances notice through the settings
// cache instead.
func (e *modelEndpoints) Evict(orgID int64) {
	e.mu.Lock()
	delete(e.byOrg, orgID)
	e.mu.Unlock()
}

// build makes an organisation's client. Its catalogue is its own, it is priced from the
// deployment's catalogue when the provider reports no charge, and what it says about each call
// reaches note.
func (e *modelEndpoints) build(orgID int64, ref ModelKeyRef, key string) *LLM {
	tr := e.transport
	if tr == nil {
		tr = orgTransport()
	}
	l := newLLM(e.cfg, endpoint{BaseURL: ref.BaseURL, Key: key, Dialect: dialectFor(ref.BaseURL),
		Model: ref.DefaultModel, EmbedModel: ref.EmbedModel,
		HTTPClient: &http.Client{Transport: tr, CheckRedirect: rejectRedirect}})
	l.own = true
	l.pricer = e.platform.priceOf
	l.report = func(problem string) { e.note(orgID, ref.Host(), problem) }
	return l
}

// orgTransport carries a model call to an address an organisation typed in. It is the transport
// every other tenant-supplied URL goes through (outbound.go) — public addresses only, decided at
// dial time — with two differences: https only, because the key rides in a header, and no limit on
// how long the response headers take. A completion is not streamed, so its headers arrive with the
// whole answer, and the twenty seconds publicTransport allows would end every long one; the call's
// own deadline is what bounds it instead.
func orgTransport() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = dialPublic
	tr.TLSHandshakeTimeout = 10 * time.Second
	return httpsOnly{publicRoundTripper{tr}}
}

type httpsOnly struct{ base http.RoundTripper }

func (t httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, errors.New("a model endpoint must be https: the key travels with every request")
	}
	return t.base.RoundTrip(r)
}

// validModelBaseURL checks an address before anything is sent to it: https, a public host, no
// credentials in the URL. The transport checks the address it actually dials again, on every call.
func validModelBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("the base URL must be a full address, such as https://api.openai.com/v1")
	}
	if u.Scheme != "https" {
		return errors.New("the base URL must start with https:// — the key travels with every request")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the base URL takes no query string: the provider's own path, such as /v1, and nothing after it")
	}
	return publicURL(u)
}

// note records how a call on an organisation's own endpoint went: at most once a minute while
// nothing changes, and at once when it goes from working to failing or back. The first failure
// is also said in the organisation's alert channel, once an hour at most.
func (e *modelEndpoints) note(orgID int64, host, problem string) {
	e.mu.Lock()
	last, seen := e.noted[orgID]
	if seen && last.problem == problem && time.Since(last.at) < time.Minute {
		e.mu.Unlock()
		return
	}
	e.noted[orgID] = keyNote{at: time.Now(), problem: problem}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.store.MarkModelKeyStatus(ctx, orgID, problem); err != nil {
		slog.Warn("could not record the model key's status", "org", orgID, "err", err)
	}
	if problem != "" && e.alert != nil && (!seen || last.problem == "") {
		e.alert(ctx, orgID, "model_key", fmt.Sprintf(":warning: This organisation's own model endpoint (%s) is failing, so the bot cannot answer: %s", host, problem))
	}
}

// ModelKeyError is a model call that the organisation's own endpoint could not make, in words a
// person can act on. It is what a Slack thread is told, so it names what went wrong and where it
// is fixed, and carries nothing a provider said that redact would not let through.
type ModelKeyError struct {
	Kind   string
	Host   string
	Model  string
	Status int
	Detail string
}

const (
	keyNotAllowed  = "not_allowed" // stored, and the plan no longer includes it
	keyUnavailable = "unavailable" // the settings could not be read just now
	keyUnreadable  = "unreadable"  // stored, and cannot be opened with this MASTER_KEY
	keyRefused     = "refused"     // 401/403
	keyQuota       = "quota"       // the provider account is out of credit or quota
	keyRateLimited = "rate_limited"
	keyNoModel     = "model"       // the endpoint does not serve the model named
	keyUnreachable = "unreachable" // DNS, TLS, a refused connection, a non-public address
	keyFailed      = "failed"      // anything else the endpoint answered
)

func (e *ModelKeyError) Error() string {
	where := "this organisation's model endpoint"
	if e.Host != "" {
		where = "the model endpoint at " + e.Host
	}
	said := ""
	if e.Detail != "" {
		said = " (" + e.Detail + ")"
	}
	switch e.Kind {
	case keyNotAllowed:
		return "this organisation's own model key is not part of its plan any more, so nothing can be sent to a model. An admin can remove the key under Settings → Models to use the included models"
	case keyUnavailable:
		return "this organisation's model settings could not be read just now. Try again in a minute"
	case keyUnreadable:
		return "this organisation's model key can no longer be read here. An admin can paste it again under Settings → Models"
	case keyRefused:
		return where + " refused this organisation's key" + said + ". An admin can replace it under Settings → Models"
	case keyQuota:
		return "this organisation's account at " + orDefault(e.Host, "its model provider") + " is out of credit or quota" + said + ". Top it up with the provider, or replace the key under Settings → Models"
	case keyRateLimited:
		return where + " is rate-limiting this organisation's key" + said + ". Try again in a minute"
	case keyNoModel:
		return where + " does not serve the model " + fmt.Sprintf("%q", e.Model) + " on this organisation's key" + said + ". An admin can choose another under Settings → Models"
	case keyUnreachable:
		return where + " could not be reached" + said + ". An admin can check it under Settings → Models"
	}
	return fmt.Sprintf("%s answered %d%s", where, e.Status, said)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// classifyModelError reads a failed call on an organisation's own endpoint. Nil for a call that
// was cancelled or ran out of time on our side: that is not the endpoint's doing, and saying it
// was would send somebody to replace a key that works.
func classifyModelError(host, model string, err error) *ModelKeyError {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	var api *openai.Error
	if errors.As(err, &api) {
		detail := strings.TrimSpace(api.Message)
		if detail == "" {
			detail = http.StatusText(api.StatusCode)
		}
		me := &ModelKeyError{Host: host, Model: model, Status: api.StatusCode,
			Detail: fmt.Sprintf("%d: %s", api.StatusCode, truncate(redact(oneLine(detail)), 200))}
		code := strings.ToLower(api.Code + " " + api.Type)
		switch {
		case api.StatusCode == http.StatusUnauthorized || api.StatusCode == http.StatusForbidden:
			me.Kind = keyRefused
		case api.StatusCode == http.StatusPaymentRequired || strings.Contains(code, "insufficient_quota") || strings.Contains(code, "billing"):
			me.Kind = keyQuota
		case api.StatusCode == http.StatusTooManyRequests:
			me.Kind = keyRateLimited
		case api.StatusCode == http.StatusNotFound || strings.Contains(code, "model_not_found") || strings.Contains(code, "deploymentnotfound"):
			me.Kind = keyNoModel
		default:
			me.Kind = keyFailed
		}
		return me
	}
	detail := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		detail = ue.Err.Error()
	}
	return &ModelKeyError{Kind: keyUnreachable, Host: host, Model: model, Detail: truncate(redact(oneLine(detail)), 200)}
}

// ---- the Agent's side ----

// llmFor is the endpoint an organisation's model calls go to. An Agent assembled without the
// resolver — the tests that predate it — answers on the deployment's endpoint, unless the settings
// say the organisation brought a key, in which case nothing may be called at all.
func (a *Agent) llmFor(ctx context.Context, orgID int64) (*LLM, error) {
	if a.endpoints != nil {
		return a.endpoints.For(ctx, orgID)
	}
	if a.settings != nil {
		if k := a.settings.Get(ctx, orgID).OwnKey; k.Present || k.Unknown {
			return nil, &ModelKeyError{Kind: keyUnavailable}
		}
	}
	return a.llm, nil
}

// llmOf is llmFor once per call. Lazily, rather than at the top of the turn, because a routine's
// pinned steps and the thread summariser both run before the turn chooses its model, and both may
// need one.
func (a *Agent) llmOf(ctx context.Context, c *Call) (*LLM, error) {
	if c.llm != nil {
		return c.llm, nil
	}
	l, err := a.llmFor(ctx, c.OrgID)
	if err != nil {
		return nil, err
	}
	c.llm = l
	return l, nil
}

// embedderFor is the endpoint an organisation's documents are embedded on. An organisation's own
// endpoint with no embedding model has document search switched off, which is a refusal, not a
// reason to embed its documents somewhere else.
func (ix *Indexer) embedderFor(ctx context.Context, orgID int64) (*LLM, error) {
	l := ix.llm
	if ix.endpoints != nil {
		var err error
		if l, err = ix.endpoints.For(ctx, orgID); err != nil {
			return nil, err
		}
	}
	if l == nil {
		return nil, errors.New("no model endpoint is configured")
	}
	if l.own && l.EmbedModel == "" {
		return nil, errDocSearchOff
	}
	return l, nil
}

var errDocSearchOff = errors.New("document search is off: this organisation's own model endpoint has no embedding model. An admin can choose one under Settings → Models")
