package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// placingSlack is a Slack that hands out a fresh ts for every message and remembers what was
// posted, edited and deleted. A routine's answer has to end up in exactly one of those places,
// which is a claim about the end state rather than about any single call, so the test needs to
// see all three.
type placingSlack struct {
	mu      sync.Mutex
	n       int
	posts   []wireMsg // channel messages and thread replies, in order
	edits   map[string]string
	deleted []string
	files   []wireFile // what files.upload.v2 finished, in order
	pending string     // the bytes of the upload in flight, until the call that shares it
}

type wireMsg struct{ ts, threadTS, text string }

// wireFile is one finished upload: where it was shared, and the bytes that were PUT for it.
type wireFile struct{ channel, threadTS, content string }

func newPlacingSlack() *placingSlack { return &placingSlack{edits: map[string]string{}} }

func (f *placingSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The upload PUT is not an API call and its body is the file itself, so it is answered
	// before anything here tries to read the request as a form.
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.pending = string(body)
		f.mu.Unlock()
		w.Write([]byte("ok"))
		return
	}
	r.ParseForm()
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	// The three calls files.upload.v2 makes, and the files.info lookup that turns the id it
	// answers with into a permalink. Each wants a different shape, so they are answered here
	// rather than through the one envelope every chat.* call shares below.
	switch method {
	case "files.getUploadURLExternal":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": "F1", "upload_url": "http://" + r.Host + "/put"})
		return
	case "files.completeUploadExternal":
		f.mu.Lock()
		f.files = append(f.files, wireFile{channel: r.Form.Get("channel_id"), threadTS: r.Form.Get("thread_ts"), content: f.pending})
		f.pending = ""
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "files": []map[string]any{{"id": "F1", "title": "Full answer"}}})
		return
	case "files.info":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": map[string]any{"id": "F1", "permalink": "https://x.slack.com/files/F1"}})
		return
	}
	text := r.Form.Get("text")
	if blocks := r.Form.Get("blocks"); blocks != "" {
		text = blocks // the real content travels as blocks; the text field is only a preview
	}

	f.mu.Lock()
	f.n++
	ts := fmt.Sprintf("17000000%02d.000100", f.n)
	switch method {
	case "chat.postMessage":
		f.posts = append(f.posts, wireMsg{ts: ts, threadTS: r.Form.Get("thread_ts"), text: text})
	case "chat.update":
		ts = r.Form.Get("ts")
		f.edits[ts] = text
	case "chat.delete":
		f.deleted = append(f.deleted, r.Form.Get("ts"))
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "channel": r.Form.Get("channel"), "ts": ts,
		"permalink": "https://example.slack.com/archives/C1/p" + strings.ReplaceAll(ts, ".", ""),
	})
}

func (f *placingSlack) snapshot() ([]wireMsg, map[string]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edits := map[string]string{}
	for k, v := range f.edits {
		edits[k] = v
	}
	return append([]wireMsg(nil), f.posts...), edits, append([]string(nil), f.deleted...)
}

func (f *placingSlack) uploads() []wireFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wireFile(nil), f.files...)
}

// placingFixture is quietFixture's twin for a routine that does post: same fake model, a Slack
// that keeps every message apart.
func placingFixture(t *testing.T, answer string, r Routine) (*Agent, *placingSlack, Routine) {
	t.Helper()
	st := testStore(t)
	llm := &fakeLLM{answer: answer}
	rec := newPlacingSlack()
	slackSrv := httptest.NewServer(rec)
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)

	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	a := &Agent{
		store: st, tools: map[string]Tool{}, runs: map[int64]*runHandle{}, loc: time.UTC,
		slacks:   testRegistry(sl),
		settings: newSettingsCache(st, Config{MaxToolRounds: 8}),
		llm:      NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"}),
	}

	r.OrgID, r.TeamID, r.Channel = 1, "T1", "C1"
	r.TZ, r.Cron = "UTC", "0 21 * * *"
	id, err := st.AddRoutine(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	return a, rec, r
}

// An answer short enough to read in the channel belongs in the message everyone can see, and
// nowhere else. The thread copy the run wrote while it worked is taken down: the same brief in
// the message and again in the thread is what this used to do, and the copy in the message was
// the one cut off halfway.
func TestShortRoutineAnswerEndsUpOnlyInTheMessage(t *testing.T) {
	const answer = "3 new leads, all US mid-market. Best fit: Intellinetics — document management, 51-200 employees."
	a, rec, r := placingFixture(t, answer, Routine{Prompt: "work the new-lead queue", Notify: "always"})

	a.runRoutineNow(context.Background(), r, r.NextRun)

	posts, edits, deleted := rec.snapshot()
	if len(posts) == 0 {
		t.Fatal("the run posted nothing at all")
	}
	root := posts[0]
	if root.threadTS != "" {
		t.Fatalf("the first message should be the header in the channel, got a reply to %s", root.threadTS)
	}
	if !strings.Contains(edits[root.ts], "Intellinetics") {
		t.Errorf("the message does not carry the answer: %q", edits[root.ts])
	}
	if wasDeleted(deleted, root.ts) {
		t.Fatal("the message holding the answer was deleted")
	}
	// One copy left standing, and it is the message. The thread copy the run wrote while it
	// worked has to be gone — the count is what matters, not which call made it so.
	live := 0
	if strings.Contains(edits[root.ts], "Intellinetics") {
		live++
	}
	for _, p := range posts[1:] {
		if strings.Contains(p.text, "Intellinetics") && !wasDeleted(deleted, p.ts) {
			live++
		}
	}
	if live != 1 {
		t.Errorf("the answer is readable in %d places, want 1: posts=%v edits=%v deleted=%v", live, posts, edits, deleted)
	}
}

