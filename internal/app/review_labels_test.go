package app

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// Label rules: a label on a pull request adds review types to the ones its branch rule chose. What
// these pin: the types are added, not swapped in, and the rule the run records names the label; a
// label put on later reviews only what it added, once, after a wait, and not at all when the head has
// had those types already — including when the opening that runs them is queued beside it. They run
// on both dialects.

// securityLabelRules is a live repository where the label security-review adds Security to General.
// Automatic types are off: these are about the types labels add, and the fixture's diff, which takes
// a lock away, would bring Concurrency into every review besides (review_auto_test.go).
const securityLabelRules = `{"mode":"live","auto_types":false,"branch_rules":[{"labels":["security-review"],"types":["security"]},{"types":["general"]}]}`

func withLabels(labels ...string) func(pr map[string]any) {
	return func(pr map[string]any) {
		var ls []map[string]any
		for _, l := range labels {
			ls = append(ls, map[string]any{"name": l})
		}
		pr["labels"] = ls
	}
}

// labeledEvent is the delivery GitHub sends when label is put on acme/web#7 at head, which then
// carries labels.
func labeledEvent(label, head string, labels ...string) []byte {
	return prEventWith("labeled", 7, head, map[string]any{"label": map[string]any{"name": label}}, withLabels(labels...))
}

// prEventWith is prEvent with more top-level fields: the label a "labeled" delivery is about.
func prEventWith(action string, number int, head string, extra map[string]any, edit ...func(pr map[string]any)) []byte {
	var m map[string]any
	json.Unmarshal(prEvent(action, number, head, edit...), &m)
	maps.Copy(m, extra)
	b, _ := json.Marshal(m)
	return b
}

// finderTypes are the review types finder passes were run for, each once, in order.
func finderTypes(reqs []reviewModelReq) []string {
	var out []string
	for _, q := range reqs {
		if !slices.Contains(out, q.Type) {
			out = append(out, q.Type)
		}
	}
	return out
}

func labelRig(t *testing.T) *laneRig {
	t.Helper()
	rig := newLaneRig(t, totalsFixture(), securityLabelRules)
	rig.confirmLock()
	rig.model.finder["security"] = func(int, reviewModelReq) reviewModelReply { return submitFindings() }
	return rig
}

func runTypes(r *ReviewRun) []string {
	var keys []string
	for _, t := range r.Types {
		keys = append(keys, t.Key)
	}
	return keys
}

// A pull request opened with the label gets the branch rule's types and the label's, one review, and
// the rule it records says why: "any → any +label:security-review", in the summary's footer too.
func TestReviewLabelAddsTypesToTheBranchRule(t *testing.T) {
	rig := labelRig(t)
	rig.gh.labels = []string{"Security-Review"}
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead, withLabels("Security-Review")))
	rig.drain()
	runs := rig.runs(7)
	if len(runs) != 1 || runs[0].Status != "posted" || !slices.Equal(runTypes(runs[0]), []string{"general", "security"}) ||
		runs[0].RuleLabel != "any → any +label:Security-Review" {
		t.Fatalf("runs = %+v", runs)
	}
	if got := finderTypes(rig.model.requests("finder")); !slices.Equal(got, []string{"general", "security"}) {
		t.Errorf("finder passes for %v, want General's and Security's", got)
	}
	if summary := rig.lastSummary(); !strings.Contains(summary, "Rule: `any → any +label:Security-Review`") {
		t.Errorf("the footer does not name the label:\n%s", summary)
	}
	// Without the label, the same head and settings are another review: the label is in the key.
	plain := labelRig(t)
	plain.deliver("pull_request", prEvent("opened", 7, reviewHead))
	plain.drain()
	if r := plain.runs(7)[0]; !slices.Equal(runTypes(r), []string{"general"}) || r.RuleLabel != "any → any" || r.CacheKey == runs[0].CacheKey {
		t.Errorf("without the label: %+v", r)
	}
}

