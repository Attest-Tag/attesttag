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

func TestCallFingerprintSeesThroughFormatting(t *testing.T) {
	a := callFingerprint("web_search", `{"query":"Acme Capital","n":8}`)
	b := callFingerprint("web_search", ` { "n" : 8, "query" : "Acme Capital" } `)
	if a != b {
		t.Errorf("key order and spacing made one call into two:\n%s\n%s", a, b)
	}
	if callFingerprint("web_search", `{"query":"Acme Capital"}`) == a {
		t.Error("different arguments read as the same call")
	}
	if callFingerprint("fetch_url", `{"query":"Acme Capital","n":8}`) == a {
		t.Error("different tools read as the same call")
	}
	if callFingerprint("x", "not json ") != callFingerprint("x", "not json") {
		t.Error("arguments that are not JSON should still compare as text")
	}
}

// The run that started this: two searches, alternated, one per round.
func TestRepeatGuardLandsAPairAskedTwiceOver(t *testing.T) {
	g := newRepeatGuard()
	round := func(calls ...string) {
		fresh := false
		for _, c := range calls {
			if g.count(c, `{"q":1}`) == 1 {
				fresh = true
			}
		}
		g.endRound(fresh)
	}
	round("a")
	round("b")
	if g.stuck() {
		t.Fatal("two fresh calls are not a loop")
	}
	round("a")
	round("b")
	round("a")
	if g.stuck() {
		t.Fatal("landed after three rounds of repeats; the threshold is four")
	}
	round("b")
	if !g.stuck() {
		t.Fatal("four rounds of nothing new should land the turn")
	}
	if g.total != 4 {
		t.Errorf("repeated = %d, want 4", g.total)
	}

	// A call the turn has not made before is progress, and resets the count.
	g = newRepeatGuard()
	round("a")
	round("a")
	round("a")
	round("a")
	round("c")
	if g.quiet != 0 || g.stuck() {
		t.Errorf("a fresh call in the middle should reset the count, got quiet=%d", g.quiet)
	}
	// A round with a fresh call and a repeat in it is a fresh round.
	round("a", "d")
	if g.quiet != 0 {
		t.Errorf("a round with anything new in it is not a repeat round, got quiet=%d", g.quiet)
	}
	// The per-call count is what the loop refuses on, and it keeps counting past the limit.
	if n := g.count("a", `{"q":1}`); n != 6 {
		t.Errorf("count = %d, want 6", n)
	}
}

// loopingLLM asks for the same tool call on every round it may make one, and answers the moment
// it may not: the model as it behaved in #leads on 2026-09-21.
//
// "May not" is tool_choice:"none" and not an empty tools array, because the landing round leaves
// the array exactly as the round before it left it — the cached prefix is worth more than the few
// hundred tokens the definitions cost. See landingChoice.
type loopingLLM struct {
	mu       sync.Mutex
	calls    int
	mayCall  int    // completions the model was allowed to make a tool call on
	withDefs int    // completions that carried the tool definitions, callable or not
	last     string // every message in the last request, joined
}

func (f *loopingLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls++
	var sb strings.Builder
	for _, m := range body.Messages {
		sb.Write(m.Content)
	}
	f.last = sb.String()
	offered := len(body.Tools) > 0 && string(body.ToolChoice) != `"none"`
	if offered {
		f.mayCall++
	}
	if len(body.Tools) > 0 {
		f.withDefs++
	}
	f.mu.Unlock()

	msg := map[string]any{"role": "assistant", "content": "Jordan Lee is a VP at Acme Capital; that is all the search gave."}
	finish := "stop"
	if offered {
		msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "probe", "arguments": `{"query":"\"Acme Capital\" \"Jordan Lee\""}`},
		}}}
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices": []map[string]any{{"index": 0, "finish_reason": finish, "message": msg}},
		"usage":   map[string]any{"prompt_tokens": 900, "completion_tokens": 40},
	})
}

