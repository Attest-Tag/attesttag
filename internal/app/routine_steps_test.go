package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRoutineSteps(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		steps     int
		err       bool
	}{
		{name: "none", raw: "", steps: 0},
		{name: "empty array", raw: "[]", steps: 0},
		{name: "one call", raw: `[{"tool":"http_request","args":{"method":"GET","url":"https://api.example.com/x"}}]`, steps: 1},
		{name: "args may be left out", raw: `[{"tool":"read_channel_history"}]`, steps: 1},
		{name: "two calls", raw: `[{"tool":"a"},{"tool":"b"}]`, steps: 2},
		{name: "not an array", raw: `{"tool":"a"}`, err: true},
		{name: "no tool name", raw: `[{"args":{}}]`, err: true},
		{name: "a tool name with a space is not one", raw: `[{"tool":"http request"}]`, err: true},
		{name: "args must be an object", raw: `[{"tool":"a","args":[1,2]}]`, err: true},
		{name: "more steps than allowed", raw: `[{"tool":"a"},{"tool":"b"},{"tool":"c"},{"tool":"d"},{"tool":"e"}]`, err: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			steps, err := parseRoutineSteps(c.raw)
			if c.err {
				if err == nil {
					t.Fatalf("parsed %q, want an error — the person typing it is the only one who can fix it", c.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRoutineSteps(%q) = %v", c.raw, err)
			}
			if len(steps) != c.steps {
				t.Fatalf("steps = %d, want %d", len(steps), c.steps)
			}
			for _, s := range steps {
				if !json.Valid(s.Args) {
					t.Errorf("step %q has no usable arguments: %q", s.Tool, s.Args)
				}
			}
		})
	}
}

// A pinned call that hard-codes a date is wrong by the next morning, so the arguments carry
// placeholders and the run fills them. Everything they can produce is digits and punctuation,
// which is what makes substituting into a JSON string safe.
func TestExpandRoutineVars(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-09-10 01:30 UTC is already 07:15 on the 10th in Kathmandu: the date has to come from
	// the routine's own zone, not the container's.
	now := time.Date(2026, 9, 10, 1, 30, 0, 0, time.UTC)
	for _, c := range []struct{ in, want string }{
		{`{"url":"x?d={{date}}"}`, `{"url":"x?d=2026-09-10"}`},
		{`{"url":"x?d={{yesterday}}"}`, `{"url":"x?d=2026-09-09"}`},
		{`{"url":"x?d={{date-7}}"}`, `{"url":"x?d=2026-09-03"}`},
		{`{"url":"x?t={{now}}"}`, `{"url":"x?t=2026-09-10T01:30:00Z"}`},
		{`{"url":"x?t={{now-15m}}"}`, `{"url":"x?t=2026-09-10T01:15:00Z"}`},
		{`{"url":"x?t={{epoch-1h}}"}`, fmt.Sprintf(`{"url":"x?t=%d"}`, now.Add(-time.Hour).Unix())},
		{`{"url":"x?a={{date}}&b={{date}}"}`, `{"url":"x?a=2026-09-10&b=2026-09-10"}`},
		{`{"url":"nothing to fill"}`, `{"url":"nothing to fill"}`},
		{`{"url":"{{not_a_placeholder}}"}`, `{"url":"{{not_a_placeholder}}"}`},
	} {
		if got := expandRoutineVars(c.in, loc, now); got != c.want {
			t.Errorf("expandRoutineVars(%s) = %s, want %s", c.in, got, c.want)
		}
		if !json.Valid([]byte(expandRoutineVars(c.in, loc, now))) {
			t.Errorf("expandRoutineVars(%s) produced invalid JSON", c.in)
		}
	}
}

// stepTool is a tool a routine can pin, standing in for the one API call its author already
// knows it needs. calls counts how often it actually ran, because "the step ran exactly once"
// is half of what these tests are asserting.
func stepTool(name, out string, fail bool) (Tool, *atomic.Int64) {
	var calls atomic.Int64
	return Tool{
		Name: name, Desc: "test", Params: schema(map[string]any{"q": str("anything")}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			calls.Add(1)
			if fail {
				return "", fmt.Errorf("the endpoint is down")
			}
			return out + " " + string(args), nil
		},
	}, &calls
}

