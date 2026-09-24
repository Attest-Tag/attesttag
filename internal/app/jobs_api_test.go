package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// fakeSlackAPI answers the Web API methods the job code calls and records what was posted.
type fakeSlackAPI struct {
	mu      sync.Mutex
	calls   []string
	posts   []string // chat.postMessage bodies (blocks + text)
	updates []string // chat.update bodies
}

func (f *fakeSlackAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	body := r.Form.Get("blocks") + " " + r.Form.Get("text") + " " + r.Form.Get("attachments")
	f.mu.Lock()
	f.calls = append(f.calls, method)
	n := len(f.calls)
	switch method {
	case "chat.postMessage":
		f.posts = append(f.posts, body)
	case "chat.update":
		f.updates = append(f.updates, body)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "chat.postMessage", "chat.update":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C1", "ts": fmt.Sprintf("1700000000.%06d", n)})
	case "conversations.replies":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{
			{"user": "U1", "text": "the parser crashes on docs without a ticket id, token ghp_abcdefghijklmnopqrstuvwxyz1234 leaked", "ts": "1.1"}}})
	default:
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not_faked"})
	}
}

func (f *fakeSlackAPI) posted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.posts...)
}

// fakeDispatcher records launches instead of starting anything.
type fakeDispatcher struct {
	mu        sync.Mutex
	launches  []JobLaunch
	cancelled []string
	state     string // what Status reports; running by default
}

func (d *fakeDispatcher) Name() string { return "fake" }
func (d *fakeDispatcher) Start(ctx context.Context, j *Job, l JobLaunch) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.launches = append(d.launches, l)
	return fmt.Sprintf("fake:%d", j.ID), nil
}
func (d *fakeDispatcher) Cancel(ctx context.Context, ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancelled = append(d.cancelled, ref)
	return nil
}
func (d *fakeDispatcher) Status(ctx context.Context, ref string) (DispatchStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return DispatchStatus{State: nonEmpty(d.state, "running")}, nil
}
func (d *fakeDispatcher) wasCancelled(ref string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.cancelled {
		if c == ref {
			return true
		}
	}
	return false
}

const testPAT = "github_pat_11SEALEDTOKEN0123456789abcdefghijklmnop"

type jobHarness struct {
	or     *fakeOpenRouter
	b      *Bot
	fs     *fakeSlackAPI
	fd     *fakeDispatcher
	api    *httptest.Server
	connID int64
	admin  string // a real console session; there is no shared admin token any more
}

// newJobHarness is a bot with a real store, sealer and proxy (GitHub answered by github), a fake
// Slack, a fake dispatcher and the worker/admin routes served over httptest.
// fakeOpenRouter stands in for OpenRouter's provisioning API: it mints per-job keys, records the
// spend limit asked for and the keys deleted, and answers usage. fail makes every mint answer 500.
type fakeOpenRouter struct {
	mu      sync.Mutex
	fail    bool
	limits  []float64
	deleted []string
	usage   float64
}

