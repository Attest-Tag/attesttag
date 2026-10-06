package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3"
)

// decodeMessage reads a completion's message the way the SDK reads a response.
func decodeMessage(t *testing.T, raw string) openai.ChatCompletionMessage {
	t.Helper()
	var resp openai.ChatCompletion
	if err := json.Unmarshal([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":`+raw+`}]}`), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Choices[0].Message
}

func marshalled(t *testing.T, p openai.ChatCompletionMessageParamUnion) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func keysOfRaw(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A tool round goes back with what the endpoint handed out alongside the call: OpenRouter's
// reasoning_details, DeepSeek's reasoning_content, and the extra_content Google's own endpoint
// puts on each call — exactly as written. Anything else it added (DeepSeek's index on a call) and
// any of them that came back null stay behind.
func TestAssistantTurnSendsBackWhatTheEndpointHandedOut(t *testing.T) {
	details := `[{"type":"reasoning.encrypted","format":"google-gemini-v1","data":"c2lnbmF0dXJl","id":"call_1","index":0}]`
	extra := `{"google":{"thought_signature":"EvACCu0CAWkU"}}`
	msg := decodeMessage(t, `{"role":"assistant","content":"","reasoning_content":"look her city up first",
		"reasoning_details":`+details+`,
		"tool_calls":[{"id":"call_1","type":"function","index":0,"extra_content":`+extra+`,
			"function":{"name":"get_home_city","arguments":"{\"person\":\"Priya\"}"}}]}`)

	got := marshalled(t, assistantTurn(msg))
	if string(got["reasoning_details"]) != details {
		t.Errorf("reasoning_details = %s, want it as the endpoint wrote it", got["reasoning_details"])
	}
	if string(got["reasoning_content"]) != `"look her city up first"` {
		t.Errorf("reasoning_content = %s", got["reasoning_content"])
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(got["tool_calls"], &calls); err != nil || len(calls) != 1 {
		t.Fatalf("tool_calls = %s (%v)", got["tool_calls"], err)
	}
	if string(calls[0]["extra_content"]) != extra {
		t.Errorf("the call's extra_content = %s, want the thought signature as issued", calls[0]["extra_content"])
	}
	if _, ok := calls[0]["index"]; ok {
		t.Errorf("a call's index went back: %s", got["tool_calls"])
	}

	// What OpenAI's own API writes gets nothing added: its strict API refuses unknown fields.
	plain := marshalled(t, assistantTurn(decodeMessage(t, `{"role":"assistant","content":null,"refusal":null,"reasoning_content":null,
		"tool_calls":[{"id":"call_1","type":"function","function":{"name":"peek","arguments":"{}"}}]}`)))
	if keys := strings.Join(keysOfRaw(plain), ","); keys != "role,tool_calls" {
		t.Errorf("plain assistant turn sent %s, want role,tool_calls", keys)
	}
}

// geminiLike is a model endpoint that holds a tool round to what Google's own endpoint does: the
// round after a call is refused unless that call comes back carrying the thought signature it was
// issued with.
type geminiLike struct {
	mu       sync.Mutex
	issued   string
	requests int
	refused  int
}

func (g *geminiLike) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ExtraContent json.RawMessage `json:"extra_content"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
	w.Header().Set("Content-Type", "application/json")
	answered := false
	for _, m := range body.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if string(tc.ExtraContent) != g.issued {
				g.refused++
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`[{"error":{"code":400,"message":"Function call is missing a thought_signature in functionCall parts.","status":"INVALID_ARGUMENT"}}]`))
				return
			}
			answered = true
		}
	}
	msg := map[string]any{"role": "assistant", "content": "Pokhara is clear, 21.4°C."}
	if !answered {
		g.issued = `{"google":{"thought_signature":"EsADCr0D"}}`
		msg = map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function", "extra_content": json.RawMessage(g.issued),
			"function": map[string]any{"name": "peek", "arguments": `{}`},
		}}}
	}
	json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": "gemini-3.8-flash",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": msg}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10}})
}

// A turn on a Gemini 3 model at Google's own endpoint has to send each call's thought signature
// back. Without it the round after the first tool call is refused, and every question that needed
// a tool ended in an error.
func TestAgentSendsGeminiThoughtSignaturesBack(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Model: "gemini-3.8-flash", MaxToolRounds: 4, TurnMaxMinutes: 12}
	a, _, _, st, sl := diggingFixture(t, cfg)
	g := &geminiLike{}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	a.llm = newLLM(cfg, endpoint{BaseURL: srv.URL, Key: "k", Dialect: dialectCompatible, Model: "gemini-3.8-flash"})

	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000800", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: "1700000000.000800", UserID: "U1",
		Text: "what is the weather where Priya lives?", Kind: "channel", Session: sess,
		Streamer: sl.NewStreamer("C1", "1700000000.000800", "U1")}
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	if g.refused != 0 {
		t.Errorf("the endpoint refused %d rounds for a missing thought signature", g.refused)
	}
	if !strings.Contains(c.FinalText, "21.4") {
		t.Errorf("final text %q, want the answer from the round after the call", c.FinalText)
	}
}