// The whole point: a routine whose work is one known call does not need a model to find it.
// Zero completions, one HTTP call, and the output posted as it came back.
func TestRawRoutineNeverCallsTheModel(t *testing.T) {
	a, rec, llm, st, r := quietFixture(t, "should never be asked",
		Routine{Prompt: "queue depth", Steps: `[{"tool":"check_queue","args":{"q":"depth"}}]`, Finish: finishRaw})
	tool, calls := stepTool("check_queue", "depth=41", false)
	a.register(tool)
	ctx := context.Background()

	a.runRoutineNow(ctx, r, r.NextRun)

	if llm.calls != 0 {
		t.Errorf("the model was called %d time(s) — a raw routine exists to call it zero times", llm.calls)
	}
	if calls.Load() != 1 {
		t.Errorf("the pinned step ran %d time(s), want 1", calls.Load())
	}
	posts := rec.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d (%v), want exactly one: the result, posted once", len(posts), posts)
	}
	if !strings.Contains(posts[0], "depth=41") {
		t.Errorf("the step's output never reached the channel: %q", posts[0])
	}
	runs, err := st.RoutineRuns(ctx, 1, r.ID, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d (%v), want 1", len(runs), err)
	}
	if runs[0].Status != runPosted || !strings.Contains(runs[0].Output, "depth=41") {
		t.Errorf("run recorded as %q / %q", runs[0].Status, runs[0].Output)
	}
	if runs[0].TokensIn != 0 || runs[0].CostUSD != 0 {
		t.Errorf("a run that called no model was charged %d tokens / $%.4f", runs[0].TokensIn, runs[0].CostUSD)
	}
}

// Raw plus "only when it matters" is the cheap health check: silence while the call works, the
// error the moment it does not, and no model on either path.
func TestRawQuietRoutineSpeaksOnlyWhenAStepFails(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		a, rec, llm, st, r := quietFixture(t, "unused",
			Routine{Prompt: "ping the API", Steps: `[{"tool":"ping"}]`, Finish: finishRaw,
				Notify: notifyWhenNeeded, NotifyWhen: "it is down"})
		tool, _ := stepTool("ping", "ok", false)
		a.register(tool)
		a.runRoutineNow(context.Background(), r, r.NextRun)

		if posts := rec.posts(); len(posts) != 0 {
			t.Errorf("a healthy check reached Slack: %v", posts)
		}
		if llm.calls != 0 {
			t.Errorf("the model was called %d time(s)", llm.calls)
		}
		runs, _ := st.RoutineRuns(context.Background(), 1, r.ID, 0)
		if len(runs) != 1 || runs[0].Status != runQuiet {
			t.Fatalf("run = %+v, want one recorded as quiet", runs)
		}
	})
	t.Run("broken", func(t *testing.T) {
		a, rec, _, st, r := quietFixture(t, "unused",
			Routine{Prompt: "ping the API", Steps: `[{"tool":"ping"}]`, Finish: finishRaw,
				Notify: notifyWhenNeeded, NotifyWhen: "it is down"})
		tool, _ := stepTool("ping", "", true)
		a.register(tool)
		a.runRoutineNow(context.Background(), r, r.NextRun)

		posts := rec.posts()
		if len(posts) != 1 {
			t.Fatalf("posts = %d (%v), want the failure posted once", len(posts), posts)
		}
		if !strings.Contains(posts[0], "the endpoint is down") {
			t.Errorf("the failure never reached the channel: %q", posts[0])
		}
		runs, _ := st.RoutineRuns(context.Background(), 1, r.ID, 0)
		if len(runs) != 1 || runs[0].Status != runPosted {
			t.Fatalf("run = %+v, want one recorded as posted", runs)
		}
	})
}

