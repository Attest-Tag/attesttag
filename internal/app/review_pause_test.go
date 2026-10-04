package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Automatic reviews pause by themselves after reviewAutoPauseAfter on one pull request, and a member
// pauses and resumes them by command. What these pin: the sixth automatic trigger posts nothing but
// the summary's footer line, a review somebody asks for still runs while paused, a run queued before
// the pause and claimed after it is turned away, resume starts the count again, and only members of
// the organisation can do either. They run on both dialects.

// pushReviewed pushes sha to acme/web#7 on a repository reviewed on every push, lets the debounce
// pass and runs the lane.
func (rig *laneRig) pushReviewed(sha string) {
	rig.t.Helper()
	rig.gh.pushTo(sha, totalsFixture().files, totalsFixture().head)
	rig.deliver("pull_request", prEvent("synchronize", 7, sha))
	if _, err := rig.st.db.ExecContext(context.Background(), `update review_runs set not_before=0 where org_id=? and status='queued'`, orgID); err != nil {
		rig.t.Fatal(err)
	}
	rig.drain()
}

func shaOf(i int) string { return strings.Repeat(fmt.Sprintf("%x", 1+i%15), 40) }

// lastSummary is the summary comment as it stands now.
func (rig *laneRig) lastSummary() string {
	rig.t.Helper()
	_, _, comments, _ := rig.gh.snapshot()
	for _, c := range comments {
		if strings.Contains(c.Body, "Confidence") || strings.Contains(c.Body, "attest_tag review") {
			return c.Body
		}
	}
	return ""
}

