package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"attesttag/internal/review"
)

// Commands on a pull request, end to end: a signed issue_comment delivery through the real webhook
// route, the dispatcher, the lane, and the fake GitHub of review_lane_test.go with what a
// conversation adds to it — reactions, and replies in a thread. What these pin is what a team
// relies on when it types to the bot: it answers its own people and nobody else, it is visibly
// picked up, a repeat on the same code costs nothing and says so, and status and help never touch
// a model. They run on both dialects.

// convo is what the bot left in the conversation besides reviews and the summary.
type convo struct {
	reactions []string // "issue:<id>:<content>" or "inline:<id>:<content>"
}

// serveConversation adds to the lane's fake GitHub the endpoints a conversation uses, under the
// tokens they must go out with.
func (rig *laneRig) serveConversation() *convo {
	g, f := rig.gh, rig.fake
	cv := &convo{}
	post := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if got := f.permsOf(r); got != postPerms {
				rig.t.Errorf("%s %s went out with a token for %q, want %q", r.Method, r.URL.Path, got, postPerms)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			h(w, r)
		}
	}
	react := func(kind string) http.HandlerFunc {
		return post(func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Content string `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			cv.reactions = append(cv.reactions, kind+":"+r.PathValue("id")+":"+in.Content)
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"id":%d,"content":%q}`, g.id(), in.Content)
		})
	}
	f.mux.HandleFunc("POST /repos/acme/web/issues/comments/{id}/reactions", react("issue"))
	f.mux.HandleFunc("POST /repos/acme/web/pulls/comments/{id}/reactions", react("inline"))
	f.mux.HandleFunc("POST /repos/acme/web/pulls/7/comments/{id}/replies", post(func(w http.ResponseWriter, r *http.Request) {
		root, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var in struct {
			Body string `json:"body"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		g.replyPosts++
		if g.replyPosts == g.limitReplyAt {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
			return
		}
		if g.failReplies > 0 {
			g.failReplies--
			w.WriteHeader(502)
			w.Write([]byte(`{"message":"Server Error"}`))
			return
		}
		if g.refuseReplies {
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"Unprocessable Entity","errors":["the conversation is locked"]}`))
			return
		}
		c := githubComment{ID: g.id(), Body: in.Body, User: githubUser{Login: "attesttag[bot]", Type: "Bot"}, InReplyToID: root}
		g.reviewComments = append(g.reviewComments, c)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(c)
	}))
	f.mux.HandleFunc("GET /repos/acme/web/pulls/7/reviews/{id}/comments", func(w http.ResponseWriter, r *http.Request) {
		if got := f.permsOf(r); got != readPerms {
			rig.t.Errorf("listing a review's comments went out with a token for %q", got)
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		g.mu.Lock()
		defer g.mu.Unlock()
		out := []githubComment{}
		for _, c := range g.reviewComments {
			if c.PullRequestReviewID == id {
				out = append(out, c)
			}
		}
		json.NewEncoder(w).Encode(out)
	})
	return cv
}

func (cv *convo) has(g *laneGitHub, r string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Contains(cv.reactions, r)
}

// answers are the bot's issue comments other than the summary: what it said to commands.
func (rig *laneRig) answers() []string {
	_, _, comments, _ := rig.gh.snapshot()
	var out []string
	for _, c := range comments {
		if !strings.Contains(c.Body, "<!-- attest_tag:") {
			out = append(out, c.Body)
		}
	}
	return out
}

