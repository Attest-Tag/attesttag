package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"attesttag/internal/review"
)

// Code review's announcements in a chat channel, on the lane rig with a chat platform that records
// what it is sent. What these pin is the shape the team asked for — one message per pull request,
// posted once and edited in place, every later event one reply in its thread but a start, which only
// edits it — that a failure and the skips somebody has to act on are told and the gate's quiet ones
// are not, that the message never stays on "Reviewing…", that notify_on leaves out what it leaves
// out, that the channel hears of shadow reviews as shadow, that a branch rule can send a pull request
// elsewhere, and that nothing the chat platform does can fail a review or ping a channel.

// notifyChat is a chat platform that keeps every post, edit and deletion, and fails them on request.
// Only what an announcement uses is implemented; anything else panics through the nil transport.
type notifyChat struct {
	transport
	mu       sync.Mutex
	next     int
	posts    []notifyMsg
	edits    []notifyMsg
	deleted  []string
	failPost error
	failEdit error
	// shared makes every channel one shared with another organisation (Slack Connect).
	shared bool
	// onRoot runs once, after the next message of its own is posted and before it is returned: what
	// another event does in that moment.
	onRoot func()
}

type notifyMsg struct{ channel, thread, ts, text string }

func (c *notifyChat) postMarkdown(_ context.Context, channel, threadTS, md, _ string) (string, error) {
	c.mu.Lock()
	if c.failPost != nil {
		c.mu.Unlock()
		return "", c.failPost
	}
	c.next++
	ts := fmt.Sprintf("1700000000.%06d", c.next)
	c.posts = append(c.posts, notifyMsg{channel: channel, thread: threadTS, ts: ts, text: md})
	hook := c.onRoot
	if threadTS == "" {
		c.onRoot = nil
	} else {
		hook = nil
	}
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ts, nil
}

func (c *notifyChat) conversation(_ context.Context, id string) (string, convInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return id, convInfo{IsExtShared: c.shared}, nil
}

func (c *notifyChat) updateMarkdown(_ context.Context, channel, ts, md, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failEdit != nil {
		return c.failEdit
	}
	c.edits = append(c.edits, notifyMsg{channel: channel, ts: ts, text: md})
	return nil
}

func (c *notifyChat) deleteMessage(_ context.Context, _, ts string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, ts)
	return nil
}

func (c *notifyChat) deletions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deleted...)
}

func (c *notifyChat) snapshot() (posts, edits []notifyMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]notifyMsg(nil), c.posts...), append([]notifyMsg(nil), c.edits...)
}

// roots and threadPosts split the posts: a message of its own in the channel, or one in a thread.
func (c *notifyChat) roots() []notifyMsg {
	return c.where(func(m notifyMsg) bool { return m.thread == "" })
}
func (c *notifyChat) threadPosts() []notifyMsg {
	return c.where(func(m notifyMsg) bool { return m.thread != "" })
}

