package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// The gap this closes, stated as a test. A routine's prompt can ask for one reply per item —
// the one in production says exactly that, and says "do not DM anyone" in the same breath — and
// until post_to_thread existed the only tool in the box that sent anything was send_dm. The run
// did what the box allowed instead of what the prompt asked.
func TestAScheduledRunCanSpeakInItsOwnThread(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := budgetAgent(t, st)
	has := func(c *Call, name string) bool {
		c.tools = nil
		_, ok := a.toolsFor(ctx, c)[name]
		return ok
	}
	routine := func() *Call {
		return &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1789190133.393909", UserID: "UALICE", Kind: "routine"}
	}
	if !has(routine(), "post_to_thread") {
		t.Error("a scheduled run with a thread was not offered post_to_thread")
	}
	if !has(routine(), "send_dm") {
		t.Error("send_dm went missing; the two are for different destinations, not alternatives")
	}

	// A quiet run has posted no header yet and a preview posts none at all, so a reply from
	// either is addressed to a message id that was never a message — the same reason
	// create_artifact is taken away from both.
	for _, tc := range []struct {
		name string
		fix  func(*Call)
	}{
		{"a quiet run", func(c *Call) { c.Silent = true }},
		{"a console preview", func(c *Call) { c.Preview = true }},
	} {
		c := routine()
		tc.fix(c)
		if has(c, "post_to_thread") {
			t.Errorf("%s was offered post_to_thread, and has no thread to post in", tc.name)
		}
	}

	// And it stays a scheduled run's tool. In a conversation there is a person in the thread who
	// can say the thing themselves, and every definition is re-sent on every round of every turn.
	person := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1.1", UserID: "UALICE", Kind: "channel", HumanTurn: true}
	if has(person, "post_to_thread") {
		t.Error("a person typing in a channel was charged for post_to_thread")
	}
}

// What the tool does when it runs: the message lands in the run's own thread, and the run may
// not do it forever. The cap is the same shape as the DM cap and for the same reason — a prompt
// that loops is the mistake a schedule repeats every day without anyone asking it to.
func TestPostingToTheThreadLandsThereAndIsCapped(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	rec := newPlacingSlack()
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, TeamID: "T1", BotUserID: "UBOT", OrgID: 1}
	a := &Agent{store: st, tools: map[string]Tool{}, settings: newSettingsCache(st, Config{}), slacks: testRegistry(sl)}
	tool := a.routineThreadTool()

	c := &Call{TeamID: "T1", OrgID: orgID, SL: sl, Channel: "C1", ThreadTS: "1789190133.393909", UserID: "UALICE", Kind: "routine"}
	out, err := tool.Run(ctx, c, []byte(`{"text":"*New lead: Jordan Lee — Northwind*"}`))
	if err != nil {
		t.Fatalf("post_to_thread: %v", err)
	}
	if !strings.Contains(out, "Do not post it again") {
		t.Errorf("the result does not tell the model the message is sent: %q", out)
	}
	posts, _, _ := rec.snapshot()
	if len(posts) != 1 {
		t.Fatalf("want one message, got %d", len(posts))
	}
	if posts[0].threadTS != "1789190133.393909" {
		t.Errorf("the brief went to thread %q, not the run's own", posts[0].threadTS)
	}
	if !strings.Contains(posts[0].text, "Jordan Lee") {
		t.Errorf("the message lost its text: %q", posts[0].text)
	}

	c.postsSent = maxRoutinePosts
	if _, err := tool.Run(ctx, c, []byte(`{"text":"one more"}`)); err == nil {
		t.Errorf("the %dth message was allowed through the cap", maxRoutinePosts+1)
	}
	// An empty message is a bug in the prompt, not a message to post.
	if _, err := tool.Run(ctx, &Call{TeamID: "T1", SL: sl, Channel: "C1", ThreadTS: "1.1"}, []byte(`{"text":"   "}`)); err == nil {
		t.Error("an empty message was posted to the channel")
	}
	// And a run with no thread to post in says so rather than posting somewhere else.
	if _, err := tool.Run(ctx, &Call{TeamID: "T1", SL: sl, Channel: "C1"}, []byte(`{"text":"a brief"}`)); err == nil {
		t.Error("a run with no thread posted anyway")
	}
}

// A routine posts on a schedule, forever, so a broadcast in its text is not one mistake — it is
// one mistake a day until somebody turns the routine off. The prompt that carried <!channel>
// into a post is in this file's history; everything else about the message is left alone.
func TestBroadcastsAreDefusedAndNothingElseIs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"<!channel> new leads are in", "@channel new leads are in"},
		{"<!channel|@channel> new leads are in", "@channel new leads are in"},
		{"heads up <!here>", "heads up @here"},
		{"<!everyone> quarterly numbers", "@everyone quarterly numbers"},
		{"<@UALICE> owns this one", "<@UALICE> owns this one"},
		{"*Lead:* <https://app.hubspot.com/contacts/1|Fabrizio> — 25/100", "*Lead:* <https://app.hubspot.com/contacts/1|Fabrizio> — 25/100"},
	} {
		if got := defuseBroadcasts(tc.in); got != tc.want {
			t.Errorf("defuseBroadcasts(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