// commentEvent is an issue_comment delivery: login, as assoc, wrote body on acme/web#7.
func commentEvent(id int64, login, assoc, body string, private bool) []byte {
	b, _ := json.Marshal(map[string]any{"action": "created",
		"installation": map[string]any{"id": fakeInstallation},
		"repository":   map[string]any{"full_name": "acme/web", "private": private},
		"sender":       map[string]any{"login": login, "type": "User"},
		"issue":        map[string]any{"number": 7, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/web/pulls/7"}},
		"comment": map[string]any{"id": id, "body": body, "author_association": assoc,
			"user": map[string]any{"login": login, "type": "User"}}})
	return b
}

// commandAudits are the review.command audit rows, newest first, each with its actor.
func (rig *laneRig) commandAudits() []map[string]any {
	rig.t.Helper()
	events, err := rig.st.AuditEvents(context.Background(), orgID, AuditFilter{Action: "review.command"})
	if err != nil {
		rig.t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range events {
		var d map[string]any
		json.Unmarshal(e.Details, &d)
		d["actor"] = e.ActorName
		out = append(out, d)
	}
	return out
}

// A repository reviewed only when somebody asks: the pull request opening is not reviewed, a
// member's command is — past the "when" setting and the author list, picked up with an eyes
// reaction — and the same command again on the same code is answered from that review, for no
// model call, with a reply that says so rather than silence.
func TestReviewCommandReviewsAndAnswersARepeatFromState(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"command","exclude_authors":["octocat"]}`)
	cv := rig.serveConversation()
	rig.confirmLock()

	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if runs := rig.runs(7); len(runs) != 0 || rig.pr(7).SkipReason != "trigger" {
		t.Fatalf("a pull request opened on a command-only repository: runs %+v, skip %q", runs, rig.pr(7).SkipReason)
	}

	rig.deliver("issue_comment", commentEvent(501, "alice", "MEMBER", "@attesttag review", true))
	if !cv.has(rig.gh, "issue:501:eyes") {
		t.Errorf("the command was not picked up with an eyes reaction: %v", cv.reactions)
	}
	rig.drain()
	runs := rig.runs(7)
	if len(runs) != 1 || runs[0].Trigger != "command" || runs[0].RequestedBy != "github:alice" || runs[0].Status != "posted" ||
		runs[0].TriggerRef != "comment:501" {
		t.Fatalf("runs after the command = %+v", runs)
	}
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 {
		t.Fatalf("%d reviews posted, want one", len(posts))
	}
	if a := rig.answers(); len(a) != 0 {
		t.Errorf("a live review was answered in words as well: %q", a)
	}

	calls := len(rig.model.requests(""))
	rig.deliver("issue_comment", commentEvent(502, "alice", "MEMBER", "@attesttag re-review please", true))
	rig.drain()
	runs = rig.runs(7)
	if len(runs) != 2 || runs[0].Status != "noop" || runs[0].Trigger != "command" {
		t.Fatalf("the repeat = %+v", runs[0])
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("the repeat on the same code made %d model calls", n-calls)
	}
	a := rig.answers()
	if len(a) != 1 || !strings.Contains(a[0], "Already reviewed `aaaaaaa`") || !strings.Contains(a[0], "`@attesttag full review`") {
		t.Errorf("the repeat was not answered from state: %q", a)
	}
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 {
		t.Errorf("the repeat posted a second review")
	}
	audits := rig.commandAudits()
	if len(audits) != 2 || audits[0]["outcome"] != "queued" || audits[0]["actor"] != "github:alice" || audits[0]["verb"] != "review" {
		t.Errorf("audit of the commands: %+v", audits)
	}
	// The same comment delivered again is the same command: no second run.
	rig.deliver("issue_comment", commentEvent(502, "alice", "MEMBER", "@attesttag re-review please", true))
	rig.drain()
	if n := len(rig.runs(7)); n != 2 {
		t.Errorf("a redelivered command queued another run: %d runs", n)
	}
}

// Types named in a command replace the branch rule's for that run; a name the organisation has no
// type for is answered with the ones it has, and nothing is queued.
func TestReviewCommandNamesTypes(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.serveConversation()
	rig.confirmLock()

	rig.deliver("issue_comment", commentEvent(601, "alice", "COLLABORATOR", "@attesttag review nonesuch", true))
	rig.drain()
	if runs := rig.runs(7); len(runs) != 0 {
		t.Fatalf("an unknown type queued a run: %+v", runs)
	}
	a := rig.answers()
	if len(a) != 1 || !strings.Contains(a[0], "no review type `nonesuch`") || !strings.Contains(a[0], "`general`, `security`, `tests`, `performance`, `concurrency`, `release`") {
		t.Fatalf("answer to an unknown type: %q", a)
	}

	rig.deliver("issue_comment", commentEvent(602, "alice", "COLLABORATOR", "@attesttag review security", true))
	rig.drain()
	runs := rig.runs(7)
	// The rule is still the one the pull request falls under — its settings are the ones the run
	// used — and the run records it, though the types are the ones named.
	if len(runs) != 1 || len(runs[0].Types) != 1 || runs[0].Types[0].Key != "security" || runs[0].RuleLabel != "any → any" {
		t.Fatalf("a review naming security ran %+v under rule %q", runs, runs[0].RuleLabel)
	}
	if n := len(rig.model.requests("finder")); n == 0 || rig.model.requests("finder")[0].Type != "security" {
		t.Errorf("the finder did not run the named type")
	}
}

// A command on a pull request into main is reviewed under main's branch rule, its types and its
// label, as the pull request's opening would be — the live report had one run as "any → any" with
// the fallback's types. Types named in the command replace the rule's and leave the rule its name,
// and status says both. A request that knows the pull request without its branches has them read
// before a rule is matched, and a pull request GitHub gives no base branch for matches none.
func TestReviewCommandMatchesTheBranchRuleOfThePullRequest(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"command","branch_rules":[{"base":"main","types":["general","security"]},{}]}`)
	rig.serveConversation()
	rig.confirmLock()
	keys := func(r *ReviewRun) []string {
		var out []string
		for _, t := range r.Types {
			out = append(out, t.Key)
		}
		return out
	}
	lastReview := func() *ReviewRun {
		for _, r := range rig.runs(7) {
			if r.Kind == "review" {
				return r
			}
		}
		t.Fatal("no review run")
		return nil
	}
	rig.deliver("issue_comment", commentEvent(2101, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	// The rule's types, and Concurrency, which the diff brought in: it takes a lock away. A command
	// naming its types, below, gets those and nothing else.
	if run := lastReview(); run.Status != "posted" || run.RuleLabel != "any → main" || !slices.Equal(keys(run), []string{"general", "security", "concurrency"}) {
		t.Fatalf("the command's review: %s under %q with %v", run.Status, run.RuleLabel, keys(run))
	}
	if _, _, comments, _ := rig.gh.snapshot(); len(comments) == 0 || !strings.Contains(comments[0].Body, "Rule: `any → main`") {
		t.Errorf("the summary does not name main's rule:\n%v", comments)
	}

	rig.gh.pushTo(reviewHeadC, totalsFixture().files, totalsFixture().head)
	rig.deliver("pull_request", prEvent("synchronize", 7, reviewHeadC))
	rig.drain()
	rig.deliver("issue_comment", commentEvent(2102, "alice", "MEMBER", "@attesttag review security", true))
	rig.drain()
	if run := lastReview(); run.RuleLabel != "any → main" || !slices.Equal(keys(run), []string{"security"}) {
		t.Errorf("a command naming security: %q with %v", run.RuleLabel, keys(run))
	}
	rig.deliver("issue_comment", commentEvent(2103, "alice", "MEMBER", "@attesttag status", true))
	if a := rig.answers(); len(a) == 0 || !strings.Contains(a[len(a)-1], "Branch rule: any → main.") ||
		!strings.Contains(a[len(a)-1], "Review types: `security`, as asked.") {
		t.Errorf("status = %q", a)
	}

	bare := &githubPull{Number: 7, State: "open", User: githubUser{Login: "octocat", Type: "User"},
		Head: githubPullRef{SHA: reviewHeadC}, Base: githubPullRef{SHA: reviewBase}}
	run, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation, Trigger: "console",
		TriggerRef: "console:no-branches", RequestedBy: "console:admin@acme.test", BypassFilters: true, Full: true, Pull: bare})
	if err != nil || run.RuleLabel != "any → main" || !slices.Equal(keys(run), []string{"general", "security"}) {
		t.Errorf("a request without the branches: %+v (%v)", run, err)
	}
	if _, _, err := planReview(review.Resolve(nil), bare, nil, "", false); err == nil {
		t.Error("a pull request with no base branch was matched to a rule")
	}
}