// A label put on a pull request reviewed without it queues a review of what it adds and nothing
// else, after the wait a push gets; its run records the label. The same label again, a label that
// adds a type already reviewed at this head, and a label no rule names queue nothing.
func TestReviewLabelPutOnLaterReviewsWhatItAdds(t *testing.T) {
	ctx := context.Background()
	rig := labelRig(t)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	finders := len(rig.model.requests("finder"))

	rig.gh.labels = []string{"security-review"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	runs := rig.runs(7)
	label := runs[0]
	if len(runs) != 2 || label.Trigger != "label" || label.Status != "queued" || !slices.Equal(runTypes(label), []string{"security"}) ||
		label.RuleLabel != "any → any +label:security-review" || time.Until(time.Unix(0, label.NotBefore)) < time.Minute {
		t.Fatalf("after the label: %+v", runs)
	}
	if rig.pr(7).HeadSHA != reviewHead {
		t.Errorf("the label's delivery moved the stored head")
	}
	if _, err := rig.st.db.ExecContext(ctx, `update review_runs set not_before=0 where org_id=? and status='queued'`, orgID); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if got, _ := rig.st.ReviewRun(ctx, orgID, label.ID); got.Status != "posted" {
		t.Fatalf("the label's review = %s %q", got.Status, got.Error)
	}
	if got := finderTypes(rig.model.requests("finder")[finders:]); !slices.Equal(got, []string{"security"}) {
		t.Errorf("the label's review ran finder passes for %v; want Security's alone", got)
	}
	if pr := rig.pr(7); pr.AutoReviews != 1 || pr.ReviewsCount != 2 {
		t.Errorf("the label's review counted towards the pause: %+v", pr)
	}

	before := len(rig.runs(7))
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	rig.deliver("pull_request", labeledEvent("wip", reviewHead, "security-review", "wip"))
	if n := len(rig.runs(7)); n != before {
		t.Errorf("a label already reviewed at this head, or one no rule names, queued %d runs", n-before)
	}
}

// The label set as the pull request was opened is delivered beside the opening, and either can be
// dispatched first. Taken first, the label's review waits; the opening's runs both types; and the
// label's, claimed after, finds its type reviewed at this head and stands aside for nothing.
func TestReviewLabelBesideTheOpeningCostsNothing(t *testing.T) {
	ctx := context.Background()
	rig := labelRig(t)
	rig.gh.labels = []string{"security-review"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead, withLabels("security-review")))
	rig.drain()
	if _, err := rig.st.db.ExecContext(ctx, `update review_runs set not_before=0 where org_id=? and status='queued'`, orgID); err != nil {
		t.Fatal(err)
	}
	calls := len(rig.model.requests(""))
	rig.drain()
	byTrigger := map[string]*ReviewRun{}
	for _, r := range rig.runs(7) {
		if r.Kind == "review" {
			byTrigger[r.Trigger] = r
		}
	}
	open, label := byTrigger["open"], byTrigger["label"]
	if open == nil || open.Status != "posted" || !slices.Equal(runTypes(open), []string{"general", "security"}) {
		t.Fatalf("the opening's review = %+v", open)
	}
	if label == nil || label.Status != "noop" || !strings.Contains(label.Error, "another review of this head") {
		t.Fatalf("the label's review = %+v", label)
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("the label's review made %d model calls", n-calls)
	}

	// The opening first: the label finds its type queued already, and queues nothing.
	other := labelRig(t)
	other.deliver("pull_request", prEvent("opened", 7, reviewHead, withLabels("security-review")))
	other.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	if runs := other.runs(7); len(runs) != 1 || runs[0].Trigger != "open" {
		t.Errorf("the label after the opening: %+v", runs)
	}
}

