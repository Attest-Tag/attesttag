package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3"
)

// fakeEndpoint is an OpenAI-compatible server that answers every completion with "ok" and keeps
// the body of each request, so a test can read exactly what was put on the wire.
type fakeEndpoint struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
}

func newFakeEndpoint(t *testing.T) *fakeEndpoint {
	t.Helper()
	f := &fakeEndpoint{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		if len(raw) > 0 {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body is not JSON: %s", raw)
			}
			f.bodies = append(f.bodies, body)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeEndpoint) last(t *testing.T) (map[string]any, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("the endpoint was never called")
	}
	body := f.bodies[len(f.bodies)-1]
	raw, _ := json.Marshal(body)
	return body, string(raw)
}

// A turn's prompt as the agent builds it: the system block as marked parts, and a tool round
// behind it, which is the message a tail breakpoint would land on.
func turnPrompt() []openai.ChatCompletionMessageParamUnion {
	return []openai.ChatCompletionMessageParamUnion{
		CachedSystemMessage(strings.Repeat("stable prompt. ", 400), "volatile line"),
		openai.UserMessage("what changed?"),
		openai.ToolMessage("result of the tool", "call_1"),
	}
}

func TestDialectFor(t *testing.T) {
	for url, want := range map[string]dialect{
		"https://openrouter.ai/api/v1":                       dialectOpenRouter,
		"https://api.openai.com/v1":                          dialectOpenAI,
		"https://API.OpenAI.com/v1/":                         dialectOpenAI,
		"https://acme.openai.azure.com/openai/v1/":           dialectOpenAI,
		"https://acme.cognitiveservices.azure.com/openai/v1": dialectOpenAI,
		"https://acme.services.ai.azure.com/openai/v1":       dialectOpenAI,
		"https://api.groq.com/openai/v1":                     dialectCompatible,
		"https://openai.example.com/v1":                      dialectCompatible,
		"https://api.openai.com.attacker.example/v1":         dialectCompatible,
		"http://127.0.0.1:4000":                              dialectCompatible,
		"::not a url":                                        dialectCompatible,
	} {
		if got := dialectFor(url); got != want {
			t.Errorf("dialectFor(%q) = %s, want %s", url, got, want)
		}
	}
}

// The deployment's own endpoint is sent exactly what it was always sent, unless it is a host
// that refuses that shape.
func TestTheDeploymentEndpointKeepsItsShape(t *testing.T) {
	f := newFakeEndpoint(t)
	l := NewLLM(Config{LLMBaseURL: f.URL, LLMKey: "sk-test", Model: "z-ai/glm-5.3-flash", DenyTraining: true})
	if l.dialect != dialectOpenRouter {
		t.Fatalf("a gateway on %s is %s; it has always been sent OpenRouter's shape", f.URL, l.dialect)
	}
	if _, _, err := l.Chat(context.Background(), "", turnPrompt(), nil, ""); err != nil {
		t.Fatal(err)
	}
	body, raw := f.last(t)
	if body["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want 0.2", body["temperature"])
	}
	for _, field := range []string{`"reasoning"`, `"provider"`, `"cache_control"`} {
		if !strings.Contains(raw, field) {
			t.Errorf("the OpenRouter body lost %s: %s", field, raw)
		}
	}
	if d := NewLLM(Config{LLMBaseURL: "https://api.openai.com/v1"}).dialect; d != dialectOpenAI {
		t.Errorf("LLM_BASE_URL on api.openai.com is %s; OpenRouter's fields would be refused there", d)
	}
}

