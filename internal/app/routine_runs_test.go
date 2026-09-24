package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A quiet routine decides for itself whether a run is worth anyone's attention, and the whole
// decision is one word at the front of its answer. The parse has to be forgiving in one
// direction only: a model that means "nothing to report" phrases it a dozen ways and none of
// them should reach the channel, but nothing that is actually a report may be swallowed.
func TestParseRoutineVerdict(t *testing.T) {
	for _, c := range []struct {
		name   string
		text   string
		report bool
		note   string
	}{
		{"bare verdict", "NOTHING", false, ""},
		{"verdict and note on the next line", "NOTHING\nall 6 VMs under 80% CPU", false, "all 6 VMs under 80% CPU"},
		{"lower case", "nothing\nnothing changed overnight", false, "nothing changed overnight"},
		{"the phrasing a model actually uses", "Nothing to report — all six VMs are under 80%.", false, "all six VMs are under 80%."},
		{"punctuated verdict", "NOTHING.\nqueue is empty", false, "queue is empty"},
		{"note folded onto one line", "NOTHING: disk at 41%", false, "disk at 41%"},
		{"an empty answer says nothing", "", false, ""},
		{"a real report", "web-3 is at 94% CPU and has been for an hour.", true, ""},
		// From a real run: the model did the work, decided correctly, and narrated the decision
		// instead of signalling it. Posting that sentence is the noise the mode exists to remove.
		{"narrated decision, as seen in production", "Found 9 Compute Engine VM instances (from the uptime metric over the last 15 minutes, deduplicated by instance id) — 10 or fewer, so no report posted.", false,
			"Found 9 Compute Engine VM instances (from the uptime metric over the last 15 minutes, deduplicated by instance id) — 10 or fewer, so no report posted."},
		{"narrated decision, second phrasing", "9 Compute Engine VMs were running in my-project over the last 15 minutes (1 in us-central1-a, 3 in us-central1-b, 5 in us-central1-c) — at or under the threshold, so no report was posted.", false,
			"9 Compute Engine VMs were running in my-project over the last 15 minutes (1 in us-central1-a, 3 in us-central1-b, 5 in us-central1-c) — at or under the threshold, so no report was posted."},
		{"nothing to report, mid-sentence", "All six services are healthy, nothing to report.", false, "All six services are healthy, nothing to report."},
		// The word appearing inside a report is not a verdict: only the first word is.
		{"mentions the word mid-report", "Nothing is wrong with web-1, but web-3 is at 94%.", true, ""},
		{"report that opens with another word", "All quiet except db-2, which is out of disk.", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			report, note := parseRoutineVerdict(c.text)
			if report != c.report {
				t.Errorf("report = %v, want %v (text %q)", report, c.report, c.text)
			}
			if note != c.note {
				t.Errorf("note = %q, want %q", note, c.note)
			}
		})
	}
}

// The decision tools are the real mechanism: a signal cannot be misread the way a sentence can.
// Whatever the model writes afterwards is ignored, because it is not the decision.
func TestQuietVerdictPrefersTheSignalOverTheProse(t *testing.T) {
	ctx := context.Background()
	a := &Agent{tools: map[string]Tool{}}
	tools := map[string]Tool{}
	for _, tl := range a.quietDecisionTools() {
		tools[tl.Name] = tl
	}

	// stay_quiet wins even though the text that follows reads like a report.
	c := &Call{Silent: true, FinalText: "web-3 is at 94% CPU!"}
	if _, err := tools["stay_quiet"].Run(ctx, c, []byte(`{"found":"all 9 VMs under the threshold"}`)); err != nil {
		t.Fatal(err)
	}
	report, body, note := c.quietVerdict()
	if report || body != "" {
		t.Errorf("stay_quiet still posted: report=%v body=%q", report, body)
	}
	if note != "all 9 VMs under the threshold" {
		t.Errorf("note = %q", note)
	}

	// report_now posts the text it was given, not whatever the turn happened to end with.
	c2 := &Call{Silent: true, FinalText: "done"}
	if _, err := tools["report_now"].Run(ctx, c2, []byte(`{"report":"web-3 is at 94% CPU and has been for an hour."}`)); err != nil {
		t.Fatal(err)
	}
	report, body, _ = c2.quietVerdict()
	if !report || body != "web-3 is at 94% CPU and has been for an hour." {
		t.Errorf("report_now = %v / %q", report, body)
	}

	// An empty report is refused rather than posted as a blank message.
	if _, err := tools["report_now"].Run(ctx, &Call{Silent: true}, []byte(`{"report":"  "}`)); err == nil {
		t.Error("an empty report should be refused")
	}

	// And with no tool call at all, it falls back to reading the text.
	c3 := &Call{Silent: true, FinalText: "web-3 is at 94% CPU."}
	if report, body, _ := c3.quietVerdict(); !report || body != "web-3 is at 94% CPU." {
		t.Errorf("fallback = %v / %q", report, body)
	}
}

