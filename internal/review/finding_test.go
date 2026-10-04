package review

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validFinding() Finding {
	return Finding{
		Path:      "src/totals.ts",
		Side:      Right,
		StartLine: 50,
		Line:      52,
		Severity:  P1,
		Category:  CategoryConcurrency,
		Title:     "Older totals can overwrite newer ones",
		Scenario:  "Two refreshes in flight: the slower, older response lands last and replaces the newer totals.",
		Symbol:    "useTotals",
		Evidence: []Evidence{{
			Repo: "acme/web", Path: "src/totals.ts", Ref: "head", StartLine: 44, EndLine: 47,
			Quote: "setTotals(await fetchTotals())",
		}},
		RuleIDs:    []string{"R12"},
		Suggestion: &Suggestion{StartLine: 50, Line: 52, Code: "const seq = ++latest.current\n"},
		Confidence: 80,
		ReviewType: "general",
	}
}

func TestAWellFormedFindingValidates(t *testing.T) {
	if err := validFinding().Validate(); err != nil {
		t.Fatal(err)
	}
	minimal := Finding{Path: "a.go", Side: Left, Line: 3, Severity: P0, Category: CategorySecurity,
		Title: "Auth check removed", Scenario: "Any caller reaches the handler."}
	if err := minimal.Validate(); err != nil {
		t.Fatalf("a finding needs no evidence, symbol or suggestion to be well-formed: %v", err)
	}
}

func TestFindingValidateRefusesWhatCannotBePosted(t *testing.T) {
	cases := []struct {
		name       string
		edit       func(*Finding)
		want       string
		suggestion bool // the problem is the suggestion's alone
	}{
		{"no path", func(f *Finding) { f.Path = "" }, "path is required", false},
		{"absolute path", func(f *Finding) { f.Path = "/etc/passwd" }, "relative to the repository root", false},
		{"newline in path", func(f *Finding) { f.Path = "a\nb" }, "control character", false},
		{"unknown side", func(f *Finding) { f.Side = "BOTH" }, "want RIGHT or LEFT", false},
		{"line 0", func(f *Finding) { f.Line = 0; f.StartLine = 0; f.Suggestion = nil }, "line must be 1 or more", false},
		{"start after line", func(f *Finding) { f.StartLine = 60; f.Suggestion = nil }, "start_line 60", false},
		{"unknown severity", func(f *Finding) { f.Severity = "P3" }, "want P0, P1 or P2", false},
		{"unknown category", func(f *Finding) { f.Category = "style" }, `category "style"`, false},
		{"title too short", func(f *Finding) { f.Title = "Ok" }, "title must be 3 to 80", false},
		{"title too long", func(f *Finding) { f.Title = strings.Repeat("x", 81) }, "got 81", false},
		{"title on two lines", func(f *Finding) { f.Title = "First line\nsecond" }, "title must be one line", false},
		{"no scenario", func(f *Finding) { f.Scenario = "  " }, "scenario is required", false},
		{"scenario too long", func(f *Finding) { f.Scenario = strings.Repeat("é", 901) }, "at most 900 characters, got 901", false},
		{"confidence over 100", func(f *Finding) { f.Confidence = 101 }, "confidence 101", false},
		{"bad review type", func(f *Finding) { f.ReviewType = "Security Review" }, "not a review type key", false},
		{"too much evidence", func(f *Finding) {
			for range 9 {
				f.Evidence = append(f.Evidence, f.Evidence[0])
			}
		}, "at most 8 evidence items", false},
		{"evidence with no quote", func(f *Finding) { f.Evidence[0].Quote = "" }, "evidence[0]: quote is required", false},
		{"evidence from a bad repo", func(f *Finding) { f.Evidence[0].Repo = "not a repo" }, "want owner/name", false},
		{"evidence at a bad ref", func(f *Finding) { f.Evidence[0].Ref = "main" }, "want head, base, default or a commit sha", false},
		{"evidence lines backwards", func(f *Finding) { f.Evidence[0].EndLine = 10 }, "end_line comes before start_line", false},
		{"rule id with a space", func(f *Finding) { f.RuleIDs = []string{"R 1"} }, "is not a rule id", false},

		{"suggestion on a deleted line", func(f *Finding) { f.Side = Left }, "only allowed on the RIGHT", true},
		{"suggestion for other lines", func(f *Finding) { f.Suggestion.StartLine = 51 }, "exactly the finding's lines 50-52, not 51-52", true},
		{"suggestion over too many lines", func(f *Finding) {
			f.StartLine, f.Suggestion.StartLine = 30, 30
		}, "at most 10 lines, not 23", true},
		{"suggestion code too long", func(f *Finding) {
			f.Suggestion.Code = strings.Repeat("x\n", 11)
		}, "code may be at most 10 lines, not 11", true},
		{"suggestion that would close its own block", func(f *Finding) {
			f.Suggestion.Code = "```\n@everyone"
		}, "may not contain ```", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := validFinding()
			f.Evidence = append([]Evidence(nil), f.Evidence...)
			s := *f.Suggestion
			f.Suggestion = &s
			c.edit(&f)
			err := f.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			if errors.Is(err, ErrSuggestion) != c.suggestion {
				t.Errorf("errors.Is(err, ErrSuggestion) = %v, want %v", !c.suggestion, c.suggestion)
			}
		})
	}
}

