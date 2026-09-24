package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// diggingLLM calls a tool for as long as it is allowed to, and answers the moment it is not.
// It is the model this whole design is about: left alone it would spend every round it is given
// and never write a word.
//
// "Allowed to" is two things, because the turn loop stops asking for calls without taking the
// tools off the wire — see landingChoice. An empty tools array and tool_choice:"none" both mean
// no call, and a provider honours both; ignoresNone is the one that does not, which is a real
// provider behaviour this code has to survive rather than a hypothetical.
type diggingLLM struct {
	mu          sync.Mutex
	calls       int
	noCall      int  // completions the model was not allowed to call a tool on, either way
	ignoresNone bool // a provider that calls a tool anyway when asked for none
	answer      string
	toolName    string
}

func (f *diggingLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls++
	askedForNone := string(body.ToolChoice) == `"none"`
	hasTools := len(body.Tools) > 0 && !(askedForNone && !f.ignoresNone)
	if len(body.Tools) == 0 || askedForNone {
		f.noCall++
	}
	f.mu.Unlock()

	msg := map[string]any{"role": "assistant", "content": f.answer}
	finish := "stop"
	if hasTools {
		msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": f.toolName, "arguments": `{}`},
		}}}
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices": []map[string]any{{"index": 0, "finish_reason": finish, "message": msg}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
	})
}

func (f *diggingLLM) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.noCall
}

// diggingFixture wires an agent whose model always wants another tool call, with one tool to call.
func diggingFixture(t *testing.T, cfg Config) (*Agent, *recordingSlack, *diggingLLM, *Store, *Chat) {
	t.Helper()
	st := testStore(t)
	llm := &diggingLLM{answer: "The tier-2 extractor is timing out on documents over 40 pages.", toolName: "peek"}
	rec := &recordingSlack{}
	slackSrv := httptest.NewServer(rec)
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)

	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	peek := Tool{Name: "peek", Desc: "look at something", Params: schema(map[string]any{}),
		Run: func(context.Context, *Call, json.RawMessage) (string, error) { return "nothing conclusive", nil }}
	a := &Agent{
		cfg: cfg, store: st, tools: map[string]Tool{"peek": peek}, order: []string{"peek"},
		runs: map[int64]*runHandle{}, loc: time.UTC, slacks: testRegistry(sl),
		settings: newSettingsCache(st, cfg),
		llm:      NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"}),
	}
	return a, rec, llm, st, sl
}

// Not every provider honours tool_choice:"none" — the same ones that ignore a named tool_choice,
// which this loop already has a repair for. The landing round leaves the tool array on the wire
// to keep the cached prefix, so one of those providers can still come back with a call. It must
// not be run: the turn is landing because it has no budget left for another result, and running
// one anyway is how a spent turn used to end on an apology instead of an answer.
func TestLandingSurvivesAProviderThatIgnoresToolChoiceNone(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12}
	a, rec, llm, st, sl := diggingFixture(t, cfg)
	llm.ignoresNone = true

	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000700", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: "1700000000.000700", UserID: "U1",
		Text: "what is causing this?", Kind: "channel", Session: sess,
		Streamer: sl.NewStreamer("C1", "1700000000.000700", "U1")}
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	if c.FinalText != llm.answer {
		t.Errorf("final text %q, want the model's answer", c.FinalText)
	}
	// Three rounds, and one repeat of the landing call with the tools genuinely gone — the model
	// answered the first landing call with nothing but a tool call, so there was no text to post.
	if calls, _ := llm.counts(); calls != 4 {
		t.Errorf("model called %d times, want 4: three rounds and one repeat without tools", calls)
	}
	for _, p := range rec.posts() {
		if strings.Contains(p, "too many tool calls") {
			t.Errorf("the thread was told the run gave up: %s", p)
		}
	}
}

// A turn that runs out of rounds still has to answer. Everything it found is in its context by
// then, and the version that ended on "I stopped after too many tool calls" threw all of it away.
func TestRoundCapEndsInAnAnswerNotAnApology(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12}
	a, rec, llm, st, sl := diggingFixture(t, cfg)

	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000100", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: "1700000000.000100", UserID: "U1",
		Text: "what is causing this?", Kind: "channel", Session: sess,
		Streamer: sl.NewStreamer("C1", "1700000000.000100", "U1")}
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	calls, noCall := llm.counts()
	if calls != 3 {
		t.Errorf("model called %d times, want the 3 rounds it was given", calls)
	}
	if noCall != 1 {
		t.Errorf("%d completions were asked for without a tool call, want exactly the last round", noCall)
	}
	if c.FinalText != llm.answer {
		t.Errorf("final text %q, want the model's answer", c.FinalText)
	}
	for _, p := range rec.posts() {
		if strings.Contains(p, "too many tool calls") {
			t.Errorf("the thread was told the run gave up: %s", p)
		}
	}
}

