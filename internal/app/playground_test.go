package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// toolNames is the set of tools a call would actually be offered.
func toolNames(t *testing.T, a *Agent, c *Call) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, d := range a.defsFor(context.Background(), c) {
		var probe struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		b, _ := json.Marshal(d)
		json.Unmarshal(b, &probe)
		out[probe.Function.Name] = true
	}
	return out
}

func previewAgent(t *testing.T) *Agent {
	t.Helper()
	st := testStore(t)
	// Somebody who can approve something, so request_access is offered to the ordinary turn a
	// preview is compared against. An organisation with no tier is offered it nowhere, and the
	// comparison below would then be checking nothing.
	role(t, st, "ops", 1, "example.com", "U0OPS00001")
	return NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
}

func previewCall(kind string) *Call {
	// HumanTurn, because the thing a preview is being compared against is a person typing in the
	// channel. Without it the two sides of the comparison differ for a second reason and the
	// personal tools would be missing from both.
	return &Call{TeamID: "T1", OrgID: 1, Channel: "C1", ThreadTS: "1", UserID: "U1",
		Text: "what is failing in production", Kind: kind, HumanTurn: true}
}

// A preview has no thread, so the tools that exist to put something into one are withheld —
// and so are the ones that would leave the channel changed by a test of it.
func TestPreviewWithholdsWhatNeedsAThread(t *testing.T) {
	a := previewAgent(t)
	normal := toolNames(t, a, previewCall("channel"))
	c := previewCall("channel")
	c.Preview = true
	preview := toolNames(t, a, c)

	// start_investigation hands the question to a background lane that reports back into the
	// thread, so it goes the same way as the rest of them. create_routine and delete_routine are
	// here for the other reason: a routine outlives the preview that made it and fires in the
	// real channel, and the page is opened on the channel-settings permission rather than the
	// routines one.
	// remember_personal is here for a third reason again: a preview's answer is returned over
	// HTTP to whoever opened the console, and it runs under their console id -- which carries no
	// workspace and can fall back to the bot's own. Nobody's private notes belong in it.
	// react is the mildest of them and here for the first reason: a preview writes nothing to
	// the channel, and a tick it left on a real message would be the one thing it did leave.
	withheld := []string{"create_artifact", "request_access", "remember", "forget",
		"start_investigation", "create_routine", "delete_routine",
		"remember_personal", "react"}
	for _, name := range withheld {
		if !normal[name] {
			t.Fatalf("%s is not offered to an ordinary turn either; this test is checking nothing", name)
		}
		if preview[name] {
			t.Errorf("%s reaches a thread nobody is reading, so a preview must not be offered it", name)
		}
	}
	// Everything else a channel has is still there: a preview that answers with a smaller tool
	// set than the channel is not a preview of that channel.
	for name := range normal {
		if slices.Contains(withheld, name) {
			continue
		}
		if !preview[name] {
			t.Errorf("a preview lost %s, which the channel would have had", name)
		}
	}
}

// Preview and Silent share "no thread" and nothing else. A quiet routine is deciding whether to
// speak and is handed the tools for saying so; a preview is answering normally, and given those
// same tools it would end its turn by announcing a decision nobody asked it for.
func TestPreviewIsNotAQuietRoutine(t *testing.T) {
	a := previewAgent(t)
	quiet := previewCall("routine")
	quiet.Silent = true
	preview := previewCall("channel")
	preview.Preview = true

	q, p := toolNames(t, a, quiet), toolNames(t, a, preview)
	for _, name := range []string{"report_now", "stay_quiet"} {
		if !q[name] {
			t.Errorf("a quiet routine still needs %s", name)
		}
		if p[name] {
			t.Errorf("a preview was offered %s; it has no quiet/speak decision to make", name)
		}
	}
	if !preview.offline() || !quiet.offline() {
		t.Error("both are offline: neither has a thread anyone will read")
	}
	if previewCall("channel").offline() {
		t.Error("an ordinary channel turn is not offline")
	}
}