func (c *notifyChat) where(keep func(notifyMsg) bool) []notifyMsg {
	posts, _ := c.snapshot()
	var out []notifyMsg
	for _, m := range posts {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

// withNotifyChat connects workspace T1 to the rig's organisation through a recording platform.
func withNotifyChat(rig *laneRig) *notifyChat {
	c := &notifyChat{}
	rig.b.slacks = testRegistry(&Chat{t: c, Platform: platformSlack, TeamID: "T1", OrgID: orgID})
	return c
}

// mergedEvent is a pull_request delivery saying acme/web#7 was merged into main by login.
func mergedEvent(head, login string) []byte {
	return prEvent("closed", 7, head, func(pr map[string]any) {
		pr["state"], pr["merged"], pr["merged_by"] = "closed", true, map[string]any{"login": login, "type": "User"}
	})
}

func (rig *laneRig) audited(action string) []AuditEvent {
	rig.t.Helper()
	events, err := rig.st.AuditEvents(context.Background(), orgID, AuditFilter{Action: action})
	if err != nil {
		rig.t.Fatal(err)
	}
	return events
}

const notifyC1 = `"notify":{"team":"T1","channel":"C1"}`

// notifyResults leaves the starts out, for the tests of what a finished review and a merge do to
// the message: with them, every review's message is posted by its start and edited by its result.
const notifyResults = `"notify_on":["finished","merged"]`

// The first review posts the pull request's one message: its title and branches, the commit, the
// score, what is open and the worst of it linked to the code, and where to read more. A push says
// nothing. The re-review edits that message to how things stand now and replies once in its thread
// with what changed; the merge does the same; and a merge GitHub delivers again is not said twice.
// Starts are left out (notifyResults), so none of it says a review is under way.
func TestReviewNotifyPostsOneMessageThenEditsItAndReplies(t *testing.T) {
	t.Setenv("ADMIN_BASE_URL", "https://console.acme.test")
	var chat *notifyChat
	rig, _ := reviewedAtAWith(t, `{"mode":"live",`+notifyC1+`,`+notifyResults+`}`, nil, func(rig *laneRig) { chat = withNotifyChat(rig) })
	first := rig.runs(7)[0]
	roots, _ := chat.snapshot()
	if len(roots) != 1 || roots[0].channel != "C1" || roots[0].thread != "" {
		t.Fatalf("after the first review the channel got %+v; want one message of its own", roots)
	}
	root := roots[0].text
	for _, want := range []string{"**[acme/web#7](https://github.com/acme/web/pull/7)** Make Add faster",
		"`feature` → `main`", "last reviewed `aaaaaaa`", "Confidence 2/5", "3 P1 open",
		"• **P1** " + sqlTitle, "(https://github.com/acme/web/blob/" + reviewHead + "/src/store.go#L6)",
		"[Summary on GitHub](https://github.com/acme/web/pull/7#issuecomment-",
		"[In the console](https://console.acme.test/admin/reviews/?tab=history&run=" + first.PublicID + ")"} {
		if !strings.Contains(root, want) {
			t.Errorf("the message lacks %q:\n%s", want, root)
		}
	}
	if strings.Contains(root, "shadow") || strings.Contains(root, "Merged") || strings.Contains(root, "Reviewing") {
		t.Errorf("a live review's message says shadow, merged or reviewing:\n%s", root)
	}
	pr := rig.pr(7)
	if pr.NotifyTeam != "T1" || pr.NotifyChannel != "C1" || pr.NotifyTS != roots[0].ts || pr.NotifyLast != "run:"+first.PublicID {
		t.Errorf("the pull request records its message as %q %q %q, last %q", pr.NotifyTeam, pr.NotifyChannel, pr.NotifyTS, pr.NotifyLast)
	}
	if got := rig.audited("review.notified"); len(got) != 1 || got[0].TargetID != "acme/web#7" {
		t.Errorf("announcements audited: %+v", got)
	}

	// A push re-renders the summary on GitHub; the channel hears nothing of it.
	fx := totalsFixture()
	fx.addFile("src/store.go", storeAtA)
	rig.pushAdded(fx, reviewHeadC, map[string]string{"src/store.go": storeAtB})
	if posts, edits := chat.snapshot(); len(posts) != 1 || len(edits) != 0 {
		t.Fatalf("a push was announced: %d posts, %d edits", len(posts), len(edits))
	}

	rig.deliver("issue_comment", commentEvent(1601, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	posts, edits := chat.snapshot()
	if len(chat.roots()) != 1 || len(edits) != 1 || edits[0].ts != roots[0].ts {
		t.Fatalf("the re-review: %d messages of their own, edits %+v; want the one message edited", len(chat.roots()), edits)
	}
	if replies := chat.threadPosts(); len(replies) != 1 || replies[0].thread != roots[0].ts ||
		replies[0].text != "Re-reviewed `ccccccc`: 1 fixed, 1 open, score 2 → 3." {
		t.Errorf("the re-review's reply = %+v", replies)
	}
	for _, want := range []string{"last reviewed `ccccccc`", "Confidence 3/5", "1 P1 open", scanTitle} {
		if !strings.Contains(edits[0].text, want) {
			t.Errorf("the edited message lacks %q:\n%s", want, edits[0].text)
		}
	}
	if strings.Contains(edits[0].text, sqlTitle) {
		t.Errorf("the edited message still lists the fixed finding:\n%s", edits[0].text)
	}

	rig.deliver("pull_request", mergedEvent(reviewHeadC, "alice"))
	posts, edits = chat.snapshot()
	if len(chat.roots()) != 1 || len(edits) != 2 || !strings.HasSuffix(edits[1].text, "Merged into `main` by alice.") {
		t.Fatalf("the merge: %d messages of their own, last edit:\n%s", len(chat.roots()), edits[len(edits)-1].text)
	}
	if replies := chat.threadPosts(); len(replies) != 2 || replies[1].thread != roots[0].ts ||
		replies[1].text != "Merged into `main` by alice — 1 P1 still open." {
		t.Errorf("the merge's reply = %+v", replies)
	}
	if pr := rig.pr(7); pr.NotifyLast != "merged" || pr.State != "merged" {
		t.Errorf("after the merge: last %q, state %q", pr.NotifyLast, pr.State)
	}
	rig.deliver("pull_request", mergedEvent(reviewHeadC, "alice"))
	if again, _ := chat.snapshot(); len(again) != len(posts) {
		t.Errorf("a merge delivered again was announced again: %d posts, was %d", len(again), len(posts))
	}
}

// A pull request merged before anything reviewed it — a dependency bump the gate skips, a draft, one
// on a repository reviewed only when asked — is not news: the channel would get a message for every
// one of them saying only that. Nothing is posted, and no model is asked anything.
func TestReviewNotifyMergedWithoutAReview(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"shadow",`+notifyC1+`}`)
	chat := withNotifyChat(rig)
	rig.deliver("pull_request", mergedEvent(reviewHead, "bob"))
	if posts, edits := chat.snapshot(); len(posts) != 0 || len(edits) != 0 {
		t.Fatalf("the merge of an unreviewed pull request was announced: posts %+v, edits %+v", posts, edits)
	}
	if pr := rig.pr(7); pr.NotifyLast != "merged" || pr.NotifyTS != "" {
		t.Errorf("the quiet merge recorded as %q, message %q", pr.NotifyLast, pr.NotifyTS)
	}
	if n := len(rig.model.requests("")); n != 0 {
		t.Errorf("a merge asked the model %d times", n)
	}
	// The message an unreviewed pull request would carry, for one the channel has heard of: no review
	// spoken of.
	root := reviewNoticeRoot(reviewNoticeView{Repo: "acme/web", Number: 7, Base: "main", Head: "feature", Score: -1,
		Merged: true, MergedBy: "bob"})
	for _, want := range []string{"acme/web#7", "`feature` → `main` · not reviewed", "Merged into `main` by bob."} {
		if !strings.Contains(root, want) {
			t.Errorf("the message lacks %q:\n%s", want, root)
		}
	}
	if strings.Contains(root, "Confidence") || strings.Contains(root, "console") {
		t.Errorf("an unreviewed pull request's message speaks of a review:\n%s", root)
	}
	// Closed without merging is not an announcement.
	rig2 := newLaneRig(t, totalsFixture(), `{"mode":"shadow",`+notifyC1+`}`)
	chat2 := withNotifyChat(rig2)
	rig2.deliver("pull_request", prEvent("closed", 7, reviewHead, func(pr map[string]any) { pr["state"] = "closed" }))
	if posts, _ := chat2.snapshot(); len(posts) != 0 {
		t.Errorf("a pull request closed unmerged was announced: %+v", posts)
	}
}

// A review recorded in shadow is announced, and says so from its start — the channel is the
// organisation's own, not GitHub — with no link to a summary on GitHub that shadow never wrote. A
// branch rule's channel takes the pull requests it matches, and an empty one silences them.
func TestReviewNotifyShadowIsLabelledAndARuleChoosesTheChannel(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"shadow",`+notifyC1+`,`+
		`"branch_rules":[{"base":"main","notify":{"team":"T1","channel":"C2"}},{}]}`)
	chat := withNotifyChat(rig)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if runs := rig.runs(7); len(runs) != 1 || runs[0].Status != "shadow" {
		t.Fatalf("runs = %+v", runs)
	}
	roots, replies := chat.roots(), chat.threadPosts()
	_, edits := chat.snapshot()
	if len(roots) != 1 || roots[0].channel != "C2" || len(edits) != 1 || edits[0].channel != "C2" || len(replies) != 1 {
		t.Fatalf("the rule's channel was not the one told: roots %+v, edits %+v, replies %+v", roots, edits, replies)
	}
	if !strings.Contains(roots[0].text, "Reviewing `aaaaaaa` (General) in shadow…") {
		t.Errorf("a shadow review's start:\n%s", roots[0].text)
	}
	if !strings.Contains(edits[0].text, "Confidence 3/5 · shadow") || strings.Contains(edits[0].text, "Summary on GitHub") ||
		strings.Contains(edits[0].text, "Reviewing") {
		t.Errorf("a shadow review's message:\n%s", edits[0].text)
	}
	if replies[0].text != "Reviewed `aaaaaaa`: 0 fixed, 1 open, score 3 (shadow)." {
		t.Errorf("a shadow review's reply = %q", replies[0].text)
	}
	for _, s := range rig.fake.sent() {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("shadow mode wrote to GitHub: %s", s)
		}
	}

	quiet := newLaneRig(t, totalsFixture(), `{"mode":"shadow",`+notifyC1+`,"branch_rules":[{"head":"feature","notify":{}},{}]}`)
	quietChat := withNotifyChat(quiet)
	quiet.confirmLock()
	quiet.deliver("pull_request", prEvent("opened", 7, reviewHead))
	quiet.drain()
	if posts, _ := quietChat.snapshot(); len(posts) != 0 || quiet.runs(7)[0].Status != "shadow" {
		t.Errorf("a rule's empty channel should silence its pull requests: %+v", posts)
	}
}

// The chat platform refusing never fails the review: it is on GitHub and recorded as posted, the
// refusal is audited with its error, and the next event tries again. A message somebody deleted is
// posted afresh rather than edited into nowhere, and an edit refused for another reason is not a
// second message beside the first.
func TestReviewNotifyFailuresNeverFailTheReview(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`,`+notifyResults+`}`)
	chat := withNotifyChat(rig)
	chat.failPost = errors.New("channel_not_found")
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if runs := rig.runs(7); len(runs) != 1 || runs[0].Status != "posted" {
		t.Fatalf("a refused announcement changed the review: %+v", runs)
	}
	failed := rig.audited("review.notify_failed")
	if len(failed) != 1 || !strings.Contains(string(failed[0].Details), "channel_not_found") {
		t.Errorf("the refusal audited: %+v", failed)
	}
	if pr := rig.pr(7); pr.NotifyTS != "" || pr.NotifyLast != "" {
		t.Errorf("a refused announcement was recorded as made: %+v", pr)
	}

	chat.failPost = nil
	ctx := context.Background()
	if _, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation,
		Trigger: "console", TriggerRef: "console-full-1", RequestedBy: "admin@acme.test", BypassFilters: true, Full: true}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	roots := chat.roots()
	if len(roots) != 1 || len(chat.threadPosts()) != 0 {
		t.Fatalf("the next review should post the message the first could not: roots %+v, replies %+v", roots, chat.threadPosts())
	}

	// Gone: posted again, as news of its own.
	chat.failEdit = errors.New("message_not_found")
	if _, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation,
		Trigger: "console", TriggerRef: "console-full-2", RequestedBy: "admin@acme.test", BypassFilters: true, Full: true}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if again := chat.roots(); len(again) != 2 || rig.pr(7).NotifyTS != again[1].ts || len(chat.threadPosts()) != 0 {
		t.Fatalf("a deleted message: roots %+v, recorded %q", again, rig.pr(7).NotifyTS)
	}

	// Refused for another reason: tried again at the next event, never a duplicate.
	chat.failEdit = errors.New("ratelimited")
	rig.deliver("pull_request", mergedEvent(reviewHead, "alice"))
	if n := len(chat.roots()); n != 2 {
		t.Errorf("an edit refused for a reason other than the message being gone posted a new one: %d", n)
	}
	if pr := rig.pr(7); pr.NotifyLast == "merged" {
		t.Error("a merge the channel never heard of was recorded as announced")
	}
}