// The last round is also reached by the clock: a turn with seconds left must spend them writing
// rather than starting a tool call it cannot finish.
func TestRunningOutOfTimeAlsoLandsSoftly(t *testing.T) {
	cfg := Config{Model: "test", MaxToolRounds: 20, TurnMaxMinutes: 12}
	a, _, llm, st, sl := diggingFixture(t, cfg)
	ctx := context.Background()
	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000200", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	// A wall shorter than one round's reserve: the first round is the last one.
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: "1700000000.000200", UserID: "U1",
		Text: "what is causing this?", Kind: "channel", Session: sess, MaxRounds: 20, Wall: 30 * time.Second,
		Streamer: sl.NewStreamer("C1", "1700000000.000200", "U1")}
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls, noCall := llm.counts(); calls != 1 || noCall != 1 {
		t.Errorf("calls=%d no_call=%d, want one round, answered with no tool call", calls, noCall)
	}
	if c.FinalText != llm.answer {
		t.Errorf("final text %q, want the model's answer", c.FinalText)
	}
}

// How hard the bot digs is a property of the room it is in: an alerts channel reading logs needs
// more rounds than a chat thread, and the number is inherited the way every scope setting is.
func TestRoundsInheritFromTheNarrowestScope(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Model: "test", MaxToolRounds: 12}
	a, _, _, st, _ := diggingFixture(t, cfg)
	c := &Call{OrgID: orgID, TeamID: "T1", Channel: "C1"}
	settings := a.settings.Get(ctx, orgID)

	if got := a.roundsFor(ctx, c, settings); got != 12 {
		t.Errorf("with nothing set, rounds=%d want the account default of 12", got)
	}
	team, err := st.UpsertScope(ctx, orgID, "team", "T1", "T1", "Workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetScopeMaxToolRounds(ctx, orgID, team.ID, 25); err != nil {
		t.Fatal(err)
	}
	if got := a.roundsFor(ctx, c, settings); got != 25 {
		t.Errorf("rounds=%d, want the workspace's 25", got)
	}
	channel, err := st.UpsertScope(ctx, orgID, "channel", "T1", "C1", "#alerts")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetScopeMaxToolRounds(ctx, orgID, channel.ID, 40); err != nil {
		t.Fatal(err)
	}
	if got := a.roundsFor(ctx, c, settings); got != 40 {
		t.Errorf("rounds=%d, want the channel's 40", got)
	}
	// A run carrying its own budget is not subject to the room it happens to be in.
	if got := a.roundsFor(ctx, &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", MaxRounds: 60}, settings); got != 60 {
		t.Errorf("rounds=%d, want the investigation's own 60", got)
	}
	// And no setting anywhere can ask for more than the process will do.
	if err := st.SetScopeMaxToolRounds(ctx, orgID, channel.ID, 100000); err != nil {
		t.Fatal(err)
	}
	if got := a.roundsFor(ctx, c, settings); got != maxRoundsCeiling {
		t.Errorf("rounds=%d, want the ceiling of %d", got, maxRoundsCeiling)
	}
}

// A long question must never be the reason a short one is refused.
func TestDeepRunsAreNotCountedAgainstTheInFlightCap(t *testing.T) {
	a, _, _, _, _ := diggingFixture(t, Config{Model: "test", PlatformMaxInFlightPerOrg: 1})
	ctx := context.Background()
	deep := &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", MaxRounds: 40}
	_, end := a.beginRun(ctx, deep)
	defer end()
	if n := a.inFlight(orgID); n != 0 {
		t.Fatalf("in-flight counts %d with only an investigation running, want 0", n)
	}
	ordinary := &Call{OrgID: orgID, TeamID: "T1", Channel: "C2"}
	_, end2 := a.beginRun(ctx, ordinary)
	defer end2()
	if n := a.inFlight(orgID); n != 1 {
		t.Fatalf("in-flight counts %d, want just the ordinary turn", n)
	}
}