// A label on a repository reviewed only when somebody asks queues nothing, and records nothing over
// the pull request's own reason.
func TestReviewLabelHonoursWhen(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"command","branch_rules":[{"labels":["perf"],"types":["performance"]},{}]}`)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.deliver("pull_request", labeledEvent("perf", reviewHead, "perf"))
	if pr := rig.pr(7); len(rig.runs(7)) != 0 || pr.SkipReason != "trigger" {
		t.Errorf("a label on a repository reviewed when asked: %d runs, %+v", len(rig.runs(7)), pr)
	}
}

// releaseQueued lets every queued run be claimed now, past its wait.
func (rig *laneRig) releaseQueued() {
	rig.t.Helper()
	if _, err := rig.st.db.ExecContext(context.Background(), `update review_runs set not_before=0 where org_id=? and status='queued'`, orgID); err != nil {
		rig.t.Fatal(err)
	}
}

// latestOf is the pull request's newest review with this trigger.
func (rig *laneRig) latestOf(trigger string) *ReviewRun {
	rig.t.Helper()
	for _, r := range rig.runs(7) {
		if r.Kind == "review" && r.Trigger == trigger {
			return r
		}
	}
	return nil
}

// A label taken off while its review waits ends that review having spent nothing: what a label's
// review runs is worked out again at the claim, from the pull request as GitHub has it then. Put back
// on, it is reviewed — the review that ended for nothing does not stand for one that ran.
func TestReviewLabelTakenOffBeforeItsReview(t *testing.T) {
	rig := labelRig(t)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	finders := len(rig.model.requests("finder"))
	rig.gh.labels = []string{"security-review"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	rig.gh.labels = nil // and off again, inside the wait
	rig.releaseQueued()
	rig.drain()
	if label := rig.latestOf("label"); label == nil || label.Status != "noop" || !strings.Contains(label.Error, "no longer on the pull request") {
		t.Fatalf("the review of a label taken off = %+v", label)
	}
	if n := len(rig.model.requests("finder")); n != finders {
		t.Errorf("the label taken off still made %d finder passes", n-finders)
	}

	first := rig.latestOf("label")
	rig.gh.labels = []string{"security-review"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	rig.releaseQueued()
	rig.drain()
	again := rig.latestOf("label")
	if again == nil || again.ID == first.ID || again.Status != "posted" || !slices.Equal(runTypes(again), []string{"security"}) {
		t.Fatalf("the label put back = %+v", again)
	}
	if got := finderTypes(rig.model.requests("finder")[finders:]); !slices.Equal(got, []string{"security"}) {
		t.Errorf("finder passes for %v; want Security's", got)
	}
}

// The label's review claimed before the opening is even queued — the "opened" delivery late, or the
// catch-up standing in for it — reviews the label's types; the opening then reviews the branch rule's
// and not those again, and names no label it did not review for.
func TestReviewLabelFirstThenTheOpening(t *testing.T) {
	rig := labelRig(t)
	rig.gh.labels = []string{"security-review"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	rig.releaseQueued()
	rig.drain()
	if label := rig.latestOf("label"); label == nil || label.Status != "posted" || !slices.Equal(runTypes(label), []string{"security"}) {
		t.Fatalf("the label's review = %+v", label)
	}
	finders := len(rig.model.requests("finder"))
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead, withLabels("security-review")))
	rig.drain()
	open := rig.latestOf("open")
	if open == nil || open.Status != "posted" || !slices.Equal(runTypes(open), []string{"general"}) || open.RuleLabel != "any → any" {
		t.Fatalf("the opening after the label's review = %+v", open)
	}
	if got := finderTypes(rig.model.requests("finder")[finders:]); !slices.Equal(got, []string{"general"}) {
		t.Errorf("the opening ran finder passes for %v; Security was reviewed at this head already", got)
	}
}

// A label's review that stood aside for the opening's covers nothing: when the opening then fails,
// the label put back on reviews its types, rather than finding them "reviewed" by the run that stood
// aside.
func TestReviewLabelStoodAsideCoversNothing(t *testing.T) {
	ctx := context.Background()
	rig := labelRig(t)
	rig.gh.labels = []string{"security-review"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead, withLabels("security-review")))
	// The opening waits and the label's review is claimed first, while the opening is queued.
	rig.releaseQueued()
	if _, err := rig.st.db.ExecContext(ctx, `update review_runs set not_before=? where org_id=? and status='queued' and trigger='open'`,
		time.Now().Add(time.Hour).UnixNano(), orgID); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	label := rig.latestOf("label")
	if label == nil || label.Status != "noop" || !strings.Contains(label.Error, "another review of this head") {
		t.Fatalf("the label's review beside a queued opening = %+v", label)
	}
	open := rig.latestOf("open")
	if _, err := rig.st.db.ExecContext(ctx, `update review_runs set status='failed', error='model: down' where id=?`, open.ID); err != nil {
		t.Fatal(err)
	}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	if again := rig.latestOf("label"); again == nil || again.ID == label.ID || again.Status != "queued" ||
		!slices.Equal(runTypes(again), []string{"security"}) {
		t.Fatalf("the label put back after the opening failed = %+v", again)
	}
}

// A pull request whose automatic reviews are paused gets no label's review: none is queued, and one
// queued before the pause is turned away at its claim, spending nothing. A label a bot puts on is
// counted towards the pause like a push, and the run says who put it on.
func TestReviewLabelHeldByThePause(t *testing.T) {
	rig := labelRig(t)
	rig.serveConversation()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()

	rig.gh.labels = []string{"security-review"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	queued := rig.latestOf("label")
	if queued == nil || queued.Status != "queued" || queued.RequestedBy != "github:octocat" {
		t.Fatalf("the label's review = %+v", queued)
	}
	rig.deliver("issue_comment", commentEvent(1401, "alice", "MEMBER", "@attesttag pause", true))
	calls := len(rig.model.requests(""))
	rig.releaseQueued()
	rig.drain()
	if got := rig.latestOf("label"); got.ID != queued.ID || got.Status != "skipped" || !strings.HasPrefix(got.Error, "paused:") {
		t.Fatalf("the label's review claimed once paused = %+v", got)
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("the paused label's review made %d model calls", n-calls)
	}
	rig.gh.labels = []string{"security-review", "perf"}
	rig.deliver("pull_request", labeledEvent("security-review", reviewHead, "security-review"))
	if got := rig.latestOf("label"); got.ID != queued.ID {
		t.Errorf("a label on a paused pull request queued %+v", got)
	}

	// Resumed, a bot's label: queued as the bot's, and counted when it runs.
	rig.deliver("issue_comment", commentEvent(1402, "alice", "MEMBER", "@attesttag resume", true))
	rig.drain()
	bot := prEventWith("labeled", 7, reviewHead, map[string]any{"label": map[string]any{"name": "security-review"},
		"sender": map[string]any{"login": "labeler[bot]", "type": "Bot"}}, withLabels("security-review"))
	rig.deliver("pull_request", bot)
	rig.releaseQueued()
	rig.drain()
	got := rig.latestOf("label")
	if got.ID == queued.ID || got.Status != "posted" || got.RequestedBy != "github:labeler[bot]" {
		t.Fatalf("a bot's label after the resume = %+v", got)
	}
	if pr := rig.pr(7); pr.AutoReviews != 1 {
		t.Errorf("a bot's label review counted %d towards the pause, want 1", pr.AutoReviews)
	}
}
