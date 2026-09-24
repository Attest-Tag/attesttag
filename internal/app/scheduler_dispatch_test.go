package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// gatedLLM answers every completion at once, except the ones whose prompt names the slow
// routine: those announce themselves and then wait to be let go. One run held open is the whole
// experiment — everything the scheduler does while it is held is what is being measured.
type gatedLLM struct {
	hold     string // the prompt text that blocks
	started  chan string
	release  chan struct{}
	holds    atomic.Int64 // how many times a run of the slow routine reached the model
	freeOnce sync.Once
}

// free lets every held request go. A test that fails while one is held would otherwise hang on
// httptest's Close, which waits for the request that is still inside the handler.
func (g *gatedLLM) free() { g.freeOnce.Do(func() { close(g.release) }) }

func (g *gatedLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	var sb strings.Builder
	for _, m := range body.Messages {
		sb.Write(m.Content)
	}
	prompt := sb.String()
	if strings.Contains(prompt, g.hold) {
		g.holds.Add(1)
		select {
		case g.started <- g.hold:
		default:
		}
		<-g.release
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "done"}}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2},
	})
}

// The scheduler used to wait for every run it had started before it looked at the clock again.
// One routine's wall is half an hour, so that was half an hour in which nothing else — no other
// channel, no other tenant — was dispatched at all. A run in flight must not decide when the
// next routine starts.
func TestASlowRunDoesNotHoldUpTheNextDispatch(t *testing.T) {
	llm := &gatedLLM{hold: "hold here", started: make(chan string, 1), release: make(chan struct{})}
	a, st, rec := schedulerFixture(t, llm)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slow := addDueRoutine(t, st, "a routine that will hold here until released")
	go a.runScheduler(ctx, time.Millisecond)

	select {
	case <-llm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow routine never reached the model")
	}

	// Now, with that run held open, a second routine comes due. It has to be dispatched while
	// the first is still going — which is exactly what waiting on the first made impossible.
	quick := addDueRoutine(t, st, "a routine that answers immediately")
	deadline := time.After(5 * time.Second)
	for {
		if runs, _ := st.RoutineRuns(context.Background(), 1, quick.ID, 0); len(runs) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the second routine was never dispatched: one slow run is still holding the scheduler")
		case <-time.After(2 * time.Millisecond):
		}
	}

	// And through all those ticks the slow routine stayed due — next_run only moves when a run
	// finishes — so nothing but the in-flight claim stopped it being started over and over.
	if n := llm.holds.Load(); n != 1 {
		t.Errorf("the slow routine was started %d times, want 1: a run in flight was dispatched again", n)
	}
	llm.free()

	deadline = time.After(5 * time.Second)
	for {
		if runs, _ := st.RoutineRuns(context.Background(), 1, slow.ID, 0); len(runs) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the slow run never recorded its own outcome")
		case <-time.After(2 * time.Millisecond):
		}
	}
	if posts := rec.posts(); len(posts) == 0 {
		t.Error("neither routine reached Slack")
	}
}

// A routine that is already running is not dispatched again, whoever asks — the console's "run
// now" goes straight to runRoutineNow and never passed the scheduler at all.
func TestRunNowDoesNotStartASecondRunOfTheSameRoutine(t *testing.T) {
	llm := &gatedLLM{hold: "hold here", started: make(chan string, 1), release: make(chan struct{})}
	a, st, _ := schedulerFixture(t, llm)
	r := addDueRoutine(t, st, "a routine that will hold here until released")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.runRoutineNow(context.Background(), r, r.NextRun) }()
	select {
	case <-llm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first run never reached the model")
	}

	a.runRoutineNow(context.Background(), r, r.NextRun) // must return without starting anything
	if n := llm.holds.Load(); n != 1 {
		t.Errorf("the model was called %d times, want 1: a second run started beside the first", n)
	}
	llm.free()
	wg.Wait()
	if runs, _ := st.RoutineRuns(context.Background(), 1, r.ID, 0); len(runs) != 1 {
		t.Errorf("runs recorded = %d, want 1", len(runs))
	}
}

// A run whose context died — a shutdown, or its own wall clock — still has to leave the row that
// says so. It is the only trace a scheduled run leaves.
func TestACancelledRunIsStillRecorded(t *testing.T) {
	llm := &gatedLLM{hold: "never", started: make(chan string, 1), release: make(chan struct{})}
	a, st, _ := schedulerFixture(t, llm)
	r := addDueRoutine(t, st, "a routine interrupted on the way")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a.runRoutineNow(ctx, r, r.NextRun)

	runs, err := st.RoutineRuns(context.Background(), 1, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs recorded = %d, want the interrupted run recorded", len(runs))
	}
	if runs[0].Status != runFailed || runs[0].Error == "" {
		t.Errorf("run = %+v, want it recorded as failed with a reason", runs[0])
	}
}

func schedulerFixture(t *testing.T, llm http.Handler) (*Agent, *Store, *recordingSlack) {
	t.Helper()
	st := testStore(t)
	rec := &recordingSlack{}
	slackSrv := httptest.NewServer(rec)
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)
	// Registered last, so it runs first: a held request has to be let go before httptest waits
	// for it, or a test that fails mid-hold hangs instead of reporting.
	if g, ok := llm.(*gatedLLM); ok {
		t.Cleanup(g.free)
	}
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	a := &Agent{
		store: st, tools: map[string]Tool{}, runs: map[int64]*runHandle{}, loc: time.UTC,
		slacks:   testRegistry(sl),
		settings: newSettingsCache(st, Config{}),
		llm:      NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"}),
	}
	return a, st, rec
}

func addDueRoutine(t *testing.T, st *Store, prompt string) Routine {
	t.Helper()
	r := Routine{OrgID: 1, TeamID: "T1", Channel: "C1", Cron: "*/5 * * * *", TZ: "UTC", Prompt: prompt,
		NextRun: time.Now().UTC().Add(-time.Minute).Format(time.DateTime), Enabled: true}
	id, err := st.AddRoutine(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	return r
}
