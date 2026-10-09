package app

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"attesttag/internal/review"
)

// Carrying reviews forward, end to end on the lane: a merged pull request reviewed at a head that
// holds a file at the same blob as this one's head lends that file its review, and what it left
// open or settled on it. What these pin: only a file the earlier review read, at exactly the blob
// it has here, is left out; a pull request every file of which was read that way costs no model
// call; what is still open there is listed and named in the risk line but never scored; what was
// argued away there is not raised again; and none of it crosses organisations. They run on both
// dialects.

// gitBlobSHA is the blob id git gives content, as the trees API lists it.
func gitBlobSHA(content string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

// carrySource is a merged pull request's review as the carry reads it: what it read and ran.
type carrySource struct {
	number  int
	head    string
	read    []string // changed files at that head a pass read
	notRead []string // changed files no pass finished
	types   []string
	shadow  bool // recorded in shadow, not posted
}

// mergedReview records acme/web#src.number as merged after a review at src.head, in org, and
// returns the pull request and the run.
func mergedReview(t *testing.T, st *Store, org int64, src carrySource) (*ReviewPR, *ReviewRun) {
	t.Helper()
	ctx := context.Background()
	pr, err := st.UpsertReviewPR(ctx, org, ReviewPRFacts{Repo: "acme/web", Number: src.number, State: "merged", HeadSHA: src.head})
	if err != nil {
		t.Fatal(err)
	}
	ck := reviewCheckpoint{FileHashes: map[string]string{}, NotReviewed: []reviewNotReviewed{}}
	for _, k := range src.types {
		ck.Types = append(ck.Types, reviewTypeRunJSON{Key: k})
	}
	for _, p := range append(append([]string{}, src.read...), src.notRead...) {
		ck.FileHashes[p] = "h:" + p
	}
	for _, p := range src.notRead {
		ck.NotReviewed = append(ck.NotReviewed, reviewNotReviewed{Path: p, Reason: "budget"})
	}
	raw, _ := json.Marshal(ck)
	run, _, err := st.EnqueueReviewRun(ctx, org, ReviewRunRequest{ReviewPRID: pr.ID, InstallationID: fakeInstallation, Kind: "review",
		Trigger: "open", DedupeKey: fmt.Sprintf("merged:%d", src.number), HeadSHA: src.head})
	if err != nil {
		t.Fatal(err)
	}
	status := "posted"
	if src.shadow {
		status = "shadow"
	}
	if _, err := st.db.ExecContext(ctx, `update review_runs set status=?, outcome_json=?, started_at=?, finished_at=?
		where org_id=? and id=?`, status, string(raw), now(), now(), org, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `update review_prs set last_reviewed_sha=? where org_id=? and id=?`, src.head, org, pr.ID); err != nil {
		t.Fatal(err)
	}
	run, _ = st.ReviewRun(ctx, org, run.ID)
	return pr, run
}

// carriedFinding files a finding on a merged pull request's review, in status, with an inline comment
// when comment is not 0.
func carriedFinding(t *testing.T, st *Store, pr *ReviewPR, run *ReviewRun, f review.Finding, status review.FindingStatus, comment int64) {
	t.Helper()
	ctx := context.Background()
	stored, err := st.InsertReviewFinding(ctx, pr.OrgID, pr.ID, run.ID, &ReviewFinding{Finding: f, Status: status, Kind: reviewKindFinding,
		Fingerprint: review.Fingerprint("acme/web", f), AnchorSHA: run.HeadSHA})
	if err != nil {
		t.Fatal(err)
	}
	if comment > 0 {
		if err := st.SetReviewFindingComment(ctx, pr.OrgID, stored.ID, comment); err != nil {
			t.Fatal(err)
		}
	}
}

// lockOnTotals is the lock finding as an earlier review filed it.
func lockOnTotals() review.Finding {
	return review.Finding{Path: "src/totals.go", Side: review.Right, Line: 12, Severity: review.P1, Category: "concurrency",
		Title: "Add writes the total without the lock", Symbol: "Add", Scenario: "Two goroutines call Add at once."}
}

// latestCheckpoint is the checkpoint of acme/web#7's newest review.
func (rig *laneRig) latestCheckpoint() *reviewCheckpoint {
	rig.t.Helper()
	for _, r := range rig.runs(7) {
		if r.Kind == "review" {
			if ck, ok := checkpointFrom(r); ok {
				return ck
			}
		}
	}
	rig.t.Fatal("no review of #7 has a checkpoint")
	return nil
}

// finderPrompts are the user messages every finder pass was sent.
func (rig *laneRig) finderPrompts() string {
	var b strings.Builder
	for _, q := range rig.model.requests("finder") {
		b.WriteString(strings.Join(q.Users, "\n"))
	}
	return b.String()
}

const (
	headFive  = "5555555555555555555555555555555555555555"
	headSix   = "6666666666666666666666666666666666666666"
	headThree = "3333333333333333333333333333333333333333"
	featureGo = "package totals\n\n// Double adds the running total to itself.\nfunc (t *Totals) Double() { t.Add(t.Get()) }\n"
	laterGo   = "package totals\n\n// Reset sets the running total back to nothing.\nfunc (t *Totals) Reset() { t.mu.Lock(); t.value = 0; t.mu.Unlock() }\n"
)

// A changed file a merged pull request's review read at the blob it has here is left out of the
// finder's passes and listed as reviewed earlier; one whose blob differs, one that review did not
// change, and one it did not finish reading are all read again. The file carried is not unreviewed,
// so the score is not capped for it.
func TestReviewCarriesFilesAnEarlierReviewReadAtTheSameBlob(t *testing.T) {
	fx := totalsFixture()
	fx.addFile("src/feature.go", featureGo)
	fx.addFile("src/later.go", laterGo)
	rig := newLaneRig(t, fx, `{"mode":"live"}`)
	rig.confirmLock()
	// #5 read feature.go as it is here, and later.go as it was before #6 changed it; it never
	// changed totals.go, which its head holds at this head's blob all the same.
	mergedReview(t, rig.st, orgID, carrySource{number: 5, head: headFive, read: []string{"src/feature.go", "src/later.go"},
		types: []string{"general"}})
	rig.gh.content[headFive] = map[string]string{"src/feature.go": featureGo, "src/later.go": "package totals // older\n",
		"src/totals.go": totalsHead}
	// #6 has later.go as it is here, but its review never finished reading it.
	mergedReview(t, rig.st, orgID, carrySource{number: 6, head: headSix, notRead: []string{"src/later.go"}, read: []string{"src/x.go"},
		types: []string{"general"}})
	rig.gh.content[headSix] = map[string]string{"src/later.go": laterGo, "src/x.go": "package x\n"}

	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()

	prompts := rig.finderPrompts()
	if strings.Contains(prompts, "## File: src/feature.go") {
		t.Error("the file #5 read at this blob was in a finder pass")
	}
	for _, p := range []string{"src/totals.go", "src/later.go"} {
		if !strings.Contains(prompts, "## File: "+p) {
			t.Errorf("%s was not read, though no earlier review read it as it is here", p)
		}
	}
	ck := rig.latestCheckpoint()
	if len(ck.Carried) != 1 || ck.Carried[0].Path != "src/feature.go" || ck.Carried[0].PR != 5 || len(ck.Carried[0].Types) != 0 {
		t.Errorf("carried = %+v, want feature.go from #5", ck.Carried)
	}
	for _, f := range ck.NotReviewed {
		if f.Path == "src/feature.go" {
			t.Errorf("the carried file is listed as not reviewed: %+v", f)
		}
	}
	summary := rig.lastSummary()
	if !strings.Contains(summary, "Reviewed earlier, unchanged since (1 file, in #5)") ||
		!strings.Contains(summary, "`src/feature.go` · [#5](https://github.com/acme/web/pull/5)") {
		t.Errorf("the summary does not list the carried file:\n%s", summary)
	}
	if strings.Contains(summary, "Not reviewed") || strings.Contains(summary, "capped at 4") {
		t.Errorf("the carried file counts against the review:\n%s", summary)
	}
}

// Every changed file read before at the same blob: the summary is posted with no finder pass and no
// model call at all, says why, and scores the pull request on what this review knows — what #5 left
// open on the file is listed with a link to its thread and named in the first sentence, and not
// counted. What #5 left open on a file this pull request does not carry is not listed.
func TestReviewEveryFileCarriedAsksNoModel(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.confirmLock()
	pr5, run5 := mergedReview(t, rig.st, orgID, carrySource{number: 5, head: headFive, read: []string{"src/totals.go", "src/gone.go"},
		types: []string{"general"}})
	rig.gh.content[headFive] = map[string]string{"src/totals.go": totalsHead, "src/gone.go": "package totals\n"}
	carriedFinding(t, rig.st, pr5, run5, lockOnTotals(), review.FindingOpen, 555)
	elsewhere := lockOnTotals()
	elsewhere.Path, elsewhere.Title = "src/gone.go", "Gone writes without the lock"
	carriedFinding(t, rig.st, pr5, run5, elsewhere, review.FindingOpen, 556)

	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()

	if n := len(rig.model.requests("")); n != 0 {
		t.Errorf("%d model calls for a pull request whose every file was reviewed before", n)
	}
	run := rig.runs(7)[0]
	if run.Status != "posted" || run.CostUSD != 0 || run.Kept != 0 {
		t.Fatalf("the run = %s, $%.2f, %d kept", run.Status, run.CostUSD, run.Kept)
	}
	summary := rig.lastSummary()
	for _, want := range []string{
		"Confidence 5/5",
		"Every changed file this review would read was reviewed in #5 at exactly the contents it has here",
		"Still open on the merged pull request this code came in with: **Add writes the total without the lock** (P1, [#5](https://github.com/acme/web/pull/5)).",
		"Open from merged pull requests (1)",
		"[Add writes the total without the lock](https://github.com/acme/web/pull/5#discussion_r555)",
		"Reviewed earlier, unchanged since (1 file, in #5)",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not say %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "Gone writes without the lock") || strings.Contains(summary, "Open findings") {
		t.Errorf("the summary lists what it should not:\n%s", summary)
	}
	if pr := rig.pr(7); pr.Score != 5 || pr.LastReviewedSHA != reviewHead {
		t.Errorf("the pull request after a carried review: %+v", pr)
	}
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 || len(posts[0]["comments"].([]reviewInlineComment)) != 0 {
		t.Errorf("review posts = %+v, want one with no inline comment", posts)
	}
}

// What a merged pull request settled on the code this one changes is told to the finder, and a
// candidate repeating one argued away there is dropped by Go, as one withdrawn on the pull request
// itself is: the author answered it once.
func TestReviewWithdrawnOnAMergedPullRequestIsNotRaisedAgain(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.confirmLock()
	// #5 read totals.go as it was before this pull request changed it again: nothing is carried.
	pr5, run5 := mergedReview(t, rig.st, orgID, carrySource{number: 5, head: headFive, read: []string{"src/totals.go"},
		types: []string{"general"}})
	rig.gh.content[headFive] = map[string]string{"src/totals.go": totalsBase}
	carriedFinding(t, rig.st, pr5, run5, lockOnTotals(), review.FindingWithdrawn, 555)
	fixed := lockOnTotals()
	fixed.Title, fixed.Symbol = "Get reads the total twice", "Get"
	carriedFinding(t, rig.st, pr5, run5, fixed, review.FindingFixed, 556)

	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()

	prompts := rig.finderPrompts()
	for _, want := range []string{
		"Settled on the merged pull requests this code came in with",
		"- Add writes the total without the lock · src/totals.go (#5, withdrawn)",
		"- Get reads the total twice · src/totals.go (#5, fixed)",
	} {
		if !strings.Contains(prompts, want) {
			t.Errorf("the finder was not told %q:\n%s", want, prompts)
		}
	}
	if n := len(rig.model.requests("verifier")); n != 0 {
		t.Errorf("the repeat went to the verifier %d times", n)
	}
	if fs, _ := rig.st.ReviewFindings(context.Background(), orgID, rig.pr(7).ID); len(fs) != 0 {
		t.Errorf("findings = %+v; the one withdrawn on #5 was raised again", fs)
	}
	var dropped *reviewDrop
	for _, d := range rig.latestCheckpoint().Drops {
		if d.Reason == "withdrawn" {
			dropped = &d
		}
	}
	if dropped == nil || !strings.Contains(dropped.Detail, "#5") {
		t.Errorf("the repeat was not dropped as withdrawn on #5: %+v", rig.latestCheckpoint().Drops)
	}
	if ck := rig.latestCheckpoint(); len(ck.Carried) != 0 || len(ck.CarriedOpen) != 0 {
		t.Errorf("a file whose blob changed was carried: %+v", ck)
	}
}

// The carry reads only the organisation's own merged pull requests in the repository, at the head
// each was last reviewed at, as a live review would show them: not another organisation's, not an
// open one, not one reviewed only in shadow, not this pull request itself.
func TestReviewCarryReadsOnlyTheOrganisationsMergedReviews(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const other = int64(2)
	mine := upsertPR(t, st, orgID, "acme/web", 7)
	pr5, run5 := mergedReview(t, st, orgID, carrySource{number: 5, head: headFive, read: []string{"a.go"}, types: []string{"general"}})
	carriedFinding(t, st, pr5, run5, review.Finding{Path: "a.go", Line: 1, Title: "Mine"}, review.FindingOpen, 0)
	theirs, theirRun := mergedReview(t, st, other, carrySource{number: 6, head: headSix, read: []string{"a.go"}, types: []string{"general"}})
	carriedFinding(t, st, theirs, theirRun, review.Finding{Path: "a.go", Line: 1, Title: "Theirs"}, review.FindingOpen, 0)
	mergedReview(t, st, orgID, carrySource{number: 3, head: headThree, read: []string{"a.go"}, types: []string{"general"}, shadow: true})
	open, _ := mergedReview(t, st, orgID, carrySource{number: 8, head: headSix, read: []string{"a.go"}, types: []string{"general"}})
	if _, err := st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "acme/web", Number: open.Number, State: "open"}); err != nil {
		t.Fatal(err)
	}

	runs, err := st.reviewCarryRuns(ctx, orgID, "acme/web", mine.ID, false, nowMinus(reviewCarryWindow), 10)
	if err != nil || len(runs) != 1 || runs[0].PRNumber != 5 || runs[0].OrgID != orgID {
		t.Fatalf("live carry runs = %+v (%v), want #5 alone", runs, err)
	}
	if runs, _ := st.reviewCarryRuns(ctx, orgID, "acme/web", mine.ID, true, nowMinus(reviewCarryWindow), 10); len(runs) != 2 {
		t.Errorf("a shadow review carries from %d runs, want #5 and #3's", len(runs))
	}
	if runs, _ := st.reviewCarryRuns(ctx, orgID, "acme/web", pr5.ID, false, nowMinus(reviewCarryWindow), 10); len(runs) != 0 {
		t.Errorf("a pull request carries from its own review: %+v", runs)
	}
	if runs, _ := st.reviewCarryRuns(ctx, orgID, "acme/web", mine.ID, false, nowMinus(-reviewCarryWindow), 10); len(runs) != 0 {
		t.Errorf("a review older than the window was read: %+v", runs)
	}
	fs, err := st.reviewCarryFindings(ctx, orgID, []int64{pr5.ID, theirs.ID}, false)
	if err != nil || len(fs) != 1 || fs[0].Title != "Mine" {
		t.Errorf("carried findings = %+v (%v), want the organisation's own", fs, err)
	}
	claim := newPublicID()
	if _, _, err := st.EnqueueReviewRun(ctx, orgID, ReviewRunRequest{ReviewPRID: mine.ID, Kind: "review", Trigger: "push",
		DedupeKey: "claimed", RequestJSON: `{"claim":"` + claim + `"}`}); err != nil {
		t.Fatal(err)
	}
	if taken, err := st.reviewClaimTaken(ctx, orgID, mine.ID, claim); err != nil || !taken {
		t.Errorf("the organisation's own queued run does not carry its claim: %v", err)
	}
	if taken, _ := st.reviewClaimTaken(ctx, other, mine.ID, claim); taken {
		t.Error("another organisation sees a claim carried by this one's run")
	}
	if taken, _ := st.reviewClaimTaken(ctx, orgID, mine.ID, "%"); taken {
		t.Error("a claim that is not a public id matched as a pattern")
	}
}

