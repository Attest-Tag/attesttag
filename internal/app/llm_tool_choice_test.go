package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3"
)

// sentChat runs one Chat against a stub endpoint and hands back the request body as the provider
// saw it: which tools were offered, and what was asked of them.
func sentChat(t *testing.T, tools []openai.ChatCompletionToolUnionParam, forceTool string) (names []string, choice json.RawMessage, usage Usage) {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","usage":{"prompt_tokens":100,"completion_tokens":40,
			"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":30},"cost":0.5},
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "sk-test"})
	_, us, err := l.Chat(context.Background(), "m", []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}, tools, forceTool)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	for _, tl := range req.Tools {
		names = append(names, tl.Function.Name)
	}
	return names, req.ToolChoice, us
}

func twoTools() []openai.ChatCompletionToolUnionParam {
	return []openai.ChatCompletionToolUnionParam{
		Tool{Name: "search", Desc: "d"}.def(),
		Tool{Name: "read", Desc: "d"}.def(),
	}
}

// The landing round stops calling tools without changing which tools are on the wire. Tool
// definitions sit ahead of the system block in a cached prefix, so an empty array on the turn's
// largest call — the one carrying every tool result it collected — is a guaranteed full-price
// re-read of the whole thing.
func TestLandingKeepsTheToolsAndAsksForNone(t *testing.T) {
	defs := twoTools()
	if got := landingChoice(defs); got != toolChoiceNone {
		t.Fatalf("landingChoice = %q, want %q", got, toolChoiceNone)
	}
	names, choice, _ := sentChat(t, defs, landingChoice(defs))
	if len(names) != 2 || names[0] != "search" || names[1] != "read" {
		t.Errorf("the landing call changed the tool array: %v", names)
	}
	if string(choice) != `"none"` {
		t.Errorf("tool_choice = %s, want \"none\"", choice)
	}

	// With no tools there is nothing to choose between, and tool_choice without tools is an
	// error on some providers.
	if got := landingChoice(nil); got != "" {
		t.Errorf("landingChoice(nil) = %q, want empty", got)
	}
	if _, choice, _ := sentChat(t, nil, ""); choice != nil {
		t.Errorf("a call with no tools sent tool_choice=%s", choice)
	}

	// A named tool still forces that tool.
	_, choice, _ = sentChat(t, defs, "search")
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(choice, &named); err != nil || named.Function.Name != "search" {
		t.Errorf("a named tool_choice no longer names its tool: %s (%v)", choice, err)
	}
}

// Both halves of what a turn costs that the token counts alone do not show: the part of the
// prompt that was cheap because a provider had it cached, and the part of the answer that was
// spent reasoning and then thrown away unread.
func TestUsageCarriesCachedAndReasoningTokens(t *testing.T) {
	_, _, us := sentChat(t, nil, "")
	if us.In != 100 || us.Out != 40 {
		t.Errorf("in/out = %d/%d, want 100/40", us.In, us.Out)
	}
	if us.CachedIn != 80 {
		t.Errorf("cached_in = %d, want 80", us.CachedIn)
	}
	if us.Reasoning != 30 {
		t.Errorf("reasoning = %d, want 30 — REASONING costs output tokens on every round and this is the only place it is counted", us.Reasoning)
	}

	var total Usage
	total.add(us)
	total.add(us)
	if total.In != 200 || total.Out != 80 || total.CachedIn != 160 || total.Reasoning != 60 || total.CostUSD != 1 {
		t.Errorf("add lost a field across two rounds: %+v", total)
	}
}

// sentTail runs one Chat whose last message is a tool result and reports whether that result went
// out carrying a cache breakpoint.
func sentTail(t *testing.T, model string) (marked bool, text string) {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "sk-test"})
	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("find it"),
		openai.ToolMessage("a long result the next round would otherwise buy again", "call_1"),
	}
	if _, _, err := l.Chat(context.Background(), model, msgs, nil, ""); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "tool" {
		t.Fatalf("last message is %q, want the tool result", last.Role)
	}
	var parts []struct {
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(last.Content, &parts); err != nil {
		var flat string
		if json.Unmarshal(last.Content, &flat) != nil {
			t.Fatalf("tool content is neither a string nor parts: %s", last.Content)
		}
		return false, flat
	}
	if len(parts) != 1 {
		t.Fatalf("want one content part, got %d: %s", len(parts), last.Content)
	}
	return len(parts[0].CacheControl) > 0, parts[0].Text
}

// The second breakpoint goes on the last tool result, so a tool loop reads back what it has
// already sent instead of buying it again every round — but only for the providers that cache up
// to a marker. For the ones that find the longest prefix themselves it is a wire change that buys
// nothing, and a wire change on the deployment's own model is not free of risk.
func TestTailBreakpointOnlyForTheProvidersThatNeedOne(t *testing.T) {
	for _, m := range []string{"anthropic/claude-sonnet-4.5", "google/gemini-3-pro", "claude-opus-4-1", "Gemini-2.5-Flash"} {
		if !needsTailBreakpoint(m) {
			t.Errorf("%s caches only up to a marker and was not sent one", m)
		}
	}
	for _, m := range []string{"z-ai/glm-5.3-flash", "openai/gpt-5", "deepseek/deepseek-chat", ""} {
		if needsTailBreakpoint(m) {
			t.Errorf("%s finds its own longest prefix; a marker is a wire change for nothing", m)
		}
	}

	marked, text := sentTail(t, "anthropic/claude-sonnet-4.5")
	if !marked {
		t.Error("no breakpoint on the tail: every round re-reads the whole tool loop at full price")
	}
	if text != "a long result the next round would otherwise buy again" {
		t.Errorf("the tool result was changed on the way out: %q", text)
	}

	// And the model this deployment actually runs is sent exactly what it was sent before.
	if marked, _ := sentTail(t, "z-ai/glm-5.3-flash"); marked {
		t.Error("a provider that caches on its own was sent a breakpoint it ignores")
	}

	// Nothing to mark: no tool result at the end, and a lone system message is withCache's.
	only := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage("s")}
	if got := withTailCache(only); len(got) != 1 || got[0].OfSystem == nil {
		t.Error("withTailCache touched a prompt with nothing to mark")
	}
	userLast := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage("s"), openai.UserMessage("q")}
	if got := withTailCache(userLast); got[1].OfUser == nil || !got[1].OfUser.Content.OfString.Valid() {
		t.Error("withTailCache rewrote a user message; only a tool result is worth an entry")
	}
}
