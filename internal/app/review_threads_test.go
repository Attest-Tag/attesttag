package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"attesttag/internal/review"
)

// The conversation around a review, end to end on the lane's fake GitHub: a finding accepted as a
// known risk, a withdrawal that does not keep a narrower claim alive, a finding looked at again from
// its thread, the commands people actually type, and questions answered from the code. They run on
// both dialects.

// ask puts login's comment on the pull request's conversation, as assoc, and sends its delivery: a
// question's run reads the comment back from GitHub, as it then stands.
func (rig *laneRig) ask(id int64, login, assoc, body string) {
	rig.t.Helper()
	rig.gh.mu.Lock()
	rig.gh.issueComments = append(rig.gh.issueComments, githubComment{ID: id, Body: body, AuthorAssociation: assoc,
		User: githubUser{Login: login, Type: "User"}})
	rig.gh.mu.Unlock()
	rig.deliver("issue_comment", commentEvent(id, login, assoc, body, !rig.public))
}

func (rig *laneRig) runsOfKind(kind string) []*ReviewRun {
	var out []*ReviewRun
	for _, r := range rig.runs(7) {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func acknowledgeAs(reason string) reviewModelReply {
	return reviewModelReply{Calls: []reviewCall{{reviewClassifyTool, map[string]any{"kind": "acknowledge", "reason": reason}}}}
}

// ---- acknowledged ----

// A member who accepts the risk — known, tracked separately — acknowledges the finding: no verdict is
// asked of a model, the finding leaves the score and the open list, the summary lists it under
// Acknowledged with the reason, and the bot answers once, briefly. A later reply in the thread is not
// answered: the thread is settled.
func TestReviewReplyAcknowledgeTakesTheFindingOutOfTheScore(t *testing.T) {
	rig, _, f := newReplyRig(t)
	rig.model.classify = func(reviewModelReq) reviewModelReply {
		return acknowledgeAs("Known and tracked separately; out of scope for this change.")
	}
	if rig.pr(7).Score != 3 {
		t.Fatalf("score before = %d", rig.pr(7).Score)
	}
	rig.reply(f, "alice", "MEMBER", "This is intended — known risk, tracked separately.")
	rig.drain()

	got := rig.finding()
	if got.Status != review.FindingAcknowledged || got.StatusBy != "github:alice" || !strings.Contains(got.StatusReason, "tracked separately") {
		t.Fatalf("finding after the acknowledgement = %s by %q: %q", got.Status, got.StatusBy, got.StatusReason)
	}
	if n := len(rig.model.requests("reply")); n != 0 {
		t.Errorf("an acknowledgement went to the verdict model %d times", n)
	}
	a := rig.botAnswers(f)
	if len(a) != 1 || !strings.HasPrefix(a[0], "**Acknowledged**") || !strings.Contains(a[0], "Score now 5/5") {
		t.Fatalf("the answer = %q", a)
	}
	if pr := rig.pr(7); pr.Score != 5 {
		t.Errorf("score after the acknowledgement = %d, want 5", pr.Score)
	}
	_, _, _, patches := rig.gh.snapshot()
	if len(patches) != 1 || !strings.Contains(patches[0], "Confidence 5/5") || !strings.Contains(patches[0], "Acknowledged (1)") ||
		!strings.Contains(patches[0], "Known and tracked separately") || strings.Contains(patches[0], "Open findings") {
		t.Errorf("the summary did not follow:\n%s", strings.Join(patches, "\n---\n"))
	}
	if ev, _ := rig.st.AuditEvents(context.Background(), orgID, AuditFilter{Action: "review.finding_changed"}); len(ev) != 1 {
		t.Errorf("the acknowledgement was audited %d times", len(ev))
	}

	rig.model.classify = func(reviewModelReq) reviewModelReply { return classifyAs("pushback", "", "") }
	rig.reply(f, "alice", "MEMBER", "Actually, more context on why.")
	rig.drain()
	if a := rig.botAnswers(f); len(a) != 1 {
		t.Errorf("a reply after the acknowledgement was answered: %q", a)
	}
}

// Accepting a P1 as a known risk takes the authority withdrawing one does: somebody who is neither a
// member nor the author gets a polite line, and the finding stays open and counted.
func TestReviewReplyAcknowledgeNeedsTheAuthorityAWithdrawalDoes(t *testing.T) {
	rig, _, f := newReplyRig(t)
	rig.model.classify = func(reviewModelReq) reviewModelReply { return acknowledgeAs("Out of scope.") }
	rig.public = true // a stranger comments only where anyone may
	rig.reply(f, "drive-by", "NONE", "Out of scope, won't fix.")
	rig.drain()
	if got := rig.finding(); got.Status != review.FindingOpen {
		t.Fatalf("a stranger acknowledged a P1: %s", got.Status)
	}
	if a := rig.botAnswers(f); len(a) != 1 || !strings.Contains(a[0], "takes a member of this repository or the author") {
		t.Errorf("answer = %q", a)
	}
	if pr := rig.pr(7); pr.Score != 3 {
		t.Errorf("score = %d", pr.Score)
	}
}

// ---- withdraw, not downgrade ----

// A reply that refutes the trigger the finding named withdraws it, whatever narrower case may be
// left: the answer names that case in a sentence and points at a review of the head, and the finding
// does not live on as a narrower claim. The verdict is told so.
func TestReviewReplyRefutedTriggerWithdrawsAndNamesTheNarrowerCase(t *testing.T) {
	rig, _, f := newReplyRig(t)
	rig.model.classify = func(reviewModelReq) reviewModelReply { return classifyAs("pushback", "", "") }
	rig.model.reply = func(int, reviewModelReq) reviewModelReply {
		v := replyVerdictOf("withdraw", "", "Add's only producer holds the outer lock, so two writes cannot overlap.", true)
		v.Calls[0].Args.(map[string]any)["narrower"] = "A caller outside the package could still call Add without the lock"
		return v
	}
	rig.reply(f, "alice", "MEMBER", "The producer's contract says Add is only called under Sum's lock.")
	rig.drain()
	if got := rig.finding(); got.Status != review.FindingWithdrawn || got.Severity != review.P1 {
		t.Fatalf("finding = %s %s; want it withdrawn, not kept lower", got.Status, got.Severity)
	}
	a := rig.botAnswers(f)
	if len(a) != 1 || !strings.HasPrefix(a[0], "**Withdrawn.**") ||
		!strings.Contains(a[0], "A narrower case may remain: A caller outside the package could still call Add without the lock.") ||
		!strings.Contains(a[0], "`@attesttag full review` on the pull request's conversation looks at the head again for it.") {
		t.Fatalf("the answer = %q", a)
	}
	sys := rig.model.requests("reply")[0].System
	if !strings.Contains(sys, "Never downgrade a finding whose trigger the reply refuted") || !strings.Contains(sys, "narrower") {
		t.Errorf("the verdict was not told to withdraw on a refuted trigger:\n%s", sys)
	}
	if sys := rig.model.requests("classify")[0].System; !strings.Contains(sys, "acknowledge:") {
		t.Errorf("the classifier has no acknowledge:\n%s", sys)
	}
}

// ---- in a finding's thread ----

// `@attesttag recheck` in a finding's thread looks at the finding again at the head, with the
// review's own verifier and none of the thread: confirmed, it stands; asked again before a push, it
// is answered from that look for no model call; after a push, refuted, it is withdrawn and the score
// follows.
func TestReviewThreadRecheck(t *testing.T) {
	rig, _, f := newReplyRig(t)
	verifiers := len(rig.model.requests("verifier"))
	rig.reply(f, "alice", "MEMBER", "@attesttag recheck")
	rig.drain()
	if n := len(rig.model.requests("classify")); n != 0 {
		t.Errorf("a recheck was sorted like a reply %d times", n)
	}
	looks := rig.model.requests("verifier")[verifiers:]
	if len(looks) != 1 || strings.Contains(strings.Join(looks[0].Users, "\n"), "@attesttag recheck") {
		t.Fatalf("the recheck's look: %d calls", len(looks))
	}
	a := rig.botAnswers(f)
	if len(a) != 1 || !strings.HasPrefix(a[0], "**Rechecked at `aaaaaaa`: it stands.**") || !strings.Contains(a[0], "Finding still open.") {
		t.Fatalf("the first recheck's answer = %q", a)
	}
	if got := rig.finding(); got.Status != review.FindingOpen {
		t.Errorf("a confirmed recheck changed the finding: %s", got.Status)
	}

	calls := len(rig.model.requests(""))
	rig.reply(f, "alice", "MEMBER", "@attesttag check again")
	rig.drain()
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("a recheck at a head already rechecked made %d model calls", n-calls)
	}
	if a = rig.botAnswers(f); len(a) != 2 || !strings.Contains(a[1], "Already rechecked at `aaaaaaa`") {
		t.Fatalf("the repeat's answer = %q", a)
	}

	// At a head not yet rechecked — a new commit nobody has reviewed — the code is checked for a fix
	// first, and then, not found fixed, refuted: withdrawn, on a member's asking.
	rig.gh.pushTo(reviewHeadC, totalsFixture().files, totalsFixture().head)
	rig.model.verify = func(reviewModelReq) reviewModelReply { return verdict("refuted", 90, review.P1) }
	rig.reply(f, "alice", "MEMBER", "@attesttag review this thread again and update the confidence")
	rig.drain()
	if got := rig.finding(); got.Status != review.FindingWithdrawn {
		t.Fatalf("a refuted recheck left the finding %s", got.Status)
	}
	if a = rig.botAnswers(f); len(a) != 3 || !strings.HasPrefix(a[2], "**Withdrawn on a second look at `ccccccc`.**") ||
		!strings.Contains(a[2], "Score now 5/5") {
		t.Fatalf("the refuted recheck's answer = %q", a)
	}
	if n := len(rig.model.requests("resolve")); n != 1 {
		t.Errorf("%d resolution checks after the new commit, want one", n)
	}
	if pr := rig.pr(7); pr.Score != 5 {
		t.Errorf("score after the withdrawal = %d", pr.Score)
	}
	runs := rig.replyRuns()
	if len(runs) != 3 || runs[0].Status != "posted" || !strings.HasPrefix(runs[0].Summary, "recheck") {
		t.Errorf("reply runs = %+v", runs)
	}
}

// A recheck after a push first asks whether the code it was about still has the problem, as a
// re-review would: fixed closes it, and its thread is told so. A stranger's recheck cannot close a
// P1, whatever the check found; a member asking next is answered from the same look, and closes it.
func TestReviewThreadRecheckAfterAPushFindsItFixed(t *testing.T) {
	rig, _, f := newReplyRig(t)
	fixedHead := map[string]string{"src/totals.go": strings.Replace(totalsHead, "\tt.value += n", "\tt.mu.Lock()\n\tt.value += n\n\tt.mu.Unlock()", 1)}
	rig.gh.pushTo(reviewHeadC, totalsFixture().files, fixedHead)
	rig.model.resolve = func(reviewModelReq) reviewModelReply {
		return resolution(resolveFixed, "Add takes the lock around the write now.")
	}
	rig.public = true
	rig.reply(f, "drive-by", "NONE", "@attesttag recheck")
	rig.drain()
	if got := rig.finding(); got.Status != review.FindingOpen {
		t.Fatalf("a stranger's recheck closed a P1: %s", got.Status)
	}
	if a := rig.botAnswers(f); len(a) != 1 || !strings.Contains(a[0], "it looks fixed") || !strings.Contains(a[0], "Not changed: closing a P1") {
		t.Fatalf("the stranger's answer = %q", a)
	}

	rig.reply(f, "alice", "MEMBER", "@attesttag recheck please")
	rig.drain()
	got := rig.finding()
	if got.Status != review.FindingFixed || !strings.HasPrefix(got.StatusReason, "fixed in ccccccc") {
		t.Fatalf("after a member's recheck: %s %q", got.Status, got.StatusReason)
	}
	a := rig.botAnswers(f)
	if len(a) != 2 || !strings.HasPrefix(a[1], "**Fixed at `ccccccc`.**") || !strings.Contains(a[1], "Add takes the lock") {
		t.Fatalf("the member's answer = %q", a)
	}
	if n := len(rig.model.requests("resolve")); n != 1 {
		t.Errorf("%d resolution checks, want the one look at this head", n)
	}
}

// A bare mention in a finding's thread gets the thread's short help: what can be said there, with no
// model asked.
func TestReviewThreadBareMentionGetsTheThreadHelp(t *testing.T) {
	rig, _, f := newReplyRig(t)
	before := len(rig.model.requests("")) // the review's own finder and verifier calls
	rig.reply(f, "alice", "MEMBER", "@ATTESTTAG")
	rig.drain()
	a := rig.botAnswers(f)
	if len(a) != 1 || !strings.Contains(a[0], "`@attesttag recheck`") || !strings.Contains(a[0], "`intended`") ||
		!strings.Contains(a[0], "`@attesttag fix`") {
		t.Fatalf("the thread's help = %q", a)
	}
	if n := len(rig.model.requests("")); n != before {
		t.Errorf("%d model calls after the help, %d before; the help asks none", n, before)
	}
}

// ---- commands on the conversation ----

// The commands people type for the ones the bot has: "start the review of pr" reviews, "check again"
// on the conversation is a review of the head (answered from the last one when nothing changed), and
// "score" is the status.
func TestReviewCommandAliases(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"command"}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("issue_comment", commentEvent(3001, "alice", "MEMBER", "@attesttag start the review of pr", true))
	rig.drain()
	runs := rig.runsOfKind("review")
	if len(runs) != 1 || runs[0].Status != "posted" || runs[0].Trigger != "command" {
		t.Fatalf("start the review of pr: %+v", runs)
	}
	rig.deliver("issue_comment", commentEvent(3002, "alice", "MEMBER", "@attesttag check again", true))
	rig.drain()
	if a := rig.answers(); len(a) != 1 || !strings.Contains(a[0], "Already reviewed `aaaaaaa`") {
		t.Fatalf("check again on the conversation: %q", a)
	}
	rig.deliver("issue_comment", commentEvent(3003, "alice", "MEMBER", "@attesttag score", true))
	if a := rig.answers(); len(a) != 2 || !strings.Contains(a[1], "Confidence **3/5**") {
		t.Fatalf("score: %q", a)
	}
	verbs := map[string]bool{}
	for _, e := range rig.commandAudits() {
		verbs[e["verb"].(string)] = true
	}
	if !verbs["review"] || !verbs["recheck"] || !verbs["status"] {
		t.Errorf("audited verbs = %v", verbs)
	}
}