// "Nothing is wrong with web-1, but web-3 is at 94%" has to post. Reading it as a verdict would
// hide the one run that mattered, which is the only failure mode of this feature that costs
// anything — so it gets its own test rather than sitting in the table above.
func TestQuietVerdictNeverSwallowsARealReport(t *testing.T) {
	for _, text := range []string{
		"Nothing is wrong with web-1, but web-3 is at 94% CPU.",
		"nothing has changed on the front end; the database is out of disk.",
		"Two things: nothing on the queue, and billing-2 is down.",
	} {
		if report, note := parseRoutineVerdict(text); !report {
			t.Errorf("stayed quiet on a report: %q (note %q)", text, note)
		}
	}
}

func TestRoutineRunLog(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id, err := st.AddRoutine(ctx, Routine{OrgID: 1, TeamID: "T1", Channel: "C1", Cron: "0 21 * * *",
		TZ: "UTC", Prompt: "check the VMs", Notify: notifyWhenNeeded, NotifyWhen: "any VM over 80%"})
	if err != nil {
		t.Fatal(err)
	}

	// The mode round-trips: a routine read back has to know it is quiet, or the scheduler posts.
	rs, err := st.Routines(ctx, 1, "")
	if err != nil || len(rs) != 1 {
		t.Fatalf("routines = %d (%v), want 1", len(rs), err)
	}
	if rs[0].Notify != notifyWhenNeeded || rs[0].NotifyWhen != "any VM over 80%" {
		t.Errorf("notify = %q/%q, want when_needed/any VM over 80%%", rs[0].Notify, rs[0].NotifyWhen)
	}

	// A quiet run's output is kept even though nobody was told: this row is the only record it
	// ran at all.
	if _, err := st.AddRoutineRun(ctx, RoutineRun{OrgID: 1, RoutineID: id, TeamID: "T1", Channel: "C1",
		ThreadTS: "routine:1:123", Status: runQuiet, Reason: "all 6 VMs under 80%",
		Output: "NOTHING\nall 6 VMs under 80%", TokensIn: 900, TokensOut: 40, CostUSD: 0.003,
		StartedAt: "2026-09-08 21:00:00", FinishedAt: "2026-09-08 21:00:04", MS: 4200}); err != nil {
		t.Fatal(err)
	}
	runs, err := st.RoutineRuns(ctx, 1, id, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d (%v), want 1", len(runs), err)
	}
	got := runs[0]
	if got.Status != runQuiet || got.Reason != "all 6 VMs under 80%" || got.CostUSD != 0.003 || got.MS != 4200 {
		t.Errorf("run round trip lost something: %+v", got)
	}
	if got.Output == "" {
		t.Error("a quiet run's output was dropped; the log is the only place it exists")
	}

	// And by id, which is what the console reads for the untruncated output.
	one, err := st.RoutineRunByID(ctx, 1, got.ID)
	if err != nil || one.Output != got.Output {
		t.Errorf("RoutineRunByID = %+v (%v)", one, err)
	}
	if _, err := st.RoutineRunByID(ctx, 2, got.ID); err == nil {
		t.Error("another organisation read the run by id")
	}
	if other, _ := st.RoutineRuns(ctx, 2, id, 0); len(other) != 0 {
		t.Errorf("another organisation listed the runs: %+v", other)
	}
}

