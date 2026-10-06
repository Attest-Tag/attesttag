package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

// An endpoint that refuses a tool_choice gets the same request without the field, and what it
// refused is remembered so the next call of that kind goes straight there instead of paying for
// the refusal again. "none" and a named tool are remembered apart: an endpoint that refuses only
// a named tool while reasoning still gets "none", which keeps the landing round's prefix cached.
func TestChatRetriesWithoutRefusedToolChoice(t *testing.T) {
	const metaRefusal = `{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"only \"auto\" is supported for tool_choice","provider_name":"Meta"}}}`
	for _, tc := range []struct {
		name   string
		refuse func(choice string) (status int, body string) // status 0 = answer
		forces []string
		want   []string // tool_choice of every request the endpoint saw, "" = absent
	}{{
		name: "only auto",
		refuse: func(choice string) (int, string) {
			if choice != "" {
				return http.StatusBadRequest, metaRefusal
			}
			return 0, ""
		},
		forces: []string{toolChoiceNone, toolChoiceNone, "search", "search"},
		want:   []string{`"none"`, "", "", "named", "", ""},
	}, {
		name: "named refused, none taken",
		refuse: func(choice string) (int, string) {
			if choice == "named" {
				return http.StatusBadRequest, `{"error":{"message":"Thinking may not be enabled when toolChoice forces tool use."}}`
			}
			return 0, ""
		},
		forces: []string{"search", "search", toolChoiceNone},
		want:   []string{"named", "", "", `"none"`},
	}, {
		name: "routing found no endpoint for the choice",
		refuse: func(choice string) (int, string) {
			if choice != "" {
				return http.StatusNotFound, `{"error":{"message":"No endpoints found for m. Every candidate endpoint was removed during routing: Filter by Tool Compatibility removed a, b","code":404}}`
			}
			return 0, ""
		},
		forces: []string{toolChoiceNone, toolChoiceNone},
		want:   []string{`"none"`, "", ""},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ToolChoice json.RawMessage `json:"tool_choice"`
				}
				body, _ := io.ReadAll(r.Body)
				json.Unmarshal(body, &req)
				choice := string(req.ToolChoice)
				if strings.HasPrefix(choice, "{") {
					choice = "named"
				}
				seen = append(seen, choice)
				w.Header().Set("Content-Type", "application/json")
				if status, body := tc.refuse(choice); status != 0 {
					w.WriteHeader(status)
					w.Write([]byte(body))
					return
				}
				w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer srv.Close()

			l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "sk-test"})
			msgs := []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}
			for _, force := range tc.forces {
				resp, _, err := l.Chat(context.Background(), "m", msgs, twoTools(), force)
				if err != nil {
					t.Fatalf("force %q: %v", force, err)
				}
				if resp.Choices[0].Message.Content != "ok" {
					t.Fatalf("force %q: content %q", force, resp.Choices[0].Message.Content)
				}
			}
			if !reflect.DeepEqual(seen, tc.want) {
				t.Fatalf("tool_choice per request = %q, want %q", seen, tc.want)
			}
		})
	}
}