// Whatever a pull request's author or a model wrote reaches the channel as text: no broadcast, no
// mention, no link of its own, no markup, on one line and cut short.
func TestReviewNotifyDefusesWhatThePullRequestWrote(t *testing.T) {
	evil := "<!channel> ping @here and @everyone: [log in](https://evil.example/x) `code` <https://evil.example|https://github.com>"
	root := reviewNoticeRoot(reviewNoticeView{Repo: "acme/web", Number: 7, Title: evil + "\nsecond line " + strings.Repeat("x", 300),
		Base: "main`<!here>", Head: "feature/@channel", ReviewedSHA: reviewHead, Score: 1,
		Open: []*ReviewFinding{{Finding: review.Finding{Path: "src/a.go", Line: 3, Severity: review.P0, Title: evil}}}})
	for _, bad := range []string{"<!channel>", "<!here>", "<https://evil", "](https://evil", "@here", "@everyone", "@channel",
		"`code`", "second line\n"} {
		if strings.Contains(root, bad) {
			t.Errorf("the message carries %q:\n%s", bad, root)
		}
	}
	for _, want := range []string{"&lt;!channel&gt;", "@\u2060here", "\n• **P0** "} {
		if !strings.Contains(root, want) {
			t.Errorf("the message lacks %q:\n%s", want, root)
		}
	}
	first, _, _ := strings.Cut(root, "\n")
	if n := len([]rune(first)); n > 260 {
		t.Errorf("the title line is %d characters long:\n%s", n, first)
	}
	if got := reviewChatText("a\n\tb   c", 100); got != "a b c" {
		t.Errorf("reviewChatText kept the lines: %q", got)
	}
	raw, _ := json.Marshal(root)
	if strings.Contains(string(raw), `\u003c!`) {
		t.Errorf("a broadcast survived somewhere in %s", raw)
	}
}