// A push that leaves the pull request's own diff as its last review read it — the base merged in —
// is not reviewed and not counted towards the pause: turned away at the claim below the ceiling, and
// at the gate at the ceiling, where it must not be the push that pauses the pull request. A push
// that changes something is reviewed, and the next one past the ceiling pauses it.
func TestReviewPushThatLeavesTheDiffAloneIsNotReviewed(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"push","auto_pause_after":2}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	calls := len(rig.model.requests(""))

	baseMerged := func(sha string) {
		rig.gh.pushTo(sha, totalsFixture().files, totalsFixture().head)
		rig.deliver("pull_request", prEvent("synchronize", 7, sha))
		rig.releaseQueued()
		rig.drain()
	}
	baseMerged(shaOf(0))
	if push := rig.latestOf("push"); push == nil || push.Status != "skipped" || !strings.HasPrefix(push.Error, "unchanged_diff:") {
		t.Fatalf("the push that merged the base in = %+v", push)
	}
	if pr := rig.pr(7); pr.AutoReviews != 1 || pr.SkipReason != reviewSkipUnchanged || len(rig.model.requests("")) != calls {
		t.Fatalf("after it: %+v, %d model calls", pr, len(rig.model.requests(""))-calls)
	}

	rig.pushReviewed(shaOf(1))
	if pr := rig.pr(7); pr.AutoReviews != 2 || pr.LastReviewedSHA != shaOf(1) {
		t.Fatalf("a push that changed something: %+v", pr)
	}
	runs := len(rig.runs(7))
	files, head := pushedTotals(shaOf(1))
	rig.gh.pushTo(shaOf(2), files, head)
	rig.deliver("pull_request", prEvent("synchronize", 7, shaOf(2)))
	if pr := rig.pr(7); pr.Paused || pr.SkipReason != reviewSkipUnchanged {
		t.Fatalf("an unchanged push at the ceiling: paused %v, skip %q", pr.Paused, pr.SkipReason)
	}
	for _, r := range rig.runs(7)[:len(rig.runs(7))-runs] {
		if r.Kind == "review" {
			t.Errorf("an unchanged push at the ceiling queued a review: %+v", r)
		}
	}
	rig.pushReviewed(shaOf(3))
	if pr := rig.pr(7); !pr.Paused || !pr.PausedAuto || pr.LastReviewedSHA != shaOf(1) {
		t.Errorf("the changed push past the ceiling: %+v", pr)
	}
}