// A refusal the retry does not cure is the endpoint's error, not a fact about tool_choice: it is
// returned, and nothing is remembered, so the next call still asks for what it wants.
func TestChatToolChoiceRefusalNotRememberedWhenRetryFails(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		seen = append(seen, string(req.ToolChoice))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"tool_choice and everything else is wrong today"}}`))
	}))
	defer srv.Close()

	l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "sk-test"})
	msgs := []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}
	for range 2 {
		if _, _, err := l.Chat(context.Background(), "m", msgs, twoTools(), toolChoiceNone); err == nil {
			t.Fatal("a 400 the retry did not cure came back as success")
		}
	}
	if want := []string{`"none"`, "", `"none"`, ""}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("tool_choice per request = %q, want %q", seen, want)
	}
}

// Only a refusal of the choice is retried without it: a 400 about anything else, or a 404 that is
// not routing's tool filter, fails as it came.
func TestToolChoiceRefused(t *testing.T) {
	named := openai.ChatCompletionNewParams{ToolChoice: openai.ChatCompletionToolChoiceOptionUnionParam{
		OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: "x"}},
	}}
	// Built the way the SDK builds one from a response, so Error() carries the body.
	apiErr := func(status int, msg string) error {
		e := &openai.Error{}
		body, _ := json.Marshal(map[string]string{"message": msg})
		if err := e.UnmarshalJSON(body); err != nil {
			t.Fatal(err)
		}
		e.StatusCode = status
		e.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		e.Response = &http.Response{StatusCode: status}
		return e
	}
	for _, tc := range []struct {
		p    openai.ChatCompletionNewParams
		err  error
		want bool
	}{
		{named, apiErr(400, `only "auto" is supported for tool_choice`), true},
		{named, apiErr(400, "Thinking may not be enabled when toolChoice forces tool use"), true},
		{named, apiErr(400, "The tool choice must be auto"), true},
		{named, apiErr(404, "Every candidate endpoint was removed during routing: Filter by Tool Compatibility removed a"), true},
		{named, apiErr(400, "context length exceeded"), false},
		{named, apiErr(404, "No endpoints found matching your data policy"), false},
		{named, apiErr(429, "tool_choice rate limited"), false},
		{named, errors.New("tool_choice: connection reset"), false},
		{openai.ChatCompletionNewParams{}, apiErr(400, `only "auto" is supported for tool_choice`), false},
	} {
		if got := toolChoiceRefused(tc.p, tc.err); got != tc.want {
			t.Errorf("toolChoiceRefused(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A failure OpenRouter reports inside a 200 — {"error": …} and no choices, which is how an
// upstream that timed out after the status line was sent arrives — is an error, not an empty
// answer: a transient one is tried once more, as the SDK retries the same failure sent as a 504,
// and anything else comes back with what the provider said.
func TestChatErrorInsideA200(t *testing.T) {
	answer := `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	for _, tc := range []struct {
		name    string
		bodies  []string // one per request, the last repeated
		force   string
		want    string // content, or the error's text
		wantErr bool
		calls   int
	}{{
		name:   "transient, then answered",
		bodies: []string{`{"error":{"message":"Upstream idle timeout exceeded","code":504,"metadata":{"error_type":"timeout"}}}`, answer},
		want:   "ok", calls: 2,
	}, {
		name:   "transient twice",
		bodies: []string{`{"error":{"message":"Upstream idle timeout exceeded","code":504}}`},
		want:   "Upstream idle timeout exceeded", wantErr: true, calls: 2,
	}, {
		name:   "not transient",
		bodies: []string{`{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"context length exceeded"}}}`},
		want:   "context length exceeded", wantErr: true, calls: 1,
	}, {
		name:   "a refused tool_choice inside a 200",
		bodies: []string{`{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"only \"auto\" is supported for tool_choice"}}}`, answer},
		force:  toolChoiceNone,
		want:   "ok", calls: 2,
	}, {
		name:   "a string code",
		bodies: []string{`{"error":{"message":"upstream broke","code":"server_error"}}`, answer},
		want:   "ok", calls: 2,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := tc.bodies[min(calls, len(tc.bodies)-1)]
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(body))
			}))
			defer srv.Close()

			l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "sk-test"})
			resp, _, err := l.Chat(context.Background(), "m", []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}, twoTools(), tc.force)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("answered %q, want an error", resp.Choices[0].Message.Content)
			case tc.wantErr && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not say %q", err, tc.want)
			case !tc.wantErr && err != nil:
				t.Fatal(err)
			case !tc.wantErr && resp.Choices[0].Message.Content != tc.want:
				t.Fatalf("content %q, want %q", resp.Choices[0].Message.Content, tc.want)
			}
			if calls != tc.calls {
				t.Fatalf("%d requests, want %d", calls, tc.calls)
			}
		})
	}
}