// Two events racing to post the first message: the one whose write finds the pull request already
// announced does not take it over.
func TestReviewNotifyRootIsTakenOnce(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{}`)
	pr, err := rig.st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "acme/web", Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := rig.st.setReviewPRNotifyRoot(ctx, orgID, pr.ID, "T1", "C1", "1.1", ""); err != nil || !ok {
		t.Fatalf("the first message = %v %v", ok, err)
	}
	if ok, err := rig.st.setReviewPRNotifyRoot(ctx, orgID, pr.ID, "T1", "C1", "1.2", ""); err != nil || ok {
		t.Fatalf("a second first message took over: %v %v", ok, err)
	}
	if ok, _ := rig.st.setReviewPRNotifyRoot(ctx, orgID+1, pr.ID, "T1", "C1", "1.3", "1.1"); ok {
		t.Error("another organisation's write moved this pull request's message")
	}
	if got := rig.pr(7); got.NotifyTS != "1.1" {
		t.Errorf("the message is %q, want the first", got.NotifyTS)
	}
}

// A review announced after the merge — one already with the model, or already posting, when the pull
// request was merged — edits the message to its own result and keeps it saying merged; and the merge
// stays the last word on what was announced, so GitHub delivering it again says nothing twice.
func TestReviewNotifyAReviewAfterTheMergeKeepsIt(t *testing.T) {
	var chat *notifyChat
	rig, _ := reviewedAtAWith(t, `{"mode":"live",`+notifyC1+`,`+notifyResults+`}`, nil, func(rig *laneRig) { chat = withNotifyChat(rig) })
	run := rig.runs(7)[0]
	rig.deliver("pull_request", mergedEvent(reviewHead, "alice"))
	ctx := context.Background()
	rig.b.notifyReviewChannel(ctx, rig.pr(7), review.Effective{Notify: review.NotifyChannel{Team: "T1", Channel: "C1"}}, reviewNotice{kind: reviewNoticeReview,
		run: run, before: -1, title: "Make Add faster", base: "main", head: "feature"})
	posts, edits := chat.snapshot()
	if len(chat.roots()) != 1 || len(edits) != 2 || len(chat.threadPosts()) != 2 {
		t.Fatalf("the late review: %d messages of their own, %d edits, replies %+v", len(chat.roots()), len(edits), chat.threadPosts())
	}
	if last := edits[1].text; !strings.HasSuffix(last, "Merged into `main`.") || !strings.Contains(last, "last reviewed `aaaaaaa`") {
		t.Errorf("the message after a review announced past the merge:\n%s", last)
	}
	if pr := rig.pr(7); pr.NotifyLast != "merged" {
		t.Errorf("a review announced after the merge recorded %q over it", pr.NotifyLast)
	}
	rig.deliver("pull_request", mergedEvent(reviewHead, "alice"))
	if again, _ := chat.snapshot(); len(again) != len(posts) {
		t.Errorf("a merge delivered again after a later review was announced again: %d posts, was %d", len(again), len(posts))
	}
}

// Two events racing to post the first message: the one whose record finds the pull request announced
// already takes its own post back, and edits the winner's message — drawn again from the pull request
// as the winner left it, so a merge that won keeps its line — and replies in its thread.
func TestReviewNotifyALostRaceRepliesInTheWinner(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`}`)
	chat := withNotifyChat(rig)
	chat.failPost = errors.New("ratelimited")
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	chat.failPost = nil
	pr := rig.pr(7)
	if pr.NotifyTS != "" {
		t.Fatalf("the first announcement was refused, and still recorded %q", pr.NotifyTS)
	}
	// The merge lands, and posts its message, while this review's post is on its way.
	chat.onRoot = func() {
		if _, err := rig.st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "acme/web", Number: 7, State: "merged"}); err != nil {
			t.Error(err)
		}
		if ok, err := rig.st.setReviewPRNotifyRoot(ctx, orgID, pr.ID, "T1", "C1", "1.winner", ""); !ok || err != nil {
			t.Errorf("the winner's message = %v %v", ok, err)
		}
	}
	rig.b.notifyReviewChannel(ctx, pr, review.Effective{Notify: review.NotifyChannel{Team: "T1", Channel: "C1"}}, reviewNotice{kind: reviewNoticeReview,
		run: rig.runs(7)[0], before: -1, title: "Make Add faster", base: "main", head: "feature"})
	roots := chat.roots()
	if len(roots) != 1 || !slices.Equal(chat.deletions(), []string{roots[0].ts}) {
		t.Fatalf("the losing post: roots %+v, deleted %v", roots, chat.deletions())
	}
	_, edits := chat.snapshot()
	if len(edits) != 1 || edits[0].ts != "1.winner" || !strings.HasSuffix(edits[0].text, "Merged into `main`.") {
		t.Errorf("the winner's message was edited to %+v", edits)
	}
	if replies := chat.threadPosts(); len(replies) != 1 || replies[0].thread != "1.winner" ||
		!strings.HasPrefix(replies[0].text, "Reviewed `aaaaaaa`") {
		t.Errorf("the reply = %+v", replies)
	}
	if got := rig.pr(7); got.NotifyTS != "1.winner" || got.NotifyLast != "run:"+rig.runs(7)[0].PublicID {
		t.Errorf("the pull request's message %q, last %q", got.NotifyTS, got.NotifyLast)
	}
}

// Nothing is posted in a channel shared with another organisation: a repository's findings are not
// another company's to read. The settings refuse one at save, and a channel shared since is refused
// at the post — the start's and the result's alike, each audited and told to the admins like any
// refusal, the review untouched.
func TestReviewNotifyRefusesASharedChannel(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`}`)
	chat := withNotifyChat(rig)
	chat.shared = true
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if posts, _ := chat.snapshot(); len(posts) != 0 {
		t.Errorf("a Slack Connect channel was told: %+v", posts)
	}
	if runs := rig.runs(7); len(runs) != 1 || runs[0].Status != "posted" {
		t.Fatalf("a refused announcement changed the review: %+v", runs)
	}
	if failed := rig.audited("review.notify_failed"); len(failed) != 2 || !strings.Contains(string(failed[0].Details), "shared with another organisation") ||
		!strings.Contains(string(failed[1].Details), "shared with another organisation") {
		t.Errorf("the refusals audited: %+v", failed)
	}

	api := newReviewAPIRig(t, `{"mode":"shadow"}`)
	withChannel(t, api.st, orgID, "T1", "C1")
	api.b.slacks = testRegistry(&Chat{t: &notifyChat{shared: true}, Platform: platformSlack, TeamID: "T1", OrgID: orgID})
	if code, out := api.call("PUT", "/api/review-settings/"+api.connID(), api.admin,
		settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"}})); code != 400 ||
		!strings.Contains(out["error"].(string), "shared with another organisation") {
		t.Errorf("saving a Slack Connect channel = %d %v", code, out)
	}
	api.b.slacks = testRegistry(&Chat{t: &notifyChat{}, Platform: platformSlack, TeamID: "T1", OrgID: orgID})
	api.must(200, "PUT", "/api/review-settings/"+api.connID(), api.admin, settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"}}))
}

// A workspace that is not the organisation's — connected to another one since the setting was saved,
// or with no owner recorded — is never posted in.
func TestReviewNotifyRefusesAnotherOrganisationsWorkspace(t *testing.T) {
	for _, owner := range []int64{orgID + 1, 0} {
		rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`}`)
		chat := &notifyChat{}
		rig.b.slacks = testRegistry(&Chat{t: chat, Platform: platformSlack, TeamID: "T1", OrgID: owner})
		rig.confirmLock()
		rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
		rig.drain()
		if posts, _ := chat.snapshot(); len(posts) != 0 {
			t.Errorf("a workspace owned by %d was posted in: %+v", owner, posts)
		}
		failed := rig.audited("review.notify_failed")
		if len(failed) != 2 { // the start's and the result's
			t.Errorf("owner %d: the refusals audited: %+v", owner, failed)
		}
		for _, f := range failed {
			if !strings.Contains(string(f.Details), "not connected to this organisation") {
				t.Errorf("owner %d: the refusal audited: %+v", owner, f)
			}
		}
	}
}

// ---- starts, failures and skips ----

// edited and posted since: what the channel was sent after n edits and m replies.
func (c *notifyChat) since(edits, replies int) ([]notifyMsg, []notifyMsg) {
	_, e := c.snapshot()
	return e[edits:], c.threadPosts()[replies:]
}

// A review's start puts the pull request's message on "Reviewing `abc1234` (General)…" — posting it
// when the channel has not heard of the pull request, nothing reviewed yet — and replies nothing: the
// result edits the message to what the review found and is the one reply. A re-review's start keeps
// the last review's open findings listed, since they are what is open while it runs.
func TestReviewNotifyStartEditsTheMessageWithoutAReply(t *testing.T) {
	var chat *notifyChat
	rig, _ := reviewedAtAWith(t, `{"mode":"live",`+notifyC1+`}`, nil, func(rig *laneRig) { chat = withNotifyChat(rig) })
	roots, replies := chat.roots(), chat.threadPosts()
	_, edits := chat.snapshot()
	if len(roots) != 1 || len(edits) != 1 || edits[0].ts != roots[0].ts || len(replies) != 1 {
		t.Fatalf("the first review: roots %+v, edits %+v, replies %+v", roots, edits, replies)
	}
	if !strings.HasSuffix(roots[0].text, "\n`feature` → `main` · not reviewed\nReviewing `aaaaaaa` (General, Security)…") {
		t.Errorf("the start's message:\n%s", roots[0].text)
	}
	if strings.Contains(edits[0].text, "Reviewing") || !strings.Contains(edits[0].text, "3 P1 open") {
		t.Errorf("the result's message:\n%s", edits[0].text)
	}
	if replies[0].text != "Reviewed `aaaaaaa`: 0 fixed, 3 open, score 2." {
		t.Errorf("the result's reply = %q", replies[0].text)
	}

	fx := totalsFixture()
	fx.addFile("src/store.go", storeAtA)
	rig.pushAdded(fx, reviewHeadC, map[string]string{"src/store.go": storeAtB})
	rig.deliver("issue_comment", commentEvent(1601, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	edits, replies = chat.since(1, 1)
	if len(chat.roots()) != 1 || len(edits) != 2 || len(replies) != 1 {
		t.Fatalf("the re-review: %d messages of their own, edits %+v, replies %+v; want a start's edit and the result's", len(chat.roots()), edits, replies)
	}
	for _, want := range []string{"· last reviewed `aaaaaaa` · Confidence 2/5\nReviewing `ccccccc` (General, Security)…\n3 P1 open", "• **P1** " + sqlTitle} {
		if !strings.Contains(edits[0].text, want) {
			t.Errorf("the re-review's start lacks %q:\n%s", want, edits[0].text)
		}
	}
	if strings.Contains(edits[1].text, "Reviewing") || replies[0].text != "Re-reviewed `ccccccc`: 1 fixed, 1 open, score 2 → 3." {
		t.Errorf("the re-review's result:\n%s\nreply %q", edits[1].text, replies[0].text)
	}
	var starts []string
	for _, e := range rig.audited("review.notified") {
		if strings.Contains(string(e.Details), `"event":"start"`) {
			starts = append(starts, string(e.Details))
		}
	}
	if len(starts) != 2 || !strings.Contains(starts[0]+starts[1], `"message":"posted"`) || !strings.Contains(starts[0]+starts[1], `"message":"edited"`) {
		t.Errorf("the starts audited: %v", starts)
	}
}

// A review that fails puts the message back to how the pull request stood — the last review's
// commit, score and open findings — with a line saying which kind of failure it was, never the
// provider's error, and says the same once in the thread with the run in the console. Told again,
// the same failure says nothing. With failures left out of notify_on, the message is still put back,
// quietly: it never stays on "Reviewing…".
func TestReviewNotifyAFailurePutsTheMessageBackAndReplies(t *testing.T) {
	t.Setenv("ADMIN_BASE_URL", "https://console.acme.test")
	ctx := context.Background()
	for _, failedOn := range []bool{true, false} {
		settings := `{"mode":"live",` + notifyC1 + `}`
		if !failedOn {
			settings = `{"mode":"live",` + notifyC1 + `,"notify_on":["started","finished","merged"]}`
		}
		var chat *notifyChat
		rig, _ := reviewedAtAWith(t, settings, nil, func(rig *laneRig) { chat = withNotifyChat(rig) })
		_, before := chat.snapshot()
		replied := len(chat.threadPosts())
		rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return reviewModelReply{Status: 402} }
		if _, err := rig.b.enqueueReview(ctx, orgID, "acme/web", 7, reviewRequest{InstallationID: fakeInstallation,
			Trigger: "console", TriggerRef: "console-full-1", RequestedBy: "admin@acme.test", BypassFilters: true, Full: true}); err != nil {
			t.Fatal(err)
		}
		rig.drain()
		failed := rig.runs(7)[0]
		if failed.Status != "failed" || !strings.HasPrefix(failed.Error, string(review.FailModel)+":") {
			t.Fatalf("failed %v: the run = %s %q", failedOn, failed.Status, failed.Error)
		}
		edits, replies := chat.since(len(before), replied)
		if len(chat.roots()) != 1 || len(edits) != 2 || !strings.Contains(edits[0].text, "Reviewing `aaaaaaa` (General, Security)…") {
			t.Fatalf("failed %v: roots %d, edits %+v", failedOn, len(chat.roots()), edits)
		}
		back := edits[1].text
		for _, want := range []string{"· last reviewed `aaaaaaa` · Confidence 2/5\n", "3 P1 open", "• **P1** " + sqlTitle} {
			if !strings.Contains(back, want) {
				t.Errorf("failed %v: the message put back lacks %q:\n%s", failedOn, want, back)
			}
		}
		line := "Review of `aaaaaaa` failed: the model provider did not answer."
		if !failedOn {
			if strings.Contains(back, "Reviewing") || strings.Contains(back, "failed") || len(replies) != 0 {
				t.Errorf("with failures left out the message is put back quietly:\n%s\nreplies %+v", back, replies)
			}
			continue
		}
		if !strings.Contains(back, "Confidence 2/5\n"+line+"\n3 P1 open") {
			t.Errorf("the message after the failure:\n%s", back)
		}
		wantReply := line + " [In the console](https://console.acme.test/admin/reviews/?tab=history&run=" + failed.PublicID + ")"
		if len(replies) != 1 || replies[0].text != wantReply {
			t.Errorf("the failure's reply = %+v, want %q", replies, wantReply)
		}
		if _, detail, _ := strings.Cut(failed.Error, ": "); detail == "" || strings.Contains(back, detail) ||
			len(replies) > 0 && strings.Contains(replies[0].text, detail) {
			t.Errorf("the provider's error %q reached the channel", detail)
		}
		if pr := rig.pr(7); pr.NotifyLast != "failed:"+failed.PublicID {
			t.Errorf("the failure recorded as %q", pr.NotifyLast)
		}
		posts, all := chat.snapshot()
		rig.b.notifyReviewChannel(ctx, rig.pr(7), review.Effective{Notify: review.NotifyChannel{Team: "T1", Channel: "C1"}},
			reviewNotice{kind: reviewNoticeFail, run: failed, why: "again"})
		if p, e := chat.snapshot(); len(p) != len(posts) || len(e) != len(all) {
			t.Error("the same failure told again was announced again")
		}
	}
}

// A review the gate stops is told to the channel only where somebody is waiting for one that is not
// coming — the plan has no code review, the money is spent, the pull request is paused — in the
// channel's words, no amounts, and once per reason however many deliveries meet it: a new message
// for a pull request the channel had not heard of, an edit and a reply for one it had. A draft and a
// bot's pull request are the gate doing what it was set to, and say nothing — on a plan with no code
// review as on any other, where the plan's refusal is recorded on them and told of none.
func TestReviewNotifySkipsSomebodyMustActOnAreTold(t *testing.T) {
	ctx := context.Background()
	freePlan := func(rig *laneRig) {
		rig.b.cfg.CodeReview = CodeReviewPro
		if org, _, _ := seedOrg(t, rig.st, RoleAdmin); org != orgID {
			t.Fatalf("the organisation is %d", org)
		}
		if err := rig.st.SetOrgPlan(ctx, orgID, PlanFree, 0); err != nil {
			t.Fatal(err)
		}
		rig.b.settings.Invalidate(orgID)
	}
	for _, c := range []struct {
		name  string
		setup func(rig *laneRig)
		why   string
	}{
		{"plan", freePlan, "this organisation's plan does not include code review"},
		{"budget", func(rig *laneRig) {
			if err := rig.st.PutSetting(ctx, orgID, "review_daily_usd", "0.5"); err != nil {
				t.Fatal(err)
			}
		}, "the organisation's code review budget is spent for now"},
	} {
		rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`}`)
		chat := withNotifyChat(rig)
		c.setup(rig)
		rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
		rig.deliver("pull_request", prEvent("reopened", 7, reviewHead))
		rig.drain()
		roots, _ := chat.snapshot()
		if pr := rig.pr(7); len(rig.runs(7)) != 0 || pr.SkipReason != c.name || pr.NotifyLast != "skip:"+c.name {
			t.Fatalf("%s: %d runs, skip %q, last %q", c.name, len(rig.runs(7)), pr.SkipReason, pr.NotifyLast)
		}
		if len(roots) != 1 || !strings.HasSuffix(roots[0].text, "· not reviewed\nReview of `aaaaaaa` skipped: "+c.why+".") {
			t.Errorf("%s: told %+v; want one message, once", c.name, roots)
		}
		if strings.Contains(roots[0].text, "$") {
			t.Errorf("%s: the channel was told an amount:\n%s", c.name, roots[0].text)
		}
	}

	// Paused by a member, on a repository reviewed at every push: the first push says so, the next
	// says nothing new.
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live","trigger":"push",`+notifyC1+`}`)
	chat := withNotifyChat(rig)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	if _, err := rig.st.pauseReviewPR(ctx, orgID, rig.pr(7).ID, false); err != nil {
		t.Fatal(err)
	}
	_, was := chat.snapshot()
	replied := len(chat.threadPosts())
	rig.pushReviewed(shaOf(0))
	rig.pushReviewed(shaOf(1))
	edits, replies := chat.since(len(was), replied)
	line := "Review of `1111111` skipped: automatic reviews of this pull request are paused; one somebody asks for still runs."
	if len(chat.roots()) != 1 || len(edits) != 1 || len(replies) != 1 || replies[0].text != line || !strings.Contains(edits[0].text, "\n"+line+"\n") {
		t.Errorf("the pause told as edits %+v, replies %+v", edits, replies)
	}

	for _, plan := range []bool{false, true} {
		quiet := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`}`)
		quietChat := withNotifyChat(quiet)
		want := map[int]string{7: "draft", 8: "bot", 9: "plan"}
		if plan {
			freePlan(quiet)
			want = map[int]string{7: "plan", 8: "plan", 9: "plan"}
		}
		quiet.deliver("pull_request", prEvent("opened", 7, reviewHead, func(pr map[string]any) { pr["draft"] = true }))
		quiet.deliver("pull_request", prEvent("opened", 8, reviewHead, func(pr map[string]any) {
			pr["user"] = map[string]any{"login": "renovate[bot]", "type": "Bot"}
		}))
		// A push to a repository reviewed when its pull requests open, which no plan would review.
		quiet.deliver("pull_request", prEvent("synchronize", 9, reviewHead))
		if posts, edits := quietChat.snapshot(); len(posts) != 0 || len(edits) != 0 {
			t.Errorf("plan refused %v: a draft, a bot's pull request or a push was announced: %+v %+v", plan, posts, edits)
		}
		for n, reason := range want {
			if pr := quiet.pr(n); pr == nil || plan && pr.SkipReason != reason || !plan && n != 9 && pr.SkipReason != reason {
				t.Errorf("plan refused %v: #%d recorded %+v, want %q", plan, n, pr, reason)
			}
		}
	}
}

