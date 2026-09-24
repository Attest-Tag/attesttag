package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// textCallLLM writes its tool calls into the message content as <tool_call> markup instead of
// returning them in tool_calls -- what glm-5.3-flash does on a follow-up turn, where the thread
// replays as prose and no structured call is in front of it to copy the shape from. always keeps
// it up for every round; otherwise it writes the markup once and then answers properly.
type textCallLLM struct {
	mu     sync.Mutex
	calls  int
	always bool
	answer string
	markup string
}

func (f *textCallLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()

	content := f.answer
	if first || f.always {
		content = f.markup
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content}}},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
	})
}

func (f *textCallLLM) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// textCallFixture wires an agent to a model that writes its calls out as text, with one tool to
// call and a record of the arguments it was handed.
func textCallFixture(t *testing.T, llm *textCallLLM) (*Agent, *recordingSlack, *Chat, *[]string) {
	t.Helper()
	st := testStore(t)
	rec := &recordingSlack{}
	slackSrv := httptest.NewServer(rec)
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)

	ran := &[]string{}
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	peek := Tool{Name: "peek", Desc: "look at something", Params: schema(map[string]any{}),
		Run: func(_ context.Context, _ *Call, args json.RawMessage) (string, error) {
			*ran = append(*ran, string(args))
			return "the app VMs are billed as Compute Engine instances", nil
		}}
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12}
	a := &Agent{
		cfg: cfg, store: st, tools: map[string]Tool{"peek": peek}, order: []string{"peek"},
		runs: map[int64]*runHandle{}, loc: time.UTC, slacks: testRegistry(sl),
		settings: newSettingsCache(st, cfg),
		llm:      NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"}),
	}
	return a, rec, sl, ran
}

func runTextCallTurn(t *testing.T, a *Agent, sl *Chat) *Call {
	t.Helper()
	ctx := context.Background()
	sess, err := a.store.EnsureSession(ctx, "T1", "C1", "1700000000.000100", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: "1700000000.000100", UserID: "U1",
		Text: "what are app vms, do they incur cost?", Kind: "channel", Session: sess,
		Streamer: sl.NewStreamer("C1", "1700000000.000100", "U1")}
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	return c
}

// The bug this exists for: a call written as text was read as a finished answer, so the markup
// went to Slack verbatim and the turn ended on it, billed as a success. The call is real -- the
// same question asked again got a proper answer -- so it gets run.
func TestATextToolCallIsRunRatherThanPosted(t *testing.T) {
	llm := &textCallLLM{
		answer: "App VMs are Compute Engine instances, and yes, they are billed hourly.",
		markup: `Let me check what those instances actually are before answering. ` +
			`<tool_call>{"name":"peek","arguments":{"url":"https://logging.googleapis.com/v2/entries:list"}}</tool_call>`,
	}
	a, rec, sl, ran := textCallFixture(t, llm)
	c := runTextCallTurn(t, a, sl)

	if len(*ran) != 1 {
		t.Fatalf("tool ran %d times, want the one call the model wrote out", len(*ran))
	}
	if !strings.Contains((*ran)[0], "logging.googleapis.com") {
		t.Errorf("tool got %q, want the arguments from the markup", (*ran)[0])
	}
	if c.FinalText != llm.answer {
		t.Errorf("final text %q, want the answer the model gave once it had the result", c.FinalText)
	}
	for _, p := range rec.posts() {
		if strings.Contains(p, "tool_call") {
			t.Errorf("raw tool-call markup reached the thread: %s", p)
		}
	}
}

// Repairing every round would spend the budget arriving where the first repair already did, so
// the rescue happens once. What comes after it is stripped, and the prose around it stands.
func TestATextToolCallIsRepairedOnlyOnce(t *testing.T) {
	llm := &textCallLLM{
		always: true,
		markup: `Still checking. <tool_call>{"name":"peek","arguments":{"q":"vms"}}</tool_call>`,
	}
	a, rec, sl, ran := textCallFixture(t, llm)
	c := runTextCallTurn(t, a, sl)

	if len(*ran) != 1 {
		t.Errorf("tool ran %d times, want exactly one repair", len(*ran))
	}
	if n := llm.count(); n != 2 {
		t.Errorf("model called %d times, want 2: the markup, then the round after the repair", n)
	}
	if strings.Contains(c.FinalText, "tool_call") {
		t.Errorf("markup survived into the answer: %q", c.FinalText)
	}
	if c.FinalText != "Still checking." {
		t.Errorf("final text %q, want the prose the markup was wrapped in", c.FinalText)
	}
	for _, p := range rec.posts() {
		if strings.Contains(p, "tool_call") {
			t.Errorf("raw tool-call markup reached the thread: %s", p)
		}
	}
}

