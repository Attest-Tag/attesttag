package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

// sentSystem runs one Chat against a stub endpoint and returns the system message as it went out
// on the wire: the text of each content part, and whether that part carried a cache breakpoint.
func sentSystem(t *testing.T, msgs []openai.ChatCompletionMessageParamUnion) (texts []string, cached []bool, role string) {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "sk-test", Timezone: "Asia/Kathmandu"})
	if _, _, err := l.Chat(context.Background(), "m", msgs, nil, ""); err != nil {
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
	if len(req.Messages) == 0 {
		t.Fatal("no messages reached the endpoint")
	}
	role = req.Messages[0].Role
	var parts []struct {
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(req.Messages[0].Content, &parts); err != nil {
		var flat string
		if json.Unmarshal(req.Messages[0].Content, &flat) == nil {
			return []string{flat}, []bool{false}, role
		}
		t.Fatalf("first message content: %v", err)
	}
	for _, p := range parts {
		texts = append(texts, p.Text)
		cached = append(cached, len(p.CacheControl) > 0)
	}
	return texts, cached, role
}

func clockCount(texts []string) int {
	n := 0
	for _, t := range texts {
		n += strings.Count(t, clockPrefix)
	}
	return n
}

// The permission checker, the channel watcher and the thread summariser each hand the model a
// prompt they wrote themselves, with no clock in it. Every one of them still leaves with today's
// date attached, because the date is added where the call is made rather than where it is built.
func TestEveryCallCarriesTheClock(t *testing.T) {
	texts, _, role := sentSystem(t, []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("Decide whether this action is covered by a rule."),
		openai.UserMessage("delete the row"),
	})
	if role != "system" {
		t.Fatalf("first message role = %q, want system", role)
	}
	if clockCount(texts) != 1 {
		t.Fatalf("clock appears %d times in %q, want once", clockCount(texts), texts)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "Decide whether this action is covered by a rule.") {
		t.Error("the caller's own prompt did not survive")
	}
	if year := time.Now().Format("2006"); !strings.Contains(joined, year) {
		t.Errorf("clock does not state the current year %s: %q", year, joined)
	}
	if !strings.Contains(joined, time.Now().UTC().Format("2006-01-02")) {
		t.Error("clock does not state today's UTC date")
	}
}

// A prompt with no system message at all is given one, so a bare user turn is not the one call
// that goes out dateless.
func TestClockIsAddedWhenThereIsNoSystemMessage(t *testing.T) {
	texts, _, role := sentSystem(t, []openai.ChatCompletionMessageParamUnion{openai.UserMessage("what day is it?")})
	if role != "system" {
		t.Fatalf("first message role = %q, want a system message to have been prepended", role)
	}
	if clockCount(texts) != 1 {
		t.Fatalf("clock appears %d times, want once", clockCount(texts))
	}
}

// The agent writes its own clock, in the organisation's zone rather than the deployment's. That
// one wins: the call goes out with one clock, not two disagreeing ones.
func TestAgentsOwnClockIsNotDuplicated(t *testing.T) {
	own := clockText(time.Now(), time.UTC)
	texts, _, _ := sentSystem(t, []openai.ChatCompletionMessageParamUnion{
		CachedSystemMessage(strings.Repeat("stable prompt. ", 400), own),
		openai.UserMessage("hi"),
	})
	if clockCount(texts) != 1 {
		t.Fatalf("clock appears %d times, want the agent's own only", clockCount(texts))
	}
}

// The clock is the thing that differs on every call, so it has to sit after the breakpoint. If it
// lands in front of one, no two calls share a prefix and the cache never reads.
func TestClockSitsAfterTheCacheBreakpoint(t *testing.T) {
	texts, cached, _ := sentSystem(t, []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(strings.Repeat("a long stable channel prompt. ", 200)),
		openai.UserMessage("hi"),
	})
	if len(texts) < 2 {
		t.Fatalf("system message has %d parts, want the prompt and the clock", len(texts))
	}
	last := len(texts) - 1
	if !strings.Contains(texts[last], clockPrefix) {
		t.Fatalf("clock is not the last part: %q", texts)
	}
	if cached[last] {
		t.Error("the clock carries a cache breakpoint — every call would write a new entry")
	}
	if !cached[0] {
		t.Error("the stable prompt lost its cache breakpoint")
	}
}

// withClock copies rather than edits: a caller that reuses its message slice must not find a
// stale clock from the previous call already in it.
func TestWithClockDoesNotMutateTheCallersMessages(t *testing.T) {
	l := &LLM{loc: time.UTC}
	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("prompt"),
		openai.UserMessage("hi"),
	}
	out := l.withClock(msgs)
	if got := msgs[0].OfSystem.Content.OfString.Value; got != "prompt" {
		t.Errorf("caller's system message changed to %q", got)
	}
	if len(out[0].OfSystem.Content.OfArrayOfContentParts) != 2 {
		t.Fatalf("returned system message has %d parts, want prompt and clock", len(out[0].OfSystem.Content.OfArrayOfContentParts))
	}
}

// A nil zone is a misconfigured deployment, not a reason to send no date.
func TestClockTextFallsBackToUTC(t *testing.T) {
	if line := clockText(time.Now(), nil); !strings.Contains(line, clockPrefix) || !strings.Contains(line, "UTC") {
		t.Errorf("clockText(nil) = %q", line)
	}
}
