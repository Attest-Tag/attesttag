package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

// What reaches the model endpoint should be the key, the model and the messages. Two things used
// to ride along: OpenRouter's HTTP-Referer and X-Title, which are how an app puts its name and a
// link on openrouter.ai's public rankings, and the SDK's X-Stainless-* telemetry, which tells
// whoever is on the other end the Go version, the OS and the architecture this runs on. Neither
// changes an answer, so neither is sent.
func TestNoAppIdentityReachesTheModelEndpoint(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "sk-test"})
	if _, err := l.client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: "m", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	}, l.chatOpts()...); err != nil {
		t.Fatal(err)
	}

	for _, h := range []string{"Http-Referer", "X-Title"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s = %q — the app names itself to the provider", h, v)
		}
	}
	for h, v := range got {
		if strings.HasPrefix(http.CanonicalHeaderKey(h), "X-Stainless-") {
			t.Errorf("%s = %v — SDK telemetry the endpoint has no use for", h, v)
		}
	}
	if ua := got.Get("User-Agent"); ua != "" {
		t.Errorf("User-Agent = %q, want none: it named the SDK and its version", ua)
	}
	// The things it does need are untouched.
	if got.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("Authorization = %q — stripping went too far", got.Get("Authorization"))
	}
	if ct := got.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q", ct)
	}
}