func (f *fakeOpenRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer or-prov-test" {
		w.WriteHeader(401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/keys":
		if f.fail {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"provisioning down"}`)
			return
		}
		var body struct {
			Limit float64 `json:"limit"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.limits = append(f.limits, body.Limit)
		fmt.Fprintf(w, `{"key":"sk-or-v1-job-%d","data":{"hash":"hash-%d","name":"n"}}`, len(f.limits), len(f.limits))
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/keys/"):
		fmt.Fprintf(w, `{"data":{"usage":%g}}`, f.usage)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/keys/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/keys/"))
		fmt.Fprint(w, `{"data":{"success":true}}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeOpenRouter) setFail(v bool) { f.mu.Lock(); f.fail = v; f.mu.Unlock() }
func (f *fakeOpenRouter) minted() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.limits...)
}
func (f *fakeOpenRouter) wasDeleted(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.deleted {
		if h == hash {
			return true
		}
	}
	return false
}

func newJobHarness(t *testing.T, github fakeAPI) *jobHarness {
	t.Helper()
	if github == nil {
		github = func(r *http.Request) (int, string) { return 200, `{"default_branch":"main"}` }
	}
	b := repoTestBot(t, github)
	fs := &fakeSlackAPI{}
	ss := httptest.NewServer(fs)
	t.Cleanup(ss.Close)
	b.slacks = testRegistry(&Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(ss.URL+"/api/"))}, TeamID: "T1", OrgID: orgID, BotUserID: "UBOT"})
	or := &fakeOpenRouter{}
	ors := httptest.NewServer(or)
	t.Cleanup(ors.Close)
	prevKeysURL := openRouterKeysURL
	openRouterKeysURL = ors.URL + "/keys"
	t.Cleanup(func() { openRouterKeysURL = prevKeysURL })
	cfg := Config{WorkerMode: "fake", HealthAddr: ":8090", Model: "m", HeavyModel: "heavy", OpenRouterProvisioningKey: "or-prov-test", WorkerLLMKey: "sk-or-shared-key-0123456789"}
	b.cfg = cfg
	b.settings = newSettingsCache(b.store, cfg)
	fd := &fakeDispatcher{}
	b.jobs = NewJobRunner(cfg, b.store, b.slacks, b.proxy, b.settings)
	b.jobs.dispatchers["fake"] = fd
	connID := storeGitHubConn(t, b, "app", "acme/app", testPAT)
	mux := http.NewServeMux()
	b.jobsRoutes(mux)
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	t.Setenv("ADMIN_BASE_URL", "")
	return &jobHarness{or: or, b: b, fs: fs, fd: fd, api: api, connID: connID, admin: seedAdmin(t, b.store)}
}

func (h *jobHarness) call(t *testing.T, method, path, token string, body any, contentType string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(v)
	default:
		raw, _ := json.Marshal(v)
		rd = bytes.NewReader(raw)
		if contentType == "" {
			contentType = "application/json"
		}
	}
	req, _ := http.NewRequest(method, h.api.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out
}

func testSpec(connID int64) JobSpec {
	return JobSpec{Repo: "acme/app", ConnectionID: connID, BaseBranch: "main", Title: "Null ticket id", Requirement: "Guard the retry path.",
		Acceptance: []string{"tests pass"}, Requester: "U1", Channel: "C1", ThreadTS: "1.1"}
}

// The whole bot side of one job: dispatch, claim, events, cancel, diff, result, report, and the
// console's view of it. Secrets travel only in the claim; the launch carries id, URL and token.
func TestJobDispatchAndWorkerRoundTrip(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	st := h.b.store

	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if j.Status != jobStarting || j.Branch != "bugfix/fix-1-null-ticket-id-attest_tag" || j.Dispatcher != "fake" || j.ExecutionRef != "fake:1" || j.StatusTS == "" {
		t.Fatalf("dispatched job: %+v", j)
	}
	if len(h.fd.launches) != 1 {
		t.Fatalf("launches: %+v", h.fd.launches)
	}
	l := h.fd.launches[0]
	if id, ok := parseJobToken(l.Token); !ok || id != j.ID || l.JobID != j.ID || l.BotURL != "http://localhost:8090" {
		t.Errorf("launch = %+v", l)
	}
	if posts := h.fs.posted(); len(posts) != 1 || !strings.Contains(posts[0], "Fix job #1") {
		t.Errorf("checklist not posted: %v", posts)
	}
	if _, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID)}); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("second job in the thread should be refused, got %v", err)
	}

	// Wrong token, wrong id, no token: all 401 and indistinguishable.
	bad := "atj1.1." + strings.Repeat("x", 43)
	for _, c := range []struct{ path, tok string }{{"/api/worker/jobs/1/claim", bad}, {"/api/worker/jobs/2/claim", l.Token}, {"/api/worker/jobs/1/claim", ""}} {
		if code, _ := h.call(t, "POST", c.path, c.tok, nil, ""); code != 401 {
			t.Errorf("%s with %q: %d, want 401", c.path, c.tok, code)
		}
	}

	// Claim: the only response with secrets.
	code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", l.Token, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, "")
	if code != 200 {
		t.Fatalf("claim: %d %v", code, body)
	}
	secrets, _ := body["secrets"].(map[string]any)
	llm, _ := secrets["llm"].(map[string]any)
	job, _ := body["job"].(map[string]any)
	spec, _ := job["spec"].(map[string]any)
	if secrets["github_token"] != testPAT || llm["api_key"] != "sk-or-v1-job-1" || llm["base_url"] != openRouterBaseURL || spec["branch"] != j.Branch || spec["requester"] != "U1" {
		t.Errorf("claim body: %v", body)
	}
	if limits := h.or.minted(); len(limits) != 1 || limits[0] != j.BudgetUSD {
		t.Errorf("per-job key limits = %v, want one key capped at the job budget %v", limits, j.BudgetUSD)
	}
	if got, _ := st.Job(ctx, orgID, j.ID); got.Status != jobRunning || got.ClaimCount != 1 {
		t.Errorf("after claim: %s claims=%d", got.Status, got.ClaimCount)
	}

	// Events: duplicates ignored, usage logged, secrets scrubbed, oversize refused.
	code, body = h.call(t, "POST", "/api/worker/jobs/1/events", l.Token, JobEventsRequest{Events: []JobEvent{
		{Seq: 1, Kind: JobKindPhase, Phase: "clone", Status: "started"},
		{Seq: 2, Kind: JobKindLog, Message: "cloning with " + testPAT + " done"},
		{Seq: 2, Kind: JobKindLog, Message: "duplicate"},
		{Seq: 3, Kind: JobKindUsage, Usage: &JobUsage{In: 100, Out: 5, CostUSD: 0.2}},
	}}, "")
	if code != 200 || body["accepted"] != float64(3) || body["cancel"] != false {
		t.Fatalf("events: %d %v", code, body)
	}
	evs, _ := st.JobEvents(ctx, orgID, j.ID, 0)
	if len(evs) != 3 || strings.Contains(evs[1].Message, "SEALED") || !strings.Contains(evs[1].Message, "[redacted") {
		t.Errorf("stored events: %+v", evs)
	}
	if spend, _ := st.MonthSpend(ctx, orgID, "", "C1"); spend < 0.19 || spend > 0.21 {
		t.Errorf("usage from events = %v", spend)
	}
	if code, _ = h.call(t, "POST", "/api/worker/jobs/1/events", l.Token, JobEventsRequest{Events: []JobEvent{{Seq: 9, Kind: JobKindLog, Message: strings.Repeat("a", JobEventMaxBytes+1)}}}, ""); code != 413 {
		t.Errorf("oversize event: %d, want 413", code)
	}

	// A cancel asked for in the thread reaches the worker on its next event.
	if ok, _ := st.RequestJobCancel(ctx, orgID, j.ID, "U1", "user"); !ok {
		t.Fatal("cancel refused")
	}
	code, body = h.call(t, "POST", "/api/worker/jobs/1/events", l.Token, JobEventsRequest{Events: []JobEvent{{Seq: 4, Kind: JobKindHeartbeat}}}, "")
	if code != 200 || body["cancel"] != true || body["reason"] != "user" {
		t.Errorf("cancel not propagated: %d %v", code, body)
	}
	if code, _ = h.call(t, "GET", "/api/worker/jobs/1", l.Token, nil, ""); code != 200 {
		t.Errorf("poll: %d", code)
	}

	// Diff, then the result: recorded once, delta usage logged once, report posted.
	if code, _ = h.call(t, "PUT", "/api/worker/jobs/1/diff", l.Token, "--- a\n+++ b\n-x\n+y\n", "text/x-diff"); code != 200 {
		t.Errorf("diff: %d", code)
	}
	res := JobResult{Seq: 5, Status: JobCancelled, Summary: "stopped before pushing", Usage: JobUsage{In: 150, Out: 10, CostUSD: 0.3},
		Branch: j.Branch, Error: JobError{Code: "cancelled", Message: "cancel received"}}
	if code, body = h.call(t, "POST", "/api/worker/jobs/1/result", l.Token, res, ""); code != 200 {
		t.Fatalf("result: %d %v", code, body)
	}
	got, _ := st.Job(ctx, orgID, j.ID)
	if got.Status != JobCancelled || got.CostUSD < 0.29 || got.CostUSD > 0.31 || got.FinishedAt == "" || got.Error == "" {
		t.Errorf("after result: %+v", got)
	}
	if spend, _ := st.MonthSpend(ctx, orgID, "", "C1"); spend < 0.29 || spend > 0.31 {
		t.Errorf("usage after result = %v, want the delta logged once", spend)
	}
	if !h.or.wasDeleted("hash-1") {
		t.Errorf("the per-job key was not deleted at finish: %v", h.or.deleted)
	}
	posts := h.fs.posted()
	if len(posts) < 2 || !strings.Contains(posts[len(posts)-1], "cancelled") {
		t.Errorf("report not posted: %v", posts)
	}
	if diff, _, _, _ := st.JobFile(ctx, orgID, j.ID, "diff"); !strings.Contains(diff, "+y") {
		t.Errorf("diff not stored: %q", diff)
	}

	// After the result: events are gone, the same result is a harmless duplicate, another is not.
	if code, _ = h.call(t, "POST", "/api/worker/jobs/1/events", l.Token, JobEventsRequest{Events: []JobEvent{{Seq: 6, Kind: JobKindHeartbeat}}}, ""); code != 410 {
		t.Errorf("events after result: %d, want 410", code)
	}
	if code, body = h.call(t, "POST", "/api/worker/jobs/1/result", l.Token, res, ""); code != 200 || body["duplicate"] != true {
		t.Errorf("duplicate result: %d %v", code, body)
	}
	res.Seq = 7
	if code, _ = h.call(t, "POST", "/api/worker/jobs/1/result", l.Token, res, ""); code != 410 {
		t.Errorf("second result: %d, want 410", code)
	}

	// The console sees the job, its events and the diff; without admin auth it sees nothing.
	if code, _ = h.call(t, "GET", "/api/jobs", "", nil, ""); code != 401 {
		t.Errorf("admin list without auth: %d", code)
	}
	req, _ := http.NewRequest("GET", h.api.URL+"/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+h.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0]["status"] != JobCancelled || list[0]["pr_url"] != "" || list[0]["token_hash"] != nil {
		t.Errorf("admin list: %v", list)
	}
	code, body = h.call(t, "GET", "/api/jobs/1", h.admin, nil, "")
	if code != 200 || body["events"] == nil || body["diff_bytes"] == float64(0) {
		t.Errorf("admin detail: %d %v", code, body)
	}
	code, body = h.call(t, "GET", "/api/jobs/1/diff", h.admin, nil, "")
	if code != 200 || !strings.Contains(body["_raw"].(string), "+y") {
		t.Errorf("admin diff: %d %v", code, body)
	}
}

// Claim refusals: a cancelled job, a token past its expiry, and the fourth claim.
func TestJobClaimRefusals(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatal(err)
	}
	tok := h.fd.launches[0].Token
	for i := 0; i < 3; i++ {
		if code, _ := h.call(t, "POST", "/api/worker/jobs/1/claim", tok, nil, ""); code != 200 {
			t.Fatalf("claim %d: %d", i+1, code)
		}
	}
	if n := len(h.or.minted()); n != 1 {
		t.Errorf("three claims minted %d keys, want the sealed one reused", n)
	}
	if code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", tok, nil, ""); code != 409 {
		t.Errorf("fourth claim: %d %v", code, body)
	}
	h.b.store.SetJobFields(ctx, orgID, j.ID, map[string]any{"token_expires": "2000-01-01 00:00:00"})
	if code, _ := h.call(t, "POST", "/api/worker/jobs/1/events", tok, JobEventsRequest{Events: []JobEvent{{Seq: 1, Kind: JobKindHeartbeat}}}, ""); code != 401 {
		t.Errorf("expired token: %d, want 401", code)
	}
	h.b.store.SetJobFields(ctx, orgID, j.ID, map[string]any{"token_expires": "2999-01-01 00:00:00"})

	// A job cancelled before its worker claimed it is finished on the spot; the claim gets 410.
	j2, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: func() JobSpec { s := testSpec(h.connID); s.ThreadTS = "2.2"; return s }(), ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatal(err)
	}
	tok2 := h.fd.launches[1].Token
	if n := h.b.jobs.CancelInThread(ctx, orgID, "T1", "C1", "2.2", "U1"); n != 1 {
		t.Fatalf("cancel in thread = %d", n)
	}
	if got, _ := h.b.store.Job(ctx, orgID, j2.ID); got.Status != JobCancelled || !h.fd.wasCancelled("fake:2") {
		t.Errorf("unclaimed job after cancel: %s cancelled=%v", got.Status, h.fd.cancelled)
	}
	if code, _ := h.call(t, "POST", "/api/worker/jobs/2/claim", tok2, nil, ""); code != 410 {
		t.Errorf("claim of a cancelled job: %d, want 410", code)
	}
	_ = time.Second
}

// Without OPENROUTER_PROVISIONING_KEY the worker gets the shared key, uncapped, and nothing is
// minted. With no shared key either, the tool's precheck and the Confirm's dispatch refuse before
// anything is launched or posted, naming both knobs.
func TestJobSharedKeyWithoutProvisioningKey(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	h.b.jobs.keys = newOpenRouterKeys("")
	if _, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"}); err != nil {
		t.Fatalf("dispatch on the shared key: %v", err)
	}
	code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", h.fd.launches[0].Token, nil, "")
	secrets, _ := body["secrets"].(map[string]any)
	llm, _ := secrets["llm"].(map[string]any)
	if code != 200 || llm["api_key"] != "sk-or-shared-key-0123456789" || llm["base_url"] != openRouterBaseURL {
		t.Fatalf("claim on the shared key: %d %v", code, body)
	}
	if n := len(h.or.minted()); n != 0 {
		t.Errorf("minted %d keys without a provisioning key", n)
	}

	h.b.jobs.cfg.WorkerLLMKey = ""
	spec := testSpec(h.connID)
	spec.ThreadTS = "2.2"
	_, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: spec, ApprovedBy: "U2", Approval: "confirm"})
	if err == nil || !strings.Contains(err.Error(), "OPENROUTER_PROVISIONING_KEY") || !strings.Contains(err.Error(), "WORKER_LLM_API_KEY") {
		t.Fatalf("dispatch with no key at all: %v", err)
	}
	if len(h.fd.launches) != 1 {
		t.Errorf("launched %d jobs, want only the first", len(h.fd.launches))
	}
	if err := h.b.jobs.precheck(ctx, orgID, "T1", "C1", "2.2"); err == nil || !strings.Contains(err.Error(), "OPENROUTER_PROVISIONING_KEY") {
		t.Errorf("precheck: %v", err)
	}
	h.b.jobs.keys = newOpenRouterKeys("or-prov-test")
	if err := h.b.jobs.precheck(ctx, orgID, "T1", "C1", "2.2"); err != nil {
		t.Errorf("precheck with a provisioning key: %v", err)
	}
}

// When the per-job key cannot be minted at claim time, the job fails with the reason on it (so
// the thread and the console show it) while the worker gets a bare 503 and no secrets.
func TestJobClaimFailsWhenMintFails(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatal(err)
	}
	h.or.setFail(true)
	code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", h.fd.launches[0].Token, nil, "")
	if code != 503 || body["secrets"] != nil || strings.Contains(fmt.Sprint(body), "provisioning down") {
		t.Fatalf("claim: %d %v", code, body)
	}
	got, _ := h.b.store.Job(ctx, orgID, j.ID)
	if got.Status != JobFailed || !strings.Contains(got.Error, "credential_unavailable") || !strings.Contains(got.Error, "could not provision a restricted worker credential") {
		t.Errorf("job after a failed mint: %s %q", got.Status, got.Error)
	}
	if posts := h.fs.posted(); !strings.Contains(strings.Join(posts, "\n"), "could not provision") {
		t.Errorf("the report does not say why: %v", posts)
	}
}

// storeGitHubAppConn makes the other kind of repository connection: one backed by a GitHub App
// installation, which stores an installation id and no token at all.
func storeGitHubAppConn(t *testing.T, b *Bot, name, repo string, installID int64) int64 {
	t.Helper()
	ctx := context.Background()
	if err := b.store.SaveGitHubInstall(ctx, &GitHubInstall{ID: installID, OrgID: orgID, AccountLogin: "acme",
		AccountType: "Organization", RepoSelection: "selected", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	bd, err := b.store.CreateBundle(ctx, orgID, repoBundleName+" app", "test")
	if err != nil {
		t.Fatal(err)
	}
	c, sec, err := b.buildConnection(&connectionInput{BundleID: bd.ID, Name: name, Preset: "github",
		CredType: "github_app", Secret: &Secret{InstallationID: installID}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Repo, c.Status, c.GitHubInstallationID = repo, "active", installID
	enc, err := b.sealSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.store.InsertConnection(ctx, orgID, c, enc)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The worker is handed whichever credential its repository is actually connected with: the
// stored token for a pasted one, a freshly minted installation token for an app-backed one.
// Before this, the claim read the stored token either way, and every app-backed job died at
// "could not open the repository token" — the one thing an app connection never has.
func TestRepoJobTokenPicksTokenOrInstallation(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()

	conn, err := h.b.store.Connection(ctx, orgID, h.connID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.b.repoJobToken(ctx, orgID, conn, "acme/app")
	if err != nil || got != testPAT {
		t.Errorf("pasted-token repository: %q %v, want the stored token", got, err)
	}

	appID := storeGitHubAppConn(t, h.b, "app-backed", "acme/web", 12345678)
	appConn, err := h.b.store.Connection(ctx, orgID, appID)
	if err != nil {
		t.Fatal(err)
	}
	// No app is configured in a test deployment, so the mint refuses — but it is the mint that
	// refuses, which is the whole point: the claim no longer looks for a token that is not there.
	_, err = h.b.repoJobToken(ctx, orgID, appConn, "acme/web")
	if err == nil || !strings.Contains(err.Error(), "could not mint a GitHub App token for acme/web") {
		t.Errorf("app-backed repository: %v, want the installation path", err)
	}
	if err != nil && strings.Contains(err.Error(), "no stored access token") {
		t.Errorf("app-backed repository fell back to the token path: %v", err)
	}
}

// An app-backed job that cannot mint fails with the reason on it, the same way a missing model
// credential does, rather than with a message that sends the operator looking for a lost token.
func TestJobClaimAppBackedReportsTheMint(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	connID := storeGitHubAppConn(t, h.b, "app-backed", "acme/web", 12345678)
	spec := testSpec(connID)
	spec.Repo = "acme/web"
	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: spec, ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", h.fd.launches[0].Token, nil, "")
	if code != 410 || body["secrets"] != nil {
		t.Fatalf("claim: %d %v", code, body)
	}
	got, _ := h.b.store.Job(ctx, orgID, j.ID)
	if got.Status != JobFailed || !strings.Contains(got.Error, "GitHub App") {
		t.Errorf("job after a failed mint: %s %q", got.Status, got.Error)
	}
}

// An app-backed job is held inside the hour its minted token lives, so it cannot do the work and
// then fail to push it. A pasted token outlives any job and is left alone.
func TestClampJobTimeout(t *testing.T) {
	pat := &Connection{CredType: "bearer"}
	app := &Connection{CredType: "github_app", GitHubInstallationID: 12345678}
	for _, c := range []struct {
		name string
		conn *Connection
		in   int
		want int
	}{
		{"a pasted token is not clamped", pat, 90 * 60, 90 * 60},
		{"an app-backed job is", app, 90 * 60, jobTimeoutCeiling},
		{"a short app-backed job is left alone", app, 45 * 60, 45 * 60},
		{"no connection, no clamp", nil, 90 * 60, 90 * 60},
	} {
		if got := clampJobTimeout(c.in, c.conn); got != c.want {
			t.Errorf("%s: clampJobTimeout(%d) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

// A minted installation token is not in the connection's stored secret, so scrubSecrets cannot
// know it — redact's own pattern is what keeps it out of the worker's log lines. Nothing
// depended on ghs_ being in that pattern until app-backed jobs started being handed one.
func TestRedactCoversMintedInstallationTokens(t *testing.T) {
	tok := "ghs_" + strings.Repeat("aB1cD2eF3g", 3) + "hJ"
	if got := redact("cloning with " + tok + " done"); strings.Contains(got, tok) {
		t.Errorf("a minted installation token survived redact: %q", got)
	}
}