func TestOpenAIIsSentOnlyWhatItAccepts(t *testing.T) {
	for _, c := range []struct {
		model, reasoning string
		temperature      bool
		effort           string
	}{
		{"gpt-4.1-mini", "low", true, ""},
		{"gpt-4o", "high", true, ""},
		{"gpt-5-mini", "low", false, "low"},
		{"gpt-5", "", false, "low"},
		{"gpt-5-nano", "minimal", false, "low"},
		{"gpt-5", "none", false, "low"},
		{"gpt-5", "high", false, "high"},
		{"o3", "medium", false, "medium"},
		{"o4-mini", "low", false, "low"},
		{"o1-mini", "low", false, ""},
		{"gpt-5-chat-latest", "low", false, ""},
		{"prod-gpt4o-deployment", "low", false, ""}, // an Azure deployment, named by its owner
		{"gpt-9-turbo", "low", false, ""},           // newer than this list
	} {
		f := newFakeEndpoint(t)
		l := newLLM(Config{Reasoning: c.reasoning}, endpoint{BaseURL: f.URL, Key: "sk-test", Dialect: dialectOpenAI, Model: c.model})
		if _, _, err := l.Chat(context.Background(), "", turnPrompt(), nil, ""); err != nil {
			t.Fatal(err)
		}
		body, raw := f.last(t)
		if _, has := body["temperature"]; has != c.temperature {
			t.Errorf("%s: temperature sent = %v, want %v", c.model, has, c.temperature)
		}
		if got, _ := body["reasoning_effort"].(string); got != c.effort {
			t.Errorf("%s with REASONING=%q: reasoning_effort = %q, want %q", c.model, c.reasoning, got, c.effort)
		}
		assertPlainShape(t, c.model, body, raw)
	}
}

func TestACompatibleEndpointIsSentThePlainShape(t *testing.T) {
	// A Claude model behind a gateway would earn a tail breakpoint on OpenRouter; here it must not.
	for _, model := range []string{"llama-3.3-70b-versatile", "claude-sonnet-4"} {
		f := newFakeEndpoint(t)
		l := newLLM(Config{Reasoning: "low", DenyTraining: true}, endpoint{BaseURL: f.URL, Key: "k", Dialect: dialectCompatible, Model: model})
		if _, _, err := l.Chat(context.Background(), "", turnPrompt(), nil, ""); err != nil {
			t.Fatal(err)
		}
		body, raw := f.last(t)
		if _, has := body["temperature"]; has {
			t.Errorf("%s: a compatible endpoint was sent a temperature", model)
		}
		if _, has := body["reasoning_effort"]; has {
			t.Errorf("%s: a compatible endpoint was sent reasoning_effort", model)
		}
		assertPlainShape(t, model, body, raw)
	}
}

// assertPlainShape: none of OpenRouter's fields, and the system block as one string that still
// carries both halves the agent wrote and the clock.
func assertPlainShape(t *testing.T, model string, body map[string]any, raw string) {
	t.Helper()
	for _, field := range []string{`"reasoning"`, `"provider"`, `"cache_control"`, `"stream_options"`} {
		if strings.Contains(raw, field) {
			t.Errorf("%s: body carries %s, which this endpoint refuses: %s", model, field, raw)
		}
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("%s: no messages in %s", model, raw)
	}
	sys, _ := msgs[0].(map[string]any)
	text, ok := sys["content"].(string)
	if !ok {
		t.Fatalf("%s: system content is %T, want one string", model, sys["content"])
	}
	for _, want := range []string{"stable prompt.", "volatile line", clockPrefix} {
		if !strings.Contains(text, want) {
			t.Errorf("%s: system message lost %q", model, want)
		}
	}
}

// Only OpenRouter keeps a separate list of embedding models; asking anyone else for it is a
// request that can only fail.
func TestOnlyOpenRouterIsAskedForItsEmbeddingList(t *testing.T) {
	for d, want := range map[dialect][]string{
		dialectOpenRouter: {"GET /models", "GET /embeddings/models"},
		dialectOpenAI:     {"GET /models"},
		dialectCompatible: {"GET /models"},
	} {
		f := newFakeEndpoint(t)
		l := newLLM(Config{}, endpoint{BaseURL: f.URL, Key: "k", Dialect: d})
		if _, err := l.ListModels(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if strings.Join(f.paths, ",") != strings.Join(want, ",") {
			t.Errorf("%s asked for %v, want %v", d, f.paths, want)
		}
	}
}
