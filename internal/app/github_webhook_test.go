package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"attesttag/guide"
)

// The webhook is unauthenticated except for its signature, and it decides which tenant's pull
// requests are ever written down. These go through the real route — githubAppRoutes on a mux — so
// a handler that drifted behind requireAdmin, or off the route, fails here rather than at GitHub.
// They run on both dialects.

const (
	hookSecret = "webhook-secret-for-tests"
	hookAppID  = "424242"
	hookInst   = int64(1001)
)

func webhookBot(t *testing.T, current, previous, appID string) *Bot {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	st := testStore(t)
	return &Bot{store: st, sealer: sealer, proxy: NewProxy(sealer, st), ghHook: newGitHubWebhookFrom(current, previous, appID)}
}

func ghSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ghPost sends one delivery through the route, signed with hookSecret and addressed to hookAppID
// unless edit says otherwise.
func ghPost(b *Bot, event, delivery string, body []byte, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/github/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", ghSign(hookSecret, body))
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", hookAppID)
	for _, e := range edit {
		e(req)
	}
	mux := http.NewServeMux()
	b.githubAppRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func prBody(installation int64, action, senderType string) []byte {
	return []byte(fmt.Sprintf(`{"action":%q,"number":7,
		"installation":{"id":%d,"node_id":"MDIz"},
		"repository":{"full_name":"acme/web","private":true},
		"sender":{"login":"someone","type":%q},
		"pull_request":{"number":7,"draft":false,"title":"PLAINTEXT-MARKER retry the upload","user":{"login":"someone","type":"User"}}}`,
		action, installation, senderType))
}

func issueCommentBody(installation int64, senderType string, onPR bool) []byte {
	pr := ""
	if onPR {
		pr = `,"pull_request":{"url":"https://api.github.com/repos/acme/web/pulls/7"}`
	}
	return []byte(fmt.Sprintf(`{"action":"created",
		"installation":{"id":%d},
		"repository":{"full_name":"acme/web","private":true},
		"sender":{"login":"someone","type":%q},
		"issue":{"number":7%s},
		"comment":{"id":99,"body":"@attest-tag review"}}`, installation, senderType, pr))
}

func installationBody(installation, appID int64, action string, perms map[string]string) []byte {
	p, _ := json.Marshal(perms)
	return []byte(fmt.Sprintf(`{"action":%q,
		"installation":{"id":%d,"app_id":%d,"account":{"login":"acme","id":5,"type":"Organization"},
			"repository_selection":"selected","permissions":%s,"events":["pull_request"],"suspended_at":null},
		"sender":{"login":"acme-admin","type":"User"}}`, action, installation, appID, p))
}

func deliveryCount(t *testing.T, b *Bot) int {
	t.Helper()
	var n int
	if err := b.store.db.QueryRow(`select count(*) from github_deliveries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int, what string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: status %d, want %d (body %q)", what, w.Code, status, w.Body.String())
	}
}

// reviewedInstall binds hookInst to org 1 and adds it to the review tree.
func reviewedInstall(t *testing.T, b *Bot) {
	t.Helper()
	bindInstall(t, b.store, 1, hookInst, "acme")
	addConnection(t, b.store, 1, hookInst)
}

// GitHub's own example from "Validating webhook deliveries". The expected signature is written out
// rather than computed, so a verifier that agrees with itself and not with GitHub fails.
func TestGitHubWebhookSignatureMatchesGitHubsTestVector(t *testing.T) {
	const secret, payload = "It's a Secret to Everybody", "Hello, World!"
	const sig = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	h := newGitHubWebhookFrom(secret, "", "")
	if !h.verify([]byte(payload), sig) {
		t.Fatal("GitHub's documented signature does not verify")
	}
	for _, bad := range []string{
		strings.ToUpper(sig[:7]) + sig[7:], // the prefix is case-sensitive at GitHub too
		sig[len("sha256="):],               // no prefix
		"sha1=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17",
		sig[:len(sig)-1] + "8",
		sig[:len(sig)-2],
		"",
	} {
		if h.verify([]byte(payload), bad) {
			t.Errorf("%q verified", bad)
		}
	}

	// And through the real handler. The body is not JSON, so a ping — which is answered before the
	// body is decoded — is the event that shows the signature passing on its own.
	b := webhookBot(t, secret, "", "")
	sign := func(s string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", s) }
	}
	w := ghPost(b, "ping", "6f1c1b0e-0000-4000-8000-000000000001", []byte(payload), sign(sig))
	wantStatus(t, w, 200, "GitHub's test vector as a ping")
	w = ghPost(b, "pull_request", "6f1c1b0e-0000-4000-8000-000000000002", []byte(payload), sign(sig))
	wantStatus(t, w, 400, "GitHub's test vector as a pull_request (verified, then not JSON)")
	w = ghPost(b, "ping", "6f1c1b0e-0000-4000-8000-000000000003", []byte(payload), sign(sig[:len(sig)-1]+"8"))
	wantStatus(t, w, 401, "one hex digit off")
}

func TestGitHubWebhookAcceptsThePreviousSecretDuringARotation(t *testing.T) {
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	signedWith := func(s string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", ghSign(s, body)) }
	}

	b := webhookBot(t, "new-secret", "old-secret", hookAppID)
	wantStatus(t, ghPost(b, "ping", "d-new", body, signedWith("new-secret")), 200, "signed with the current secret")
	wantStatus(t, ghPost(b, "ping", "d-old", body, signedWith("old-secret")), 200, "signed with the previous secret")
	wantStatus(t, ghPost(b, "ping", "d-other", body, signedWith("some-other-secret")), 401, "signed with neither")

	// Once the previous one is unset it is retired, and so is anything still signed with it.
	b.ghHook = newGitHubWebhookFrom("new-secret", "", hookAppID)
	wantStatus(t, ghPost(b, "ping", "d-old-2", body, signedWith("old-secret")), 401, "the retired secret")

	// A previous secret on its own is half a rotation, not a configuration.
	if newGitHubWebhookFrom("", "old-secret", hookAppID).configured() {
		t.Error("a previous secret with no current one counts as configured")
	}
}

func TestGitHubWebhookBadSignaturesAreRefusedAndThrottled(t *testing.T) {
	b := webhookBot(t, hookSecret, "", hookAppID)
	body := []byte(`{"zen":"Design for failure."}`)
	forged := func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", ghSign("guessed", body)) }
	for i := range githubBadSignaturesPerHour {
		wantStatus(t, ghPost(b, "ping", fmt.Sprintf("bad-%d", i), body, forged), 401, fmt.Sprintf("bad signature %d", i+1))
	}
	w := ghPost(b, "ping", "bad-over", body, forged)
	wantStatus(t, w, http.StatusTooManyRequests, "one bad signature over the hourly budget")
	if w.Header().Get("Retry-After") == "" {
		t.Error("a throttled answer without Retry-After")
	}
	// Only failures are counted, and a throttle that turned GitHub away would lose events for
	// good: a correctly signed delivery from the same address still goes through.
	wantStatus(t, ghPost(b, "ping", "good-after", body), 200, "a valid delivery from a throttled address")
	// No header at all is a bad signature like any other, not a crash.
	w = ghPost(b, "ping", "no-header", body, func(r *http.Request) { r.Header.Del("X-Hub-Signature-256") })
	wantStatus(t, w, http.StatusTooManyRequests, "no signature from the throttled address")
}

func TestGitHubWebhookRefusesDeliveriesForAnotherApp(t *testing.T) {
	b := webhookBot(t, hookSecret, "", hookAppID)
	body := []byte(`{"zen":"Approachable is better than simple."}`)
	target := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("X-GitHub-Hook-Installation-Target-ID", v) }
	}
	wantStatus(t, ghPost(b, "ping", "t-1", body, target("99999")), 400, "another app's target id")
	wantStatus(t, ghPost(b, "ping", "t-2", body, func(r *http.Request) { r.Header.Del("X-GitHub-Hook-Installation-Target-ID") }), 400, "no target id")
	wantStatus(t, ghPost(b, "ping", "t-3", body), 200, "this app's target id")

	// With no GITHUB_APP_ID there is nothing to compare with, and the check is skipped.
	b.ghHook = newGitHubWebhookFrom(hookSecret, "", "")
	wantStatus(t, ghPost(b, "ping", "t-4", body, target("99999")), 200, "any target id when no app id is configured")
}

func TestGitHubWebhookRefusesWhatItCannotCheck(t *testing.T) {
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b)

	// Over GitHub's own 25 MB cap: no genuine delivery is that large, even an installation's.
	huge := bytes.Repeat([]byte("a"), githubWebhookBodyLimit+1)
	wantStatus(t, ghPost(b, "installation", "big-1", huge), http.StatusRequestEntityTooLarge, "an installation body over 25 MiB")
	// A pull request's delivery is held to its own, smaller cap, whether its length is declared or
	// it arrives chunked and the cap is found while reading.
	big := bytes.Repeat([]byte("a"), githubPRBodyLimit+1)
	wantStatus(t, ghPost(b, "pull_request", "big-2", big), http.StatusRequestEntityTooLarge, "a pull request body over its cap")
	wantStatus(t, ghPost(b, "issue_comment", "big-3", big, func(r *http.Request) { r.ContentLength = -1 }),
		http.StatusRequestEntityTooLarge, "a chunked comment body over its cap")

	body := prBody(hookInst, "opened", "User")
	wantStatus(t, ghPost(b, "pull_request", "", body), 400, "no delivery id")
	wantStatus(t, ghPost(b, "", "h-1", body), 400, "no event")
	wantStatus(t, ghPost(b, "pull_request", "not a guid; drop table", body), 400, "a delivery id that is not a GUID")

	// No secret: nothing can be verified, so nothing is taken.
	for _, hook := range []*githubWebhook{nil, newGitHubWebhookFrom("", "", hookAppID)} {
		b.ghHook = hook
		wantStatus(t, ghPost(b, "pull_request", "h-2", body), http.StatusServiceUnavailable, "no webhook secret")
	}
	if n := deliveryCount(t, b); n != 0 {
		t.Fatalf("%d deliveries stored from requests that were all refused", n)
	}
}

// unreadBody stands in for a request body that must not be read: the request is to be answered
// from its headers alone.
type unreadBody struct {
	io.Reader
	read *atomic.Bool
}

func (u unreadBody) Read(p []byte) (int, error) {
	u.read.Store(true)
	return u.Reader.Read(p)
}

func (u unreadBody) Close() error { return nil }

// The body is the one cost an unsigned caller can impose, so what can be decided from the headers
// is decided before a byte of it is read.
func TestGitHubWebhookAnswersFromTheHeadersWhatItCan(t *testing.T) {
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b)
	body := prBody(hookInst, "opened", "User")
	for i, c := range []struct {
		name, event string
		status      int
		edit        func(*http.Request)
	}{
		{"no signature", "pull_request", http.StatusUnauthorized, func(r *http.Request) { r.Header.Del("X-Hub-Signature-256") }},
		{"a signature not in GitHub's form", "pull_request", http.StatusUnauthorized, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256=abc") }},
		{"an event nobody listens to", "push", http.StatusOK, nil},
		{"a declared length over the event's cap", "pull_request", http.StatusRequestEntityTooLarge,
			func(r *http.Request) { r.ContentLength = githubPRBodyLimit + 1 }},
	} {
		var read atomic.Bool
		w := ghPost(b, c.event, fmt.Sprintf("hdr-%d", i), body, func(r *http.Request) {
			r.Body = unreadBody{Reader: bytes.NewReader(body), read: &read}
			if c.edit != nil {
				c.edit(r)
			}
		})
		wantStatus(t, w, c.status, c.name)
		if read.Load() {
			t.Errorf("%s: the body was read before the request was answered", c.name)
		}
	}
	if n := deliveryCount(t, b); n != 0 {
		t.Fatalf("%d deliveries stored from requests answered from their headers", n)
	}
}

// Every body held at once is bounded across the process, so twenty unsigned bodies arriving
// together cannot take the instance — and Slack and the console with it — down.
func TestGitHubWebhookBoundsTheBodiesHeldAtOnce(t *testing.T) {
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b)
	body := prBody(hookInst, "opened", "User")
	b.ghHook.inflightMax = int64(len(body)) - 1
	w := ghPost(b, "pull_request", "inflight-1", body)
	wantStatus(t, w, http.StatusServiceUnavailable, "a delivery past the in-flight budget")
	if w.Header().Get("Retry-After") == "" {
		t.Error("a full budget answered without Retry-After")
	}
	b.ghHook.inflightMax = int64(len(body))
	wantStatus(t, ghPost(b, "pull_request", "inflight-2", body), 200, "a delivery that fits")
	wantStatus(t, ghPost(b, "pull_request", "inflight-3", body), 200, "the next one, after the first gave its bytes back")
	if n := b.ghHook.inflight.Load(); n != 0 {
		t.Errorf("%d bytes still held after every request was answered", n)
	}
	// A chunked body, with no length to reserve by, is reserved at its event's cap.
	b.ghHook.inflightMax = githubPRBodyLimit - 1
	wantStatus(t, ghPost(b, "pull_request", "inflight-4", body, func(r *http.Request) { r.ContentLength = -1 }),
		http.StatusServiceUnavailable, "a chunked delivery reserved at its cap")
}

// A repository switched off is not written down, though its installation is reviewed; and one
// switched on under a connection that is off is.
func TestGitHubWebhookStoresOnlyRepositoriesThatAreReviewed(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b) // the connection is in shadow
	conn := mustTreeNode(t, b.store, 1, func(r *ReviewSetting) bool { return r.Kind == reviewKindConnection })
	web, err := b.store.EnsureReviewRepo(ctx, 1, conn.ID, "acme/web", "admin@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.store.UpdateReviewSettings(ctx, 1, web.ID, json.RawMessage(`{"mode":"off"}`), "admin@acme.test"); err != nil {
		t.Fatal(err)
	}
	w := ghPost(b, "pull_request", "m-1", prBody(hookInst, "opened", "User"))
	wantStatus(t, w, 200, "a pull request on a repository switched off")
	if !strings.Contains(w.Body.String(), "review is off for this repository") {
		t.Errorf("the answer does not say why: %q", w.Body.String())
	}
	wantStatus(t, ghPost(b, "issue_comment", "m-2", issueCommentBody(hookInst, "User", true)), 200, "a command on it")
	if n := deliveryCount(t, b); n != 0 {
		t.Fatalf("%d deliveries stored for a repository whose reviews are off", n)
	}

	// The connection off, the repository on: the nearest level wins.
	if err := b.store.UpdateReviewSettings(ctx, 1, conn.ID, json.RawMessage(`{"mode":"off"}`), "admin@acme.test"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.UpdateReviewSettings(ctx, 1, web.ID, json.RawMessage(`{"mode":"shadow"}`), "admin@acme.test"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, ghPost(b, "pull_request", "m-3", prBody(hookInst, "opened", "User")), 200, "a repository on under a connection off")
	if n := deliveryCount(t, b); n != 1 {
		t.Fatalf("%d deliveries, want the switched-on repository's one", n)
	}
	// Any other repository of that connection inherits its off.
	other := bytes.Replace(prBody(hookInst, "opened", "User"), []byte("acme/web"), []byte("acme/api"), 1)
	wantStatus(t, ghPost(b, "pull_request", "m-4", other), 200, "another repository under the connection that is off")
	if n := deliveryCount(t, b); n != 1 {
		t.Fatalf("%d deliveries, want the other repository's pull request dropped", n)
	}
	// Settings that do not read as settings are a failure GitHub can show and redeliver, never a
	// guess at what they meant.
	if err := b.store.UpdateReviewSettings(ctx, 1, web.ID, json.RawMessage(`{"mode":5}`), "x"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, ghPost(b, "pull_request", "m-5", prBody(hookInst, "opened", "User")), http.StatusServiceUnavailable, "unreadable settings")
}

func TestGitHubWebhookStoresOnlyReviewedInstallations(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)

	// An installation nobody here holds: answered, never bound, never stored.
	for _, c := range []struct{ event, id string }{{"pull_request", "u-1"}, {"installation", "u-2"}} {
		body := prBody(hookInst, "opened", "User")
		if c.event == "installation" {
			body = installationBody(hookInst, 424242, "created", map[string]string{"pull_requests": "write"})
		}
		w := ghPost(b, c.event, c.id, body)
		wantStatus(t, w, 200, "an unknown installation's "+c.event)
		if !strings.Contains(w.Body.String(), "not connected") {
			t.Errorf("the answer to an unknown installation does not say why: %q", w.Body.String())
		}
	}
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g != nil {
		t.Fatalf("a delivery bound installation %d to org %d", hookInst, g.OrgID)
	}
	if n := deliveryCount(t, b); n != 0 {
		t.Fatalf("%d deliveries stored for an installation nobody holds", n)
	}

	// Bound, but not in any review tree: its installation events are kept, its pull requests not.
	bindInstall(t, b.store, 1, hookInst, "acme")
	w := ghPost(b, "pull_request", "n-1", prBody(hookInst, "opened", "User"))
	wantStatus(t, w, 200, "a pull request on an installation nobody reviews")
	if !strings.Contains(w.Body.String(), "not reviewed") {
		t.Errorf("the answer does not say the installation is not reviewed: %q", w.Body.String())
	}
	if n := deliveryCount(t, b); n != 0 {
		t.Fatalf("a pull request of an installation nobody reviews was stored")
	}
	wantStatus(t, ghPost(b, "installation_repositories", "n-2",
		installationBody(hookInst, 424242, "added", map[string]string{"contents": "write"})), 200, "installation_repositories")
	if n := deliveryCount(t, b); n != 1 {
		t.Fatalf("%d deliveries after an installation event, want it kept whoever reviews what", n)
	}

	// Reviewed: kept, sealed, and filed under the organisation the installation belongs to.
	addConnection(t, b.store, 1, hookInst)
	body := prBody(hookInst, "opened", "User")
	wantStatus(t, ghPost(b, "pull_request", "r-1", body), 200, "a pull request on a reviewed installation")
	var org, inst int64
	var event, action string
	var enc []byte
	if err := b.store.db.QueryRowContext(ctx, `select org_id, installation_id, event, action, payload_enc
		from github_deliveries where delivery_id=?`, "r-1").Scan(&org, &inst, &event, &action, &enc); err != nil {
		t.Fatalf("the reviewed installation's pull request was not stored: %v", err)
	}
	if org != 1 || inst != hookInst || event != "pull_request" || action != "opened" {
		t.Errorf("stored as org=%d installation=%d event=%q action=%q", org, inst, event, action)
	}
	if bytes.Contains(enc, []byte("PLAINTEXT-MARKER")) || bytes.Contains(enc, []byte("acme/web")) {
		t.Fatal("the payload is stored in the clear")
	}
	if plain, err := b.sealer.Open(enc); err != nil || !bytes.Equal(plain, body) {
		t.Fatalf("the sealed payload does not open to the body GitHub sent: %v", err)
	}

	// Another organisation's reviewed installation stays its own.
	bindInstall(t, b.store, 2, 2002, "octo-org")
	addConnection(t, b.store, 2, 2002)
	wantStatus(t, ghPost(b, "pull_request", "r-2", prBody(2002, "opened", "User")), 200, "org 2's pull request")
	if err := b.store.db.QueryRowContext(ctx, `select org_id from github_deliveries where delivery_id=?`, "r-2").Scan(&org); err != nil || org != 2 {
		t.Fatalf("org 2's delivery filed under org %d (%v)", org, err)
	}

	// Stopping reviews, or forgetting the installation, stops the storing at once.
	conn := mustTreeNode(t, b.store, 1, func(r *ReviewSetting) bool { return r.Kind == reviewKindConnection })
	if err := b.store.RemoveReviewConnection(ctx, 1, conn.ID, "admin@acme.test"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, ghPost(b, "pull_request", "r-3", prBody(hookInst, "synchronize", "User")), 200, "after reviews stopped")
	if err := b.store.RevokeGitHubInstall(ctx, 2, 2002, "forgotten"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, ghPost(b, "pull_request", "r-4", prBody(2002, "synchronize", "User")), 200, "after the installation was forgotten")
	if n := deliveryCount(t, b); n != 3 {
		t.Fatalf("%d deliveries, want 3: stopped or forgotten installations kept on being stored", n)
	}
}

func TestGitHubWebhookDropsBotCommentsButKeepsBotPushes(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b)
	stored := func(id string) bool {
		var one int
		return b.store.db.QueryRowContext(ctx, `select 1 from github_deliveries where delivery_id=?`, id).Scan(&one) == nil
	}

	cases := []struct {
		name, event, id string
		body            []byte
		kept            bool
	}{
		{"a person's command on a pull request", "issue_comment", "c-1", issueCommentBody(hookInst, "User", true), true},
		// Our own review echoing back is the loop this exists to stop.
		{"a bot's comment on a pull request", "issue_comment", "c-2", issueCommentBody(hookInst, "Bot", true), false},
		{"a comment on an issue", "issue_comment", "c-3", issueCommentBody(hookInst, "User", false), false},
		{"a bot's review", "pull_request_review", "c-4", prBody(hookInst, "submitted", "Bot"), false},
		{"a bot's review comment", "pull_request_review_comment", "c-5", prBody(hookInst, "created", "Bot"), false},
		{"a bot resolving a thread", "pull_request_review_thread", "c-6", prBody(hookInst, "resolved", "Bot"), false},
		{"a person's review comment", "pull_request_review_comment", "c-7", prBody(hookInst, "created", "User"), true},
		// A merge bot's rebase moves the head a review is anchored to, whoever pushed it.
		{"a bot's push to a pull request", "pull_request", "c-8", prBody(hookInst, "synchronize", "Bot"), true},
		{"an event review does not listen to", "push", "c-9", prBody(hookInst, "", "User"), false},
	}
	for _, c := range cases {
		wantStatus(t, ghPost(b, c.event, c.id, c.body), 200, c.name)
		if got := stored(c.id); got != c.kept {
			t.Errorf("%s: stored=%v, want %v", c.name, got, c.kept)
		}
	}
}

func TestGitHubWebhookStoresARedeliveryOnce(t *testing.T) {
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b)
	body := prBody(hookInst, "opened", "User")
	for i := range 3 {
		wantStatus(t, ghPost(b, "pull_request", "72d3162e-cc78-11e3-81ab-4c9367dc0958", body), 200, fmt.Sprintf("delivery %d", i+1))
	}
	if n := deliveryCount(t, b); n != 1 {
		t.Fatalf("one delivery redelivered three times was stored %d times", n)
	}
}

// A full inbox is a 503, so GitHub lists the delivery as failed and somebody can redeliver it,
// where a 200 would have lost it with nothing anywhere saying so.
func TestGitHubWebhookFullInboxIsAFailureGitHubCanShow(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b)
	for i := range githubOrgPendingLimit {
		if _, err := b.store.enqueueGitHubDelivery(ctx, fmt.Sprintf("fill-%d", i), 1, hookInst, "pull_request", "opened", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	wantStatus(t, ghPost(b, "pull_request", "one-too-many", prBody(hookInst, "opened", "User")), http.StatusServiceUnavailable, "a delivery past the organisation's share")
}

func TestGitHubWebhookReceiptFilter(t *testing.T) {
	const (
		bot    = `{"sender":{"type":"Bot"}}`
		person = `{"sender":{"type":"User"}}`
		onPR   = `{"sender":{"type":"User"},"issue":{"number":7,"pull_request":{"url":"x"}}}`
		nullPR = `{"sender":{"type":"User"},"issue":{"number":7,"pull_request":null}}`
		noPR   = `{"sender":{"type":"User"},"issue":{"number":7}}`
	)
	for _, c := range []struct {
		event, env string
		reviewed   bool
		dropped    bool
	}{
		{"installation", bot, false, false}, // an installation's own events, whoever sent them
		{"installation_repositories", person, false, false},
		{"pull_request", bot, true, false},
		{"issue_comment", onPR, true, false},
		{"issue_comment", nullPR, false, true},
		{"issue_comment", noPR, false, true},
		{"issue_comment", person, false, true},
		{"pull_request_review", bot, false, true},
		{"pull_request_review_thread", person, true, false},
		{"ping", person, false, true},
		{"push", person, false, true},
		{"github_app_authorization", person, false, true},
	} {
		var env githubEnvelope
		if err := json.Unmarshal([]byte(c.env), &env); err != nil {
			t.Fatal(err)
		}
		reviewed, drop := githubReceipt(c.event, &env)
		if reviewed != c.reviewed || (drop != "") != c.dropped {
			t.Errorf("%s %s: reviewed=%v drop=%q, want reviewed=%v dropped=%v", c.event, c.env, reviewed, drop, c.reviewed, c.dropped)
		}
	}
}

// ---- the dispatcher ----

func dispatchAll(t *testing.T, b *Bot) int {
	t.Helper()
	n := 0
	for b.dispatchNextGitHubDelivery(context.Background()) {
		if n++; n > 50 {
			t.Fatal("the dispatcher never ran out of work")
		}
	}
	return n
}

func TestGitHubDispatcherKeepsTheInstallationInStepWithGitHub(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)
	bindInstall(t, b.store, 1, hookInst, "acme")

	// new_permissions_accepted: the grant GitHub now reports is the one stored.
	perms := map[string]string{"contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write"}
	wantStatus(t, ghPost(b, "installation", "p-1", installationBody(hookInst, 424242, "new_permissions_accepted", perms)), 200, "new_permissions_accepted")
	if n := dispatchAll(t, b); n != 1 {
		t.Fatalf("dispatched %d deliveries, want 1", n)
	}
	g, _ := b.store.GitHubInstall(ctx, hookInst)
	var got map[string]string
	if err := json.Unmarshal([]byte(g.Permissions), &got); err != nil || got["issues"] != "write" || len(got) != len(perms) {
		t.Fatalf("permissions after new_permissions_accepted = %q (%v)", g.Permissions, err)
	}
	if g.RepoSelection != "selected" {
		t.Errorf("repository selection after new_permissions_accepted = %q, want what GitHub reported", g.RepoSelection)
	}
	var doneAt int64
	var payload []byte
	if err := b.store.db.QueryRowContext(ctx, `select done_at, payload_enc from github_deliveries where delivery_id='p-1'`).
		Scan(&doneAt, &payload); err != nil || doneAt == 0 || payload != nil {
		t.Fatalf("the dispatched delivery: done_at=%d payload kept=%v err=%v", doneAt, payload != nil, err)
	}

	// installation_repositories: the owner switched the installation to every repository.
	allRepos := []byte(fmt.Sprintf(`{"action":"added","repository_selection":"all",
		"installation":{"id":%d,"app_id":424242,"account":{"login":"acme","id":5,"type":"Organization"},
			"repository_selection":"all","permissions":{"contents":"write"}},
		"repositories_added":[{"full_name":"acme/api"}],"repositories_removed":[],
		"sender":{"login":"acme-admin","type":"User"}}`, hookInst))
	wantStatus(t, ghPost(b, "installation_repositories", "sel-1", allRepos), 200, "installation_repositories")
	dispatchAll(t, b)
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g.RepoSelection != "all" {
		t.Errorf("repository selection after the owner chose every repository = %q", g.RepoSelection)
	}

	// suspend and unsuspend.
	wantStatus(t, ghPost(b, "installation", "s-1", installationBody(hookInst, 424242, "suspend", perms)), 200, "suspend")
	dispatchAll(t, b)
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g.Status != "suspended" || g.SuspendedAt == "" {
		t.Fatalf("after suspend: status=%q suspended_at=%q", g.Status, g.SuspendedAt)
	}
	wantStatus(t, ghPost(b, "installation", "s-2", installationBody(hookInst, 424242, "unsuspend", perms)), 200, "unsuspend")
	dispatchAll(t, b)
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g.Status != "active" || g.SuspendedAt != "" || g.LastError != "" {
		t.Fatalf("after unsuspend: %+v", g)
	}

	// A pull-request body resent under the installation event's name — headers are not signed —
	// says "deleted" too. It must not revoke anything: it carries no installation record.
	forged := []byte(fmt.Sprintf(`{"action":"deleted","installation":{"id":%d},"comment":{"id":1},
		"issue":{"number":7,"pull_request":{}},"sender":{"login":"someone","type":"User"}}`, hookInst))
	wantStatus(t, ghPost(b, "installation", "f-1", forged), 200, "a comment body under the installation event's name")
	dispatchAll(t, b)
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g.Status != "active" {
		t.Fatalf("a resent comment body revoked the installation: %+v", g)
	}
	// Nor does a real installation record of some other App.
	wantStatus(t, ghPost(b, "installation", "f-2", installationBody(hookInst, 777, "deleted", perms)), 200, "another app's record")
	dispatchAll(t, b)
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g.Status != "active" {
		t.Fatalf("another App's installation record revoked this one: %+v", g)
	}

	// deleted: uninstalled at GitHub, so forgotten here, and said so in the audit log.
	wantStatus(t, ghPost(b, "installation", "d-1", installationBody(hookInst, 424242, "deleted", perms)), 200, "deleted")
	dispatchAll(t, b)
	g, _ = b.store.GitHubInstall(ctx, hookInst)
	if g.Status != "revoked" || g.LastError != "uninstalled at GitHub" {
		t.Fatalf("after deleted: status=%q last_error=%q", g.Status, g.LastError)
	}
	events, err := b.store.AuditEvents(ctx, 1, AuditFilter{Action: "github_install.uninstalled"})
	if err != nil || len(events) != 1 || events[0].TargetID != fmt.Sprint(hookInst) || events[0].Via != viaSystem {
		t.Fatalf("audit of the uninstall: %+v (%v)", events, err)
	}

	// A suspension dispatched after the installation was forgotten must not bring it back.
	if err := b.store.SuspendGitHubInstall(ctx, 1, hookInst, "2026-10-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g.Status != "revoked" {
		t.Fatalf("a late suspension un-revoked the installation: %q", g.Status)
	}
}

func TestGitHubDispatcherFinishesPullRequestDeliveries(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)
	reviewedInstall(t, b)
	wantStatus(t, ghPost(b, "pull_request", "pr-1", prBody(hookInst, "opened", "User")), 200, "pull_request")
	wantStatus(t, ghPost(b, "issue_comment", "pr-2", issueCommentBody(hookInst, "User", true)), 200, "issue_comment")
	if n := dispatchAll(t, b); n != 2 {
		t.Fatalf("dispatched %d, want 2", n)
	}
	var pending int
	if err := b.store.db.QueryRowContext(ctx, `select count(*) from github_deliveries where done_at=0 or payload_enc is not null`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("%d deliveries left pending or holding a payload (%v)", pending, err)
	}
}

// A delivery whose work fails goes back with a delay and a reason, rather than being finished as
// though it had worked or retried at once.
func TestGitHubDispatcherBacksOffAFailedDelivery(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)
	bindInstall(t, b.store, 1, hookInst, "acme")
	// Sealed under some other MASTER_KEY: opening it fails, as it would after a key was rotated
	// with no MASTER_KEY_PREVIOUS.
	if _, err := b.store.enqueueGitHubDelivery(ctx, "bad-seal", 1, hookInst, "installation", "deleted", []byte("not a sealed box at all")); err != nil {
		t.Fatal(err)
	}
	if !b.dispatchNextGitHubDelivery(ctx) {
		t.Fatal("the delivery was not claimed")
	}
	r := readGitHubDelivery(t, b.store, "bad-seal")
	if r.doneAt != 0 || r.deadAt != 0 || r.attempts != 1 || r.lastError == "" || r.leaseUntil <= time.Now().UnixNano() {
		t.Fatalf("after a failed dispatch: %+v", r)
	}
	if b.dispatchNextGitHubDelivery(ctx) {
		t.Fatal("a failed delivery was claimed again before its backoff ran out")
	}
	if g, _ := b.store.GitHubInstall(ctx, hookInst); g.Status != "active" {
		t.Fatalf("a delivery that could not be opened changed the installation: %q", g.Status)
	}
}

// The lease is renewed while a dispatch runs, and a dispatcher that finds it has been taken over
// stops and records nothing: the new holder owns the outcome, and a finish or a failure written
// by the old one would close or delay work it no longer has.
func TestGitHubDispatcherStopsWhenItsLeaseIsTaken(t *testing.T) {
	ctx := context.Background()
	b := webhookBot(t, hookSecret, "", hookAppID)
	bindInstall(t, b.store, 1, hookInst, "acme")
	every, route := githubDeliveryTouchEvery, routeGitHubDelivery
	t.Cleanup(func() { githubDeliveryTouchEvery, routeGitHubDelivery = every, route })
	githubDeliveryTouchEvery = 20 * time.Millisecond

	started := make(chan struct{})
	stopped := make(chan error, 1)
	routeGitHubDelivery = func(_ *Bot, ctx context.Context, _ *githubDelivery, _ []byte) error {
		close(started)
		select {
		case <-ctx.Done():
			stopped <- ctx.Err()
			return ctx.Err()
		case <-time.After(10 * time.Second):
			stopped <- nil
			return nil
		}
	}
	sealed, err := b.sealer.Seal(installationBody(hookInst, 424242, "suspend", map[string]string{"contents": "write"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.enqueueGitHubDelivery(ctx, "slow-1", 1, hookInst, "installation", "suspend", sealed); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- b.dispatchNextGitHubDelivery(ctx) }()
	<-started

	// Renewed while it runs.
	first := readGitHubDelivery(t, b.store, "slow-1").leaseUntil
	deadline := time.Now().Add(5 * time.Second)
	for readGitHubDelivery(t, b.store, "slow-1").leaseUntil == first {
		if time.Now().After(deadline) {
			t.Fatal("the lease was never renewed while the dispatch ran")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Another dispatcher takes the row over, as a claim after a stall would.
	taken := time.Now().Add(time.Hour).UnixNano()
	if _, err := b.store.db.ExecContext(ctx, `update github_deliveries set lease_until=? where delivery_id='slow-1'`, taken); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the work was not stopped when the lease was lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the work ran on after the lease was taken")
	}
	if !<-done {
		t.Error("a claimed delivery reported no work")
	}
	r := readGitHubDelivery(t, b.store, "slow-1")
	if r.doneAt != 0 || r.deadAt != 0 || r.lastError != "" || r.leaseUntil != taken || r.payload == nil {
		t.Fatalf("the old holder wrote to a delivery it no longer held: %+v", r)
	}
}

// Every event code review needs is one the receipt filter keeps, and one guide/configuration.md
// tells an operator to tick, by the name GitHub's App settings give it: an App subscribed to fewer
// gets its reviews and silently never hears a command or a reply.
func TestGitHubAppEventsAreTheOnesListenedToAndInTheGuide(t *testing.T) {
	raw, err := guide.Files.ReadFile("configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	page := strings.Join(strings.Fields(string(raw)), " ")
	var env githubEnvelope
	if err := json.Unmarshal([]byte(`{"sender":{"type":"User"},"issue":{"number":7,"pull_request":{"url":"x"}}}`), &env); err != nil {
		t.Fatal(err)
	}
	for _, e := range githubAppEvents {
		if reviewed, drop := githubReceipt(e.header, &env); !reviewed {
			t.Errorf("%s is listed as an event code review needs, and the receipt drops it: %s", e.header, drop)
		}
		if !strings.Contains(page, "**"+e.label+"**") {
			t.Errorf("guide/configuration.md does not tell an operator to tick %q", e.label)
		}
	}
}
