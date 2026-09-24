package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// fakeProvider serves an OpenAI-compatible model list and counts the calls it gets.
func fakeProvider(t *testing.T, models, embeddings string) (*LLM, *int32, string) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"User not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(&hits, 1)
			w.Write([]byte(models))
		case "/v1/embeddings/models":
			if embeddings == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(embeddings))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	l := &LLM{client: openai.NewClient(option.WithBaseURL(srv.URL+"/v1"), option.WithAPIKey("test-key"))}
	return l, &hits, srv.URL + "/v1"
}

const openRouterModels = `{"data":[
 {"id":"z-ai/glm-5.3-flash","name":"Z.AI: GLM 5.3 Flash","context_length":200000,
  "architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},
  "pricing":{"prompt":"0.0000004","completion":"0.0000016"}},
 {"id":"anthropic/claude-fable-5.1","name":"Anthropic: Claude Fable 5.1","context_length":1000000,
  "architecture":{"input_modalities":["text","image","file"],"output_modalities":["text"]},
  "pricing":{"prompt":"0.00001","completion":"0.00005"}},
 {"id":"openrouter/auto","name":"Auto Router","context_length":2000000,
  "architecture":{"input_modalities":["text"],"output_modalities":["text"]},
  "pricing":{"prompt":"-1","completion":"-1"}}
]}`

const openRouterEmbeddings = `{"data":[
 {"id":"qwen/qwen3-embedding-8b","name":"Qwen: Qwen3 Embedding 8B","context_length":32768,
  "architecture":{"input_modalities":["text"],"output_modalities":["embeddings"]},
  "pricing":{"prompt":"0.00000001","completion":"0"}}
]}`

func TestListModelsOpenRouter(t *testing.T) {
	l, hits, _ := fakeProvider(t, openRouterModels, openRouterEmbeddings)
	ms, err := l.ListModels(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	if got, want := strings.Join(ids, ","), "anthropic/claude-fable-5.1,openrouter/auto,qwen/qwen3-embedding-8b,z-ai/glm-5.3-flash"; got != want {
		t.Fatalf("ids sorted = %s, want %s", got, want)
	}
	byID := map[string]ModelInfo{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	glm := byID["z-ai/glm-5.3-flash"]
	if glm.Kind != "chat" || glm.Name != "Z.AI: GLM 5.3 Flash" || glm.ContextLength != 200000 || len(glm.Inputs) != 2 {
		t.Errorf("glm parsed wrong: %+v", glm)
	}
	if glm.PromptPerM == nil || *glm.PromptPerM < 0.399 || *glm.PromptPerM > 0.401 || glm.CompletionPerM == nil || *glm.CompletionPerM < 1.599 || *glm.CompletionPerM > 1.601 {
		t.Errorf("glm pricing per million wrong: %v %v", glm.PromptPerM, glm.CompletionPerM)
	}
	if auto := byID["openrouter/auto"]; auto.PromptPerM != nil {
		t.Errorf("negative price should be unknown, got %v", *auto.PromptPerM)
	}
	if emb := byID["qwen/qwen3-embedding-8b"]; emb.Kind != "embedding" {
		t.Errorf("embedding model kind = %q", emb.Kind)
	}

	// Cached: a second call doesn't reach the provider; refresh does.
	if _, err := l.ListModels(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("cached call hit the provider: %d requests", n)
	}
	if _, err := l.ListModels(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Errorf("refresh should hit the provider once more: %d requests", n)
	}
}

func TestListModelsPlainOpenAI(t *testing.T) {
	l, _, _ := fakeProvider(t, `{"object":"list","data":[
	 {"id":"text-embedding-3-small","object":"model","created":1,"owned_by":"openai"},
	 {"id":"gpt-4o-mini","object":"model","created":1,"owned_by":"openai"},
	 {"id":"gpt-4o-mini","object":"model","created":1,"owned_by":"openai"}
	]}`, "")
	ms, err := l.ListModels(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 deduped models, got %d: %+v", len(ms), ms)
	}
	if ms[0].ID != "gpt-4o-mini" || ms[0].Kind != "chat" || ms[0].OwnedBy != "openai" || ms[0].Name != "" || ms[0].PromptPerM != nil {
		t.Errorf("chat model parsed wrong: %+v", ms[0])
	}
	if ms[1].ID != "text-embedding-3-small" || ms[1].Kind != "embedding" {
		t.Errorf("embedding model by id should be kind embedding: %+v", ms[1])
	}
}

func TestListModelsError(t *testing.T) {
	_, _, base := fakeProvider(t, openRouterModels, "")
	l := &LLM{client: openai.NewClient(option.WithBaseURL(base), option.WithAPIKey("wrong-key"))}
	ms, err := l.ListModels(context.Background(), false)
	if err == nil {
		t.Fatalf("want an error for a rejected key, got %d models", len(ms))
	}
	if !strings.Contains(err.Error(), "list models") || !strings.Contains(err.Error(), "401") {
		t.Errorf("error should name the call and status: %v", err)
	}
}