// The one thing a preview does not do is change data, so the attempt has to survive somewhere:
// the model is told plainly that nothing was sent, and the console is handed the same words.
func TestPreviewHoldKeepsWhatWasNotSent(t *testing.T) {
	c := previewCall("channel")
	c.Preview = true
	out := c.previewHold("POST https://api.example.com/issues", previewInChannel(nil))

	if len(c.previewHeld) != 1 || c.previewHeld[0] != "POST https://api.example.com/issues" {
		t.Fatalf("the held write should be kept for the console, got %v", c.previewHeld)
	}
	if !strings.Contains(out, "NOT sent") {
		t.Errorf("the model has to be told it did not happen: %q", out)
	}
	if !strings.Contains(out, "press Confirm") {
		t.Errorf("it should say what the channel would do with it: %q", out)
	}
	// A write the channel would run without asking is held too, and said to be one.
	if got := previewInChannel(&Connection{Writes: "auto"}); !strings.Contains(got, "without asking") {
		t.Errorf("an automatic connection's write was described as %q", got)
	}
}

// The playground keeps its conversation in the turns table, because there is no Slack thread to
// read it back from. Order is the whole value of it: a replay out of order is a different chat.
func TestPlaygroundThreadReplayAndClear(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	thread := newPlaygroundThread()
	if !isPlaygroundThread(thread) {
		t.Fatalf("%q is not recognisable as a playground thread", thread)
	}
	if strings.Count(thread, ".") > 0 {
		t.Errorf("a playground key must not look like a Slack ts, got %q", thread)
	}

	st.EnsureSession(ctx, "T1", "C1", thread, "playground", "")
	st.AddTurn(ctx, "T1", "C1", thread, "user", "U1", "first", "", 0, 0)
	st.AddTurn(ctx, "T1", "C1", thread, "assistant", "UBOT", "answer", "", 0, 0)
	st.AddTurn(ctx, "T1", "C1", thread, "user", "U1", "second", "", 0, 0)

	got := st.ThreadTurns(ctx, "T1", "C1", thread)
	want := []string{"first", "answer", "second"}
	if len(got) != len(want) {
		t.Fatalf("replayed %d turns, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Content != w {
			t.Errorf("turn %d is %q, want %q", i, got[i].Content, w)
		}
	}
	if got[1].Role != "assistant" {
		t.Errorf("the bot's turn must come back as the assistant, got %q", got[1].Role)
	}
	// Another channel's thread is another conversation, even under the same key.
	if n := len(st.ThreadTurns(ctx, "T1", "C2", thread)); n != 0 {
		t.Errorf("a thread key is not enough on its own: got %d turns from the wrong channel", n)
	}

	st.ForgetThread(ctx, "T1", "C1", thread)
	if n := len(st.ThreadTurns(ctx, "T1", "C1", thread)); n != 0 {
		t.Errorf("clear left %d turns behind", n)
	}
}

// A run's tool calls are read back by watermark rather than by time: two runs a second apart in
// the same thread would otherwise show each other's work.
func TestToolCallsAfterWatermark(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	thread := newPlaygroundThread()
	st.LogToolCall(ctx, 1, "T1", "C1", thread, "before", "{}", "old", true, 1)
	mark := st.LastToolCallID(ctx, 1)
	st.LogToolCall(ctx, 1, "T1", "C1", thread, "during", "{}", "new", true, 2)
	st.LogToolCall(ctx, 1, "T1", "C1", "other", "elsewhere", "{}", "new", true, 3)
	st.LogToolCall(ctx, 2, "T1", "C1", thread, "other_org", "{}", "new", true, 4)

	got := st.ToolCallsAfter(ctx, 1, thread, mark)
	if len(got) != 1 || got[0].Name != "during" {
		names := []string{}
		for _, g := range got {
			names = append(names, g.Name)
		}
		t.Fatalf("expected just the call this run made, got %v", names)
	}
}

// The channel is looked up inside the organisation on the session. A channel id from somewhere
// else is not a channel this request can name, however real it is.
func TestPlaygroundRefusesAnotherOrgsChannel(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	st.UpsertChannelScope(ctx, 2, "T9", "C_THEIRS", "#theirs", false)
	b := &Bot{store: st}

	r := httptest.NewRequest("POST", "/api/playground", strings.NewReader(`{"channel":"C_THEIRS","text":"hello"}`))
	r = r.WithContext(context.WithValue(r.Context(), userKey, &AdminUser{ID: 1, OrgID: 1, UserID: "U1"}))
	w := httptest.NewRecorder()
	b.handlePlayground(w, r)

	if w.Code != 404 {
		t.Errorf("a channel in another organisation should be unfindable, got %d", w.Code)
	}
	if n := len(st.ThreadTurns(ctx, "T9", "C_THEIRS", "")); n != 0 {
		t.Errorf("the refused request still wrote %d turns", n)
	}
}

