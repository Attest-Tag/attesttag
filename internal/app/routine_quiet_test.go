package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// recordingSlack is a Slack that remembers everything anyone tried to send it. A quiet run's
// whole promise is that this list stays empty, so the test asserts on the wire, not on a flag.
type recordingSlack struct {
	mu   sync.Mutex
	sent []string // "method channel: text"
}

func (f *recordingSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	text := r.Form.Get("text")
	if blocks := r.Form.Get("blocks"); text == "" && blocks != "" {
		text = blocks
	}
	// A card goes out as an attachment, so without this a recorded post is an empty line and a
	// test that asks what was in the card has nothing to read.
	if att := r.Form.Get("attachments"); text == "" && att != "" {
		text = att
	}
	f.mu.Lock()
	// Only the calls that put something in front of a person. assistant.threads.setStatus is
	// not one: Slack refuses it outside an assistant thread, which a routine never runs in.
	switch method {
	case "chat.postMessage", "chat.update", "files.getUploadURLExternal", "chat.startStream":
		f.sent = append(f.sent, method+" "+r.Form.Get("channel")+": "+text)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	switch method {
	case "chat.postMessage":
		enc.Encode(map[string]any{"ok": true, "channel": r.Form.Get("channel"), "ts": "1700000000.000100"})
	case "chat.update":
		enc.Encode(map[string]any{"ok": true, "channel": r.Form.Get("channel"), "ts": r.Form.Get("ts")})
	default:
		enc.Encode(map[string]any{"ok": true})
	}
}

func (f *recordingSlack) posts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// fakeLLM answers one canned completion, with no tool calls, so a turn runs straight to its
// final answer. It also keeps the last prompt, so a test can check what the model was told.
type fakeLLM struct {
	mu sync.Mutex
	// toolCall, when set, is returned on the first completion as a call to that tool with
	// toolArgs; the turn then gets answer as its final text on the next round.
	toolCall, toolArgs string
	answer             string
	last               string
	calls              int
	// The tool definitions the last request carried, by name. What a turn is charged for is
	// mostly this list, so a test that claims a turn was cheap has to be able to see it.
	lastTools []string
	// The model the last request asked for.
	lastModel string
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.lastModel = body.Model
	f.lastTools = nil
	for _, t := range body.Tools {
		f.lastTools = append(f.lastTools, t.Function.Name)
	}
	var sb strings.Builder
	for _, m := range body.Messages {
		sb.Write(m.Content)
	}
	f.last = sb.String()
	f.calls++
	first := f.calls == 1
	tool, args, answer := f.toolCall, f.toolArgs, f.answer
	f.mu.Unlock()

	msg := map[string]any{"role": "assistant", "content": answer}
	finish := "stop"
	if tool != "" && first {
		msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": tool, "arguments": args},
		}}}
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices":// one choice, either a tool call or the final answer
		[]map[string]any{{"index": 0, "finish_reason": finish, "message": msg}},
		"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 40},
	})
}

func (f *fakeLLM) prompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeLLM) model() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastModel
}

func (f *fakeLLM) tools() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lastTools...)
}

