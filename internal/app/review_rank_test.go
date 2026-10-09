package app

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"attesttag/internal/review"
)

// What a large pull request has read first, and what its cut falls on: within a tier, the files whose
// paths say a bug there costs most, weighed against how much changed; after the tests, the files
// that change how things look; and the clock that grows with what there is to read.

func TestReviewPathWords(t *testing.T) {
	got := reviewPathWords("src/HTTPHandler/useSaveField_v2.tsx")
	if want := []string{"src", "http", "handler", "use", "save", "field", "v", "2", "tsx"}; !slices.Equal(got, want) {
		t.Errorf("words = %v, want %v", got, want)
	}
	for w, want := range map[string]string{"migrations": "migration", "policies": "policy", "queries": "query", "apis": "api",
		"authorize": "auth", "router": "route", "jobs": "job", "sql": "sql", "apiary": "", "status": "", "async": "",
		"access": "", "components": ""} {
		if got := reviewRiskWord(w); got != want {
			t.Errorf("reviewRiskWord(%q) = %q, want %q", w, got, want)
		}
	}
	if reviewRisk("src/hooks/useSaveField.ts", 1) != reviewRisk("src/useSaveField.ts", 1) || reviewRisk("src/use.ts", 1) != reviewRisk("src/x.ts", 1) {
		t.Error("a hook counts once, by its folder or its name, and a file named only \"use\" is not one")
	}
}

func TestReviewRankReadsTheRiskiestFirst(t *testing.T) {
	var files []*reviewFile
	for _, f := range []struct {
		path    string
		changed int
	}{
		{"package-lock.json", 2000},
		{"docs/guide.md", 10},
		{"src/Button.stories.tsx", 80},
		{"src/__mocks__/api.ts", 40},
		{"src/locales/en.json", 300},
		{"src/styles/app.css", 500},
		{"src/utils/format.test.ts", 200},
		{"src/store/documents.test.ts", 50},
		{"src/components/Toolbar.tsx", 100},
		{"src/store/documents.ts", 20},
		{"src/hooks/useSaveField.ts", 30},
		{"src/utils/format.ts", 400},
		{"api/handlers/payment_service.go", 12},
	} {
		rf := &reviewFile{File: review.File{Path: f.path, Additions: f.changed}}
		rf.tier, rf.risk = reviewTier(f.path), reviewRisk(f.path, f.changed)
		files = append(files, rf)
	}
	reviewRank(files)
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
	}
	want := []string{
		// Source: four risk words (capped at three) on twelve lines; four hundred plain lines; a hook and a
		// store, whose risk word outweighs a page's hundred lines.
		"api/handlers/payment_service.go", "src/utils/format.ts", "src/hooks/useSaveField.ts", "src/store/documents.ts",
		"src/components/Toolbar.tsx",
		// Tests, the store's first.
		"src/store/documents.test.ts", "src/utils/format.test.ts",
		// What changes how a thing looks or what a test is fed.
		"src/styles/app.css", "src/locales/en.json", "src/__mocks__/api.ts", "src/Button.stories.tsx",
		"docs/guide.md", "package-lock.json",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ranked\n%v\nwant\n%v", got, want)
	}
}

// Past the cap, what is not read is what ranks last — the stylesheets here, however large — and it is
// said, and caps the score.
func TestReviewCapCutsWhatRanksLast(t *testing.T) {
	r := &reviewRun{spec: reviewSpec{Settings: review.Resolve(nil)}, out: &reviewOutcome{FileHashes: map[string]string{}},
		byPath: map[string]*reviewFile{}, notReviewed: map[string]string{}}
	var gf []review.File
	for i := range reviewMaxFiles {
		gf = append(gf, review.File{Path: fmt.Sprintf("src/pkg/file%03d.go", i), Status: "modified", Additions: 1, Deletions: 1,
			Patch: "@@ -1,1 +1,1 @@\n-a\n+b"})
	}
	for i := range 5 {
		gf = append(gf, review.File{Path: fmt.Sprintf("src/styles/sheet%d.css", i), Status: "modified", Additions: 900,
			Patch: "@@ -1,1 +1,1 @@\n-a {}\n+b {}"})
	}
	r.prepare(gf, false)
	if len(r.reviewable) != reviewMaxFiles || r.out.Reviewable != reviewMaxFiles+5 {
		t.Fatalf("%d files to read of %d", len(r.reviewable), r.out.Reviewable)
	}
	for i := range 5 {
		if why := r.notReviewed[fmt.Sprintf("src/styles/sheet%d.css", i)]; why != "too many files" {
			t.Errorf("sheet%d.css: %q, want it cut as too many files", i, why)
		}
	}
	r.finish(nil)
	if r.out.FullCoverage {
		t.Error("a review that cut files claims full coverage")
	}
}

// Six minutes of finding for up to four passes and 45 seconds for each pass more, to twelve; four
// minutes of verifying for up to eight and 15 seconds for each more, to eight. In proportion to the
// engine's walls, so a test's shrunken walls shrink these.
func TestReviewTimeGrowsWithTheWork(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{{0, 6 * time.Minute}, {4, 6 * time.Minute}, {5, 6*time.Minute + 45*time.Second}, {8, 9 * time.Minute},
		{12, 12 * time.Minute}, {40, 12 * time.Minute}} {
		if got := reviewFinderBudget(6*time.Minute, tc.n); got != tc.want {
			t.Errorf("finder, %d passes: %s, want %s", tc.n, got, tc.want)
		}
	}
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{{0, 4 * time.Minute}, {8, 4 * time.Minute}, {9, 4*time.Minute + 15*time.Second}, {16, 6 * time.Minute},
		{24, 8 * time.Minute}, {100, 8 * time.Minute}} {
		if got := reviewVerifyBudget(4*time.Minute, tc.n); got != tc.want {
			t.Errorf("verifier, %d checks: %s, want %s", tc.n, got, tc.want)
		}
	}
	if got := reviewFinderBudget(time.Second, 40); got != 2*time.Second {
		t.Errorf("a shrunken wall grew to %s", got)
	}
	if e := newReviewEngine(nil); e.finderWall != 6*time.Minute || e.verifyWall != 4*time.Minute {
		t.Errorf("the engine's walls are %s and %s; the guide says six and four minutes", e.finderWall, e.verifyWall)
	}
}
