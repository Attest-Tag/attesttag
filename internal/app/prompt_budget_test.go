package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

// The tool loop re-sends the whole prompt on every round, so what one call carries before the
// conversation starts is paid for again and again: a four-round run buys the system prompt and
// the tool list four times. These tests hold that base down, and hold the shape that lets a
// provider serve it from cache instead of re-reading it.

// promptBudget is the system prompt plus the JSON of every tool definition, in characters.
func promptBudget(t *testing.T, kind string, silent bool) (sysChars, toolChars int, names []string) {
	t.Helper()
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
	ctx := context.Background()
	// Seeded, because an empty store measures a prompt nobody has. Everything that grows with
	// use -- the channel's memories, the workspace's, and the asker's own private notes -- is
	// absent from a fresh database, so a block added to the prompt for any of them would not
	// move this number and the budget below would pass while the real one grew.
	st.AddMemory(ctx, 1, "T1", channelMemoryScope("T1", "C1"), "the staging cluster is rebuilt every Friday", "U2")
	st.AddMemory(ctx, 1, "T1", teamMemoryScope("T1"), "standup moved to 10:45", "U2")
	st.AddPersonalMemory(ctx, personalKey{orgID: 1, teamID: "T1", owner: "U1"}, "I owe Priya the Q3 numbers")
	// And a tier that could approve something: request_access is withheld from an organisation
	// with nobody to ask (see TestAccessToolIsWithheldWithNobodyToAsk), so an unseeded store
	// would measure a channel turn that is missing a tool most of them carry.
	// HumanTurn is what a channel turn is and a routine is not, and it is what decides whether
	// the three personal tools are carried -- so the two measurements differ here the way the
	// two lanes really differ.
	role(t, st, "ops", 1, "example.com", "U0OPS00001")
	c := &Call{TeamID: "T1", OrgID: 1, Channel: "C1", ThreadTS: "1", UserID: "U1",
		Text: "count the VMs reporting uptime", Kind: kind, Silent: silent, HumanTurn: kind != "routine"}
	defs := a.defsFor(ctx, c)
	b, err := json.Marshal(defs)
	if err != nil {
		t.Fatalf("marshal tool defs: %v", err)
	}
	for _, d := range defs {
		var probe struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		db, _ := json.Marshal(d)
		json.Unmarshal(db, &probe)
		names = append(names, probe.Function.Name)
	}
	return len(a.systemPrompt(ctx, c)), len(b), names
}

// A scheduled run carries less than a conversation does, because some of what a conversation
// needs it cannot use: it has nobody to ask a question of, and no business editing schedules.
func TestRoutinePromptIsSmallerThanAChannelTurn(t *testing.T) {
	chatSys, chatTools, chatNames := promptBudget(t, "channel", false)
	runSys, runTools, runNames := promptBudget(t, "routine", true)
	t.Logf("channel  system %d chars, tools %d chars in %d defs (~%d tok)", chatSys, chatTools, len(chatNames), (chatSys+chatTools)/4)
	t.Logf("routine  system %d chars, tools %d chars in %d defs (~%d tok)", runSys, runTools, len(runNames), (runSys+runTools)/4)

	if runSys >= chatSys || runTools >= chatTools {
		t.Errorf("a quiet routine should carry less than a channel turn, got system %d vs %d and tools %d vs %d",
			runSys, chatSys, runTools, chatTools)
	}
	// The budget is per round, and a run spends three or four of them. Re-baseline deliberately
	// if a new tool is worth its place in every scheduled run, several times an hour, forever.
	if base := (runSys + runTools) / 4; base > 3000 {
		t.Errorf("quiet routine base is ~%d tokens, over the 3000 budget", base)
	}
}