// Nothing left to review once the ignored files are set aside is the settings at work: said nowhere
// when no start was announced, and where one was, the message is put back with why and a reply, so
// it does not stay on "Reviewing…".
func TestReviewNotifyNothingToReviewIsToldOnlyAfterAStart(t *testing.T) {
	for _, started := range []bool{false, true} {
		on := `"notify_on":["finished","failed","merged"]`
		if started {
			on = `"notify_on":["started","finished","failed","merged"]`
		}
		rig := newLaneRig(t, totalsFixture(), `{"mode":"live","ignore_paths":["**"],`+notifyC1+`,`+on+`}`)
		chat := withNotifyChat(rig)
		rig.confirmLock()
		rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
		rig.drain()
		if run := rig.runs(7)[0]; run.Status != "skipped" || !strings.HasPrefix(run.Error, "nothing_to_review") {
			t.Fatalf("started %v: the run = %s %q", started, run.Status, run.Error)
		}
		posts, edits := chat.snapshot()
		if !started {
			if len(posts) != 0 || len(edits) != 0 {
				t.Errorf("a quiet skip was announced: %+v %+v", posts, edits)
			}
			continue
		}
		line := "Review of `aaaaaaa` skipped: every changed file is ignored, binary or generated."
		replies := chat.threadPosts()
		if len(chat.roots()) != 1 || len(edits) != 1 || strings.Contains(edits[0].text, "Reviewing") ||
			!strings.HasSuffix(edits[0].text, "\n"+line) || len(replies) != 1 || replies[0].text != line {
			t.Errorf("after a start: roots %+v, edits %+v, replies %+v", chat.roots(), edits, replies)
		}
	}
}

