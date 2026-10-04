package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CODE_REVIEW: which organisations have code review here. What these pin is that an organisation
// without it starts nothing that spends — no review as a pull request opens, none a command, a reply,
// the console or the catch-up would start, and none queued before the plan changed — that it is told
// so once a day on a pull request and with a 402 in the console, and that "off" takes the feature off
// the deployment: no routes, no deliveries kept, no lane. They run on both dialects.

func TestCodeReviewPolicy(t *testing.T) {
	for in, want := range map[string]string{"": "all", "all": "all", " Pro ": "pro", "enterprise": "enterprise", "OFF": "off",
		"premium": "pro", "pro,enterprise": "pro", "pr0": "pro", " ": "all"} {
		if got := codeReviewOf(in); got != want {
			t.Errorf("CODE_REVIEW=%q reads as %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		policy, plan string
		ok           bool
		reason, need string
	}{
		{"", PlanFree, true, "", ""}, // a Config built in code: the default
		{CodeReviewAll, PlanFree, true, "", ""},
		{CodeReviewPro, PlanFree, false, "plan", PlanPro},
		{CodeReviewPro, PlanPro, true, "", ""},
		{CodeReviewPro, PlanEnterprise, true, "", ""},
		{CodeReviewPro, "gold", false, "plan", PlanPro}, // an unknown plan is free
		{CodeReviewEnterprise, PlanPro, false, "plan", PlanEnterprise},
		{CodeReviewEnterprise, PlanEnterprise, true, "", ""},
		{CodeReviewOff, PlanEnterprise, false, "off", ""},
	} {
		got := Config{CodeReview: c.policy}.codeReviewFor(c.plan)
		if got.Available != c.ok || got.Reason != c.reason || got.Needs != c.need || (!c.ok && got.Message == "") {
			t.Errorf("CODE_REVIEW=%q, plan %q: %+v", c.policy, c.plan, got)
		}
	}
	if got := (Config{CodeReview: CodeReviewPro}).codeReviewFor(PlanFree).Message; !strings.Contains(got, "Free plan") {
		t.Errorf("the refusal does not say which plan the organisation is on: %q", got)
	}
}

// LoadConfig reads CODE_REVIEW, and a typo in it is the default: a self-host is one organisation.
func TestLoadConfigReadsCodeReview(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
	t.Setenv("SLACK_SIGNING_SECRET", "test-signing-secret")
	t.Setenv("LLM_API_KEY", "test-model-key")
	t.Setenv("SLACK_CLIENT_ID", "test-client-id")
	t.Setenv("SLACK_CLIENT_SECRET", "test-client-secret")
	for in, want := range map[string]string{"pro": CodeReviewPro, "": CodeReviewAll, "pr0": CodeReviewPro, "off": CodeReviewOff} {
		t.Setenv("CODE_REVIEW", in)
		if got := LoadConfig().CodeReview; got != want {
			t.Errorf("CODE_REVIEW=%q loads as %q, want %q", in, got, want)
		}
	}
}

// planRig is the console's rig with CODE_REVIEW=pro and the organisation on the free plan.
func planRig(t *testing.T) *reviewAPIRig {
	t.Helper()
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.b.cfg.CodeReview = CodeReviewPro
	rig.onPlan(PlanFree)
	return rig
}

// onPlan moves the rig's organisation to plan, at once.
func (rig *reviewAPIRig) onPlan(plan string) {
	rig.t.Helper()
	if err := rig.st.SetOrgPlan(context.Background(), orgID, plan, 0); err != nil {
		rig.t.Fatal(err)
	}
	rig.b.settings.Invalidate(orgID)
}

// On the free plan where code review is sold on pro: a pull request that opens is not reviewed and
// says why, quietly; a member's command gets one line a day and nothing else, naming no plan; Start
// review and Try are refused with a 402 that says which plan would have it, and /api/me says so for
// the console. Moved onto pro, the same pull request is reviewed.
func TestReviewPlanGateStopsWhatWouldSpend(t *testing.T) {
	rig := planRig(t)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	if pr := rig.pr(7); pr == nil || pr.SkipReason != "plan" || len(rig.runs(7)) != 0 {
		t.Fatalf("the opening on the free plan: %+v, %d runs", pr, len(rig.runs(7)))
	}
	rig.wroteNothing()

	rig.deliver("issue_comment", commentEvent(901, "alice", "MEMBER", "@attesttag review", true))
	rig.deliver("issue_comment", commentEvent(902, "alice", "MEMBER", "@attesttag status", true))
	a := rig.answers()
	if len(a) != 1 || !strings.Contains(a[0], "not turned on for this organisation's account") || strings.Contains(strings.ToLower(a[0]), "plan") {
		t.Fatalf("answers to two commands on the free plan: %q", a)
	}
	if n := len(rig.runs(7)); n != 0 {
		t.Errorf("a command queued %d runs on the free plan", n)
	}
	if audits := rig.commandAudits(); len(audits) != 1 || !strings.HasPrefix(audits[0]["outcome"].(string), "refused: Code review is part of the Pro") {
		t.Errorf("command audits = %v", audits)
	}

	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/api/reviews", map[string]any{"repo": "acme/web", "pr": 7}},
		{"/api/review-types/try", map[string]any{"repo": "acme/web", "pr": 7, "type": map[string]any{"name": "Try"}}},
	} {
		code, out := rig.call("POST", c.path, rig.admin, c.body)
		if code != 402 || out["reason"] != "plan" || out["needs"] != PlanPro || !strings.Contains(out["error"].(string), "Pro") {
			t.Errorf("POST %s on the free plan = %d %v", c.path, code, out)
		}
	}
	me := rig.must(200, "GET", "/api/me", rig.viewer, nil)
	if cr, _ := me["code_review"].(map[string]any); cr == nil || cr["available"] != false || cr["reason"] != "plan" || cr["needs"] != PlanPro {
		t.Errorf("/api/me code_review = %v", me["code_review"])
	}
	// What was set and reviewed before stays readable.
	rig.must(200, "GET", "/api/review-settings", rig.viewer, nil)
	rig.must(200, "GET", "/api/reviews", rig.viewer, nil)

	rig.onPlan(PlanPro)
	if me := rig.must(200, "GET", "/api/me", rig.viewer, nil); me["code_review"].(map[string]any)["available"] != true {
		t.Errorf("/api/me on pro = %v", me["code_review"])
	}
	rig.deliver("pull_request", prEvent("reopened", 7, reviewHead))
	rig.drain()
	if runs := rig.runs(7); len(runs) != 1 || runs[0].Status != "posted" || rig.pr(7).SkipReason != "" {
		t.Errorf("on pro the pull request was not reviewed: %+v", runs)
	}
}

// A review queued while the organisation had code review and claimed after it lost it spends
// nothing: skipped, the reason on the pull request, nothing posted. A reply in a thread is not even
// queued.
func TestReviewPlanGateAtTheClaim(t *testing.T) {
	ctx := context.Background()
	rig := planRig(t)
	rig.onPlan(PlanPro)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	f := rig.finding()

	rig.onPlan(PlanFree)
	rig.reply(f, "octocat", "CONTRIBUTOR", "This is fine.")
	if n := len(rig.replyRuns()); n != 0 {
		t.Errorf("a reply on the free plan queued %d runs", n)
	}

	rig.onPlan(PlanPro)
	run, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation, Trigger: "console",
		TriggerRef: "console-plan", RequestedBy: "admin@acme.test", BypassFilters: true, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	rig.onPlan(PlanFree)
	calls := len(rig.model.requests(""))
	rig.drain()
	if got, _ := rig.st.ReviewRun(ctx, orgID, run.ID); got.Status != "skipped" || !strings.HasPrefix(got.Error, "plan:") {
		t.Errorf("the run claimed after the plan changed = %s %q", got.Status, got.Error)
	}
	if rig.pr(7).SkipReason != "plan" || len(rig.model.requests("")) != calls {
		t.Errorf("skip %q; %d model calls", rig.pr(7).SkipReason, len(rig.model.requests(""))-calls)
	}
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 {
		t.Errorf("%d reviews posted; the one from before the plan changed", len(posts))
	}
}

// The catch-up passes over an organisation without code review before it lists anything at GitHub.
func TestReviewPlanGateCatchupListsNothing(t *testing.T) {
	rig := newE2ERig(t)
	rig.liveOnTesting()
	rig.b.cfg.CodeReview = CodeReviewEnterprise
	rig.onPlan(PlanPro)
	rig.catchupListing(func(head string) []map[string]any {
		return []map[string]any{pullItem(7, head, "octocat", "User", time.Now())}
	})
	rig.b.reviewCatchup(context.Background(), 0)
	for _, s := range rig.fake.sent() {
		if strings.Contains(s, "/pulls") {
			t.Errorf("the catch-up asked GitHub %s for an organisation without code review", s)
		}
	}
	if rig.pr(7) != nil {
		t.Errorf("the catch-up recorded a pull request: %+v", rig.pr(7))
	}
}

// CODE_REVIEW=off takes code review off the deployment: its console routes are not there, a pull
// request's delivery is not kept — an installation's own still are — and no lane runs.
func TestCodeReviewOffHidesTheFeature(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.b.cfg.CodeReview = CodeReviewOff
	for _, path := range []string{"/api/review-settings", "/api/review-types", "/api/reviews", "/api/review-pulls?repo=acme/web"} {
		if code, out := rig.call("GET", path, rig.admin, nil); code != 404 || out["reason"] != "off" {
			t.Errorf("GET %s with code review off = %d %v", path, code, out)
		}
	}
	if code, _ := rig.call("POST", "/api/reviews", rig.admin, map[string]any{"repo": "acme/web", "pr": 7}); code != 404 {
		t.Errorf("POST /api/reviews with code review off = %d", code)
	}
	me := rig.must(200, "GET", "/api/me", rig.admin, nil)
	if cr := me["code_review"].(map[string]any); cr["available"] != false || cr["reason"] != "off" {
		t.Errorf("/api/me code_review = %v", cr)
	}
	w := ghPost(rig.b, "pull_request", "off-1", prEvent("opened", 7, reviewHead))
	wantStatus(t, w, 200, "pull_request")
	if !strings.Contains(w.Body.String(), "ignored: code review is off on this deployment") || deliveryCount(t, rig.b) != 0 {
		t.Errorf("a pull request's delivery with code review off: %q, %d stored", w.Body.String(), deliveryCount(t, rig.b))
	}
	if rig.b.reviewLaneOn() || rig.b.reviewCatchupOn() {
		t.Error("the review lane or the catch-up runs with code review off")
	}
	// Connecting a repository asks nobody to set up code review, or to grant it write access: the
	// webhook is still told, since installations' own events use it.
	rig.b.ghApp = rig.b.proxy.ghApp
	if err := rig.st.SetGitHubInstallPermissions(context.Background(), orgID, fakeInstallation, `{"contents":"write","pull_requests":"read"}`); err != nil {
		t.Fatal(err)
	}
	rig.b.ghHook = newGitHubWebhookFrom("", "", "")
	ins := rig.must(200, "GET", "/api/github/installations", rig.viewer, nil)
	list := ins["installations"].([]any)
	if ins["code_review"] != false || len(reviewStrings(ins["review_missing"])) != 0 || len(list) != 1 ||
		len(reviewStrings(list[0].(map[string]any)["missing_permissions"])) != 0 || list[0].(map[string]any)["permissions_known"] != false {
		t.Errorf("installations with code review off = %v", ins)
	}
	if hook, _ := ins["webhook_url"].(string); !strings.HasSuffix(hook, "/github/webhook") {
		t.Errorf("the webhook went with code review: %v", ins)
	}
}
