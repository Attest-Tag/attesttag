package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"attesttag/internal/app"
)

// A check the sandbox stopped before it finished is reported as one that did not finish, never as
// one that failed (killed.go): in the result, the pull request, the ticket comment and the brief the
// coding agent is given.

const likelyOOM = "SIGKILL, most likely out of memory"

func TestKilledBy(t *testing.T) {
	for _, c := range []struct {
		name          string
		out           procOut
		before, after int64
		want          string
	}{
		{"the cgroup counted an OOM kill — a test runner's worker, reported as a failure",
			procOut{Code: 1, Output: "Jest worker encountered 4 child process exceptions"}, 3, 4, "out of memory"},
		{"the command itself killed", procOut{Code: -1, Signal: sigKill}, -1, -1, likelyOOM},
		{"a shell passing on a child's SIGKILL", procOut{Code: 137}, -1, -1, likelyOOM},
		{"a shell's own word for it", procOut{Code: 1, Output: "PASS a.test.ts\nPASS b.test.ts\nKilled\n"}, -1, -1, likelyOOM},
		{"yarn's", procOut{Code: 1, Output: `error Command failed with signal "SIGKILL".`}, -1, -1, likelyOOM},
		{"Node's heap", procOut{Code: 134, Output: "FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory"},
			-1, -1, "out of memory in the JavaScript heap"},
		{"an ordinary failure", procOut{Code: 1, Output: "Tests: 2 failed, 40 passed"}, 3, 3, ""},
		{"a test that mentions being killed is not a kill", procOut{Code: 1, Output: "Killed\n--- FAIL: TestReaper"}, -1, -1, ""},
		{"a crash is the code's", procOut{Code: -1, Signal: 11}, -1, -1, ""},
		{"the counter unreadable and nothing else", procOut{Code: 2}, -1, -1, ""},
	} {
		if got := killedBy(c.out, c.before, c.after); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOOMKillsReadsTheCgroupCount(t *testing.T) {
	dir := t.TempDir()
	v2, v1 := filepath.Join(dir, "memory.events"), filepath.Join(dir, "memory.oom_control")
	os.WriteFile(v2, []byte("low 0\nhigh 0\nmax 12\noom 3\noom_kill 2\noom_group_kill 0\n"), 0o644)
	os.WriteFile(v1, []byte("oom_kill_disable 0\nunder_oom 0\noom_kill 5\n"), 0o644)
	saved := cgroupOOMCounters
	t.Cleanup(func() { cgroupOOMCounters = saved })
	for _, c := range []struct {
		paths []string
		want  int64
	}{
		{[]string{v2, v1}, 2},
		{[]string{filepath.Join(dir, "absent"), v1}, 5},
		{[]string{filepath.Join(dir, "absent")}, -1},
	} {
		cgroupOOMCounters = c.paths
		if got := oomKills(); got != c.want {
			t.Errorf("%v: %d, want %d", c.paths, got, c.want)
		}
	}
}

// Real processes: a command SIGKILLed, a shell reporting a child's SIGKILL as 137, and a run during
// which the cgroup counted an OOM kill all come back as checks that did not finish, with no failure
// counted for them; an ordinary failure, and a job the worker cancelled itself, do not.
func TestRunStepReportsAKilledCheckAsUnfinished(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals are a Unix sandbox's")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "memory.events")
	os.WriteFile(counter, []byte("oom_kill 0\n"), 0o644)
	saved := cgroupOOMCounters
	t.Cleanup(func() { cgroupOOMCounters = saved })
	cgroupOOMCounters = []string{counter}
	ws := &Workspace{JobDir: dir, RepoDir: dir, Env: baseEnv(dir, t.TempDir()), Scrub: newScrubber()}
	run := func(ctx context.Context, script string) app.JobTestRun {
		return runStep(ctx, ws, &app.RecipeStep{Argv: []string{"sh", "-c", script}}, 30*time.Second)
	}
	ctx := context.Background()
	for _, c := range []struct {
		name, script, want string
	}{
		{"SIGKILL", "echo 3 passed; kill -9 $$", likelyOOM},
		{"137 from a shell", `sh -c 'kill -9 $$'; exit $?`, likelyOOM},
		{"an OOM kill counted while it ran", "echo 'oom_kill 1' > " + counter + "; exit 1", "out of memory"},
	} {
		tr := run(ctx, c.script)
		if !tr.Ran || tr.OK || tr.Killed != c.want || tr.Failed != 0 || tr.Failing() {
			t.Errorf("%s: %+v, want did not finish (%s) with no failure counted", c.name, tr, c.want)
		}
	}
	if tr := run(ctx, "echo '2 failed'; exit 1"); tr.Killed != "" || tr.Failed != 2 || !tr.Failing() {
		t.Errorf("an ordinary failure: %+v", tr)
	}
	cancelled, cancel := context.WithCancel(ctx)
	time.AfterFunc(200*time.Millisecond, cancel)
	if tr := run(cancelled, "sleep 5"); tr.Killed != "" || tr.OK {
		t.Errorf("a run the worker cancelled itself: %+v", tr)
	}
	if got := stepLine("test after", app.JobTestRun{Ran: true, Killed: "out of memory", Seconds: 95}); got != "test after: did not finish (killed: out of memory, after 95s)" {
		t.Errorf("stepLine = %q", got)
	}
}

// The pull request leads with a killed suite as unchecked, not failing — the case a large
// JavaScript repository hit, its tests and build killed in the sandbox the same way before the
// change as after it — and its table says "did not finish", with no count of the part that ran.
// A failure that finished still heads the pull request over a check that did not, and a suite that
// failed the same way before the change is said to have, not to have been broken by it.
func TestPRBodySaysAKilledCheckDidNotFinish(t *testing.T) {
	spec := app.JobSpec{Requirement: "Remove Help from the left nav.", Constraints: app.JobConstraints{Engine: "fake", Model: "m"}}
	pass := app.JobTestRun{Ran: true, OK: true, Seconds: 30}
	killed := app.JobTestRun{Ran: true, Killed: likelyOOM, Seconds: 95, Output: "PASS a.test.ts\nKilled"}
	recipe := &app.Recipe{Source: app.RecipeSourceDetected, Test: step("test", "npm", "run", "test"), Build: step("build", "npm", "run", "build")}
	res := &app.JobResult{Recipe: recipe, PR: &app.JobPR{URL: "https://github.com/acme/web/pull/9"},
		Tests: app.JobCheck{Command: "npm run test", Before: killed, After: killed},
		Build: app.JobCheck{Command: "npm run build", Before: pass, After: pass}}
	body := prBody(spec, res, "Removed it.", recipe, 3, nil, nil)
	for _, want := range []string{
		"> **The tests did not finish: they were killed (" + likelyOOM + "), so they did not check this change.** Please run them before merging; see the output below.",
		"| test before | `npm run test` | **did not finish** (killed: " + likelyOOM + ", after 95s) |",
		"| test after | `npm run test` | **did not finish** (killed: " + likelyOOM + ", after 95s) |",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"still fail", "**fail**", "failed, 0 passed"} {
		if strings.Contains(body, bad) {
			t.Errorf("a killed suite was reported as failing (%q):\n%s", bad, body)
		}
	}
	if got := ticketComment(spec, res); !strings.Contains(got, "(tests did not finish, killed: "+likelyOOM+")") {
		t.Errorf("ticket comment: %q", got)
	}

	// The build failing after the change says more than the tests being killed, and leads.
	res.Build.After = app.JobTestRun{Ran: true, Failed: 1, Output: "error TS2304"}
	if body := prBody(spec, res, "Removed it.", recipe, 3, nil, nil); !strings.Contains(body,
		"> **The build fails after this change, and passed before it.** Opened as a draft so a person can pick it up; see the output below.") {
		t.Errorf("a broken build behind killed tests:\n%s", body)
	}
	// A suite failing as it did before the change is said to have been failing.
	res.Build.After = pass
	res.Tests = app.JobCheck{Command: "npm run test", Before: app.JobTestRun{Ran: true, Failed: 2, Passed: 40}, After: app.JobTestRun{Ran: true, Failed: 2, Passed: 41}}
	if body := prBody(spec, res, "Removed it.", recipe, 3, nil, nil); !strings.Contains(body, "> **Tests failed before this change and still fail after it.**") {
		t.Errorf("a suite failing as before:\n%s", body)
	}
	if got := verdictSentence("tests", app.CheckWorse, " in `web/`", ""); got != "More tests in `web/` fail after this change than before it" {
		t.Errorf("verdictSentence = %q", got)
	}
}

// The coding agent is told a killed baseline was killed — not "FAILED before your change — that may
// be the bug", which sends it looking for a failure in the code that is not there — and to check
// its change with a narrower run.
func TestBriefSaysAKilledBaselineWasKilled(t *testing.T) {
	killed := app.JobTestRun{Ran: true, Killed: likelyOOM, Output: "PASS a.test.ts\nKilled"}
	b := Brief{JobID: 1, Recipe: &app.Recipe{Ecosystem: "node", Test: step("test", "npm", "run", "test")}, Baseline: killed}
	got := recipeBlock(b)
	for _, want := range []string{
		"- test: npm run test  (did not finish before your change: it was killed — " + likelyOOM + " — not failed.",
		"such as only the tests for the files you change",
		"Test output before your change, up to where it was killed (" + likelyOOM + "):\nPASS a.test.ts\nKilled",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "FAILED before your change") {
		t.Errorf("a killed baseline was called a failure:\n%s", got)
	}
}
