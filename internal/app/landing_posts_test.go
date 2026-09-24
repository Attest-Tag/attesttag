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

	"github.com/openai/openai-go/v3"
	"github.com/slack-go/slack"
)

// postingLLM is Routine #1 on 2026-09-23: it digs for as long as it may, answers the landing
// round with a call anyway (a provider that ignores tool_choice "none"), and, asked again with
// the tools gone, writes that same call out as markup. Everything it ever writes is a tool call.
type postingLLM struct {
	mu        sync.Mutex
	calls     int
	landCall  [2]string // name and arguments of the call it makes when asked for none
	retry     string    // its whole content once the tools are gone
	retryLast string    // the last message of that request, as the model received it
}

func (f *postingLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Messages   []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls++
	msg := map[string]any{"role": "assistant", "content": ""}
	call := func(name, args string) {
		msg["tool_calls"] = []map[string]any{{"id": "call_1", "type": "function",
			"function": map[string]any{"name": name, "arguments": args}}}
	}
	switch {
	case len(body.Tools) == 0:
		msg["content"] = f.retry
		if n := len(body.Messages); n > 0 {
			var s string
			json.Unmarshal(body.Messages[n-1].Content, &s)
			f.retryLast = s
		}
	case string(body.ToolChoice) == `"none"`:
		call(f.landCall[0], f.landCall[1])
	default:
		call("peek", `{}`)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": msg}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
	})
}

func postingFixture(t *testing.T, llm *postingLLM) (*Agent, *Chat, *Store) {
	t.Helper()
	st := testStore(t)
	slackSrv := httptest.NewServer(&recordingSlack{})
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	peek := Tool{Name: "peek", Desc: "look at something", Params: schema(map[string]any{}),
		Run: func(context.Context, *Call, json.RawMessage) (string, error) { return "nothing conclusive", nil }}
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12}
	a := &Agent{
		cfg: cfg, store: st, tools: map[string]Tool{"peek": peek}, order: []string{"peek"},
		runs: map[int64]*runHandle{}, loc: time.UTC, slacks: testRegistry(sl),
		settings: newSettingsCache(st, cfg),
		llm:      NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"}),
	}
	return a, sl, st
}

func runPostingTurn(t *testing.T, a *Agent, sl *Chat, st *Store, ts string) *Call {
	t.Helper()
	ctx := context.Background()
	sess, err := st.EnsureSession(ctx, "T1", "C1", ts, "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: ts, UserID: "U1",
		Text: "process today's new leads", Kind: "channel", Session: sess, Streamer: sl.NewStreamer("C1", ts, "U1")}
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	return c
}

// The run had done its work and was posting the first brief when its budget ran out. The brief
// is the part the channel was waiting for, so it survives the call it was written into.
func TestALandingKeepsThePostItWasAboutToMake(t *testing.T) {
	brief := "*Tom Perry* — LA eCycle. Manual triage: a generic info@ inbox, nobody named behind it."
	llm := &postingLLM{
		landCall: [2]string{"post_to_thread", `{"text":` + strconvQuote(brief) + `}`},
		retry:    "<tool_call>post_to_thread\n<arg_key>text</arg_key>\n<arg_value>" + brief + "</arg_value>\n</tool_call>",
	}
	a, sl, st := postingFixture(t, llm)
	c := runPostingTurn(t, a, sl, st, "1700000000.000901")

	if !strings.Contains(c.FinalText, brief) {
		t.Errorf("final text %q lost the brief", c.FinalText)
	}
	if !strings.HasPrefix(c.FinalText, "_I ran out of tool rounds before I could write up what I found.") {
		t.Errorf("final text %q does not say why the run stopped", c.FinalText)
	}
	if strings.Contains(c.FinalText, "Please ask again") {
		t.Errorf("final text %q is the apology", c.FinalText)
	}
	if strings.Count(c.FinalText, brief) != 1 {
		t.Errorf("the brief is in the answer %d times, want once: the call and its markup copy are one post", strings.Count(c.FinalText, brief))
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if !strings.Contains(llm.retryLast, "tools are gone") {
		t.Errorf("the repeat without tools ended on %q, want the note that no call can happen", llm.retryLast)
	}
	if llm.calls != 4 {
		t.Errorf("model called %d times, want 4: two rounds, the landing and one repeat", llm.calls)
	}
}

// Nothing to keep — the dropped call was not a post and the repeat wrote only thinking — and
// the reply still says what happened instead of asking somebody to ask again.
func TestALandingThatWritesNothingSaysWhyItStopped(t *testing.T) {
	llm := &postingLLM{landCall: [2]string{"peek", `{}`}, retry: "<think>one more look would settle it</think>"}
	a, sl, st := postingFixture(t, llm)
	c := runPostingTurn(t, a, sl, st, "1700000000.000902")

	want := "_I ran out of tool rounds before I could write up what I found. I made 2 tool calls on the way (peek ×2); each one is in the console's activity log._"
	if c.FinalText != want {
		t.Errorf("final text\n%q\nwant\n%q", c.FinalText, want)
	}
}

func TestLandedEmptyNamesTheBusiestToolsFirst(t *testing.T) {
	if got, want := landedEmpty("I hit my budget", nil), "_I hit my budget before I could write up an answer._"; got != want {
		t.Errorf("no calls: %q, want %q", got, want)
	}
	got := landedEmpty("I hit my budget", map[string]int{"web_search": 8, "http_request": 30, "post_to_thread": 1,
		"a": 1, "b": 1, "c": 1})
	if !strings.Contains(got, "I made 42 tool calls on the way (http_request ×30, web_search ×8, a ×1, b ×1, c ×1, …)") {
		t.Errorf("got %q", got)
	}
}

func TestLandingPostsKeepsOnlyWhatWasMeantForTheChannel(t *testing.T) {
	calls := []openai.ChatCompletionMessageToolCallUnion{
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "send_dm", Arguments: `{"user":"U1","text":"for you only"}`}},
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "post_to_thread", Arguments: `{"text":"<!channel> lead one"}`}},
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "post_to_thread", Arguments: `{"text":"lead two"}`}},
	}
	got := landingPosts(calls, `<tool_call>{"name":"post_to_thread","arguments":{"text":"lead two"}}</tool_call>`)
	if got != "@channel lead one\n\nlead two" {
		t.Errorf("got %q", got)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