// ---- questions ----

// A question in somebody's own words is answered on the conversation from the code: one run of its
// own, on the model the organisation's settings give code review, shown the title and description,
// what the review says and the diff, with the reviewer's read-only tools; the answer is posted under
// a footer saying it changed nothing, and its cost is the run's and the asker's.
func TestReviewQuestionIsAnsweredFromTheCode(t *testing.T) {
	ctx := context.Background()
	rig, cv, f := newReplyRig(t)
	rig.model.finder[""] = func(n int, q reviewModelReq) reviewModelReply {
		if n == 0 {
			return reviewModelReply{Calls: []reviewCall{{"read_file", map[string]any{"path": "src/totals.go"}}}, Cost: 0.01}
		}
		return reviewModelReply{Calls: []reviewCall{{reviewAnswerTool, map[string]any{
			"answer": "Only `Sum` calls `Add` (src/totals.go:11), and it does not take `mu` first, so the race the finding names is reachable."}}},
			Cost: 0.01}
	}
	rig.ask(4001, "alice", "MEMBER", "@attesttag is Add ever called without the lock?")
	if !cv.has(rig.gh, "issue:4001:eyes") {
		t.Errorf("the question was not picked up: %v", cv.reactions)
	}
	rig.drain()

	a := rig.answers()
	if len(a) != 1 || !strings.Contains(a[0], "Only `Sum` calls `Add`") ||
		!strings.Contains(a[0], "<sub>Answered from the code at `aaaaaaa`. An answer changes no finding") {
		t.Fatalf("the answer = %q", a)
	}
	runs := rig.runsOfKind("answer")
	if len(runs) != 1 || runs[0].Status != "posted" || runs[0].RequestedBy != "github:alice" || runs[0].CostUSD < 0.019 {
		t.Fatalf("answer runs = %+v", runs)
	}
	qs := rig.model.requests("finder")
	var asked []reviewModelReq
	for _, q := range qs {
		if q.Type == "" {
			asked = append(asked, q)
		}
	}
	if len(asked) != 2 {
		t.Fatalf("%d calls for the question, want a read and the answer", len(asked))
	}
	prompt := strings.Join(asked[0].Users, "\n")
	for _, want := range []string{"<pr_data>\nTitle: Make Add faster", "## File: src/totals.go", "scores it 3/5",
		"Add writes the total without the lock", "<question>\nis Add ever called without the lock?\n</question>", "@alice (a member of the repository)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the question's prompt lacks %q:\n%s", want, prompt)
		}
	}
	if tools := asked[0].Tools; !slices.Contains(tools, "read_file") || !slices.Contains(tools, "find_code") ||
		!slices.Contains(tools, reviewAnswerTool) || slices.Contains(tools, reviewSubmitTool) {
		t.Errorf("the question's tools = %v", tools)
	}
	if res := strings.Join(asked[1].ToolResults, "\n"); !strings.Contains(res, "acme/web src/totals.go @ aaaaaaa") {
		t.Errorf("the read did not come back to the model:\n%s", res)
	}
	if got := rig.finding(); got.Status != f.Status || got.Severity != f.Severity {
		t.Errorf("a question changed the finding: %s %s", got.Status, got.Severity)
	}
	var user string
	rig.st.db.QueryRowContext(ctx, `select user_id from usage where org_id=? order by id desc limit 1`, orgID).Scan(&user)
	if user != "github:alice" {
		t.Errorf("the question's spend was charged to %q", user)
	}
	if ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.replied"}); len(ev) != 1 {
		t.Errorf("the answer was audited %d times", len(ev))
	}

	// The same comment delivered again is the same question.
	rig.deliver("issue_comment", commentEvent(4001, "alice", "MEMBER", "@attesttag is Add ever called without the lock?", true))
	rig.drain()
	if n := len(rig.runsOfKind("answer")); n != 1 {
		t.Errorf("a redelivered question queued %d runs", n)
	}
}