// notify_on leaves out what it leaves out, event by event: nothing at all with none; with only the
// merge, no word of the review and the merge posted as news of a pull request a review read; with
// only starts, the message the start posted is put back quietly when the review ends — the reply
// nobody asked for not posted, the message not left on "Reviewing…" — and the merge unsaid.
func TestReviewNotifyOnLeavesOutEachEvent(t *testing.T) {
	for _, c := range []struct {
		on                    string
		roots, edits, replies int
		last                  string // what the message ends on, "" none
	}{
		{`[]`, 0, 0, 0, ""},
		{`["merged"]`, 1, 0, 0, "Merged into `main` by alice."},
		{`["started"]`, 1, 1, 0, "[Summary on GitHub](https://github.com/acme/web/pull/7#issuecomment-"},
	} {
		rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`,"notify_on":`+c.on+`}`)
		chat := withNotifyChat(rig)
		rig.confirmLock()
		rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
		rig.drain()
		rig.deliver("pull_request", mergedEvent(reviewHead, "alice"))
		roots, replies := chat.roots(), chat.threadPosts()
		_, edits := chat.snapshot()
		if len(roots) != c.roots || len(edits) != c.edits || len(replies) != c.replies {
			t.Errorf("%s: roots %+v, edits %+v, replies %+v", c.on, roots, edits, replies)
			continue
		}
		msg := ""
		if len(edits) > 0 {
			msg = edits[len(edits)-1].text
		} else if len(roots) > 0 {
			msg = roots[0].text
		}
		if c.last != "" && (!strings.Contains(msg, c.last) || strings.Contains(msg, "Reviewing") || !strings.Contains(msg, "last reviewed `aaaaaaa`")) {
			t.Errorf("%s: the message:\n%s", c.on, msg)
		}
	}
}