// The opening and four pushes are five automatic reviews; the fifth push is the sixth automatic
// trigger, which pauses the pull request — recorded on it and audited — and says so in the footer of
// the summary, rendered again for nothing, and nowhere else. A push after that is passed over
// quietly. A command still reviews the head, and `status` and the API say the reviews are paused.
func TestReviewLaneAutoPausesAfterFive(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"push"}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	for i := range 4 {
		rig.pushReviewed(shaOf(i))
	}
	pr := rig.pr(7)
	if pr.AutoReviews != reviewAutoPauseAfter || pr.Paused || pr.ReviewsCount != 5 {
		t.Fatalf("after the opening and four pushes: %d automatic reviews of %d, paused %v", pr.AutoReviews, pr.ReviewsCount, pr.Paused)
	}
	calls := len(rig.model.requests(""))
	answers := len(rig.answers())

	rig.pushReviewed(shaOf(4))
	pr = rig.pr(7)
	if !pr.Paused || pr.SkipReason != "paused" || pr.ReviewsCount != 5 {
		t.Fatalf("after the sixth automatic trigger: paused %v, skip %q, %d reviews", pr.Paused, pr.SkipReason, pr.ReviewsCount)
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("the paused push made %d model calls", n-calls)
	}
	if n := len(rig.answers()); n != answers {
		t.Errorf("pausing posted %d comments; only the footer says so", n-answers)
	}
	if summary := rig.lastSummary(); !strings.Contains(summary, "Automatic reviews paused after 5 — `@attesttag resume`") {
		t.Errorf("the footer does not say the reviews are paused:\n%s", summary)
	}
	if ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.paused"}); len(ev) != 1 {
		t.Errorf("the pause was audited %d times", len(ev))
	}
	queued := len(rig.runs(7))
	rig.pushReviewed(shaOf(5))
	for _, r := range rig.runs(7)[:len(rig.runs(7))-queued] {
		if r.Kind == "review" {
			t.Errorf("a push to a paused pull request queued a review: %+v", r)
		}
	}
	if ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.paused"}); len(ev) != 1 {
		t.Errorf("a second push audited the pause again: %d", len(ev))
	}

	// Asked for, it runs: the pause is for the reviews nobody asked for.
	rig.deliver("issue_comment", commentEvent(1101, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	if runs := rig.runs(7); runs[0].Trigger != "command" || runs[0].Status != "posted" {
		t.Errorf("a command on a paused pull request = %+v", runs[0])
	}
	if pr := rig.pr(7); pr.AutoReviews != reviewAutoPauseAfter || !pr.Paused {
		t.Errorf("the command counted towards the pause, or lifted it: %+v", pr)
	}
	rig.deliver("issue_comment", commentEvent(1102, "alice", "MEMBER", "@attesttag status", true))
	a := rig.answers()
	if status := a[len(a)-1]; !strings.Contains(status, "Automatic reviews are paused: they pause by themselves after 5") ||
		!strings.Contains(status, "`@attesttag resume`") || strings.Contains(status, "Why the last request was not reviewed") {
		t.Errorf("status on a paused pull request:\n%s", status)
	}
}

// A push review queued while the pull request had four automatic reviews, and claimed once it had
// five, is the one past the pause: skipped, having spent nothing, and the pull request paused.
func TestReviewLaneAutoPauseAtTheClaim(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"push"}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	rig.gh.pushTo(reviewHeadC, totalsFixture().files, totalsFixture().head)
	rig.deliver("pull_request", prEvent("synchronize", 7, reviewHeadC))
	if _, err := rig.st.db.ExecContext(ctx, `update review_prs set auto_reviews=? where org_id=?`, reviewAutoPauseAfter, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.db.ExecContext(ctx, `update review_runs set not_before=0 where org_id=? and status='queued'`, orgID); err != nil {
		t.Fatal(err)
	}
	calls := len(rig.model.requests(""))
	rig.drain()
	var push *ReviewRun
	for _, r := range rig.runs(7) {
		if r.Kind == "review" && r.Trigger == "push" {
			push = r
		}
	}
	if push == nil || push.Status != "skipped" || !strings.HasPrefix(push.Error, "paused:") {
		t.Fatalf("the push review claimed past the pause = %+v", push)
	}
	if pr := rig.pr(7); !pr.Paused || pr.SkipReason != "paused" || len(rig.model.requests("")) != calls {
		t.Errorf("pull request %+v; %d model calls", pr, len(rig.model.requests(""))-calls)
	}
}

// pause and resume are a member's: a collaborator is refused, a member pauses — one answer, the
// footer follows — and resumes, which starts the count again, and the next push is reviewed. The
// API shows where the pull request stands.
func TestReviewCommandPauseAndResume(t *testing.T) {
	ctx := context.Background()
	api := newReviewAPIRig(t, `{"mode":"live","trigger":"push"}`)
	rig := api.laneRig
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()

	rig.deliver("issue_comment", commentEvent(1201, "carol", "COLLABORATOR", "@attesttag pause", true))
	if a := rig.answers(); len(a) != 1 || !strings.Contains(a[0], "`@attesttag pause` is for members of the organisation") || rig.pr(7).Paused {
		t.Fatalf("a collaborator's pause: %q, paused %v", a, rig.pr(7).Paused)
	}
	rig.deliver("issue_comment", commentEvent(1202, "alice", "MEMBER", "@attesttag pause automatic reviews", true))
	rig.drain()
	a := rig.answers()
	if pr := rig.pr(7); !pr.Paused || len(a) != 2 || !strings.HasPrefix(a[1], "Paused:") || !strings.Contains(a[1], "`@attesttag resume`") {
		t.Fatalf("a member's pause: paused %v, answers %q", pr.Paused, a)
	}
	if summary := rig.lastSummary(); !strings.Contains(summary, "Automatic reviews paused — `@attesttag resume`") {
		t.Errorf("the footer after a member's pause:\n%s", summary)
	}
	run := rig.runs(7)[len(rig.runs(7))-1] // the opening's review
	out := api.must(200, "GET", "/api/reviews/"+run.PublicID, api.viewer, nil)
	if p := out["pr"].(map[string]any); p["paused"] != true || p["paused_by"] != "member" || p["auto_pause_after"] != float64(reviewAutoPauseAfter) {
		t.Errorf("the API's pull request = %v", p)
	}
	rig.deliver("issue_comment", commentEvent(1203, "alice", "MEMBER", "@attesttag pause", true))
	if a := rig.answers(); !strings.Contains(a[len(a)-1], "already paused") {
		t.Errorf("pausing twice: %q", a[len(a)-1])
	}
	rig.pushReviewed(reviewHeadC)
	if pr := rig.pr(7); pr.LastReviewedSHA != reviewHead || pr.SkipReason != "paused" {
		t.Fatalf("a push while paused was reviewed: %+v", pr)
	}

	if _, err := rig.st.db.ExecContext(ctx, `update review_prs set auto_reviews=3 where org_id=?`, orgID); err != nil {
		t.Fatal(err)
	}
	rig.deliver("issue_comment", commentEvent(1204, "alice", "MEMBER", "@attesttag resume", true))
	rig.drain()
	a = rig.answers()
	if pr := rig.pr(7); pr.Paused || pr.AutoReviews != 0 || !strings.HasPrefix(a[len(a)-1], "Resumed:") {
		t.Fatalf("after resume: %+v, answer %q", pr, a[len(a)-1])
	}
	if summary := rig.lastSummary(); strings.Contains(summary, "paused") {
		t.Errorf("the footer still says paused after resume:\n%s", summary)
	}
	// The push turned away while paused left "paused" as the gate's last word; resumed, it is not
	// true any more, and `status` does not say it.
	rig.deliver("issue_comment", commentEvent(1205, "alice", "MEMBER", "@attesttag status", true))
	a = rig.answers()
	if pr := rig.pr(7); pr.SkipReason != "" || strings.Contains(a[len(a)-1], "paused") {
		t.Errorf("status after resume: skip %q\n%s", pr.SkipReason, a[len(a)-1])
	}
	rig.pushReviewed(shaOf(7))
	if pr := rig.pr(7); pr.LastReviewedSHA != shaOf(7) || pr.AutoReviews != 1 {
		t.Errorf("the push after resume: %+v", pr)
	}
	var outcomes []string
	for _, e := range rig.commandAudits() {
		outcomes = append(outcomes, e["outcome"].(string))
	}
	if strings.Join(outcomes, "|") != "answered|resumed|already paused|paused|refused: pausing and resuming automatic reviews is for members of the organisation" {
		t.Errorf("command audits = %q", outcomes)
	}
}

// A request that is one already done asks for no review, so at the ceiling it pauses nothing: the
// fifth automatic review's push delivered again, under a new delivery id, leaves the pull request
// as it was. The next push is the sixth, and pauses it.
func TestReviewLaneDuplicateAtTheCeilingPausesNothing(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"push"}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	for i := range 4 {
		rig.pushReviewed(shaOf(i))
	}
	runs := len(rig.runs(7))
	rig.deliver("pull_request", prEvent("synchronize", 7, shaOf(3)))
	if pr := rig.pr(7); pr.Paused || pr.SkipReason != "" || len(rig.runs(7)) != runs {
		t.Fatalf("the fifth push delivered again: paused %v, skip %q, %d runs more", pr.Paused, pr.SkipReason, len(rig.runs(7))-runs)
	}
	if ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.paused"}); len(ev) != 0 {
		t.Errorf("the duplicate audited a pause: %d", len(ev))
	}
	rig.pushReviewed(shaOf(4))
	if pr := rig.pr(7); !pr.Paused || !pr.PausedAuto {
		t.Errorf("the sixth automatic trigger did not pause the pull request: %+v", pr)
	}
}