// Questions are held to the commands' gates and their own: a stranger is refused like any command,
// one person gets five answered an hour and then one line saying so, and nothing past that line.
func TestReviewQuestionThrottles(t *testing.T) {
	rig, _, _ := newReplyRig(t)
	rig.model.finder[""] = func(int, reviewModelReq) reviewModelReply {
		return reviewModelReply{Calls: []reviewCall{{reviewAnswerTool, map[string]any{"answer": "It is called from Sum only."}}}}
	}
	for i := range reviewQuestionsPerHour + 2 {
		rig.ask(int64(4100+i), "alice", "MEMBER", fmt.Sprintf("@attesttag question number %d about Add?", i))
	}
	rig.drain()
	if n := len(rig.runsOfKind("answer")); n != reviewQuestionsPerHour {
		t.Errorf("%d questions queued; one person gets %d an hour", n, reviewQuestionsPerHour)
	}
	a := rig.answers()
	limits := 0
	for _, s := range a {
		if strings.Contains(s, "questions to attest_tag have reached their limit") {
			limits++
		}
	}
	if len(a) != reviewQuestionsPerHour+1 || limits != 1 {
		t.Errorf("%d answers, %d of them the limit's: %q", len(a), limits, a)
	}

	rig.public = true
	rig.ask(4200, "drive-by", "NONE", "@attesttag why is this safe?")
	rig.drain()
	if n := len(rig.runsOfKind("answer")); n != reviewQuestionsPerHour {
		t.Errorf("a stranger's question was queued")
	}
}

