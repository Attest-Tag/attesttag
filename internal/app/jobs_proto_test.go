package app

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCheckedPutsThePrimaryFirst(t *testing.T) {
	res := &JobResult{
		Recipe:    &Recipe{Workdir: "services/api", Ecosystem: "go"},
		Tests:     JobTests{Before: JobTestRun{Ran: true, OK: true}, After: JobTestRun{Ran: true, OK: false}},
		CheckNote: "ran on 3.8",
		Packages:  []JobPackage{{Workdir: "web", Build: JobCheck{After: JobTestRun{Ran: true, OK: false}}}},
	}
	got := res.Checked()
	if len(got) != 2 || got[0].Workdir != "services/api" || got[0].Note != "ran on 3.8" || got[1].Workdir != "web" {
		t.Fatalf("checked: %+v", got)
	}
	if got[0].StillFailing() != "tests" || got[1].StillFailing() != "build" {
		t.Fatalf("still failing: %q %q", got[0].StillFailing(), got[1].StillFailing())
	}
	if (&JobResult{}).Checked()[0].Workdir != "." {
		t.Fatal("a result with no recipe is the root")
	}
}

func TestSetByAdmin(t *testing.T) {
	for _, c := range []struct {
		r    *Recipe
		want bool
	}{
		{nil, false},
		{&Recipe{Source: RecipeSourceDetected}, false},
		{&Recipe{Source: RecipeSourceConnection}, true},
		{&Recipe{}, true}, // stored before Source existed: it was treated as a decision then
	} {
		if got := c.r.SetByAdmin(); got != c.want {
			t.Errorf("%+v: got %v", c.r, got)
		}
	}
}

func TestKeepTailKeepsTheEndOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 100) + "FAIL: TestX"
	got := KeepTail(s, 40)
	if len(got) > 40 || !strings.HasSuffix(got, "FAIL: TestX") || !utf8.ValidString(got) || !strings.HasPrefix(got, "…\n") {
		t.Fatalf("%q (%d bytes)", got, len(got))
	}
	if KeepTail("short", 40) != "short" {
		t.Fatal("a short string is cut")
	}
}

func TestEncodeJobResultDoesNotEscapeHTML(t *testing.T) {
	b, err := EncodeJobResult(&JobResult{Summary: "a < b && c > d"})
	if err != nil || !strings.Contains(string(b), "a < b && c > d") || strings.HasSuffix(string(b), "\n") {
		t.Fatalf("%s %v", b, err)
	}
}

// A result over the bot's cap is refused whole, so Fit has to get a worst case under it and keep
// the part a reviewer needs: the end of a failing check's output.
func TestFitGetsTheWorstCaseUnderTheCap(t *testing.T) {
	out := func(tail string) string { return strings.Repeat("<a&b> line\n", 400) + tail }
	failing := JobTestRun{Ran: true, OK: false, Failed: 1, Output: out("--- FAIL: TestLast")}
	passing := JobTestRun{Ran: true, OK: true, Output: out("ok")}
	check := JobCheck{Before: passing, After: failing}
	res := &JobResult{
		Summary: strings.Repeat("s", JobSummaryMaxBytes-100), LogTail: strings.Repeat("l<>\n", JobLogTailMaxBytes/4),
		Build: check, Tests: check, Lint: check, Setup: JobStepRun{Ran: true, OK: false, Output: out("npm ERR!")},
	}
	for i := 0; i < 2; i++ {
		res.Packages = append(res.Packages, JobPackage{Workdir: "p", Build: check, Tests: check, Lint: check,
			Setup: JobStepRun{Ran: true, OK: true, Output: out("done")}})
	}
	for i := 0; i < 200; i++ {
		res.FilesChanged = append(res.FilesChanged, strings.Repeat("d/", 99)+"f")
	}
	if !res.Fit(JobResultMaxBytes) {
		t.Fatal("did not fit")
	}
	if b, _ := EncodeJobResult(res); len(b) > JobResultMaxBytes {
		t.Fatalf("%d bytes", len(b))
	}
	if !strings.HasSuffix(res.Tests.After.Output, "--- FAIL: TestLast") {
		t.Fatalf("the failing tail was lost: %q", res.Tests.After.Output)
	}
	if res.Tests.Before.Output != "" || res.Packages[0].Setup.Output != "" {
		t.Fatal("passing output was kept while failing output was cut")
	}
}

func TestFitLeavesAResultThatFitsAlone(t *testing.T) {
	res := &JobResult{Summary: "done", Tests: JobTests{After: JobTestRun{Ran: true, OK: true, Output: "ok"}},
		FilesChanged: []string{"a.go"}, Packages: []JobPackage{{Workdir: "web"}}}
	want := *res
	if !res.Fit(JobResultMaxBytes) || !reflect.DeepEqual(*res, want) {
		t.Fatalf("changed a result under the cap: %+v", res)
	}
}

func TestKeepFilesFoldsAnEarlierCount(t *testing.T) {
	files := append(make([]string, 0, 201), make([]string, 200)...)
	files = append(files, "… and 30 more files")
	got := keepFiles(files, 50)
	if len(got) != 51 || got[50] != "… and 180 more files" {
		t.Fatalf("%d %q", len(got), got[len(got)-1])
	}
}

// Old rows and old workers know nothing of packages; the shape they read must not move.
func TestJobResultWithoutPackagesEncodesAsBefore(t *testing.T) {
	b, _ := json.Marshal(JobResult{Status: JobSucceeded})
	for _, k := range []string{`"packages"`, `"unchecked"`, `"check_note"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("%s appears in a single-package result: %s", k, b)
		}
	}
}
