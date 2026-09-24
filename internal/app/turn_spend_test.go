package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// What each completion spendLLM answers costs, so a usage row can be traced to the calls in it.
const spendIn, spendOut, spendCost = 900, 40, 0.0125

// spendLLM answers its first completion with a tool call, so the turn goes round again, and
// does what then says with the second: "fail" it, "hang" until the run is stopped, or "answer".
type spendLLM struct {
	then  string
	held  chan struct{} // closed when the hanging call arrives
	mu    sync.Mutex
	calls int
}

func (f *spendLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read to the end first: the server only notices a client hanging up once the request body
	// is consumed, and until then a stopped turn's hanging call sat out its whole ten seconds.
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	msg := map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
		"id": "call_1", "type": "function", "function": map[string]any{"name": "peek", "arguments": `{}`}}}}
	if n > 1 {
		switch f.then {
		case "fail":
			// What a long turn dies of in a later round: the prompt has outgrown the model's context,
			// and the provider refuses it once every round before has been billed. A 400 is also
			// one the SDK does not retry, where a 500 would be tried twice more, with a backoff.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"This model's maximum context length is 131072 tokens.","type":"invalid_request_error"}}`))
			return
		case "hang":
			if n == 2 {
				close(f.held)
			}
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		msg = map[string]any{"role": "assistant", "content": "Nothing conclusive turned up."}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": msg}},
		"usage":   map[string]any{"prompt_tokens": spendIn, "completion_tokens": spendOut, "cost": spendCost},
	})
}

func (f *spendLLM) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func spendFixture(t *testing.T, llm *spendLLM) (*Agent, *Call, *Store) {
	t.Helper()
	st := testStore(t)
	slackSrv := httptest.NewServer(&recordingSlack{})
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	peek := Tool{Name: "peek", Desc: "look at something", Params: schema(map[string]any{}),
		Run: func(context.Context, *Call, json.RawMessage) (string, error) { return "nothing conclusive", nil }}
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12}
	a := &Agent{
		cfg: cfg, store: st, tools: map[string]Tool{"peek": peek}, order: []string{"peek"},
		runs: map[int64]*runHandle{}, loc: time.UTC, slacks: testRegistry(sl),
		settings: newSettingsCache(st, cfg),
		llm:      NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"}),
	}
	const ts = "1700000000.000700"
	sess, err := st.EnsureSession(context.Background(), "T1", "C1", ts, "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: ts, UserID: "U1",
		Text: "why did last night's import fail?", Kind: "channel", Session: sess, Streamer: sl.NewStreamer("C1", ts, "U1")}
	return a, c, st
}

type spendRow struct {
	user, model string
	in, out     int
	cost        float64
}

func spendRows(t *testing.T, st *Store, threadTS string) []spendRow {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(),
		`select user_id, model, tokens_in, tokens_out, cost_usd from usage where org_id=? and thread_ts=?`, 1, threadTS)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []spendRow
	for rows.Next() {
		var r spendRow
		if err := rows.Scan(&r.user, &r.model, &r.in, &r.out, &r.cost); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The first round was bought and the second failed. Only the answer and the round cap used to
// write a usage row, so this turn wrote none: its first round was never taken off credit, and no
// budget and no rate limit ever saw it.
func TestAFailedTurnRecordsWhatItHadAlreadySpent(t *testing.T) {
	llm := &spendLLM{then: "fail"}
	a, c, st := spendFixture(t, llm)
	ctx := context.Background()
	if err := a.Run(ctx, c); err == nil {
		t.Fatal("the second round failed and the turn reported no error")
	}
	if n := llm.called(); n != 2 {
		t.Fatalf("model called %d times, want 2: one round paid for, one refused", n)
	}
	rows := spendRows(t, st, c.ThreadTS)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if r := rows[0]; r.in != spendIn || r.out != spendOut || !near(r.cost, spendCost) || r.user != "U1" || r.model != "test" {
		t.Errorf("usage row = %+v, want the first round's %d in, %d out and $%v, for U1 on test", r, spendIn, spendOut, spendCost)
	}
	if spent, _ := st.MonthSpend(ctx, 1, "T1", "C1"); !near(spent, spendCost) {
		t.Errorf("the channel's month spend = %v, want %v", spent, spendCost)
	}
}

// A stopped run is written once, by Run on its way out, and not a second time by finishStopped.
func TestAStoppedTurnRecordsItsSpendOnce(t *testing.T) {
	llm := &spendLLM{then: "hang", held: make(chan struct{})}
	a, c, st := spendFixture(t, llm)
	ts := c.ThreadTS
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background(), c) }()
	select {
	case <-llm.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never reached its second round")
	}
	if n := a.StopRuns(1, "T1", "C1", ts, "U9"); n != 1 {
		t.Fatalf("stopped %d runs, want 1", n)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a stopped run is not a failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stopped run never returned")
	}
	rows := spendRows(t, st, ts)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if r := rows[0]; r.in != spendIn || r.out != spendOut {
		t.Errorf("usage row = %+v, want the first round's %d in and %d out", r, spendIn, spendOut)
	}
}

// The answer no longer writes its own row, so this is what still holds it to one, with both
// rounds in it and the same totals on the assistant turn the thread keeps.
func TestAnAnsweredTurnRecordsItsSpendOnce(t *testing.T) {
	llm := &spendLLM{then: "answer"}
	a, c, st := spendFixture(t, llm)
	ctx := context.Background()
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	rows := spendRows(t, st, c.ThreadTS)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if r := rows[0]; r.in != 2*spendIn || r.out != 2*spendOut || !near(r.cost, 2*spendCost) {
		t.Errorf("usage row = %+v, want both rounds: %d in, %d out, $%v", r, 2*spendIn, 2*spendOut, 2*spendCost)
	}
	var in, out int
	if err := st.db.QueryRowContext(ctx, `select tokens_in, tokens_out from turns where team_id=? and thread_ts=? and role='assistant'`,
		"T1", c.ThreadTS).Scan(&in, &out); err != nil {
		t.Fatalf("the answer was not stored as a turn: %v", err)
	}
	if in != 2*spendIn || out != 2*spendOut {
		t.Errorf("assistant turn tokens = %d in, %d out, want %d and %d", in, out, 2*spendIn, 2*spendOut)
	}
}