// A preview turn end to end: the model answers, the answer comes back on the Call, and nothing
// reaches Slack. The second question is the part worth proving — a playground thread has no Slack
// history to replay, so if its own turns are not read back it starts from nothing every time and
// the box is a series of one-line conversations rather than a conversation.
func TestPreviewTurnAnswersWithoutTouchingSlack(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	llm := &fakeLLM{answer: "Both extractors are healthy."}
	rec := &recordingSlack{}
	slackSrv := httptest.NewServer(rec)
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)

	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12}
	a := &Agent{cfg: cfg, store: st, tools: map[string]Tool{}, runs: map[int64]*runHandle{}, loc: time.UTC,
		slacks: testRegistry(sl), settings: newSettingsCache(st, cfg),
		llm: NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"})}

	thread := newPlaygroundThread()
	ask := func(text string) *Call {
		t.Helper()
		sess, err := st.EnsureSession(ctx, "T1", "C1", thread, "playground", "")
		if err != nil {
			t.Fatal(err)
		}
		st.AddTurn(ctx, "T1", "C1", thread, "user", "U1", text, "", 0, 0)
		c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: thread, UserID: "U1",
			Text: text, Kind: "channel", Session: sess, Preview: true,
			Streamer: sl.NewSilentStreamer("C1", thread, "U1")}
		if err := a.Run(ctx, c); err != nil {
			t.Fatalf("run %q: %v", text, err)
		}
		return c
	}

	first := ask("are the extractors up?")
	if first.FinalText != "Both extractors are healthy." {
		t.Errorf("the answer should come back on the call, got %q", first.FinalText)
	}
	if first.usage.In == 0 {
		t.Error("the console shows what the turn spent; nothing was recorded")
	}
	// And how many rounds it took, which for a straight answer is one — the page used to show
	// the channel's limit here, so every reply read "3 rounds" (or 50) whatever it had done.
	if first.roundsUsed != 1 {
		t.Errorf("a one-round answer reports %d rounds", first.roundsUsed)
	}

	llm.answer = "Yes, both of them."
	if second := ask("both of them?"); second.FinalText != "Yes, both of them." {
		t.Errorf("second answer %q", second.FinalText)
	}

	prompt := llm.prompt()
	for _, want := range []string{"are the extractors up?", "Both extractors are healthy.", "both of them?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the follow-up never saw %q — the conversation is not being replayed", want)
		}
	}
	if n := len(st.ThreadTurns(ctx, "T1", "C1", thread)); n != 4 {
		t.Errorf("the transcript has %d turns, want two questions and two answers", n)
	}
	// The whole promise of the page: whoever is in that channel saw none of this.
	if posts := rec.posts(); len(posts) != 0 {
		t.Errorf("a preview reached Slack: %v", posts)
	}
}

// The playground is a turn like any other, so it meets the limits a turn meets. It used to check
// only the account's monthly budget, which left the per-person rate limit and the cap on turns in
// flight to the Slack path alone — and the playground is the cheaper of the two to hold open.
func TestPlaygroundMeetsTheSameLimitsAsAChannel(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	st.UpsertChannelScope(ctx, 1, "T1", "C1", "#ops", false)
	if err := st.PutSetting(ctx, 1, "user_rate_limit", "1"); err != nil {
		t.Fatal(err)
	}
	// One turn already spent this hour, by the person about to ask for another.
	st.LogUsageBy(ctx, 1, "T1", "C1", "thread", "U1", "test", Usage{In: 10, Out: 10, CostUSD: 0.01})
	sl := &Chat{BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	cfg := Config{Timezone: "UTC"}
	b := &Bot{store: st, slacks: testRegistry(sl), settings: newSettingsCache(st, cfg)}
	b.agent = &Agent{cfg: cfg, store: st, settings: b.settings, slacks: b.slacks, loc: time.UTC}

	r := httptest.NewRequest("POST", "/api/playground", strings.NewReader(`{"channel":"C1","text":"hello"}`))
	r = r.WithContext(context.WithValue(r.Context(), userKey, &AdminUser{ID: 1, OrgID: 1, UserID: "U1"}))
	w := httptest.NewRecorder()
	b.handlePlayground(w, r)

	if w.Code != 429 {
		t.Fatalf("a rate-limited person should be refused, got %d: %s", w.Code, w.Body.String())
	}
	// And it is refused before anything is written: a turn that never ran is not in the transcript.
	for _, turn := range st.ThreadTurns(ctx, "T1", "C1", "") {
		if turn.Content == "hello" {
			t.Error("the refused request still recorded its question")
		}
	}
}
