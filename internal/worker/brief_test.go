package worker

import (
	"strings"
	"testing"
	"time"

	"attesttag/internal/app"
)

// The brief asks the engine to run a check whole only when it was quick before the change; a slow
// one is the harness's to run after it, and the engine checks its change with a narrower run.
func TestBriefRunsOnlyQuickChecksWhole(t *testing.T) {
	r := &app.Recipe{Ecosystem: "node", Build: step("build", "npm", "run", "build"),
		Lint: step("lint", "npm", "run", "lint"), Test: step("test", "npm", "run", "test")}
	b := Brief{JobID: 1, Recipe: r,
		Build:    app.JobTestRun{Ran: true, OK: true, Seconds: 31},
		Baseline: app.JobTestRun{Ran: true, OK: true, Seconds: 420}}
	got := briefText(b)
	for _, want := range []string{
		"- build: npm run build  (passed before your change in 31s: quick, so run it whole before you stop)\n",
		"- lint: npm run lint\n",
		"- test: npm run test  (passed before your change, but took 7 minutes: too slow to repeat, so run only the part that covers your change)\n",
		"the narrowest run that covers it",
		"Do not spend turns making a command run that the sandbox stops",
		"which checks you ran and whether they passed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "run them yourself first") {
		t.Errorf("the brief still asks for every check to be run whole:\n%s", got)
	}

	// A run whose time is not known reads as it always did.
	b.Baseline.Seconds = 0
	if got := recipeBlock(b); !strings.Contains(got, "- test: npm run test  (passed before your change)\n") {
		t.Errorf("a pass with no time:\n%s", got)
	}

	for d, want := range map[time.Duration]string{
		300 * time.Millisecond: "under a second",
		90 * time.Second:       "90s",
		quickCheck:             "2 minutes",
		400 * time.Second:      "7 minutes",
	} {
		if got := briefDuration(d); got != want {
			t.Errorf("briefDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