// With finish "answer" the model is still asked — it writes the result up — but it is asked
// once, with the output already in front of it and no tools at all. The tool list is most of
// what a round costs, so the assertion is on the wire: the request carried none.
func TestPinnedStepsAnswerInOneCallWithNoTools(t *testing.T) {
	a, rec, llm, _, r := quietFixture(t, "The queue is 41 deep, which is normal for a Monday.",
		Routine{Prompt: "report the queue depth", Steps: `[{"tool":"check_queue","args":{"q":"depth"}}]`})
	tool, calls := stepTool("check_queue", "depth=41", false)
	a.register(tool)

	a.runRoutineNow(context.Background(), r, r.NextRun)

	if llm.calls != 1 {
		t.Errorf("the model was called %d time(s), want exactly 1", llm.calls)
	}
	if calls.Load() != 1 {
		t.Errorf("the pinned step ran %d time(s), want 1", calls.Load())
	}
	if tools := llm.tools(); len(tools) != 0 {
		t.Errorf("the call carried %d tool definition(s) (%v) — the work was already done", len(tools), tools)
	}
	if p := llm.prompt(); !strings.Contains(p, "depth=41") {
		t.Errorf("the step's output never reached the model: %q", p)
	}
	if posts := rec.posts(); len(posts) == 0 || !strings.Contains(strings.Join(posts, " "), "41 deep") {
		t.Errorf("the answer never reached the channel: %v", posts)
	}
}

// A quiet routine says what it decided by calling a tool. Taking the tools away because its
// steps already ran must not take those two with them, or the decision has nowhere to go.
func TestPinnedStepsKeepTheQuietDecisionTools(t *testing.T) {
	a, _, llm, _, r := quietFixture(t, "done",
		Routine{Prompt: "check the queue", Steps: `[{"tool":"check_queue"}]`,
			Notify: notifyWhenNeeded, NotifyWhen: "the queue is over 100"})
	tool, _ := stepTool("check_queue", "depth=41", false)
	a.register(tool)

	a.runRoutineNow(context.Background(), r, r.NextRun)

	got := strings.Join(llm.tools(), ",")
	if got != "report_now,stay_quiet" && got != "stay_quiet,report_now" {
		t.Errorf("tools on the call = %q, want only the two that carry the decision", got)
	}
}

// finish "agent" is the middle setting: the steps are a head start, not the whole run, so the
// model keeps everything it would normally have.
func TestAgentFinishKeepsItsTools(t *testing.T) {
	a, _, llm, _, r := quietFixture(t, "The queue is fine.",
		Routine{Prompt: "look into the queue", Steps: `[{"tool":"check_queue"}]`, Finish: finishAgent})
	tool, _ := stepTool("check_queue", "depth=41", false)
	a.register(tool)

	a.runRoutineNow(context.Background(), r, r.NextRun)

	if tools := llm.tools(); len(tools) == 0 {
		t.Error("the agent finish was sent no tools; it exists to be able to dig further")
	}
	if p := llm.prompt(); !strings.Contains(p, "depth=41") {
		t.Errorf("the step's output never reached the model: %q", p)
	}
}

// Steps that do not parse are refused where they are written — the console form, the API, the
// Slack tool — so the only way to store a broken one is to go behind them.
func TestBrokenStepsAreRefusedOnTheWayIn(t *testing.T) {
	st := testStore(t)
	if _, err := st.AddRoutine(context.Background(), Routine{OrgID: 1, TeamID: "T1", Channel: "C1",
		Cron: "0 9 * * *", TZ: "UTC", Prompt: "x", Steps: `[{"tool":"http request"}]`}); err == nil {
		t.Error("a routine with an unusable step was stored; the mistake would surface at 6am with nobody watching")
	}
}

// And if one gets in anyway — an older row, a hand-edited database — the run fails with the
// reason rather than posting a header and breaking underneath it.
func TestBrokenStepsFailTheRunWithoutPosting(t *testing.T) {
	a, rec, llm, st, r := quietFixture(t, "unused", Routine{Prompt: "x"})
	if _, err := st.db.ExecContext(context.Background(), `update routines set steps=? where id=?`, "not json", r.ID); err != nil {
		t.Fatal(err)
	}
	r.Steps = "not json"
	a.runRoutineNow(context.Background(), r, r.NextRun)

	if posts := rec.posts(); len(posts) != 0 {
		t.Errorf("posts = %v — nothing was announced, so there is nothing to correct in the channel", posts)
	}
	if llm.calls != 0 {
		t.Errorf("the model was called %d time(s) for a routine that never got past its own steps", llm.calls)
	}
	runs, _ := st.RoutineRuns(context.Background(), 1, r.ID, 0)
	if len(runs) != 1 || runs[0].Status != runFailed {
		t.Fatalf("run = %+v, want one recorded as failed", runs)
	}
	if !strings.Contains(runs[0].Error, "steps must be") {
		t.Errorf("the run log does not say what was wrong: %q", runs[0].Error)
	}
}
