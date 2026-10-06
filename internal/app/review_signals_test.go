package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A live review's signals on its pull request (review_signals.go): the eyes while it runs and a
// rocket once it is posted, and a check run among the pull request's checks. They run through the
// whole lane against the fake GitHub, whose check-run routes fail the test when a request carries any
// token but the one for checks alone.

// A live review on an installation that granted Checks: the eyes go on as it starts, and a check run
// starts in progress beside them; once the review is posted the eyes come off, a rocket goes on, and
// the check completes as a success with the score, the finding linked to its thread, and its details
// on the summary comment. A later review of the same pull request takes the rocket off as it starts
// and puts it back once it stands, with a check of its own on the commit it reviewed.
func TestReviewSignalsOfAPostedReview(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.grantChecks()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	run := rig.runs(7)[0]
	if run.Status != "posted" {
		t.Fatalf("run = %s %q", run.Status, run.Error)
	}
	log, now, checkLog, checks := rig.gh.signals()
	if !slices.Equal(log, []string{"+eyes", "-eyes", "+rocket"}) || !slices.Equal(now, []string{"rocket"}) {
		t.Errorf("reactions went %v and are %v; want the eyes on, then off, and a rocket", log, now)
	}
	if !slices.Equal(checkLog, []string{"create in_progress", "update completed/success"}) || len(checks) != 1 {
		t.Fatalf("check runs written %v: %v", checkLog, checks)
	}
	_, _, comments, _ := rig.gh.snapshot()
	summaryURL := fmt.Sprintf("https://github.com/acme/web/pull/7#issuecomment-%d", comments[0].ID)
	cr := checks[0]
	out, _ := cr["output"].(map[string]any)
	if cr["name"] != reviewCheckName || cr["head_sha"] != reviewHead || cr["external_id"] != run.PublicID ||
		cr["details_url"] != summaryURL || out["title"] != "Confidence 3/5 · 1 open finding" || cr["completed_at"] == nil {
		t.Errorf("the check run = %v", cr)
	}
	fs, _ := rig.st.ReviewFindings(ctx, orgID, run.ReviewPRID)
	thread := fmt.Sprintf("[Add writes the total without the lock](https://github.com/acme/web/pull/7#discussion_r%d)", fs[0].GitHubCommentID)
	if text, _ := out["summary"].(string); !strings.Contains(text, "**P1** · General · "+thread) ||
		!strings.Contains(text, "[summary comment]("+summaryURL+")") || !strings.Contains(text, "Merge after fixing") {
		t.Errorf("the check's summary:\n%s", text)
	}

	// Pushed to and asked for again: the rocket comes off as the review starts and goes back on once
	// it stands; the new head has a check of its own, and the first one is left as it ended.
	rig.gh.pushTo(reviewHeadC, totalsFixture().files, totalsFixture().head)
	rig.deliver("pull_request", prEvent("synchronize", 7, reviewHeadC))
	rig.drain()
	if _, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation,
		Trigger: "console", TriggerRef: "console-request-signals", RequestedBy: "admin@acme.test", BypassFilters: true}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	log, now, _, checks = rig.gh.signals()
	if want := []string{"+eyes", "-eyes", "+rocket", "-rocket", "+eyes", "-eyes", "+rocket"}; !slices.Equal(log, want) ||
		!slices.Equal(now, []string{"rocket"}) {
		t.Errorf("reactions over two reviews went %v and are %v; want %v", log, now, want)
	}
	if len(checks) != 2 || checks[1]["head_sha"] != reviewHeadC || checks[1]["conclusion"] != "success" ||
		checks[0]["conclusion"] != "success" {
		t.Errorf("check runs after the second review: %v", checks)
	}
}