// On a pull request whose review is recorded in shadow, a question is neither asked nor answered:
// nothing is written to GitHub and no model is paid for.
func TestReviewQuestionInShadowSaysNothing(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"shadow"}`)
	rig.serveConversation()
	rig.ask(4300, "alice", "MEMBER", "@attesttag what does this change do?")
	rig.drain()
	if n := len(rig.runsOfKind("answer")); n != 0 {
		t.Errorf("a question in shadow queued %d runs", n)
	}
	if n := len(rig.model.requests("")); n != 0 {
		t.Errorf("a question in shadow made %d model calls", n)
	}
	for _, s := range rig.fake.sent() {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("a question in shadow wrote to GitHub: %s", s)
		}
	}
}

// ---- placement ----

// A finding whose range starts above its hunk — a finder citing a function from its signature down
// to an unchanged line the diff shows — is commented on inline, on the part of the range the hunk
// shows, rather than listed as outside the diff; one on a single unchanged context line inside a
// hunk is inline as it is.
func TestReviewEnginePutsFindingsOnContextLinesInline(t *testing.T) {
	rig := newReviewRig(t, totalsFixture())
	ranged := lockFinding()
	ranged["start_line"] = 5
	ranged["suggestion"] = map[string]any{"start_line": 5, "line": 12, "code": "type Totals struct {"}
	context1 := map[string]any{
		"path": "src/totals.go", "side": "RIGHT", "line": 11, "severity": "P1", "category": "contract", "symbol": "Add",
		"title": "Add no longer says it is unsafe", "confidence": 85,
		"scenario": "Callers that read Add's signature still assume it locks, and call it from several goroutines at once.",
		"evidence": []map[string]any{{"path": "src/totals.go", "ref": "head", "start_line": 11, "quote": "func (t *Totals) Add(n int) {"}},
	}
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(ranged, context1) }
	rig.model.verify = confirmAll(95)
	out, err := rig.run(rig.spec("general"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Findings) != 2 {
		t.Fatalf("findings = %+v / drops %+v", out.Findings, out.Dropped)
	}
	for _, f := range out.Findings {
		if f.Where != reviewWhereInline || f.Placement != review.PlacementInline {
			t.Errorf("%q was put %s/%s, want inline", f.Title, f.Where, f.Placement)
		}
		if f.Title == "Add writes the total without the lock" && (f.StartLine != 10 || f.Line != 12 || f.Suggestion != nil) {
			t.Errorf("the range was not narrowed to its hunk: %d-%d, suggestion %v", f.StartLine, f.Line, f.Suggestion)
		}
	}
}

func TestFitAnchorNarrowsARangeToTheHunkThatShowsIt(t *testing.T) {
	hunks, err := review.ParsePatch("@@ -10,6 +10,4 @@ type Totals struct {\n // Add\n func Add() {\n-\tlock()\n-\tunlock()\n \tv += n\n }\n" +
		"@@ -30,3 +28,4 @@\n a\n+b\n c\n d")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		side        review.Side
		start, line int
		from, to    int
		ok          bool
	}{
		{"starts above the hunk", review.Right, 5, 12, 10, 12, true},
		{"ends below the hunk", review.Right, 12, 20, 12, 13, true},
		{"spans two hunks: the larger share", review.Right, 12, 30, 28, 30, true},
		{"between the hunks", review.Right, 15, 20, 0, 0, false},
		{"on the base side", review.Left, 8, 12, 10, 12, true},
		{"an unknown side", review.Side("MIDDLE"), 5, 12, 0, 0, false},
	} {
		from, to, ok := fitAnchor(hunks, tc.side, tc.start, tc.line)
		if ok != tc.ok || from != tc.from || to != tc.to {
			t.Errorf("%s: fitAnchor = %d-%d %v, want %d-%d %v", tc.name, from, to, ok, tc.from, tc.to, tc.ok)
		}
		if ok && !review.ValidAnchor(hunks, tc.side, from, to) {
			t.Errorf("%s: %d-%d is not an anchor GitHub takes", tc.name, from, to)
		}
	}
}

// The review posted when nothing went inline never reads as having nothing to say beside an open
// finding: it names how many are open, and how severe, and where they are.
func TestReviewPlainNoteNamesTheOpenFindings(t *testing.T) {
	p1 := review.SummaryFinding{Finding: review.Finding{Path: "src/totals.go", Line: 12, Severity: review.P1, Title: "Race"},
		Status: review.FindingOpen, Placement: review.PlacementSummary}
	acked := p1
	acked.Status = review.FindingAcknowledged
	st := review.SummaryState{ReviewedSHA: reviewHead, FullCoverage: true, Findings: []review.SummaryFinding{p1, acked}}
	note := reviewPlainNote(st, "https://github.com/acme/web/pull/7#issuecomment-9")
	if strings.Contains(note, "Nothing to comment on") || !strings.Contains(note, "Confidence 3/5") ||
		!strings.Contains(note, "Its open finding (1 P1) is listed in the [summary comment]") {
		t.Errorf("note with an open P1 = %q", note)
	}
	st.Findings = []review.SummaryFinding{acked}
	if note := reviewPlainNote(st, ""); !strings.Contains(note, "Nothing to comment on in the diff") || !strings.Contains(note, "Confidence 5/5") {
		t.Errorf("note with only an acknowledged finding = %q", note)
	}
}
