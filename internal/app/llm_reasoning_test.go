package app

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestReasoningBody(t *testing.T) {
	for in, want := range map[string]map[string]any{
		"":     {"effort": "low", "exclude": true},
		"low":  {"effort": "low", "exclude": true},
		"high": {"effort": "high", "exclude": true},
		"none": {"enabled": false},
		"off":  {"enabled": false},
	} {
		if got := reasoningBody(in); !reflect.DeepEqual(got, want) {
			t.Errorf("reasoningBody(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestReasoningLive sends one real completion with the configured model and key (LLM_LIVE=1),
// to catch an endpoint that refuses the reasoning field we send.
func TestReasoningLive(t *testing.T) {
	if os.Getenv("LLM_LIVE") != "1" {
		t.Skip("set LLM_LIVE=1 to call the configured model")
	}
	cfg := LoadConfig()
	l := NewLLM(cfg)
	model := env("LLM_LIVE_MODEL", cfg.Model) // the heavy model is worth checking too
	t.Logf("model=%s reasoning=%q", model, cfg.Reasoning)
	resp, err := l.client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    model,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Reply with the single word: ok")},
	}, l.chatOpts()...)
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("no choices came back")
	}
	t.Logf("answer=%q", stripThinking(resp.Choices[0].Message.Content))
}