// Without Checks granted — an App made for fix jobs, or an installation whose owner has not accepted
// the new permission — the reactions are all there is: no check run is asked for, and no token is
// minted for checks, which GitHub would refuse.
func TestReviewSignalsWithoutChecksGranted(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if run := rig.runs(7)[0]; run.Status != "posted" {
		t.Fatalf("run = %s %q", run.Status, run.Error)
	}
	if log, _, checkLog, _ := rig.gh.signals(); !slices.Equal(log, []string{"+eyes", "-eyes", "+rocket"}) || len(checkLog) != 0 {
		t.Errorf("reactions %v, check runs %v; want the reactions alone", log, checkLog)
	}
	for _, s := range rig.fake.sent() {
		if strings.Contains(s, "check-runs") {
			t.Errorf("asked GitHub about check runs without the permission: %s", s)
		}
	}
	for _, m := range rig.fake.mints {
		if m.Permissions["checks"] != "" {
			t.Errorf("a token was minted for checks the installation never granted: %v", m.Permissions)
		}
	}
}

// Shadow writes nothing to GitHub, signals included: not a reaction, not a check run, and not even a
// read of either, whatever the installation granted.
func TestReviewSignalsInShadowAreNone(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"shadow"}`)
	rig.grantChecks()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if run := rig.runs(7)[0]; run.Status != "shadow" {
		t.Fatalf("run = %s %q", run.Status, run.Error)
	}
	for _, s := range rig.fake.sent() {
		if strings.Contains(s, "check-runs") || strings.Contains(s, "/reactions") || !strings.HasPrefix(s, "GET ") {
			t.Errorf("a shadow review signalled on GitHub: %s", s)
		}
	}
}

// A review that fails ends its check as neutral — never a failure, which would read as the code
// failing and block a merge where the check is required — saying which kind of failure it was and
// how to ask again, and takes its eyes off with no rocket.
func TestReviewSignalsOfAFailedReview(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.grantChecks()
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return reviewModelReply{Status: 402} }
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if run := rig.runs(7)[0]; run.Status != "failed" {
		t.Fatalf("run = %s %q", run.Status, run.Error)
	}
	log, now, checkLog, checks := rig.gh.signals()
	if !slices.Equal(log, []string{"+eyes", "-eyes"}) || len(now) != 0 {
		t.Errorf("reactions went %v and are %v; want the eyes on, then off", log, now)
	}
	if !slices.Equal(checkLog, []string{"create in_progress", "update completed/neutral"}) || len(checks) != 1 {
		t.Fatalf("check runs written %v", checkLog)
	}
	out, _ := checks[0]["output"].(map[string]any)
	text, _ := out["summary"].(string)
	if out["title"] != "Review failed: the model provider did not answer" || !strings.Contains(text, "`@attesttag review`") ||
		strings.Contains(text, "402") || checks[0]["details_url"] != nil {
		t.Errorf("the failed review's check: %v", checks[0])
	}
}

// A review asked for again on code it already reviewed is answered from that review: the rocket goes
// back on, and the new check says the earlier review stands, with its score and its summary.
func TestReviewSignalsOfAReviewThatStands(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.grantChecks()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	again, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation,
		Trigger: "console", TriggerRef: "console-request-stands", RequestedBy: "admin@acme.test", BypassFilters: true})
	if err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if got, _ := rig.st.ReviewRun(ctx, orgID, again.ID); got.Status != "noop" {
		t.Fatalf("the second review = %s, want answered from the first", got.Status)
	}
	log, now, _, checks := rig.gh.signals()
	if want := []string{"+eyes", "-eyes", "+rocket", "-rocket", "+eyes", "-eyes", "+rocket"}; !slices.Equal(log, want) ||
		!slices.Equal(now, []string{"rocket"}) {
		t.Errorf("reactions went %v and are %v; want %v", log, now, want)
	}
	_, _, comments, _ := rig.gh.snapshot()
	summaryURL := fmt.Sprintf("https://github.com/acme/web/pull/7#issuecomment-%d", comments[0].ID)
	if len(checks) != 2 {
		t.Fatalf("check runs: %v", checks)
	}
	out, _ := checks[1]["output"].(map[string]any)
	if checks[1]["conclusion"] != "success" || out["title"] != "Confidence 3/5 · already reviewed" ||
		checks[1]["details_url"] != summaryURL || !strings.Contains(out["summary"].(string), "("+summaryURL+")") {
		t.Errorf("the standing review's check: %v", checks[1])
	}
}

// A run put back after its signals went up — GitHub's rate limit on the summary — leaves them up, and
// the attempt that resumes it from its checkpoint finds them: the eyes by the App's login and the
// check run by the run it belongs to, so the pull request ends with one check, completed, and the
// rocket, never a second check left in progress.
func TestReviewSignalsOfARunPutBack(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.grantChecks()
	rig.confirmLock()
	rig.gh.limitComments = 1
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	run := rig.runs(7)[0]
	if run.Status != "queued" {
		t.Fatalf("after the rate limit: %s", run.Status)
	}
	if log, now, checkLog, _ := rig.gh.signals(); !slices.Equal(log, []string{"+eyes"}) || !slices.Equal(now, []string{"eyes"}) ||
		!slices.Equal(checkLog, []string{"create in_progress"}) {
		t.Errorf("while put back: reactions %v (now %v), checks %v; want the eyes on and the check in progress", log, now, checkLog)
	}
	if _, err := rig.st.db.ExecContext(ctx, `update review_runs set not_before=0 where org_id=? and id=?`, orgID, run.ID); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if run = rig.runs(7)[0]; run.Status != "posted" {
		t.Fatalf("after resuming: %s %q", run.Status, run.Error)
	}
	log, now, checkLog, checks := rig.gh.signals()
	if !slices.Equal(log, []string{"+eyes", "-eyes", "+rocket"}) || !slices.Equal(now, []string{"rocket"}) {
		t.Errorf("reactions went %v and are %v", log, now)
	}
	if !slices.Equal(checkLog, []string{"create in_progress", "update completed/success"}) || len(checks) != 1 ||
		checks[0]["external_id"] != run.PublicID {
		t.Errorf("check runs written %v: %v; want the one, completed", checkLog, checks)
	}
}

// A pull request this App opened — a fix job's — is not passed over as a bot's: it opens as a draft
// and is skipped as one, and is reviewed once somebody marks it ready. Another bot's still is not.
func TestReviewLaneReviewsTheAppsOwnPullRequests(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	ours := func(pr map[string]any) { pr["user"] = map[string]any{"login": "attesttag[bot]", "type": "Bot"} }
	draft := func(pr map[string]any) { pr["draft"] = true }
	rig.deliver("pull_request", prEvent("opened", 21, reviewHead, ours, draft))
	if pr := rig.pr(21); pr == nil || pr.SkipReason != "draft" || len(rig.runs(21)) != 0 {
		t.Fatalf("the fix job's draft: %+v", pr)
	}
	rig.deliver("pull_request", prEvent("ready_for_review", 21, reviewHead, ours))
	if runs := rig.runs(21); len(runs) != 1 || rig.pr(21).SkipReason != "" {
		t.Errorf("the fix job's pull request made ready: %d runs, skipped for %q", len(runs), rig.pr(21).SkipReason)
	}
	rig.deliver("pull_request", prEvent("opened", 22, reviewHead, func(pr map[string]any) {
		pr["user"] = map[string]any{"login": "renovate[bot]", "type": "Bot"}
	}))
	if pr := rig.pr(22); pr == nil || pr.SkipReason != "bot" || len(rig.runs(22)) != 0 {
		t.Errorf("another bot's pull request: %+v", pr)
	}
	// The authors' list still applies to the App's own.
	rig.setSettings(`{"mode":"live","exclude_authors":["attesttag*"]}`)
	rig.deliver("pull_request", prEvent("opened", 23, reviewHead, ours))
	if pr := rig.pr(23); pr == nil || pr.SkipReason != "excluded_author" {
		t.Errorf("the App's own, excluded by name: %+v", pr)
	}
}