// startedThenPutBack opens acme/web#7 and has its review put back once its start is announced:
// GitHub's answer moves under it while it reads the pull request. The run is claimable again at once.
func (rig *laneRig) startedThenPutBack(chat *notifyChat) *ReviewRun {
	rig.t.Helper()
	g := rig.gh
	g.mu.Lock()
	g.filesFn = func(call int) []map[string]any {
		// Under the fake's lock: the head moves away while the files are read and back the next
		// time, so the reader never sees one head twice and puts the run back (readPull).
		if g.head = reviewHead; call == 1 {
			g.head = strings.Repeat("d", 40)
		}
		return g.files[reviewHead]
	}
	g.mu.Unlock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	g.mu.Lock()
	g.filesFn = nil
	g.mu.Unlock()
	run := rig.runs(7)[0]
	roots := chat.roots()
	if run.Status != "queued" || len(roots) != 1 || !strings.HasSuffix(roots[0].text, "Reviewing `aaaaaaa` (General, Security)…") ||
		rig.pr(7).NotifyLast != "start:"+run.PublicID {
		rig.t.Fatalf("put back: run %s, roots %+v, last %q", run.Status, roots, rig.pr(7).NotifyLast)
	}
	if _, err := rig.st.db.ExecContext(context.Background(), `update review_runs set not_before=0 where org_id=? and id=?`, orgID, run.ID); err != nil {
		rig.t.Fatal(err)
	}
	return run
}

// A run put back after its start was announced — GitHub's answer moving under it while it read the
// pull request — and claimed again says nothing new as it starts again: notify_last remembers the
// start, so the message is posted once, and edited once, by the result.
func TestReviewNotifyARetriedRunAnnouncesItsStartOnce(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`}`)
	chat := withNotifyChat(rig)
	rig.confirmLock()
	rig.startedThenPutBack(chat)
	rig.drain()
	_, edits := chat.snapshot()
	if run := rig.runs(7)[0]; run.Status != "posted" || len(chat.roots()) != 1 || len(edits) != 1 || len(chat.threadPosts()) != 1 ||
		strings.Contains(edits[0].text, "Reviewing") {
		t.Errorf("claimed again: run %s, roots %d, edits %+v, replies %d", run.Status, len(chat.roots()), edits, len(chat.threadPosts()))
	}
}

