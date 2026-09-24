package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
)

// streamSlack keeps the raw body of every call, so a test can ask what actually reached the
// message rather than what the code meant to put there.
type streamSlack struct {
	mu     sync.Mutex
	bodies []string
}

func (f *streamSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body, err := url.QueryUnescape(string(b))
	if err != nil {
		body = string(b)
	}
	f.mu.Lock()
	f.bodies = append(f.bodies, strings.TrimPrefix(r.URL.Path, "/api/")+" "+body)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C1", "ts": "1700000000.000100"})
}

func (f *streamSlack) sent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.bodies, "\n")
}

// deltas cuts s into the small pieces a stream arrives in, with no regard for where a tag
// starts or ends -- which is the hard part: "<tool" can be the whole of one delta.
func deltas(s string, n int) []string {
	var out []string
	for len(s) > n {
		out, s = append(out, s[:n]), s[n:]
	}
	return append(out, s)
}

// The bug this exists for: a stream is written once and cannot be taken back. Markup was only
// removed from the string the turn ended with, so a <tool_call> block the model wrote as part of
// its answer was appended to the message verbatim; then the tail was sliced by the byte count of
// everything written, and the thread got the markup followed by an answer that began 367 bytes
// in, mid-sentence.
func TestStreamedMarkupNeverReachesTheThread(t *testing.T) {
	rec := &streamSlack{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	st := sl.NewStreamer("C1", "1700000000.000100", "U1")
	ctx := context.Background()

	raw := `<tool_call>gcp_query_logs<arg_key>filter</arg_key><arg_value>"extractor_api_error"</arg_value></tool_call>` +
		"Queried the logs: the extractor API returned a 500 HTML page on every attempt."
	for _, d := range deltas(raw, 7) {
		st.Write(ctx, d)
	}

	answer := stripThinking(raw)
	if tail := streamTail(st, answer); tail != "" {
		t.Errorf("tail %q, want nothing left to append: the stream already carried the answer", tail)
	}
	if _, err := st.Stop(ctx, streamTail(st, answer), "model · Configure"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sent := rec.sent()
	if strings.Contains(sent, "tool_call") {
		t.Errorf("raw tool-call markup reached the message: %s", sent)
	}
	if !strings.Contains(sent, "Queried the logs: the extractor API returned a 500 HTML page") {
		t.Errorf("the answer did not arrive whole: %s", sent)
	}
}

// What streamed and what the turn ends on can be different strings -- a re-streamed answer comes
// back worded its own way. Slicing one by the length of the other is how a reply came to start
// mid-sentence, so an answer that is no continuation of what was written is posted whole.
func TestStreamTailNeverPostsAFragment(t *testing.T) {
	sl := &Chat{BotUserID: "UBOT", TeamID: "T1"}
	answer := "The extractor API returned 500 on every attempt."

	fresh := sl.NewStreamer("C1", "1700000000.000100", "U1")
	if got := streamTail(fresh, answer); got != answer {
		t.Errorf("nothing streamed: tail %q, want the whole answer", got)
	}

	partial := sl.NewStreamer("C1", "1700000000.000100", "U1")
	partial.silent = true // keeps the text without needing a Slack server
	partial.Write(context.Background(), "The extractor API ")
	if got, want := streamTail(partial, answer), "returned 500 on every attempt."; got != want {
		t.Errorf("half streamed: tail %q, want %q", got, want)
	}

	// Whitespace drift alone is not a different answer: the stream is filtered delta by delta
	// while the answer is stripped whole, so line breaks around something dropped can differ.
	spaced := sl.NewStreamer("C1", "1700000000.000100", "U1")
	spaced.silent = true
	spaced.Write(context.Background(), "The extractor API\n\nreturned 500 on every attempt.")
	if got := streamTail(spaced, answer); got != "" {
		t.Errorf("same text, different line breaks: tail %q, want nothing", got)
	}

	other := sl.NewStreamer("C1", "1700000000.000100", "U1")
	other.silent = true
	other.Write(context.Background(), "Let me look at the logs again.")
	if got := streamTail(other, answer); got != answer {
		t.Errorf("a stream that carried something else: tail %q, want the whole answer", got)
	}
}

func TestMarkupFilterHoldsBackPartialTags(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"plain text", "plain text"},
		{"<think>weighing it up</think>the answer", "the answer"},
		{"before<tool_call>{\"name\":\"peek\"}</tool_call>after", "beforeafter"},
		{"before<tool_result id=\"3\">rows</tool_result>after", "beforeafter"},
		{"a < b and c<d", "a < b and c<d"},
		{"<thinking out loud>", "<thinking out loud>"},
		{"answer<tool_call>never closed", "answer"}, // the close tag is what usually goes missing
	}
	for _, c := range cases {
		for _, n := range []int{1, 3, 7, 1000} {
			var f markupFilter
			var got strings.Builder
			for _, d := range deltas(c.raw, n) {
				got.WriteString(f.push(d))
			}
			if got.String() != c.want {
				t.Errorf("deltas of %d over %q gave %q, want %q", n, c.raw, got.String(), c.want)
			}
		}
	}
}