// Only the repository's own people command the bot. A stranger — who can comment only on a public
// repository — is refused once a day per pull request, politely; nothing is queued or reacted to,
// and past the first refusal in a day nothing is audited either, so a script cannot flood the audit
// log. On a private repository whoever comments has access, so a member GitHub delivers as "NONE"
// is obeyed. A development App's handle is not this App's, and a bot's comment is never a command,
// whatever it says.
func TestReviewCommandAuthority(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	cv := rig.serveConversation()
	rig.confirmLock()

	rig.deliver("issue_comment", commentEvent(702, "stranger", "NONE", "@attesttag review", false))
	rig.deliver("issue_comment", commentEvent(703, "stranger", "FIRST_TIME_CONTRIBUTOR", "@attesttag status", false))
	rig.drain()
	if runs := rig.runs(7); len(runs) != 0 {
		t.Fatalf("a stranger's command queued %+v", runs)
	}
	a := rig.answers()
	if len(a) != 1 || !strings.Contains(a[0], "started by its members and collaborators") {
		t.Fatalf("answers to strangers: %q; want one refusal, on the public repository", a)
	}
	if len(cv.reactions) != 0 {
		t.Errorf("a stranger's command was reacted to: %v", cv.reactions)
	}
	audits := rig.commandAudits()
	if len(audits) != 1 || !strings.HasPrefix(audits[0]["outcome"].(string), "refused") {
		t.Errorf("audit of the strangers' commands: %+v; want the first refusal in the day, and not the second", audits)
	}
	for i := range 5 {
		rig.deliver("issue_comment", commentEvent(int64(704+i), "stranger", "NONE", "@attesttag review", false))
	}
	if n := len(rig.commandAudits()); n != 1 || len(rig.answers()) != 1 {
		t.Errorf("more strangers' commands the same day: %d audited, %d answers", n, len(rig.answers()))
	}

	// Another App's handle, a quoted command, a mention mid-sentence: none are ours.
	for i, body := range []string{"@attesttag-dev review", "> @attesttag review\nquoting the docs", "thanks @attesttag review later"} {
		rig.deliver("issue_comment", commentEvent(int64(710+i), "alice", "MEMBER", body, true))
	}
	// A bot's comment, even one GitHub delivered as sent by a person.
	bot, _ := json.Marshal(map[string]any{"action": "created", "installation": map[string]any{"id": fakeInstallation},
		"repository": map[string]any{"full_name": "acme/web", "private": true}, "sender": map[string]any{"login": "alice", "type": "User"},
		"issue":   map[string]any{"number": 7, "pull_request": map[string]any{"url": "x"}},
		"comment": map[string]any{"id": 720, "body": "@attesttag review", "author_association": "MEMBER", "user": map[string]any{"login": "attesttag[bot]", "type": "Bot"}}})
	rig.deliver("issue_comment", bot)
	rig.drain()
	if runs := rig.runs(7); len(runs) != 0 {
		t.Fatalf("a comment not addressed to this App queued %+v", runs)
	}
	if n := len(rig.commandAudits()); n != 1 {
		t.Errorf("%d commands audited; the ones not addressed to this App are not commands", n)
	}
	if len(cv.reactions) != 0 || len(rig.answers()) != 1 {
		t.Errorf("a comment not addressed to this App was answered: %v %q", cv.reactions, rig.answers())
	}

	// A member GitHub under-reports on a private repository is obeyed: nobody else can comment there.
	rig.deliver("issue_comment", commentEvent(730, "bob", "NONE", "@attesttag review", true))
	rig.drain()
	if runs := rig.runs(7); len(runs) != 1 {
		t.Errorf("a commenter on the private repository, delivered as NONE, queued %d runs; want one", len(runs))
	}
}