// quietFixture wires an agent to a fake Slack and a fake model, with one routine to run.
func quietFixture(t *testing.T, answer string, r Routine) (*Agent, *recordingSlack, *fakeLLM, *Store, Routine) {
	t.Helper()
	st := testStore(t)
	llm := &fakeLLM{answer: answer}
	rec := &recordingSlack{}
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
	if r.TZ == "" {
		r.TZ = "UTC"
	}
	if r.Cron == "" {
		r.Cron = "0 21 * * *"
	}
	id, err := st.AddRoutine(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	return a, rec, llm, st, r
}

// The whole point of a quiet routine: it runs, it decides there is nothing to say, and the
// channel never hears from it. Not "posts a shorter message" — posts nothing at all.
func TestQuietRoutineSaysNothingAndIsStillRecorded(t *testing.T) {
	a, rec, llm, st, r := quietFixture(t, "NOTHING\nall 6 VMs under 80% CPU",
		Routine{Prompt: "check the GCP VMs", Notify: notifyWhenNeeded, NotifyWhen: "any VM is over 80% CPU"})
	ctx := context.Background()

	a.runRoutineNow(ctx, r, r.NextRun)

	if posts := rec.posts(); len(posts) != 0 {
		t.Errorf("a quiet run reached Slack %d time(s): %v", len(posts), posts)
	}
	// The bar it was given has to reach the model, or it has nothing to decide against.
	if p := llm.prompt(); !strings.Contains(p, "any VM is over 80% CPU") || !strings.Contains(p, "stay_quiet") {
		t.Errorf("the quiet instruction never reached the model: %q", p)
	}

	runs, err := st.RoutineRuns(ctx, 1, r.ID, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d (%v), want 1 — a quiet run is the one that most needs recording", len(runs), err)
	}
	run := runs[0]
	if run.Status != runQuiet {
		t.Errorf("status = %q, want %q", run.Status, runQuiet)
	}
	if run.Reason != "all 6 VMs under 80% CPU" {
		t.Errorf("reason = %q — the note is all anyone gets to see", run.Reason)
	}
	if !strings.Contains(run.Output, "all 6 VMs") {
		t.Errorf("output = %q, want the whole answer kept", run.Output)
	}
	if run.TokensIn != 900 || run.CostUSD < 0 {
		t.Errorf("usage not recorded: in=%d out=%d cost=%v", run.TokensIn, run.TokensOut, run.CostUSD)
	}
	// The run key is not a Slack ts, and it is what the usage rows were written under.
	if !strings.HasPrefix(run.ThreadTS, "routine:") {
		t.Errorf("run key = %q, want a synthetic routine key", run.ThreadTS)
	}
	// And the routine's own row carries the outcome, for the list that does not join.
	rs, _ := st.Routines(ctx, 1, "")
	if rs[0].LastStatus != runQuiet || rs[0].LastError != "" {
		t.Errorf("last status = %q / error %q, want quiet and no error", rs[0].LastStatus, rs[0].LastError)
	}
}

// The other half: when it does clear its bar, it speaks — once, as a single message, with no
// "working…" placeholder in front of it because nothing was posted while it worked.
func TestQuietRoutineThatClearsItsBarPostsOnce(t *testing.T) {
	a, rec, _, st, r := quietFixture(t, "web-3 is at 94% CPU and has been for an hour.",
		Routine{Prompt: "check the GCP VMs", Notify: notifyWhenNeeded, NotifyWhen: "any VM is over 80% CPU"})
	ctx := context.Background()

	a.runRoutineNow(ctx, r, r.NextRun)

	posts := rec.posts()
	sends := 0
	for _, p := range posts {
		if strings.HasPrefix(p, "chat.postMessage") {
			sends++
			if strings.Contains(p, "working…") {
				t.Errorf("a quiet run announced itself before it had anything to say: %s", p)
			}
			if !strings.Contains(p, "94% CPU") {
				t.Errorf("the report did not carry the answer: %s", p)
			}
		}
	}
	if sends != 1 {
		t.Errorf("posted %d messages, want exactly 1: %v", sends, posts)
	}

	runs, _ := st.RoutineRuns(ctx, 1, r.ID, 0)
	if len(runs) != 1 || runs[0].Status != runPosted {
		t.Fatalf("runs = %+v, want one posted run", runs)
	}
	if runs[0].Reason != "" {
		t.Errorf("a run that reported should carry no quiet note, got %q", runs[0].Reason)
	}
}

// A routine that always posts must behave exactly as it did before any of this: a header
// message first, then the answer edited into it.
func TestAlwaysRoutineStillPostsItsHeaderAndResult(t *testing.T) {
	a, rec, llm, st, r := quietFixture(t, "Six VMs, all healthy.", Routine{Prompt: "check the GCP VMs"})
	ctx := context.Background()

	a.runRoutineNow(ctx, r, r.NextRun)

	posts := rec.posts()
	var header, updated bool
	for _, p := range posts {
		if strings.HasPrefix(p, "chat.postMessage") && strings.Contains(p, "working…") {
			header = true
		}
		if strings.HasPrefix(p, "chat.update") {
			updated = true
		}
	}
	if !header {
		t.Errorf("the header message is gone from the always path: %v", posts)
	}
	if !updated {
		t.Errorf("the header was never edited with the result: %v", posts)
	}
	// And it is not told to decide anything — that instruction belongs to quiet runs only.
	if p := llm.prompt(); strings.Contains(p, "running quietly") {
		t.Errorf("an always-posting routine was given the quiet instruction: %q", p)
	}

	runs, _ := st.RoutineRuns(ctx, 1, r.ID, 0)
	if len(runs) != 1 || runs[0].Status != runPosted {
		t.Fatalf("runs = %+v, want one posted run", runs)
	}
}

// The regression this feature actually shipped with, from a real run in #ops: the model did the
// work, decided correctly that 9 VMs was under the threshold, and then wrote a sentence saying
// so. That sentence was posted — a routine set to stay quiet announcing that it is staying
// quiet, which is precisely the noise it exists to remove.
func TestQuietRoutineDoesNotPostItsOwnDecision(t *testing.T) {
	for _, answer := range []string{
		"Found 9 Compute Engine VM instances (from the uptime metric over the last 15 minutes, deduplicated by instance id) — 10 or fewer, so no report posted.",
		"9 Compute Engine VMs were running in my-project over the last 15 minutes (1 in us-central1-a, 3 in us-central1-b, 5 in us-central1-c) — at or under the threshold, so no report was posted.",
	} {
		a, rec, _, st, r := quietFixture(t, answer, Routine{
			Prompt: "Count the Compute Engine VMs in my-project",
			Notify: notifyWhenNeeded, NotifyWhen: "there are more than 10 VMs"})
		a.runRoutineNow(context.Background(), r, r.NextRun)

		if posts := rec.posts(); len(posts) != 0 {
			t.Errorf("posted its own decision not to post: %v", posts)
		}
		runs, _ := st.RoutineRuns(context.Background(), 1, r.ID, 0)
		if len(runs) != 1 || runs[0].Status != runQuiet {
			t.Fatalf("runs = %+v, want one quiet run", runs)
		}
		if runs[0].Output == "" {
			t.Error("the reasoning should still be in the log, just not in the channel")
		}
	}
}

// And with the decision tools, which is how it is meant to go: the signal decides, and the
// text the turn happens to end on is not posted.
func TestQuietRoutineUsesTheDecisionTools(t *testing.T) {
	ctx := context.Background()

	a, rec, llm, st, r := quietFixture(t, "done", Routine{Prompt: "check the VMs",
		Notify: notifyWhenNeeded, NotifyWhen: "more than 10 VMs"})
	llm.toolCall, llm.toolArgs = "stay_quiet", `{"found":"9 VMs, under the threshold"}`
	a.runRoutineNow(ctx, r, r.NextRun)

	if posts := rec.posts(); len(posts) != 0 {
		t.Errorf("stay_quiet still reached Slack: %v", posts)
	}
	runs, _ := st.RoutineRuns(ctx, 1, r.ID, 0)
	if len(runs) != 1 || runs[0].Status != runQuiet || runs[0].Reason != "9 VMs, under the threshold" {
		t.Fatalf("runs = %+v, want one quiet run carrying the tool's note", runs)
	}

	// report_now posts the text it was given, not the turn's trailing "done".
	a2, rec2, llm2, st2, r2 := quietFixture(t, "done", Routine{Prompt: "check the VMs",
		Notify: notifyWhenNeeded, NotifyWhen: "more than 10 VMs"})
	llm2.toolCall, llm2.toolArgs = "report_now", `{"report":"14 VMs running — 4 over the threshold."}`
	a2.runRoutineNow(ctx, r2, r2.NextRun)

	var posted string
	for _, p := range rec2.posts() {
		if strings.HasPrefix(p, "chat.postMessage") {
			posted = p
		}
	}
	if !strings.Contains(posted, "4 over the threshold") {
		t.Errorf("report_now did not post its text: %q", posted)
	}
	if strings.Contains(posted, "done") {
		t.Errorf("the turn's trailing text was posted instead of the report: %q", posted)
	}
	runs2, _ := st2.RoutineRuns(ctx, 1, r2.ID, 0)
	if len(runs2) != 1 || runs2[0].Status != runPosted {
		t.Fatalf("runs = %+v, want one posted run", runs2)
	}
}

// A quiet routine that breaks must not break quietly: the person who made it is still told.
func TestQuietRoutineStillReportsItsFailures(t *testing.T) {
	st := testStore(t)
	rec := &recordingSlack{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	// No LLM at all: the turn cannot complete, which is the failure this is about.
	a := &Agent{store: st, tools: map[string]Tool{}, runs: map[int64]*runHandle{}, loc: time.UTC,
		slacks: testRegistry(sl), settings: newSettingsCache(st, Config{MaxToolRounds: 8}),
		llm: NewLLM(Config{LLMBaseURL: "http://127.0.0.1:1", LLMKey: "k", Model: "test"})}

	ctx := context.Background()
	id, err := st.AddRoutine(ctx, Routine{OrgID: 1, TeamID: "T1", Channel: "C1", Cron: "0 21 * * *", TZ: "UTC",
		Prompt: "check the VMs", CreatedBy: "UMAKER", Notify: notifyWhenNeeded})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := st.Routines(ctx, 1, "")
	a.runRoutineNow(ctx, rs[0], "")

	var dm bool
	for _, p := range rec.posts() {
		if strings.HasPrefix(p, "chat.postMessage") && strings.Contains(p, "UMAKER") {
			dm = true
		}
	}
	if !dm {
		t.Errorf("nobody was told a quiet routine is broken: %v", rec.posts())
	}
	runs, _ := st.RoutineRuns(ctx, 1, id, 0)
	if len(runs) != 1 || runs[0].Status != runFailed || runs[0].Error == "" {
		t.Fatalf("runs = %+v, want one failed run carrying its error", runs)
	}
}

// A routine prompt is somebody's text, posted as a header before the model has said anything.
// Unescaped, <!channel> in a prompt broadcast to the whole channel on every run — recurring by
// construction, and nothing the model did afterwards could take it back.
func TestRoutineHeaderDoesNotBroadcast(t *testing.T) {
	for _, bait := range []string{"<!channel> check the deploy", "<!here> ping", "<!everyone>"} {
		header := fmt.Sprintf(":alarm_clock: *Routine #%d* — %s", 1, escapeMrkdwn(truncate(bait, 200)))
		for _, live := range []string{"<!channel>", "<!here>", "<!everyone>"} {
			if strings.Contains(header, live) {
				t.Errorf("a prompt of %q posts a live %s: %s", bait, live, header)
			}
		}
		if !strings.Contains(header, "&lt;!") {
			t.Errorf("a prompt of %q lost its text entirely: %s", bait, header)
		}
	}
}
