package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A job's row is the source of truth across restarts, so the store must enforce the lifecycle:
// compare-and-set transitions, at most three claims, idempotent events, one finish.
func TestJobLifecycle(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	j := &Job{OrgID: orgID, TeamID: "T1", Channel: "C1", ThreadTS: "1.1", Requester: "U1", ConnectionID: 7, Repo: "acme/app", BaseBranch: "main",
		Title: "Null ticket id", Spec: `{"v":1}`, Engine: "fake", Model: "m", BudgetUSD: 3, TimeoutS: 600, DraftPR: true, Dispatcher: "fake"}
	id, err := st.InsertJob(ctx, j)
	if err != nil || id == 0 {
		t.Fatalf("insert: %d %v", id, err)
	}
	tok := mintJobToken(id)
	if got, ok := parseJobToken(tok); !ok || got != id {
		t.Fatalf("parseJobToken(%q) = %d %v", tok, got, ok)
	}
	if err := st.SetJobFields(ctx, orgID, id, map[string]any{"token_hash": hashJobToken(tok), "token_expires": "2999-01-01 00:00:00", "branch": "fix-1-null-attest_tag"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobFields(ctx, orgID, id, map[string]any{"nope": 1}); err == nil {
		t.Error("an unknown column must be refused")
	}
	got, err := st.Job(ctx, orgID, id)
	if err != nil || got == nil || got.Status != jobQueued || got.Spec != `{"v":1}` || got.TokenHash == "" || !got.DraftPR || got.Branch != "fix-1-null-attest_tag" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	list, err := st.Jobs(ctx, orgID, JobFilter{})
	if err != nil || len(list) != 1 || list[0].Spec != "" {
		t.Fatalf("list: %+v %v (bodies must stay off the list)", list, err)
	}
	if n := st.CountActiveJobs(ctx); n != 1 {
		t.Errorf("active = %d", n)
	}
	if in, _ := st.ActiveJobsInThread(ctx, orgID, "T1", "C1", "1.1"); len(in) != 1 {
		t.Errorf("active in thread = %d", len(in))
	}
	if _, why, _ := st.ClaimJob(ctx, orgID, id, "w"); why != "not dispatched" {
		t.Errorf("claim before dispatch: %q", why)
	}

	// Transitions are compare-and-set.
	if ok, _ := st.SetJobStatus(ctx, orgID, id, []string{jobQueued}, jobStarting, map[string]any{"execution_ref": "pid:1"}); !ok {
		t.Fatal("queued → starting refused")
	}
	if ok, _ := st.SetJobStatus(ctx, orgID, id, []string{jobQueued}, jobRunning, nil); ok {
		t.Fatal("a stale from-status must not transition")
	}

	// Three claims (a lost response may make an honest worker retry), then refused.
	for i := 1; i <= 3; i++ {
		c, why, err := st.ClaimJob(ctx, orgID, id, `{"mode":"fake"}`)
		if err != nil || c == nil || c.Status != jobRunning || c.ClaimCount != i {
			t.Fatalf("claim %d: %+v %q %v", i, c, why, err)
		}
	}
	if _, why, _ := st.ClaimJob(ctx, orgID, id, "w"); why != "already claimed" {
		t.Errorf("fourth claim: %q", why)
	}

	// Events: duplicates by seq are ignored, usage is summed, the phase is tracked.
	evs := []JobEvent{
		{Seq: 1, Kind: JobKindPhase, Phase: "clone", Status: "started"},
		{Seq: 2, Kind: JobKindUsage, Usage: &JobUsage{In: 100, Out: 10, CostUSD: 0.5}},
		{Seq: 2, Kind: JobKindUsage, Usage: &JobUsage{In: 100, Out: 10, CostUSD: 0.5}},
	}
	acc, used, err := st.AddJobEvents(ctx, orgID, id, evs)
	if err != nil || acc != 2 || used.CostUSD != 0.5 || used.In != 100 {
		t.Fatalf("events: accepted=%d used=%+v err=%v", acc, used, err)
	}
	got, _ = st.Job(ctx, orgID, id)
	if got.LastSeq != 2 || got.Phase != "clone" || got.CostUSD != 0.5 || got.TokensIn != 100 {
		t.Errorf("after events: seq=%d phase=%q cost=%v in=%d", got.LastSeq, got.Phase, got.CostUSD, got.TokensIn)
	}
	if evs, _ := st.JobEvents(ctx, orgID, id, 0); len(evs) != 2 || evs[1].Usage == nil {
		t.Errorf("stored events: %+v", evs)
	}

	// A stale job that speaks again is running again.
	st.SetJobStatus(ctx, orgID, id, []string{jobRunning}, jobStale, nil)
	st.AddJobEvents(ctx, orgID, id, []JobEvent{{Seq: 3, Kind: JobKindHeartbeat}})
	if got, _ = st.Job(ctx, orgID, id); got.Status != jobRunning {
		t.Errorf("stale + event = %s, want running", got.Status)
	}

	// Cancel: once, and the status moves to cancelling.
	if ok, _ := st.RequestJobCancel(ctx, orgID, id, "U2", "user"); !ok {
		t.Fatal("cancel refused")
	}
	if ok, _ := st.RequestJobCancel(ctx, orgID, id, "U3", "user"); ok {
		t.Error("second cancel must be a no-op")
	}
	got, _ = st.Job(ctx, orgID, id)
	if got.Status != jobCancelling || !got.CancelRequested || got.CancelBy != "U2" || got.CancelAt == "" {
		t.Errorf("after cancel: %+v", got)
	}

	// Finish once: the result's usage is the total, the delta is what events did not carry.
	delta, ok, err := st.FinishJob(ctx, orgID, id, JobSucceeded, &JobResult{Usage: JobUsage{In: 150, Out: 20, CostUSD: 0.8},
		PR: &JobPR{URL: "https://github.com/acme/app/pull/1"}, Branch: "fix-1-null-attest_tag"})
	if err != nil || !ok || delta.In != 50 || delta.Out != 10 || delta.CostUSD < 0.29 || delta.CostUSD > 0.31 {
		t.Fatalf("finish: ok=%v delta=%+v err=%v", ok, delta, err)
	}
	if _, ok, _ := st.FinishJob(ctx, orgID, id, JobFailed, nil); ok {
		t.Error("a second finish must be a no-op")
	}
	got, _ = st.Job(ctx, orgID, id)
	if got.Status != JobSucceeded || got.PRURL == "" || got.CostUSD != 0.8 || got.FinishedAt == "" || got.Result == "" {
		t.Errorf("after finish: %+v", got)
	}
	if n := st.CountActiveJobs(ctx); n != 0 {
		t.Errorf("active after finish = %d", n)
	}

	// Files and housekeeping.
	if err := st.PutJobFile(ctx, orgID, id, "diff", "--- a\n+++ b\n"); err != nil {
		t.Fatal(err)
	}
	st.SetJobFileLink(ctx, orgID, id, "diff", "F1", "https://x/F1")
	if c, f, l, _ := st.JobFile(ctx, orgID, id, "diff"); !strings.HasPrefix(c, "--- a") || f != "F1" || l == "" {
		t.Errorf("job file: %q %q %q", c, f, l)
	}
	st.SetJobFields(ctx, orgID, id, map[string]any{"finished_at": "2000-01-01 00:00:00"})
	st.RevokeJobTokens(ctx, time.Minute)
	if got, _ = st.Job(ctx, orgID, id); got.TokenHash != "" {
		t.Error("token not revoked after the grace period")
	}
	if n, _ := st.PurgeJobData(ctx, 1, time.Minute); n != 3 {
		t.Errorf("purged %d events, want 3", n)
	}
}

func TestJobTokensBranchesAndSettings(t *testing.T) {
	tok := mintJobToken(42)
	if id, ok := parseJobToken(tok); !ok || id != 42 {
		t.Fatalf("parse: %d %v", id, ok)
	}
	for _, bad := range []string{"", "nope", "atj1.43.short", "atj2.42." + strings.Repeat("a", 43), "atj1.x." + strings.Repeat("a", 43)} {
		if _, ok := parseJobToken(bad); ok {
			t.Errorf("parseJobToken(%q) accepted", bad)
		}
	}
	if h := hashJobToken(tok); len(h) != 64 || h == tok {
		t.Errorf("hash: %q", h)
	}
	// Both the job token and a fine-grained GitHub token are redacted from text.
	for _, s := range []string{tok, "github_pat_11ABCDEFG0123456789abcdefghijklmnop_more"} {
		if out := redact("x " + s + " y"); strings.Contains(out, s) || !strings.Contains(out, "[redacted-secret]") {
			t.Errorf("not redacted: %q → %q", s, out)
		}
	}
	// No house convention: the branch starts at fix- and ends with the mark.
	if got := jobBranch("", "attest_tag", 12, "Null ticket id on retry!!"); got != "fix-12-null-ticket-id-on-retry-attest_tag" {
		t.Errorf("branch = %q", got)
	}
	if got := jobBranch("", "", 3, ""); got != "fix-3-change-attest_tag" {
		t.Errorf("branch = %q", got)
	}
	// The organisation's own prefix is kept, separator supplied or not, and the mark still lands
	// on the end.
	if got := jobBranch("hotfix/", "attest_tag", 7, "Retry storm"); got != "hotfix/fix-7-retry-storm-attest_tag" {
		t.Errorf("branch = %q", got)
	}
	if got := jobBranch("feature", "attest_tag", 7, "x"); got != "feature/fix-7-x-attest_tag" {
		t.Errorf("branch = %q", got)
	}
	if got := jobBranch("bot-", "_tag", 7, "x"); got != "bot-fix-7-x_tag" {
		t.Errorf("branch = %q", got)
	}
	if got := jobBranchID("hotfix/", "attest_tag", "<n>", "x"); got != "hotfix/fix-<n>-x-attest_tag" {
		t.Errorf("confirm-card branch = %q", got)
	}
	// A house rule that names a prefix per kind of change: the job's own kind picks one, and the
	// mark still lands on the end. An unrecognised kind takes the first, because a rule that
	// lists prefixes says one of them is required.
	for _, tc := range []struct{ setting, kind, want string }{
		{defaultBranchPrefix, jobKindFeature, "feature/"},
		{defaultBranchPrefix, jobKindBugfix, "bugfix/"},
		{defaultBranchPrefix, jobKindHotfix, "hotfix/"},
		{"feature/ bugfix/", jobKindBugfix, "bugfix/"},
		{defaultBranchPrefix, "", "bugfix/"}, // a job that does not say is a bug fix
		{"bugfix/,feature/", "", "bugfix/"},
		{"feature/, bugfix/", "chore", "bugfix/"},
		{"hotfix/", jobKindFeature, "hotfix/"},
		{"", jobKindBugfix, ""},
		{"  ", jobKindFeature, ""},
		// "none" is how an organisation says it keeps no convention; empty means unset, and the
		// caller has already turned that into the default.
		{"none", jobKindBugfix, ""},
		{"None", jobKindFeature, ""},
	} {
		if got := pickBranchPrefix(tc.setting, tc.kind); got != tc.want {
			t.Errorf("pickBranchPrefix(%q, %q) = %q, want %q", tc.setting, tc.kind, got, tc.want)
		}
	}
	if got := jobBranch(pickBranchPrefix("feature/, bugfix/", jobKindFeature), "attest_tag", 9, "Add CSV export"); got != "feature/fix-9-add-csv-export-attest_tag" {
		t.Errorf("feature branch = %q", got)
	}
	if got := jobBranch(pickBranchPrefix("feature/, bugfix/", jobKindBugfix), "attest_tag", 9, "Retry storm"); got != "bugfix/fix-9-retry-storm-attest_tag" {
		t.Errorf("bugfix branch = %q", got)
	}
	// The model's word for the kind is kept inside the vocabulary the convention is written in.
	for in, want := range map[string]string{"feature": jobKindFeature, "Feature": jobKindFeature, "feat": jobKindFeature,
		"bugfix": jobKindBugfix, "": jobKindBugfix, "chore": jobKindBugfix,
		"hotfix": jobKindHotfix, "Hot fix": jobKindHotfix} {
		if got := normalizeJobKind(in); got != want {
			t.Errorf("normalizeJobKind(%q) = %q, want %q", in, got, want)
		}
	}
	good := map[string]string{"worker_engine": "fake", "worker_job_budget_usd": "2.5", "worker_timeout_minutes": "45", "worker_max_jobs": "2",
		"worker_branch_prefix": defaultBranchPrefix, "worker_branch_suffix": "attest_tag", "worker_event_retention_days": "30", "worker_allow_rules": "0", "worker_model": "z-ai/glm-5.3"}
	for k, v := range good {
		if err := validateWorkerSetting(k, v); err != nil {
			t.Errorf("%s=%s refused: %v", k, v, err)
		}
	}
	bad := map[string]string{"worker_engine": "gpt", "worker_job_budget_usd": "1000", "worker_timeout_minutes": "1", "worker_max_jobs": "0",
		"worker_allow_rules": "yes", "worker_branch_prefix": "/leading", "worker_branch_suffix": "attest/", "worker_event_retention_days": "0"}
	for k, v := range bad {
		if err := validateWorkerSetting(k, v); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
	// The prefix is optional — an organisation with no convention leaves it blank. The suffix is
	// not: the worker's push guard is the only thing standing between it and someone else's branch.
	if err := validateWorkerSetting("worker_branch_prefix", ""); err != nil {
		t.Errorf("empty branch prefix refused: %v", err)
	}
	if err := validateWorkerSetting("worker_branch_prefix", "none"); err != nil {
		t.Errorf("branch prefix \"none\" refused: %v", err)
	}
	if err := validateWorkerSetting("worker_branch_suffix", ""); err == nil {
		t.Error("empty branch suffix accepted")
	}
}

// Every pull request the worker opens is a draft, and there is no setting for it. worker_pr_draft
// was one — a console switch the worker never read — so an organisation may still hold a row saying
// "0". It has to read harmlessly: the settings still load, a job is still a draft, and the key
// cannot be saved again.
func TestAStoredDraftSwitchNeitherBreaksSettingsNorUndraftsAJob(t *testing.T) {
	b, mux, st := identityBot(t)
	_, org, token := signedUp(t, b, mux, st, "founder@example.com")
	ctx := context.Background()
	if err := st.PutSetting(ctx, org, "worker_pr_draft", "0"); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(org)

	if code, body := authReq(t, mux, "GET", "/api/settings", nil, token); code != 200 {
		t.Fatalf("settings with the old row stored = %d: %v", code, body)
	}
	if c := (&JobRunner{}).constraints(b.settings.Get(ctx, org), nil, ""); !c.DraftPR {
		t.Error("a job's constraints say its pull request is not a draft")
	}
	if code, _ := authReq(t, mux, "PUT", "/api/settings", map[string]string{"worker_pr_draft": "0"}, token); code != 400 {
		t.Errorf("PUT /api/settings accepted worker_pr_draft (%d), which has nothing left to change", code)
	}
}