// status and help are answered at once from what is stored, with no model call: before a review,
// after one — the score, what is open, which commit — and after a push, which the status says has
// not been reviewed.
func TestReviewCommandStatusAndHelp(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.serveConversation()
	rig.confirmLock()

	rig.deliver("issue_comment", commentEvent(801, "alice", "MEMBER", "@attesttag status", true))
	rig.deliver("issue_comment", commentEvent(802, "alice", "MEMBER", "@attesttag", true))
	a := rig.answers()
	if len(a) != 2 || !strings.Contains(a[0], "Not reviewed yet") || !strings.Contains(a[1], "`@attesttag full review`") ||
		!strings.Contains(a[1], "Review types here: `general`, `security`, `tests`, `performance`, `concurrency`, `release`") ||
		!strings.Contains(a[1], "`@attesttag pause`") || !strings.Contains(a[1], "`@attesttag resume`") {
		t.Fatalf("status before a review, and help: %q", a)
	}

	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	calls := len(rig.model.requests(""))
	rig.gh.pushTo(reviewHeadC, totalsFixture().files, totalsFixture().head)
	rig.deliver("pull_request", prEvent("synchronize", 7, reviewHeadC))
	rig.drain()
	rig.deliver("issue_comment", commentEvent(803, "alice", "MEMBER", "@attesttag why is the score 3?", true))
	a = rig.answers()
	status := a[len(a)-1]
	for _, want := range []string{"Confidence **3/5**", "Open findings: 1 P1.", "Last reviewed `aaaaaaa`", "head is now `ccccccc`, not reviewed",
		"Branch rule: any → any."} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("status made %d model calls", n-calls)
	}
}