// Which events a channel hears of is review's noise, not its reach: an editor sets it, only to the
// four events and each once; a repository inherits it whole or sets its own, an empty set telling its
// channel nothing; and the channel itself stays connections.manage's.
func TestReviewAPINotifyOnIsAnEditorsToSet(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	withChannel(t, rig.st, orgID, "T1", "C1")
	conn := "/api/review-settings/" + rig.connID()
	only := func(v any) map[string]any {
		return map[string]any{"fields": []string{"notify_on"}, "settings": map[string]any{"notify_on": v}}
	}
	effective := func() (string, any) {
		eff := rig.must(200, "GET", rig.repoPath(), rig.viewer, nil)["effective"].(map[string]any)
		return fmt.Sprint(eff["notify_on"]), eff["source"].(map[string]any)["notify_on"]
	}
	if got, from := effective(); got != "[started finished failed merged]" || from != "default" {
		t.Errorf("by default the channel hears %s from %v", got, from)
	}
	rig.must(200, "PUT", conn, rig.editor, only([]string{"finished", "failed"}))
	if got, from := effective(); got != "[finished failed]" || from != "connection" {
		t.Errorf("the repository hears %s from %v, want the connection's", got, from)
	}
	rig.must(200, "PUT", rig.repoPath(), rig.editor, only([]string{}))
	if got, from := effective(); got != "[]" || from != "repo" {
		t.Errorf("an empty set at the repository: %s from %v", got, from)
	}
	for _, bad := range []any{[]string{"posted"}, []string{"failed", "failed"}, "failed"} {
		if code, out := rig.call("PUT", conn, rig.editor, only(bad)); code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "notify_on") {
			t.Errorf("notify_on %v = %d %v, want a 400 about it", bad, code, out)
		}
	}
	rig.must(200, "PUT", rig.repoPath(), rig.editor, map[string]any{"fields": []string{"notify_on"}, "settings": map[string]any{}})
	if got, from := effective(); got != "[finished failed]" || from != "connection" {
		t.Errorf("reset, the repository hears %s from %v", got, from)
	}
	if code, out := rig.call("PUT", conn, rig.editor, map[string]any{"fields": []string{"notify"},
		"settings": map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"}}}); code != 403 || !slices.Contains(fieldsOf(out), "notify") {
		t.Errorf("an editor naming the channel = %d %v", code, out)
	}
}

// A run put back after its start was announced can end where no lane tells its channel: cancelled in
// the queue as its pull request is closed, ended at its next claim before it is planned (the
// repository turned off while it waited), or retired by the sweep after its lane died on its last
// attempt. Each puts the message back all the same, drawn from the title and branches the pull
// request last stored — quietly where the ending is not news, and with a line and a reply where it is
// a failure the channel hears of — so it never says "Reviewing…" for ever.
func TestReviewNotifyAStartIsPutBackWhereverItsRunEnds(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		end    func(rig *laneRig, run *ReviewRun)
		status string
		line   string // the message's line and the reply; "" put back quietly
		last   string // notify_last after, the run's public id for %s
	}{
		{"closed", func(rig *laneRig, _ *ReviewRun) {
			rig.deliver("pull_request", prEvent("closed", 7, reviewHead, func(pr map[string]any) { pr["state"] = "closed" }))
		}, "cancelled", "", "end:%s"},
		{"off", func(rig *laneRig, _ *ReviewRun) {
			rig.setSettings(`{"mode":"off",` + notifyC1 + `}`)
			rig.drain()
		}, "skipped", "", "skip:off"},
		{"swept", func(rig *laneRig, run *ReviewRun) {
			// Claimed for its last attempt by a lane that then died.
			if _, err := rig.st.db.ExecContext(ctx, `update review_runs set status='running', attempts=?, lease_until=1
				where org_id=? and id=?`, reviewRunMaxAttempts, orgID, run.ID); err != nil {
				t.Fatal(err)
			}
			rig.b.sweepReviewLane(ctx)
		}, "failed", "Review of `aaaaaaa` failed: an internal error; the console has the details.", "failed:%s"},
	} {
		rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`}`)
		chat := withNotifyChat(rig)
		rig.confirmLock()
		run := rig.startedThenPutBack(chat)
		c.end(rig, run)
		run = rig.runs(7)[0]
		_, edits := chat.snapshot()
		replies := chat.threadPosts()
		if run.Status != c.status || len(chat.roots()) != 1 || len(edits) != 1 {
			t.Fatalf("%s: run %s %q, roots %d, edits %+v", c.name, run.Status, run.Error, len(chat.roots()), edits)
		}
		msg := edits[0].text
		if strings.Contains(msg, "Reviewing") || !strings.Contains(msg, "** Make Add faster\n`feature` → `main` · not reviewed") {
			t.Errorf("%s: the message put back:\n%s", c.name, msg)
		}
		if c.line != "" {
			if !strings.HasSuffix(msg, "\n"+c.line) || len(replies) != 1 || !strings.HasPrefix(replies[0].text, c.line) {
				t.Errorf("%s: the failure told as\n%s\nreplies %+v", c.name, msg, replies)
			}
		} else if len(replies) != 0 {
			t.Errorf("%s: a quiet put-back replied %+v", c.name, replies)
		}
		if want := strings.ReplaceAll(c.last, "%s", run.PublicID); rig.pr(7).NotifyLast != want {
			t.Errorf("%s: recorded %q, want %q", c.name, rig.pr(7).NotifyLast, want)
		}
	}
}

// A quiet put-back of a message somebody deleted posts nothing: with only starts announced, the
// review's result is not the channel's to hear, and a fresh message would tell it.
func TestReviewNotifyAQuietWordNeverPostsAGoneMessageAgain(t *testing.T) {
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live",`+notifyC1+`,"notify_on":["started"]}`)
	chat := withNotifyChat(rig)
	chat.onRoot = func() {
		chat.mu.Lock()
		chat.failEdit = errors.New("message_not_found")
		chat.mu.Unlock()
	}
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	posts, edits := chat.snapshot()
	if run := rig.runs(7)[0]; run.Status != "posted" || len(posts) != 1 || len(edits) != 0 {
		t.Errorf("run %s; the channel got posts %+v, edits %+v; want the start's message alone", run.Status, posts, edits)
	}
	if failed := rig.audited("review.notify_failed"); len(failed) != 0 {
		t.Errorf("a gone message put back quietly was audited as a failure: %+v", failed)
	}
}

// A run whose hold saw its start announced puts the message back when it ends even after another
// run's late record has moved notify_last off its start — but never over another run's start, which
// is true while that run works, and is that run's to put back.
func TestReviewNoticeWantedTrustsTheHoldsStart(t *testing.T) {
	eff := review.Effective{Notify: review.NotifyChannel{Team: "T1", Channel: "C1"}, NotifyOn: []review.NotifyEvent{review.NotifyStarted}}
	b := &ReviewRun{PublicID: "b"}
	for _, c := range []struct {
		last    string
		started bool
		want    bool
	}{
		{"start:b", false, true},
		{"run:a", true, true},
		{"merged", true, true},
		{"run:a", false, false},
		{"start:c", true, false},
	} {
		quiet, ok := reviewNoticeWanted(eff, &ReviewPR{NotifyLast: c.last}, reviewNotice{kind: reviewNoticeEnd, run: b, started: c.started})
		if !quiet || ok != c.want {
			t.Errorf("last %q, started %v: quiet %v, put back %v; want %v", c.last, c.started, quiet, ok, c.want)
		}
	}
}