// The engine's recovery: a finding whose only fault is its suggestion is posted without one.
func TestABadSuggestionCanBeDroppedToKeepTheFinding(t *testing.T) {
	f := validFinding()
	f.Suggestion.StartLine = 51
	err := f.Validate()
	if !errors.Is(err, ErrSuggestion) {
		t.Fatalf("err = %v, want ErrSuggestion", err)
	}
	f.Suggestion = nil
	if err := f.Validate(); err != nil {
		t.Fatalf("without its suggestion the finding is fine: %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	f := validFinding()
	f.Severity, f.Category, f.Title = "high", "style", ""
	err := f.Validate()
	for _, want := range []string{"severity", "category", "title"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %s", err, want)
		}
	}
}

func TestNormalizeForgivesWhatAModelGetsAlmostRight(t *testing.T) {
	f := Finding{
		Path: "  src/a.go ", Side: "right", StartLine: 7, Line: 7, Severity: " p1", Category: "Security",
		Title: "  Tenant check\n missing  ", Scenario: " x ", ReviewType: " Security ",
		Evidence:   []Evidence{{Path: " src/a.go", Ref: "HEAD", StartLine: 3, EndLine: 3, Quote: "q"}},
		RuleIDs:    []string{" R1 "},
		Suggestion: &Suggestion{Code: "fixed()"},
	}
	f.Normalize()
	if f.Path != "src/a.go" || f.Side != Right || f.Severity != P1 || f.Category != CategorySecurity ||
		f.Title != "Tenant check missing" || f.Scenario != "x" || f.ReviewType != "security" {
		t.Errorf("normalised to %+v", f)
	}
	if f.StartLine != 0 {
		t.Errorf("start_line equal to line should become 0, since GitHub refuses it: got %d", f.StartLine)
	}
	if f.Suggestion.Line != 7 || f.Suggestion.StartLine != 0 {
		t.Errorf("a suggestion with no lines takes the finding's: %+v", f.Suggestion)
	}
	if e := f.Evidence[0]; e.Path != "src/a.go" || e.Ref != "head" || e.EndLine != 0 {
		t.Errorf("evidence normalised to %+v", e)
	}
	if f.RuleIDs[0] != "R1" {
		t.Errorf("rule id %q", f.RuleIDs[0])
	}
	if err := f.Validate(); err != nil {
		t.Errorf("normalised finding should validate: %v", err)
	}

	noSide := Finding{}
	noSide.Normalize()
	if noSide.Side != Right {
		t.Errorf("a missing side defaults to RIGHT, as GitHub's does; got %q", noSide.Side)
	}
	unknown := Finding{Severity: "critical", Category: "style"}
	unknown.Normalize()
	if unknown.Severity != "critical" || unknown.Category != "style" {
		t.Error("Normalize must leave what it does not recognise for Validate to refuse")
	}
}

func TestRange(t *testing.T) {
	if s, e := (Finding{Line: 9}).Range(); s != 9 || e != 9 {
		t.Errorf("single line: %d-%d", s, e)
	}
	if s, e := (Finding{StartLine: 4, Line: 9}).Range(); s != 4 || e != 9 {
		t.Errorf("range: %d-%d", s, e)
	}
}

func TestParseSeverityAndCategory(t *testing.T) {
	for in, want := range map[string]Severity{"P0": P0, "p1": P1, " P2 ": P2} {
		if got, err := ParseSeverity(in); err != nil || got != want {
			t.Errorf("ParseSeverity(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "P3", "high", "1"} {
		if _, err := ParseSeverity(bad); err == nil {
			t.Errorf("ParseSeverity(%q) should fail", bad)
		}
	}
	for _, c := range Categories() {
		if got, err := ParseCategory(strings.ToUpper(string(c))); err != nil || got != c {
			t.Errorf("ParseCategory(%q) = %q, %v", c, got, err)
		}
	}
	if _, err := ParseCategory("style"); err == nil {
		t.Error("style is not a category")
	}
	if len(Categories()) != 9 {
		t.Errorf("the schema offers nine categories, got %d", len(Categories()))
	}
}

// A submit_review finding, in the tool schema's own shape, decodes straight into a Finding.
func TestFindingDecodesFromTheToolSchema(t *testing.T) {
	raw := `{
		"path": "src/totals.ts", "side": "RIGHT", "start_line": 50, "line": 52,
		"severity": "P1", "category": "concurrency",
		"title": "Older totals can overwrite newer ones",
		"scenario": "trigger then consequence",
		"symbol": "useTotals",
		"evidence": [{"repo": "acme/api", "path": "lib/totals.py", "ref": "default",
		              "start_line": 44, "end_line": 47, "quote": "exact text"}],
		"rule_ids": ["R12"],
		"suggestion": {"start_line": 50, "line": 52, "code": "replacement lines"},
		"pre_existing": false, "confidence": 0, "review_type": "general"
	}`
	var f Finding
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	if f.Side != Right || f.StartLine != 50 || f.Severity != P1 || f.Evidence[0].Repo != "acme/api" ||
		f.Suggestion == nil || f.Suggestion.Code != "replacement lines" || f.ReviewType != "general" {
		t.Errorf("decoded %+v", f)
	}
	if err := f.Validate(); err != nil {
		t.Errorf("the schema's own example should validate: %v", err)
	}
}

func TestFingerprintIsTheSameFindingWhenOnlyTheWordingMoves(t *testing.T) {
	base := Finding{Path: "store/list.go", Category: CategorySecurity, Symbol: "ListOrders",
		Title: "Tenant check missing on list query", Line: 52, Severity: P1}
	fp := Fingerprint("acme/api", base)

	same := map[string]func(*Finding){
		"words reordered and filler added": func(f *Finding) { f.Title = "Missing tenant check in the list query" },
		"case and punctuation":             func(f *Finding) { f.Title = "TENANT CHECK MISSING — on list query!" },
		"a repeated word":                  func(f *Finding) { f.Title = "Tenant check missing on list query query" },
		"lines moved":                      func(f *Finding) { f.Line, f.StartLine = 90, 85 },
		"severity downgraded":              func(f *Finding) { f.Severity = P2 },
		"found by another review type":     func(f *Finding) { f.ReviewType = "security" },
		"a hedge":                          func(f *Finding) { f.Title = "Tenant check may be missing on list query" },
		"surrounding whitespace":           func(f *Finding) { f.Symbol = " ListOrders " },
	}
	for name, edit := range same {
		f := base
		edit(&f)
		if got := Fingerprint("acme/api", f); got != fp {
			t.Errorf("%s: fingerprint changed", name)
		}
	}
	if Fingerprint("Acme/API", base) != fp {
		t.Error("GitHub repository names are case-insensitive")
	}

	different := map[string]func(*Finding){
		"another path":     func(f *Finding) { f.Path = "store/get.go" },
		"another category": func(f *Finding) { f.Category = CategoryBug },
		"another symbol":   func(f *Finding) { f.Symbol = "GetOrder" },
		"another failure":  func(f *Finding) { f.Title = "Tenant check missing on delete query" },
		"a negation":       func(f *Finding) { f.Title = "Tenant check not missing on list query" },
	}
	for name, edit := range different {
		f := base
		edit(&f)
		if Fingerprint("acme/api", f) == fp {
			t.Errorf("%s: should be a different finding", name)
		}
	}
	if Fingerprint("acme/web", base) == fp {
		t.Error("another repository is another finding")
	}
}

// Fingerprints are stored, and a withdrawn finding stays withdrawn only while its
// fingerprint is computed the same way. A change to the formula or the stop-words re-opens
// every withdrawn finding in every repository at once; this pins it so that cannot happen
// by accident.
func TestFingerprintFormulaIsPinned(t *testing.T) {
	f := Finding{Path: "store/list.go", Category: CategorySecurity, Symbol: "ListOrders",
		Title: "Tenant check missing on list query"}
	const want = "4c3e17a7701d9adf9b3d0a856b31784fca63bc18d86aec33afd450b3d2688ac7"
	if got := Fingerprint("acme/api", f); got != want {
		t.Errorf("Fingerprint = %s\nThe formula changed; stored fingerprints will no longer match. If that is intended, migrate them and update this value.", got)
	}
	if got := titleTerms(f.Title); got != "check list missing query tenant" {
		t.Errorf("titleTerms = %q", got)
	}
}