// A full review is a member's to ask for, once per commit a day: it runs the model again on code
// already reviewed, without being told what is open, and the second one that day is refused.
func TestReviewCommandFullReview(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	finders := len(rig.model.requests("finder"))

	rig.deliver("issue_comment", commentEvent(901, "carol", "COLLABORATOR", "@attesttag full review", true))
	rig.drain()
	if a := rig.answers(); len(a) != 1 || !strings.Contains(a[0], "is for members of the organisation") {
		t.Fatalf("a collaborator's full review: %q", a)
	}

	rig.deliver("issue_comment", commentEvent(902, "alice", "OWNER", "@attesttag full review", true))
	rig.drain()
	runs := rig.runs(7)
	if runs[0].Trigger != "command" || runs[0].Status != "posted" || !strings.HasPrefix(runs[0].DedupeKey, "full:") {
		t.Fatalf("the full review = %+v", runs[0])
	}
	fq := rig.model.requests("finder")
	if len(fq) == finders {
		t.Fatal("the full review did not run the model again on a reviewed commit")
	}
	if last := fq[len(fq)-1]; strings.Contains(strings.Join(last.Users, "\n"), "Already raised on this pull request") {
		t.Error("the full review's finder was told what is already open")
	}
	// The finding it found again is the one already open: not posted twice.
	fs, _ := rig.st.ReviewFindings(context.Background(), orgID, rig.pr(7).ID)
	if len(fs) != 1 {
		t.Errorf("%d findings after the full review, want the one", len(fs))
	}

	rig.deliver("issue_comment", commentEvent(903, "alice", "OWNER", "@attesttag full review", true))
	rig.drain()
	a := rig.answers()
	if !strings.Contains(a[len(a)-1], "has had a full review in the last day") {
		t.Errorf("a second full review the same day: %q", a)
	}
	if n := len(rig.runs(7)); n != 2 {
		t.Errorf("%d runs; the second full review must not be queued", n)
	}
}