// A routine may run every 15 minutes forever, so the log has a ceiling and the newest runs are
// the ones that survive it.
func TestRoutineRunLogTrimsToTheNewest(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	for i := 0; i < routineRunsKept+15; i++ {
		if _, err := st.AddRoutineRun(ctx, RoutineRun{OrgID: 1, RoutineID: 7, Status: runPosted,
			Output: "run", Reason: time.Unix(int64(i), 0).UTC().Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := st.RoutineRuns(ctx, 1, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != routineRunsKept {
		t.Fatalf("kept %d runs, want %d", len(runs), routineRunsKept)
	}
	// Newest first, and the newest is the last one written.
	if want := time.Unix(int64(routineRunsKept+14), 0).UTC().Format(time.RFC3339); runs[0].Reason != want {
		t.Errorf("newest run = %q, want %q — the trim kept the wrong end", runs[0].Reason, want)
	}

	// Deleting the routine takes its history with it.
	rid, err := st.AddRoutine(ctx, Routine{OrgID: 1, TeamID: "T1", Channel: "C1", Cron: "0 9 * * *", TZ: "UTC", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	st.AddRoutineRun(ctx, RoutineRun{OrgID: 1, RoutineID: rid, Status: runPosted})
	if err := st.DeleteRoutine(ctx, 1, rid); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.RoutineRuns(ctx, 1, rid, 0); len(left) != 0 {
		t.Errorf("%d run(s) outlived the routine they belonged to", len(left))
	}
}

// An unknown mode must read as the one that talks. A typo in a stored value or an API body
// should never be what takes a routine out of its channel.
func TestNotifyModeFallsBackToSpeaking(t *testing.T) {
	for _, in := range []string{"", "always", "quiet", "WHEN_NEEDED", "when needed", "true"} {
		if got := notifyMode(in); got != notifyAlways {
			t.Errorf("notifyMode(%q) = %q, want %q", in, got, notifyAlways)
		}
	}
	if got := notifyMode(notifyWhenNeeded); got != notifyWhenNeeded {
		t.Errorf("notifyMode(when_needed) = %q", got)
	}
}

// The silent streamer is what makes a quiet run incapable of posting rather than merely
// unlikely to. A streamer that fails to start still falls back to a plain post — that is the
// behaviour this must not share, so the test asserts on the fallback path specifically.
func TestSilentStreamerKeepsTheAnswerAndPostsNothing(t *testing.T) {
	// A nil transport would panic on any platform call, which is the point: if this streamer
	// reaches for the platform anywhere in the flow below, the test crashes rather than quietly
	// passing.
	sl := &Chat{}
	st := sl.NewSilentStreamer("C1", "routine:1:123", "U1")
	ctx := context.Background()

	st.Task(ctx, "t1", "read_channel_history", taskRunning, "")
	st.Write(ctx, "web-3 is at ")
	st.Write(ctx, "94% CPU")
	st.Task(ctx, "t1", "read_channel_history", taskDone, "")

	if st.Started() {
		t.Error("the silent streamer opened a stream")
	}
	ts, err := st.Stop(ctx, " and climbing.", "footer")
	if err != nil || ts != "" {
		t.Errorf("Stop = %q, %v; want no message and no error", ts, err)
	}
	// The text is still there, because the routine decides from it afterwards.
	if got := st.fallbackMD.String(); !strings.Contains(got, "94% CPU and climbing.") {
		t.Errorf("the answer was lost: %q", got)
	}
	// streamTail reads the same buffer, and the turn uses it to work out what is left to send.
	if tail := streamTail(st, "web-3 is at 94% CPU and climbing."); tail != "" {
		t.Errorf("streamTail = %q, want empty — everything was already buffered", tail)
	}
}