// The queue is what makes a ten-minute answer survive a deploy: a run whose container went away
// leaves a row whose lease expires, and the next container picks it up.
func TestAnInterruptedInvestigationIsClaimedAgain(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	id, err := st.EnqueueInvestigation(ctx, &Investigation{OrgID: orgID, TeamID: "T1", Channel: "C1",
		ThreadTS: "1700000000.000100", Requester: "U1", Question: "why is tier-2 failing?", Rounds: 40, Minutes: 12})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.claimInvestigation(ctx)
	if err != nil || first == nil {
		t.Fatalf("claim: %v %v", first, err)
	}
	if first.ID != id || first.Attempts != 1 || first.Rounds != 40 {
		t.Fatalf("claimed %+v, want id %d on its first attempt with its budget intact", first, id)
	}
	// Held: nobody else may take it while the lease stands.
	if v, err := st.claimInvestigation(ctx); err != nil || v != nil {
		t.Fatalf("a leased run was claimed twice: %v %v", v, err)
	}
	// The container running it disappears; the lease is what notices.
	if _, err := st.db.ExecContext(ctx, `update investigations set lease_until=1 where id=?`, id); err != nil {
		t.Fatal(err)
	}
	again, err := st.claimInvestigation(ctx)
	if err != nil || again == nil {
		t.Fatalf("an abandoned run was not picked up: %v %v", again, err)
	}
	if again.ID != id || again.Attempts != 2 {
		t.Fatalf("reclaimed %+v, want id %d on attempt 2", again, id)
	}
	if err := st.finishInvestigation(ctx, id, "done", "", Usage{In: 100, Out: 20, CostUSD: 0.01}); err != nil {
		t.Fatal(err)
	}
	if v, err := st.claimInvestigation(ctx); err != nil || v != nil {
		t.Fatalf("a finished run was claimed again: %v %v", v, err)
	}
	if n := st.CountOpenInvestigations(ctx, orgID); n != 0 {
		t.Errorf("%d runs still count as open after one finished", n)
	}
}

// One run, one worker. Each round releases every claimer on the same instant, the way the lane's
// workers line up when they poll in step, and on Postgres that used to hand one question to
// several of them: the thread got an answer from each, and the row was charged for every run.
// SQLite runs this process's queries on one connection and cannot fail it, so the Postgres leg is
// what counts.
func TestWorkersPollingTogetherClaimARunOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	for round := range 20 {
		id, err := st.EnqueueInvestigation(ctx, &Investigation{OrgID: orgID, TeamID: "T1", Channel: "C1",
			ThreadTS: "1700000000.000100", Question: "why?", Rounds: 40, Minutes: 12})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		claims := make(chan *Investigation, 8)
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				v, err := st.claimInvestigation(ctx)
				if err != nil {
					errs <- err
				}
				if v != nil {
					claims <- v
				}
			}()
		}
		close(start)
		wg.Wait()
		close(claims)
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		if len(claims) != 1 {
			t.Fatalf("round %d: %d workers claimed investigation %d at once", round, len(claims), id)
		}
		v := <-claims
		if v.ID != id || v.Attempts != 1 {
			t.Fatalf("round %d: claimed %+v, want id %d on its first attempt", round, v, id)
		}
		if err := st.finishInvestigation(ctx, v.ID, "done", "", Usage{}); err != nil {
			t.Fatal(err)
		}
	}
}