// A question in somebody's own words is pointed at help, once an hour; ten commands an hour is
// what one person gets, and the eleventh is not answered.
func TestReviewCommandQuestionsAndThrottle(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.serveConversation()
	rig.deliver("issue_comment", commentEvent(1001, "alice", "MEMBER", "@attesttag what does Add do?", true))
	rig.deliver("issue_comment", commentEvent(1002, "alice", "MEMBER", "@attesttag and Get?", true))
	if a := rig.answers(); len(a) != 1 || !strings.Contains(a[0], "`@attesttag help`") {
		t.Fatalf("answers to two questions: %q", a)
	}
	for i := range 9 {
		rig.deliver("issue_comment", commentEvent(int64(1010+i), "alice", "MEMBER", "@attesttag help", true))
	}
	got := len(rig.answers())
	audits := rig.commandAudits() // newest first
	if last := audits[0]; last["outcome"] != "throttled" {
		t.Errorf("the eleventh command in an hour: %+v", last)
	}
	if got != 1+8 {
		t.Errorf("%d answers; want the question's and eight helps before the throttle", got)
	}
	// Somebody else is not held to alice's count.
	rig.deliver("issue_comment", commentEvent(1030, "bob", "MEMBER", "@attesttag help", true))
	if n := len(rig.answers()); n != got+1 {
		t.Errorf("another member's command was throttled with alice's")
	}
}

// On a shadow repository a command is acted on — the review is queued and recorded — and nothing
// at all is written to GitHub: no reaction, no answer, not even to status or a stranger. Shadow's
// promise is that it writes not a word there.
func TestReviewCommandOnAShadowRepositoryWritesNothing(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"shadow"}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("issue_comment", commentEvent(1101, "alice", "MEMBER", "@attesttag review", true))
	rig.deliver("issue_comment", commentEvent(1102, "alice", "MEMBER", "@attesttag status", true))
	rig.deliver("issue_comment", commentEvent(1103, "alice", "MEMBER", "@attesttag review nonesuch", true))
	rig.deliver("issue_comment", commentEvent(1104, "stranger", "NONE", "@attesttag review", false))
	rig.drain()
	rig.deliver("issue_comment", commentEvent(1105, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	runs := rig.runs(7)
	if len(runs) != 2 || runs[1].Status != "shadow" || runs[0].Status != "noop" {
		t.Fatalf("runs = %+v", runs)
	}
	for _, s := range rig.fake.sent() {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("a command on a shadow repository wrote to GitHub: %s", s)
		}
	}
	if n := len(rig.commandAudits()); n != 5 {
		t.Errorf("%d commands audited, want every one", n)
	}
}

// The money, a fork with forks off and the throttles still stop a command, and the person is told
// in words that say what kind of reason it was and nothing about the organisation's spending.
func TestReviewCommandGateRefusalsAreAnswered(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","max_usd":1}`)
	rig.serveConversation()
	if err := rig.st.PutSetting(ctx, orgID, "review_daily_usd", "0.5"); err != nil {
		t.Fatal(err)
	}
	rig.b.settings.Invalidate(orgID)
	rig.deliver("issue_comment", commentEvent(1201, "alice", "MEMBER", "@attesttag review", true))
	a := rig.answers()
	if len(a) != 1 || !strings.Contains(a[0], "budget is spent") || strings.Contains(a[0], "$") {
		t.Fatalf("a command the money stopped: %q", a)
	}
	if pr := rig.pr(7); pr == nil || pr.SkipReason != "budget" {
		t.Errorf("the skip reason was not recorded")
	}
}