// The ceiling is the settings': ten when they say nothing, where the footer says so; none at all
// with auto_pause_after 0, however many automatic reviews there have been.
func TestReviewPauseCeilingIsASetting(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"push"}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if _, err := rig.st.db.ExecContext(ctx, `update review_prs set auto_reviews=? where org_id=?`, review.DefaultAutoPauseAfter-1, orgID); err != nil {
		t.Fatal(err)
	}
	rig.pushReviewed(shaOf(0))
	if pr := rig.pr(7); pr.Paused || pr.AutoReviews != review.DefaultAutoPauseAfter {
		t.Fatalf("the tenth automatic review: %+v", pr)
	}
	rig.pushReviewed(shaOf(1))
	if pr := rig.pr(7); !pr.Paused || !pr.PausedAuto {
		t.Fatalf("the eleventh automatic trigger: %+v", pr)
	}
	if summary := rig.lastSummary(); !strings.Contains(summary, "Automatic reviews paused after 10 — `@attesttag resume`") {
		t.Errorf("the footer:\n%s", summary)
	}

	rig.setSettings(`{"mode":"live","trigger":"push","auto_pause_after":0}`)
	rig.deliver("issue_comment", commentEvent(1401, "alice", "MEMBER", "@attesttag resume", true))
	rig.drain()
	if a := rig.answers(); !strings.HasPrefix(a[len(a)-1], "Resumed: this pull request is reviewed by itself again. ") {
		t.Errorf("resume under no ceiling: %q", a[len(a)-1])
	}
	if _, err := rig.st.db.ExecContext(ctx, `update review_prs set auto_reviews=50 where org_id=?`, orgID); err != nil {
		t.Fatal(err)
	}
	rig.pushReviewed(shaOf(2))
	if pr := rig.pr(7); pr.Paused || pr.LastReviewedSHA != shaOf(2) {
		t.Errorf("a push under auto_pause_after 0: %+v", pr)
	}
}

