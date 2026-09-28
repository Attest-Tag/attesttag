package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The first job on a repository leaves what it worked out on the connection. That guess is
// about whatever package that job was about, and it used to be handed to every later job as if
// an admin had set it — pinning a monorepo's every job to the first one's folder and telling the
// pull request the recipe came from the console.
func TestRememberedRecipeDoesNotSteerTheNextJob(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	guess := &Recipe{Source: RecipeSourceDetected, Ecosystem: "go", Workdir: "services/api",
		Test: &RecipeStep{Name: "test", Argv: []string{"go", "test", "./..."}}}
	if err := h.b.store.SetConnectionRecipe(ctx, orgID, h.connID, guess); err != nil {
		t.Fatal(err)
	}
	conn, err := h.b.store.Connection(ctx, orgID, h.connID)
	if err != nil || conn.Recipe == nil || conn.Recipe.Source != RecipeSourceDetected {
		t.Fatalf("stored guess: %+v %v", conn.Recipe, err)
	}
	c := h.b.jobs.constraints(h.b.settings.Get(ctx, orgID), conn, jobKindBugfix)
	if c.Recipe != nil {
		t.Fatalf("a remembered guess was handed on as the recipe: %+v", c.Recipe)
	}
	spec := testSpec(h.connID)
	spec.Constraints = JobConstraints{Recipe: guess}
	if checks := describeChecks(spec); strings.Contains(checks, "set in the console") || strings.Contains(checks, "services/api") {
		t.Errorf("the card presents a guess as a setting: %q", checks)
	}

	if _, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", h.fd.launches[0].Token, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, "")
	if code != 200 {
		t.Fatalf("claim: %d %v", code, body)
	}
	job, _ := body["job"].(map[string]any)
	sp, _ := job["spec"].(map[string]any)
	cons, _ := sp["constraints"].(map[string]any)
	if cons["recipe"] != nil {
		t.Fatalf("the worker was handed the remembered guess: %v", cons["recipe"])
	}
}