// A repeat after a push: the review the command asked for on the new head held a minor finding to
// what changed since the last one, and the same command again on that head is the same review — so
// it is answered from it, for no model call and no second review, as a repeat on any reviewed head is.
func TestReviewCommandRepeatAfterAPushIsAnsweredFromState(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"command"}`)
	rig.serveConversation()
	rig.confirmLock()
	rig.deliver("issue_comment", commentEvent(501, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	rig.gh.pushTo(reviewHeadC, totalsFixture().files, totalsFixture().head)
	rig.deliver("pull_request", prEvent("synchronize", 7, reviewHeadC))
	rig.drain()
	rig.deliver("issue_comment", commentEvent(502, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	var last *ReviewRun
	for _, r := range rig.runs(7) {
		if r.Kind == "review" && r.TriggerRef == "comment:502" {
			last = r
		}
	}
	if last == nil || last.Status != "posted" || last.HeadSHA != reviewHeadC || reviewOptionsOf(last).Scope != reviewScopeSinceLast {
		t.Fatalf("the review after the push = %+v", last)
	}
	calls := len(rig.model.requests(""))
	posts, _, _, _ := rig.gh.snapshot()

	rig.deliver("issue_comment", commentEvent(503, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	runs := rig.runs(7)
	if runs[0].TriggerRef != "comment:503" || runs[0].Status != "noop" {
		t.Fatalf("the repeat on the head a review of what changed reviewed = %+v", runs[0])
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("the repeat made %d model calls", n-calls)
	}
	if now, _, _, _ := rig.gh.snapshot(); len(now) != len(posts) {
		t.Errorf("the repeat posted another review")
	}
	if a := rig.answers(); len(a) == 0 || !strings.Contains(a[len(a)-1], "Already reviewed `ccccccc`") {
		t.Errorf("the repeat was not answered from state: %q", a)
	}
}

// Where a command is answered follows where the pull request's review goes, which its branch rule
// decides: a rule posting live on a repository in shadow gets the reaction and the answers, and a
// rule recording in shadow on a live repository gets nothing written at all, with the audit log
// saying so.
func TestReviewCommandFollowsTheBranchRule(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"shadow","branch_rules":[{"base":"main","post":"live"},{}]}`)
	cv := rig.serveConversation()
	rig.deliver("issue_comment", commentEvent(1301, "alice", "MEMBER", "@attesttag status", true))
	if !cv.has(rig.gh, "issue:1301:eyes") || len(rig.answers()) != 1 {
		t.Errorf("a command on a pull request into main, posted live by its rule: reactions %v, answers %q", cv.reactions, rig.answers())
	}

	rig = newLaneRig(t, totalsFixture(), `{"mode":"live","branch_rules":[{"base":"main","post":"shadow"},{}]}`)
	rig.serveConversation()
	rig.deliver("issue_comment", commentEvent(1302, "alice", "MEMBER", "@attesttag status", true))
	rig.deliver("issue_comment", commentEvent(1303, "stranger", "NONE", "@attesttag review", false))
	for _, s := range rig.fake.sent() {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("a command on a pull request recorded in shadow by its rule wrote to GitHub: %s", s)
		}
	}
	audits := rig.commandAudits()
	if len(audits) != 2 {
		t.Fatalf("audits = %+v", audits)
	}
	for _, a := range audits {
		if a["shadow"] != true || !strings.Contains(a["outcome"].(string), "nothing posted on GitHub") {
			t.Errorf("a command in shadow audited as %+v", a)
		}
	}
}

// A review recorded in shadow on a pull request that otherwise posts is somebody's decision not to
// say it there: status says one was recorded, and nothing of its score or what it found.
func TestReviewCommandStatusKeepsAShadowReviewToItself(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.serveConversation()
	rig.confirmLock()
	if _, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation, Trigger: "console",
		TriggerRef: "console:record-only", RequestedBy: "console:admin@acme.test", Post: review.ModeShadow, BypassFilters: true}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if runs := rig.runs(7); len(runs) != 1 || runs[0].Status != "shadow" {
		t.Fatalf("runs = %+v", runs)
	}
	rig.deliver("issue_comment", commentEvent(1401, "carol", "COLLABORATOR", "@attesttag status", false))
	a := rig.answers()
	if len(a) != 1 || !strings.Contains(a[0], "recorded in the attest_tag console") {
		t.Fatalf("status = %q", a)
	}
	for _, leak := range []string{"Confidence", "/5", "P1", "Open findings", "aaaaaaa"} {
		if strings.Contains(a[0], leak) {
			t.Errorf("status of a shadow review says %q:\n%s", leak, a[0])
		}
	}
}
