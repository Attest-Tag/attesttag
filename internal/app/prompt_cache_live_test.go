package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

// TestPromptCacheLive asks the configured model the same long-prefix question twice and reports
// what it served from cache the second time. Whether a breakpoint pays is a fact about the
// provider, not about this code, so it is checked against the real one:
//
//	CACHE_LIVE=1 ENV_FILE=../../.env.testing go test ./internal/app -run TestPromptCacheLive -v
func TestPromptCacheLive(t *testing.T) {
	if os.Getenv("CACHE_LIVE") != "1" {
		t.Skip("set CACHE_LIVE=1 to run against the configured model")
	}
	cfg := LoadConfig()
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	llm := NewLLM(cfg)

	// A stable prefix big enough to be worth caching (providers ask for ~1k tokens), shaped
	// like the real thing: a long system block that repeats, then a short question that varies.
	var b strings.Builder
	b.WriteString("You are a assistant for an engineering team. Follow these standing rules.\n")
	for i := 0; i < 200; i++ {
		b.WriteString("Rule: prefer the named tool over a raw request, cite what you used, and never invent a result you did not fetch.\n")
	}
	stable := b.String()

	ask := func(q string) Usage {
		t.Helper()
		msgs := []openai.ChatCompletionMessageParamUnion{
			CachedSystemMessage(stable, "Current time: irrelevant to this test."),
			openai.UserMessage(q + " Answer with one word."),
		}
		_, us, err := llm.Chat(context.Background(), cfg.Model, msgs, nil, "")
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		return us
	}

	first := ask("Is the sky blue?")
	second := ask("Is grass green?")
	t.Logf("model %s", cfg.Model)
	t.Logf("call 1: in=%d cached=%d out=%d cost=$%.5f", first.In, first.CachedIn, first.Out, first.CostUSD)
	t.Logf("call 2: in=%d cached=%d out=%d cost=$%.5f", second.In, second.CachedIn, second.Out, second.CostUSD)
	if second.CachedIn == 0 {
		t.Logf("no cache hit reported — this provider either does not cache this model or does not report cached_tokens")
		return
	}
	t.Logf("cache hit: %d of %d prompt tokens (%d%%), cost %.0f%% of the first call",
		second.CachedIn, second.In, 100*second.CachedIn/second.In, 100*second.CostUSD/first.CostUSD)
}