// Tools the model can see but not call are worse than absent: it spends a round finding out,
// and the whole prompt is re-sent to do it. defsFor must advertise only what runTool will run.
func TestOnlyRunnableToolsAreAdvertised(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		silent     bool
		gone       []string
		kept       []string
	}{
		{"quiet routine", "routine", true,
			// The three personal tools are here for a different reason than the rest: not that a
			// routine cannot run them, but that it runs as somebody who is not present, into a
			// channel other people read. The fixture's routine is the creator's own id, so this
			// is the case that looks exactly like a person and is not one.
			[]string{"create_artifact", "request_access", "create_routine", "list_routines", "delete_routine",
				"remember_personal", "recall_personal", "forget_personal"},
			[]string{"report_now", "stay_quiet", "run_js"}},
		{"channel turn", "channel", false,
			nil,
			[]string{"create_artifact", "request_access", "create_routine", "run_js",
				"remember_personal", "recall_personal", "forget_personal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, names := promptBudget(t, tc.kind, tc.silent)
			has := map[string]bool{}
			for _, n := range names {
				has[n] = true
			}
			for _, n := range tc.gone {
				if has[n] {
					t.Errorf("%s is advertised to a %s but cannot be run there", n, tc.name)
				}
			}
			for _, n := range tc.kept {
				if !has[n] {
					t.Errorf("%s is missing from a %s", n, tc.name)
				}
			}
		})
	}
}

// requestAccess refuses outright when no tier has both members and reach -- "nobody is set up to
// approve access requests" -- so in that organisation the definition is ~270 tokens a round buying
// one sentence, and the round that discovers it re-sends the whole prompt.
func TestAccessToolIsWithheldWithNobodyToAsk(t *testing.T) {
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
	ctx := context.Background()
	c := &Call{TeamID: "T1", OrgID: 1, Channel: "C1", ThreadTS: "1", UserID: "U1", Kind: "channel", HumanTurn: true}
	if _, ok := a.ensureTools(ctx, c)["request_access"]; ok {
		t.Error("request_access is offered where nothing can approve it")
	}
	// And it comes back the moment there is somebody to ask.
	role(t, st, "ops", 1, "example.com", "U0OPS00001")
	c2 := &Call{TeamID: "T1", OrgID: 1, Channel: "C1", ThreadTS: "1", UserID: "U1", Kind: "channel", HumanTurn: true}
	if _, ok := a.ensureTools(ctx, c2)["request_access"]; !ok {
		t.Error("request_access is missing from an organisation that has an approver")
	}
}

// Prompt caching only pays if the prefix arrives unchanged, so nothing that moves may sit in
// the cacheable half. The clock is the one thing that changes on every single call.
func TestClockIsOutsideTheCacheablePrompt(t *testing.T) {
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
	c := &Call{TeamID: "T1", OrgID: 1, Channel: "C1", ThreadTS: "1", UserID: "U1", Text: "x", Kind: "channel"}
	ctx := context.Background()

	if got := a.systemPrompt(ctx, c); strings.Contains(got, "Current time") {
		t.Error("the clock is back in systemPrompt; it moves every call and nothing after it can be cached")
	}
	if got := a.clockLine(ctx, c); !strings.Contains(got, "Current time") {
		t.Errorf("clockLine should carry the clock, got %q", got)
	}

	// The breakpoint belongs on the stable part, and only on it.
	b, err := json.Marshal(CachedSystemMessage("stable", "volatile"))
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Content []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatalf("system message is not the two-part form: %v (%s)", err, b)
	}
	if len(msg.Content) != 2 || msg.Content[0].Text != "stable" || msg.Content[1].Text != "volatile" {
		t.Fatalf("want stable then volatile, got %s", b)
	}
	if string(msg.Content[0].CacheControl) != `{"type":"ephemeral"}` {
		t.Errorf("no breakpoint on the stable part: %s", b)
	}
	if msg.Content[1].CacheControl != nil {
		t.Errorf("breakpoint on the volatile part would cache something that always differs: %s", b)
	}
}