// A report of a few thousand characters is a report, not a download. The threshold used to sit
// at 3,500, which filed almost everything a routine wrote: the channel got a lead and a
// paperclip where it could have had the thing itself.
func TestAFewThousandCharacterAnswerStillLandsInTheMessage(t *testing.T) {
	answer := "Lead brief.\n\n" + strings.Repeat("Intellinetics is a document management vendor in Columbus, Ohio. ", 120)
	if n := len(answer); n <= 3500 || n >= messageAnswerCap {
		t.Fatalf("the fixture answer is %d chars; it has to be past the old threshold and inside a message", n)
	}
	a, rec, r := placingFixture(t, answer, Routine{Prompt: "work the new-lead queue", Notify: "always"})

	a.runRoutineNow(context.Background(), r, r.NextRun)

	posts, edits, _ := rec.snapshot()
	if len(posts) == 0 {
		t.Fatal("the run posted nothing at all")
	}
	if msg := edits[posts[0].ts]; !strings.Contains(msg, "Columbus, Ohio") {
		t.Errorf("the answer was filed away instead of posted: %q", msg)
	}
	if up := rec.uploads(); len(up) != 0 {
		t.Errorf("an answer that fits in a message was uploaded as a file: %d file(s)", len(up))
	}
}

// The other half of the rule: an answer that will not fit in a message goes up as a file, and
// the message carries the lead. It used to say "the full answer is in the thread" and leave the
// wall of text sitting there — the same thing that was too long to read, one click further away.
func TestAnAnswerTooLongForAMessageIsFiledWithItsLead(t *testing.T) {
	answer := "Four leads worked, two worth a call.\n\n" + strings.Repeat("Intellinetics is a document management vendor in Columbus, Ohio. ", 220)
	if len(answer) <= messageAnswerCap {
		t.Fatalf("the fixture answer is %d chars, which still fits in a message", len(answer))
	}
	a, rec, r := placingFixture(t, answer, Routine{Prompt: "work the new-lead queue", Notify: "always"})

	a.runRoutineNow(context.Background(), r, r.NextRun)

	posts, edits, deleted := rec.snapshot()
	if len(posts) == 0 {
		t.Fatal("the run posted nothing at all")
	}
	root := posts[0]
	msg := edits[root.ts]
	if !strings.Contains(msg, "Four leads worked") {
		t.Errorf("the message lost the lead of the answer: %q", msg)
	}
	if !strings.Contains(msg, "attached as a file") {
		t.Errorf("the message does not say the rest is attached: %q", msg)
	}
	up := rec.uploads()
	if len(up) != 1 {
		t.Fatalf("want one file in the thread, got %d", len(up))
	}
	if up[0].threadTS != root.ts {
		t.Errorf("the file went to thread %q, want the run's own thread %q", up[0].threadTS, root.ts)
	}
	if !strings.Contains(up[0].content, "Columbus, Ohio") {
		t.Errorf("the file does not carry the answer: %.200q", up[0].content)
	}
	// And the copy the run streamed while it worked comes down, so the answer reads once.
	live := 0
	for _, p := range posts[1:] {
		if strings.Contains(p.text, "Columbus, Ohio") && !wasDeleted(deleted, p.ts) {
			live++
		}
	}
	if live != 0 {
		t.Errorf("the long answer is still sitting in the thread in %d place(s) next to its own file", live)
	}
}

// The header is a lead, not the prompt over again: one line, and no arithmetic about how much
// of a nine-thousand-character prompt was left out.
func TestRoutineHeaderPreviewsThePromptWithoutACharacterCount(t *testing.T) {
	long := "You are an autonomous SDR agent. Every run, work the new-lead queue end to end.\n\n" +
		strings.Repeat("Search HubSpot for contacts created in the last three days. ", 150)
	a, rec, r := placingFixture(t, "done", Routine{Prompt: long, Notify: "always"})

	a.runRoutineNow(context.Background(), r, r.NextRun)

	posts, _, _ := rec.snapshot()
	if len(posts) == 0 {
		t.Fatal("the run posted nothing at all")
	}
	header := posts[0].text
	if strings.Contains(header, "truncated") {
		t.Errorf("the header still counts out what it left behind: %q", header)
	}
	if !strings.Contains(header, "autonomous SDR agent") {
		t.Errorf("the header lost the start of the prompt: %q", header)
	}
	if n := len(header); n > 600 {
		t.Errorf("the header is %d chars — it is a lead, not the prompt", n)
	}
}

func wasDeleted(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