// While the ceiling holds the pause, a fix somebody with access claims in a finding's thread lets one
// push through: the push that came before the claim is reviewed when the claim is recorded, and a
// claim made with nothing new to review lets the next push through at the gate. Neither review counts
// towards the pause, and a claim once carried lets nothing else through.
func TestReviewFixClaimLetsOnePushThroughThePause(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"push","auto_pause_after":1}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.model.classify = func(reviewModelReq) reviewModelReply { return classifyAs("fixed_claim", "", "") }
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	f := rig.finding()

	rig.pushReviewed(shaOf(0))
	if pr := rig.pr(7); !pr.Paused || !pr.PausedAuto || pr.LastReviewedSHA != reviewHead {
		t.Fatalf("past the ceiling: %+v", pr)
	}

	// Pushed, then claimed: the claim queues the review of the head it is about.
	rig.reply(f, "octocat", "CONTRIBUTOR", "Fixed.")
	rig.drain()
	rig.releaseQueued()
	rig.drain()
	pr := rig.pr(7)
	if pr.LastReviewedSHA != shaOf(0) || !pr.Paused || pr.AutoReviews != 1 {
		t.Fatalf("after a claim on a paused pull request: %+v", pr)
	}
	if push := rig.latestOf("push"); push == nil || push.Status != "posted" || reviewOptionsOf(push).Claim == "" {
		t.Fatalf("the review the claim let through = %+v", push)
	}

	// Claimed again with the head already reviewed: nothing is queued, and the next push is let
	// through at the gate.
	rig.reply(f, "alice", "MEMBER", "Addressed.")
	rig.drain()
	rig.releaseQueued()
	if n := rig.drain(); n != 0 {
		t.Errorf("a claim with nothing new to review ran %d more runs", n)
	}
	rig.pushReviewed(shaOf(1))
	if pr := rig.pr(7); pr.LastReviewedSHA != shaOf(1) || !pr.Paused || pr.AutoReviews != 1 {
		t.Fatalf("the push after a claim: %+v", pr)
	}

	// Both claims are carried now: the next push stays paused.
	rig.pushReviewed(shaOf(2))
	if pr := rig.pr(7); pr.LastReviewedSHA != shaOf(1) || pr.SkipReason != "paused" {
		t.Errorf("a push once every claim was carried: %+v", pr)
	}
}