// A routine may spend two hundred rounds. One that asks the same thing every round is stopped
// by what it is doing, not by that number: the call runs three times, is refused twice, and
// the turn is landed on its answer — six model calls, not two hundred.
func TestTurnThatRepeatsItselfIsLandedNotBudgeted(t *testing.T) {
	st := testStore(t)
	llm := &loopingLLM{}
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
	ran := 0
	a.register(Tool{Name: "probe", Desc: "a search", Params: schema(map[string]any{"query": str("q")}, "query"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			ran++
			return "1. Jordan Lee - VP @ Acme Capital | Digital", nil
		}})
	ctx := context.Background()
	r := Routine{OrgID: 1, TeamID: "T1", Channel: "C1", Prompt: "research the new leads", TZ: "UTC", Cron: "0 21 * * *"}
	id, err := st.AddRoutine(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id

	a.runRoutineNow(ctx, r, r.NextRun)

	if ran != repeatRunsAllowed {
		t.Errorf("the tool ran %d times, want %d", ran, repeatRunsAllowed)
	}
	// One fresh round, then stuckAfterRounds rounds of repeats, then the landed answer.
	if want := stuckAfterRounds + 2; llm.calls != want {
		t.Errorf("model called %d times, want %d", llm.calls, want)
	}
	if want := stuckAfterRounds + 1; llm.mayCall != want {
		t.Errorf("a tool call was allowed on %d calls, want %d — the last call must ask for none", llm.mayCall, want)
	}
	// And it asks for none without moving the tool array, which sits ahead of the system block
	// in the cached prefix: the landing call is the largest of the turn and re-reading it at
	// full price is what taking the definitions away used to cost.
	if want := stuckAfterRounds + 2; llm.withDefs != want {
		t.Errorf("the tool definitions reached %d of %d calls; the landing call dropped them and moved the cached prefix",
			llm.withDefs, want)
	}
	last := llm.last
	for _, want := range []string{"Note: this is the 2nd time", "error: refused", "Your tools have been withdrawn"} {
		if !strings.Contains(last, want) {
			t.Errorf("the model was never told %q; the transcript it saw:\n%s", want, last)
		}
	}
	// The refusals are on the activity page, as failed calls, so the run's shape is readable.
	var refused int
	if err := st.db.QueryRowContext(ctx, `select count(*) from tool_calls where ok = 0 and result like 'error: refused%'`).Scan(&refused); err != nil {
		t.Fatal(err)
	}
	if want := stuckAfterRounds + 1 - repeatRunsAllowed; refused != want {
		t.Errorf("%d refusals logged, want %d", refused, want)
	}
	// And the thread got an answer rather than "I stopped after too many tool calls".
	answered := false
	for _, p := range rec.posts() {
		if strings.HasPrefix(p, "chat.update") && strings.Contains(p, "that is all the search gave") {
			answered = true
		}
		if strings.Contains(p, "too many tool calls") {
			t.Errorf("the turn ended on the round cap instead of landing: %s", p)
		}
	}
	if !answered {
		t.Errorf("the answer never reached the thread: %v", rec.posts())
	}
	runs, _ := st.RoutineRuns(ctx, 1, r.ID, 0)
	if len(runs) != 1 || runs[0].Status != runPosted {
		t.Fatalf("runs = %+v, want one posted run", runs)
	}
}

func TestShapeFingerprintIgnoresNumbersAndNothingElse(t *testing.T) {
	if shapeFingerprint("run_js", `{"code":"p=0"}`) != shapeFingerprint("run_js", `{"code":"p=250"}`) {
		t.Error("a page offset should not make one call into two")
	}
	if shapeFingerprint("run_js", `{"code":"p=0"}`) == shapeFingerprint("run_js", `{"code":"q=0"}`) {
		t.Error("only the numbers are ignored; the rest of the code still tells calls apart")
	}
	if shapeFingerprint("a", `{"x":1}`) == shapeFingerprint("b", `{"x":1}`) {
		t.Error("different tools are different calls whatever their numbers")
	}
}

// The loop from 2026-09-21: sixteen run_js calls walking a page offset, every one of them new
// by its exact arguments, every one of them answering "total: 1000".
func TestRepeatGuardSeesAPagingLoopThatLearnsNothing(t *testing.T) {
	g := newRepeatGuard()
	args := func(p int) string {
		return fmt.Sprintf(`{"code":"let all=[];let p=%d;while(true){}"}`, p)
	}
	fresh := 0
	for p := 0; p <= 60; p += 10 {
		if g.count("run_js", args(p)) != 1 {
			t.Fatalf("p=%d is a first-time call by exact arguments; the shape pairing is what must catch it", p)
		}
		if g.progressed("run_js", args(p), "total: 1000") {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("seven calls and one answer between them: want 1 that progressed, got %d", fresh)
	}
	if g.total != 6 {
		t.Fatalf("the six calls that added nothing should be counted, got %d", g.total)
	}
}

// The half that keeps it honest: same shape, different answers is what paging looks like when
// it is working, and it must not be touched.
func TestRepeatGuardLeavesRealPagingAlone(t *testing.T) {
	g := newRepeatGuard()
	for p := 0; p < 5; p++ {
		args := fmt.Sprintf(`{"url":"https://api.example.com/tasks?page=%d"}`, p)
		if !g.progressed("http_request", args, fmt.Sprintf("rows %d…%d", p*100, p*100+99)) {
			t.Fatalf("page %d returned rows no other page returned; that is progress", p)
		}
	}
	if g.total != 0 {
		t.Fatalf("nothing here repeated, got total=%d", g.total)
	}
}

// And two writes that differ only in a number are two writes, so long as the service answers
// them differently. Requiring the result to match as well is what makes a number-blind
// fingerprint safe to act on.
func TestRepeatGuardLeavesDistinctWritesAlone(t *testing.T) {
	g := newRepeatGuard()
	first := g.progressed("create_task", `{"name":"Fix bug 1"}`, `{"id":"abc"}`)
	second := g.progressed("create_task", `{"name":"Fix bug 2"}`, `{"id":"def"}`)
	if !first || !second {
		t.Fatal("two tasks created and two ids returned: both are progress")
	}
}