// Who paused is recorded, not read off the count: a member who pauses a pull request that has had
// its five automatic reviews is a member pausing it — in the API, the footer and `status` — and the
// ceiling's own pause still reads as the ceiling's.
func TestReviewMemberPauseAfterFiveIsTheMembers(t *testing.T) {
	api := newReviewAPIRig(t, `{"mode":"live","trigger":"push"}`)
	rig := api.laneRig
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	for i := range 4 {
		rig.pushReviewed(shaOf(i))
	}
	rig.deliver("issue_comment", commentEvent(1301, "alice", "MEMBER", "@attesttag pause", true))
	rig.drain()
	if pr := rig.pr(7); !pr.Paused || pr.PausedAuto || pr.AutoReviews != reviewAutoPauseAfter {
		t.Fatalf("a member's pause after five: %+v", pr)
	}
	run := rig.runs(7)[len(rig.runs(7))-1]
	if p := api.must(200, "GET", "/api/reviews/"+run.PublicID, api.viewer, nil)["pr"].(map[string]any); p["paused_by"] != "member" {
		t.Errorf("paused_by = %v, want member", p["paused_by"])
	}
	if summary := rig.lastSummary(); !strings.Contains(summary, "Automatic reviews paused — `@attesttag resume`") {
		t.Errorf("the footer after a member's pause:\n%s", summary)
	}
	rig.deliver("issue_comment", commentEvent(1302, "alice", "MEMBER", "@attesttag status", true))
	a := rig.answers()
	if status := a[len(a)-1]; !strings.Contains(status, "Automatic reviews are paused: a member paused them") {
		t.Errorf("status after a member's pause:\n%s", status)
	}

	// Resumed, and paused by the ceiling at the sixth trigger since — five more are counted in place
	// here, since the day's throttle would stop them first — it is the ceiling's.
	rig.deliver("issue_comment", commentEvent(1303, "alice", "MEMBER", "@attesttag resume", true))
	rig.drain()
	if _, err := rig.st.db.ExecContext(context.Background(), `update review_prs set auto_reviews=? where org_id=?`, reviewAutoPauseAfter, orgID); err != nil {
		t.Fatal(err)
	}
	rig.pushReviewed(shaOf(5))
	if pr := rig.pr(7); !pr.Paused || !pr.PausedAuto {
		t.Fatalf("the sixth automatic trigger since the resume: %+v", pr)
	}
	if p := api.must(200, "GET", "/api/reviews/"+run.PublicID, api.viewer, nil)["pr"].(map[string]any); p["paused_by"] != "auto" {
		t.Errorf("paused_by = %v, want auto", p["paused_by"])
	}
}