// Every call made through this package gets a breakpoint, not only the turn loop that builds
// its own conversation: watchVerdict runs on every message in a watched channel and repeats a
// system prompt that never changes.
func TestWithCacheCoversPlainPrompts(t *testing.T) {
	long := strings.Repeat("channel instructions. ", 300) // > minCacheChars
	marked := withCache([]openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(long), openai.UserMessage("a message"),
	})
	b, _ := json.Marshal(marked[0])
	if !strings.Contains(string(b), `"cache_control":{"type":"ephemeral"}`) {
		t.Error("a long plain system prompt was sent without a breakpoint")
	}
	if len(marked) != 2 {
		t.Errorf("withCache changed the message count: %d", len(marked))
	}

	// Too small to cache: providers have a minimum prefix and charge to write the entry.
	small, _ := json.Marshal(withCache([]openai.ChatCompletionMessageParamUnion{openai.SystemMessage("tiny")})[0])
	if strings.Contains(string(small), "cache_control") {
		t.Errorf("a short prompt should not be marked: %s", small)
	}

	// And a prompt the caller already built the two-part way is left exactly as it is.
	built := CachedSystemMessage("stable", "volatile")
	want, _ := json.Marshal(built)
	got, _ := json.Marshal(withCache([]openai.ChatCompletionMessageParamUnion{built})[0])
	if string(got) != string(want) {
		t.Errorf("withCache rewrote an already-marked prompt:\n got %s\nwant %s", got, want)
	}
}

// The final answer used to be generated twice: once to decide it, and again -- the whole
// conversation re-sent -- so the words would appear rather than land. Replay shows the text that
// was already paid for, so what the channel reads is what the turn decided and the second
// generation is gone.
func TestReplayShowsTheAnswerWithoutBuyingItTwice(t *testing.T) {
	ctx := context.Background()
	st := &Streamer{failed: true} // collects, never posts
	const answer = "The staging cluster is rebuilt every Friday, so Thursday's numbers are the last full day."
	st.Replay(ctx, answer)
	if got := streamTail(st, answer); got != "" {
		t.Errorf("replay left %q for Stop to post; it should have written all of it", got)
	}
	st.mu.Lock()
	written := st.fallbackMD.String()
	st.mu.Unlock()
	if written != answer {
		t.Errorf("replayed text differs from the answer:\n got %q\nwant %q", written, answer)
	}
	// And the pieces a live streamer writes join back into exactly the answer -- including when
	// the text is shorter than the chunk count, and when a character is more than one byte.
	for _, in := range []string{answer, "ok", "", "résumé — 日本語のテキスト, and then some more"} {
		if got := strings.Join(replayChunks(in, 5), ""); got != in {
			t.Errorf("chunks do not rebuild the text:\n got %q\nwant %q", got, in)
		}
		if n := len(replayChunks(in, 5)); n > 5 {
			t.Errorf("replayChunks(%q) made %d pieces, more than the 5 it was asked for", in, n)
		}
	}
}

// What a tool result says when it does not fit. The fact of the cut was always there; the remedy
// was not, and a model that is only told a number counts what it can see and answers short.
func TestATruncatedToolResultSaysWhatToDo(t *testing.T) {
	short := "42 rows"
	if got := truncateToolOutput(short); got != short {
		t.Errorf("a result under the cap should be untouched, got %q", got)
	}
	long := truncateToolOutput(strings.Repeat("x", toolOutputCap+5000))
	if !strings.HasPrefix(long, strings.Repeat("x", toolOutputCap)) {
		t.Error("the kept part should be the first toolOutputCap characters")
	}
	for _, want := range []string{"5000 more characters", "incomplete", "run_js"} {
		if !strings.Contains(long, want) {
			t.Errorf("the cut-off note does not mention %q: %s", want, long[toolOutputCap:])
		}
	}
	// It costs nothing on the results that fit, which is most of them.
	if n := len(long) - (toolOutputCap + 5000); n > 0 {
		t.Logf("note adds %d chars, and only to a result that was truncated anyway", len(long)-toolOutputCap)
	}
}