// A run that keeps being interrupted is given up on rather than retried forever, and the thread
// that asked is told — a question dropped in silence is the failure nobody can see.
func TestAnInvestigationGivesUpAfterItsAttempts(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.EnqueueInvestigation(ctx, &Investigation{OrgID: orgID, TeamID: "T1", Channel: "C1",
		ThreadTS: "1700000000.000100", Question: "why?", Rounds: 40, Minutes: 12}); err != nil {
		t.Fatal(err)
	}
	for i := range investigationMaxAttempts {
		v, err := st.claimInvestigation(ctx)
		if err != nil || v == nil {
			t.Fatalf("attempt %d: %v %v", i+1, v, err)
		}
		if _, err := st.db.ExecContext(ctx, `update investigations set lease_until=1 where id=?`, v.ID); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := st.claimInvestigation(ctx); err != nil || v != nil {
		t.Fatalf("claimed a run whose attempts were spent: %v %v", v, err)
	}
	left, err := st.abandonedInvestigations(ctx)
	if err != nil || len(left) != 1 {
		t.Fatalf("abandoned runs: %v %v, want the one nobody finished", left, err)
	}
}

// "stop" has to reach the run that has not started yet as well as the one in flight.
func TestStopClosesQueuedInvestigations(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	for range 2 {
		if _, err := st.EnqueueInvestigation(ctx, &Investigation{OrgID: orgID, TeamID: "T1", Channel: "C1",
			ThreadTS: "1700000000.000100", Question: "why?", Rounds: 40, Minutes: 12}); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.StopQueuedInvestigations(ctx, orgID, "T1", "C1", "1700000000.000100", "U1"); n != 2 {
		t.Fatalf("stopped %d, want both queued runs", n)
	}
	if v, err := st.claimInvestigation(ctx); err != nil || v != nil {
		t.Fatalf("a stopped run was still claimed: %v %v", v, err)
	}
	open, err := st.OpenInvestigationsInThread(ctx, orgID, "T1", "C1", "1700000000.000100")
	if err != nil || len(open) != 0 {
		t.Fatalf("open runs after a stop: %v %v", open, err)
	}
}

// The tool queues one run per thread and says so rather than stacking them up.
func TestStartInvestigationQueuesOnePerThread(t *testing.T) {
	ctx := context.Background()
	a, _, _, st, sl := diggingFixture(t, Config{Model: "test", MaxToolRounds: 12})
	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000100", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: orgID, SL: sl, Channel: "C1", ThreadTS: "1700000000.000100", UserID: "U1", Kind: "channel", Session: sess}
	tool := a.investigationTool(c)
	args := json.RawMessage(`{"question":"why is tier-2 failing?","brief":"68.54%, 61 of 89 on 2026-09-09","plan":["read the errors"]}`)
	out, err := tool.Run(ctx, c, args)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !strings.Contains(out, "#1") {
		t.Errorf("the model was not told which investigation it started: %q", out)
	}
	if _, err := tool.Run(ctx, c, args); err == nil || !strings.Contains(err.Error(), "already working") {
		t.Errorf("second call in the same thread: %v, want a refusal naming the run in flight", err)
	}
	open, err := st.OpenInvestigationsInThread(ctx, orgID, "T1", "C1", "1700000000.000100")
	if err != nil || len(open) != 1 {
		t.Fatalf("open runs: %v %v, want exactly one", open, err)
	}
	if open[0].Rounds < minInvestigationRounds || open[0].Minutes < minInvestigationMinutes {
		t.Errorf("queued with budget rounds=%d minutes=%d, below the lane's floor", open[0].Rounds, open[0].Minutes)
	}
	if !strings.Contains(open[0].Brief, "68.54%") || !strings.Contains(open[0].Brief, "read the errors") {
		t.Errorf("brief lost what the reply turn already knew: %q", open[0].Brief)
	}
}

// An investigation is never offered the tool that would queue another one.
func TestTheLaneCannotQueueItself(t *testing.T) {
	ctx := context.Background()
	a, _, _, st, sl := diggingFixture(t, Config{Model: "test", MaxToolRounds: 12})
	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000100", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	ask := "what is causing these failures?"
	deep := &Call{TeamID: "T1", OrgID: orgID, SL: sl, Channel: "C1", ThreadTS: "1700000000.000100", UserID: "U1",
		Kind: "channel", Text: ask, Session: sess, MaxRounds: 40}
	if _, ok := a.toolsFor(ctx, deep)["start_investigation"]; ok {
		t.Error("an investigation was offered start_investigation")
	}
	ordinary := &Call{TeamID: "T1", OrgID: orgID, SL: sl, Channel: "C1", ThreadTS: "1700000000.000100", UserID: "U1",
		Kind: "channel", Text: ask, Session: sess}
	if _, ok := a.toolsFor(ctx, ordinary)["start_investigation"]; !ok {
		t.Error("a question about a cause was not offered start_investigation")
	}
}

// End to end: a queued question is claimed, worked in the thread it came from, answered there,
// and the row closed with what it spent.
func TestTheLaneAnswersInTheThread(t *testing.T) {
	ctx := context.Background()
	a, rec, llm, st, _ := diggingFixture(t, Config{Model: "test", MaxToolRounds: 12, InvestigationWorkers: 1})
	if _, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000100", "channel", ""); err != nil {
		t.Fatal(err)
	}
	id, err := st.EnqueueInvestigation(ctx, &Investigation{OrgID: orgID, TeamID: "T1", Channel: "C1",
		ThreadTS: "1700000000.000100", Requester: "U1", Question: "why is tier-2 failing?",
		Brief: "68.54%, 61 of 89 on 2026-09-09", Rounds: 2, Minutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if worked := a.nextInvestigation(ctx); !worked {
		t.Fatal("the lane found no work with a run queued")
	}
	if calls, noCall := llm.counts(); calls != 2 || noCall != 1 {
		t.Errorf("calls=%d no_call=%d, want the two rounds it was queued with, the last one answering", calls, noCall)
	}
	// The answer goes out as a stream, so what proves it landed is the turn recorded against
	// the thread — the same row the console and the next turn's replay read.
	var answer string
	if err := st.db.QueryRowContext(ctx, `select content from turns where channel=? and thread_ts=? and role='assistant'`,
		"C1", "1700000000.000100").Scan(&answer); err != nil {
		t.Fatalf("no answer was recorded in the thread: %v", err)
	}
	if !strings.Contains(answer, "timing out") {
		t.Errorf("recorded answer %q, want the model's", answer)
	}
	if posted := strings.Join(rec.posts(), "\n"); !strings.Contains(posted, "C1") {
		t.Errorf("nothing was sent to the channel: %s", posted)
	}
	var status string
	var in, out int
	if err := st.db.QueryRowContext(ctx, `select status, tokens_in, tokens_out from investigations where id=?`, id).
		Scan(&status, &in, &out); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Errorf("status %q after a finished run, want done", status)
	}
	if in == 0 || out == 0 {
		t.Errorf("run recorded no spend (in=%d out=%d)", in, out)
	}
	if worked := a.nextInvestigation(ctx); worked {
		t.Error("a finished run was picked up again")
	}
}

// A deploy in the middle of a ten-minute answer must not be recorded as a failed answer: the
// row has to stay claimable, or the thread waits forever for a run nobody will start again.
func TestAContainerGoingAwayLeavesTheRunClaimable(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Call{run: &runHandle{}}
	if status, _ := investigationOutcome(dead, c, context.Canceled); status != "retry" {
		t.Errorf("a cancelled run came out as %q, want retry", status)
	}
	live := context.Background()
	if status, _ := investigationOutcome(live, c, errors.New("the model refused")); status != "failed" {
		t.Errorf("a real error came out as %q, want failed", status)
	}
	if status, _ := investigationOutcome(live, c, nil); status != "done" {
		t.Errorf("a finished run came out as %q, want done", status)
	}
	stopped := &Call{run: &runHandle{stopped: true, by: "U1"}}
	if status, _ := investigationOutcome(live, stopped, context.Canceled); status != "stopped" {
		t.Errorf("a run someone stopped came out as %q, want stopped", status)
	}
}

// A released run is queued again with the attempt it used, not finished.
func TestAReleasedRunGoesBackOnTheQueue(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	id, err := st.EnqueueInvestigation(ctx, &Investigation{OrgID: orgID, TeamID: "T1", Channel: "C1",
		ThreadTS: "1700000000.000100", Question: "why?", Rounds: 40, Minutes: 12})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.claimInvestigation(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.releaseInvestigation(ctx, id, "container went away", Usage{In: 50, Out: 5}); err != nil {
		t.Fatal(err)
	}
	again, err := st.claimInvestigation(ctx)
	if err != nil || again == nil {
		t.Fatalf("a released run was not claimable: %v %v", again, err)
	}
	if again.Attempts != 2 || again.TokensIn != 50 {
		t.Errorf("reclaimed %+v, want attempt 2 with what the first attempt spent kept", again)
	}
}

// The brief the reply turn wrote is what the run starts from, so it has to reach the model.
func TestTheBriefReachesTheRun(t *testing.T) {
	v := &Investigation{Question: "why is tier-2 failing?", Brief: "68.54%, 61 of 89 on 2026-09-09"}
	brief := investigationBrief(v, 40, 12*time.Minute)
	for _, want := range []string{"why is tier-2 failing?", "68.54%", "40 tool rounds", "could not establish"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief is missing %q:\n%s", want, brief)
		}
	}
}

// The tool is not carried by every turn: its definition is re-sent on every round, and a
// conversation that is not about a failure would pay for it and never call it.
func TestOnlyAsksAboutCausesCarryTheTool(t *testing.T) {
	ctx := context.Background()
	a, _, _, st, sl := diggingFixture(t, Config{Model: "test", MaxToolRounds: 12})
	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000100", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(text string) *Call {
		return &Call{TeamID: "T1", OrgID: orgID, SL: sl, Channel: "C1", ThreadTS: "1700000000.000100",
			UserID: "U1", Kind: "channel", Text: text, Session: sess}
	}
	for _, text := range []string{
		"what is cause of this issue ?",
		"and which client document are these",
		"why is the tier-2 extractor failing?",
		"can you look into the spike in errors last night",
		"root cause please",
	} {
		if _, ok := a.toolsFor(ctx, call(text))["start_investigation"]; !ok {
			t.Errorf("%q was not offered the lane", text)
		}
	}
	for _, text := range []string{
		"hello there",
		"thanks, that worked",
		"summarise this thread for me",
		"book the room for 3pm",
	} {
		if _, ok := a.toolsFor(ctx, call(text))["start_investigation"]; ok {
			t.Errorf("%q was made to carry the lane's definition", text)
		}
	}
}