func TestTextToolCallParsing(t *testing.T) {
	cases := []struct {
		name, in, wantName, wantArgs string
		wantOK                       bool
	}{
		{"object arguments", `<tool_call>{"name":"peek","arguments":{"a":1}}</tool_call>`, "peek", `{"a":1}`, true},
		{"string arguments", `<tool_call>{"name":"peek","arguments":"{\"a\":1}"}</tool_call>`, "peek", `{"a":1}`, true},
		{"no closing tag", `thinking <tool_call>{"name":"peek","arguments":{}}`, "peek", `{}`, true},
		{"absent arguments", `<tool_call>{"name":"peek"}</tool_call>`, "peek", `{}`, true},
		{"key/value pairs", "<tool_call>peek\n<arg_key>days</arg_key>\n<arg_value>7</arg_value>\n</tool_call>", "peek", `{"days":7}`, true},
		{"key/value with no arguments", `<tool_call>peek</tool_call>`, "peek", `{}`, true},
		{"key/value with no closing tag", "<tool_call>peek<arg_key>q</arg_key><arg_value>why did it fail?</arg_value>", "peek", `{"q":"why did it fail?"}`, true},
		{"not json", `<tool_call>peek(a=1)</tool_call>`, "", "", false},
		{"no name", `<tool_call>{"arguments":{}}</tool_call>`, "", "", false},
		{"no markup at all", `a plain answer about app VMs`, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, args, ok := textToolCall(tc.in)
			if ok != tc.wantOK || name != tc.wantName || args != tc.wantArgs {
				t.Errorf("textToolCall(%q) = %q, %q, %v; want %q, %q, %v",
					tc.in, name, args, ok, tc.wantName, tc.wantArgs, tc.wantOK)
			}
		})
	}
}

// Whatever else is wrong with an answer, raw markup is not something to post into a channel.
func TestStripThinkingRemovesToolCallMarkup(t *testing.T) {
	got := stripThinking(`Checking. <tool_call>{"name":"peek","arguments":{}}</tool_call>`)
	if got != "Checking." {
		t.Errorf("stripThinking left %q", got)
	}
}

// The shape that got past all of this: GLM renders a call in its own chat template as the tool's
// name followed by <arg_key>/<arg_value> pairs, so that is what the model writes when it writes
// one as text. The JSON reader above could not parse it, so nothing was rescued, nothing was
// logged, and the markup went to the thread with the answer resuming mid-sentence after it.
func TestAKeyValueToolCallIsRunRatherThanPosted(t *testing.T) {
	llm := &textCallLLM{
		answer: "App VMs are Compute Engine instances, and yes, they are billed hourly.",
		markup: "Let me check the logs first. <tool_call>peek\n" +
			"<arg_key>filter</arg_key>\n<arg_value>\"extractor_api_error\" AND severity>=ERROR</arg_value>\n" +
			"<arg_key>limit</arg_key>\n<arg_value>20</arg_value>\n</tool_call>",
	}
	a, rec, sl, ran := textCallFixture(t, llm)
	c := runTextCallTurn(t, a, sl)

	if len(*ran) != 1 {
		t.Fatalf("tool ran %d times, want the one call the model wrote out", len(*ran))
	}
	// The filter keeps its own quotes, and a number stays a number: a tool that unmarshals
	// limit into an int gets nothing at all from a quoted one and falls back to its default.
	want := `{"filter":"\"extractor_api_error\" AND severity>=ERROR","limit":20}`
	if (*ran)[0] != want {
		t.Errorf("tool got %s, want %s", (*ran)[0], want)
	}
	if c.FinalText != llm.answer {
		t.Errorf("final text %q, want the answer the model gave once it had the result", c.FinalText)
	}
	for _, p := range rec.posts() {
		if strings.Contains(p, "arg_key") {
			t.Errorf("raw tool-call markup reached the thread: %s", p)
		}
	}
}