// Every output a result carries is what the repository's own code printed, in every package, so
// every one is scrubbed before it is stored; each keeps its tail, where a failure is; and the
// lists a worker sends are capped whatever it sends.
func TestJobResultCarriesPackages(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	tok := h.fd.launches[0].Token
	if code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", tok, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, ""); code != 200 {
		t.Fatalf("claim: %d %v", code, body)
	}
	leak := "token " + testPAT + " printed by a build\n"
	long := strings.Repeat("x", 5000) + "\nFAIL: the last line"
	failing := JobCheck{Before: JobTestRun{Ran: true, OK: true}, After: JobTestRun{Ran: true, OK: false, Failed: 2, Output: leak + long}}
	pkg := func(dir string) JobPackage {
		return JobPackage{Workdir: dir, Recipe: &Recipe{Source: RecipeSourceDetected, Workdir: dir, Why: leak},
			Setup: JobStepRun{Ran: true, OK: true, Output: leak}, Build: JobCheck{After: JobTestRun{Ran: true, OK: true, Output: leak}},
			Tests: failing, Note: "ran on 3.8\n" + leak}
	}
	var unchecked []string
	for i := 0; i < 25; i++ {
		unchecked = append(unchecked, "pkg"+string(rune('a'+i)))
	}
	res := JobResult{Seq: 9, Status: JobSucceeded, Summary: "done", Branch: j.Branch,
		Recipe: &Recipe{Source: RecipeSourceDetected, Workdir: "services/api"},
		Setup:  JobStepRun{Ran: true, OK: false, Output: leak + long}, Build: failing, Tests: failing,
		Lint: JobCheck{After: JobTestRun{Ran: true, OK: false, Output: leak}}, CheckNote: "pins Python 3.5; " + leak,
		Packages: []JobPackage{pkg("web"), pkg("legacy"), pkg("tools/x")}, Unchecked: unchecked}
	if code, body := h.call(t, "POST", "/api/worker/jobs/1/result", tok, res, ""); code != 200 {
		t.Fatalf("result: %d %v", code, body)
	}
	got, _ := h.b.store.Job(ctx, orgID, j.ID)
	if strings.Contains(got.Result, "SEALEDTOKEN") {
		t.Fatalf("a secret reached storage: %s", got.Result)
	}
	var stored JobResult
	if err := json.Unmarshal([]byte(got.Result), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Packages) != JobCheckedPackagesMax-1 || len(stored.Unchecked) != JobUncheckedMax {
		t.Fatalf("caps: %d packages, %d unchecked", len(stored.Packages), len(stored.Unchecked))
	}
	for _, out := range []string{stored.Setup.Output, stored.Build.After.Output, stored.Tests.After.Output, stored.Packages[1].Tests.After.Output} {
		if !strings.HasSuffix(out, "FAIL: the last line") {
			t.Errorf("an output lost its tail: …%q", out[max(0, len(out)-60):])
		}
	}
	if n := len(stored.Packages[0].Tests.After.Output); n > JobPackageOutputMaxBytes {
		t.Errorf("a package output is %d bytes", n)
	}
	if strings.Contains(stored.Packages[0].Note, "\n") || stored.Packages[0].Workdir != "web" {
		t.Errorf("package note/workdir: %+v", stored.Packages[0])
	}
	posts := h.fs.posted()
	report := posts[len(posts)-1]
	for _, want := range []string{"*Tests* (`services/api`):", "*`web`:* tests before pass / after 2 failed", "*Also changed, not checked:* `pkga`", "and 12 more", ":warning: pins Python 3.5"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// A single-package report reads exactly as it did before a job could check several.
func TestJobReportSinglePackageIsUnchanged(t *testing.T) {
	j := &Job{ID: 7, Status: JobSucceeded, Title: "Null ticket id", Branch: "bugfix/fix-7-null-attest_tag",
		CostUSD: 0.42, CreatedAt: "2026-09-02 10:00:00", FinishedAt: "2026-09-02 10:14:00"}
	res := &JobResult{Status: JobSucceeded, PR: &JobPR{URL: "https://github.com/acme/app/pull/9", Number: 9, Draft: true},
		Summary: "Guarded the retry path.", Note: "the engine used all 500 turns",
		Tests:    JobTests{Before: JobTestRun{Ran: true, OK: false, Failed: 1}, After: JobTestRun{Ran: true, OK: true}},
		DiffStat: JobDiffStat{Files: 2, Insertions: 10, Deletions: 3}}
	want := ":white_check_mark: *Fix job #7 finished* — <https://github.com/acme/app/pull/9|#9 Null ticket id> (draft) from `bugfix/fix-7-null-attest_tag`" +
		"\n*Summary:* Guarded the retry path." +
		"\n:warning: the engine used all 500 turns" +
		"\n*Tests:* before 1 failed / after pass   *Diff:* 2 files, +10 −3   *Cost:* " + jobSpendLine(j)
	if got := jobReport(j, res); got != want {
		t.Errorf("report changed:\n got %q\nwant %q", got, want)
	}
}

func TestJobReportNamesEveryPackage(t *testing.T) {
	j := &Job{ID: 8, Status: JobSucceeded}
	res := &JobResult{Status: JobSucceeded, Recipe: &Recipe{Workdir: "services/api"},
		Tests: JobTests{Before: JobTestRun{Ran: true, OK: true}, After: JobTestRun{Ran: true, OK: true}},
		Packages: []JobPackage{
			{Workdir: "web", Tests: JobTests{Before: JobTestRun{Ran: true, OK: true}, After: JobTestRun{Ran: true, OK: true}},
				Build: JobCheck{After: JobTestRun{Ran: true, OK: false}}},
			{Workdir: "legacy", Build: JobCheck{Before: JobTestRun{Ran: true, OK: true}, After: JobTestRun{Ran: true, OK: true}},
				Note: "pins Python 3.5, which the worker cannot provide; ran on 3.8"},
			{Workdir: "a<b`c", Skipped: "not enough time left in the job"},
		},
		Unchecked: []string{".", "docs-site"}}
	got := jobReport(j, res)
	for _, want := range []string{
		"*Tests* (`services/api`): before pass / after pass",
		"*`web`:* tests before pass / after pass · build still fails",
		"*`legacy`:* build before pass / after pass — pins Python 3.5, which the worker cannot provide; ran on 3.8",
		"*`a&lt;b'c`:* not checked (not enough time left in the job)",
		"*Also changed, not checked:* the root, `docs-site`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}
