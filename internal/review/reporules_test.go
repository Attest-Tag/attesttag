package review

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// The lines that tell somebody what to do become rules, wherever in a file they are written — a list
// item, a sentence in a paragraph — and the rest of the file stays context: what describes the code,
// a heading, a table, and anything inside a code block, where "never" is a word in a string.
func TestExtractRepoRulesReadsDirectives(t *testing.T) {
	agents := strings.Join([]string{
		"# Conventions",
		"",
		"The store keeps one row per organisation.",
		"",
		"- Never abbreviate column as col in a name.",
		"- Boolean props must start with is or has,",
		"  so a reader can tell them from values.",
		"- Test files are TypeScript.",
		"* Use the logger instead of fmt.Println.",
		"",
		"Handlers live in api/. Do not write SQL outside the store package. Migrations are numbered.",
		"",
		"| rule | why |",
		"|---|---|",
		"| never use col | it reads badly |",
		"",
		"```go",
		"// never call this twice",
		"```",
		"1. Always wrap errors with the operation that failed.",
	}, "\n")
	contributing := "Avoid global state.\n\n- never abbreviate column as col in a name.\n- Prefer table-driven tests.\n"
	rules := ExtractRepoRules([]InstructionFile{{"AGENTS.md", agents}, {"CONTRIBUTING.md", contributing}})

	want := []RepoRule{
		{"C1", "Never abbreviate column as col in a name.", "AGENTS.md"},
		{"C2", "Boolean props must start with is or has, so a reader can tell them from values.", "AGENTS.md"},
		{"C3", "Use the logger instead of fmt.Println.", "AGENTS.md"},
		{"C4", "Do not write SQL outside the store package.", "AGENTS.md"},
		{"C5", "Always wrap errors with the operation that failed.", "AGENTS.md"},
		{"C6", "Avoid global state.", "CONTRIBUTING.md"},
		{"C7", "Prefer table-driven tests.", "CONTRIBUTING.md"},
	}
	if fmt.Sprint(rules) != fmt.Sprint(want) {
		t.Errorf("rules =\n%v\nwant\n%v", rules, want)
	}
	if r, ok := FindRepoRule(rules, "c4"); !ok || r.Source != "AGENTS.md" {
		t.Errorf("FindRepoRule(c4) = %+v, %v", r, ok)
	}
	if _, ok := FindRepoRule(rules, "C8"); ok {
		t.Error("a rule that was not read was found")
	}
	for id, want := range map[string]bool{"C1": true, "c12": true, "C": false, "C0": false, "R1": false, "S2": false, "Cx": false} {
		if RepoRuleID(id) != want {
			t.Errorf("RepoRuleID(%q) = %v", id, !want)
		}
	}
}

// Forty rules, shared out a file at a time, so one long CONTRIBUTING.md cannot push the AGENTS.md
// nearest the change out of the list; each rule is cut to a line a finding can quote.
func TestExtractRepoRulesSharesTheCapFairly(t *testing.T) {
	var long, near strings.Builder
	for i := range 60 {
		fmt.Fprintf(&long, "- Contributors must follow guideline number %d.\n", i)
	}
	for i := range 5 {
		fmt.Fprintf(&near, "- Components here must not import module %d.\n", i)
	}
	near.WriteString("- Never " + strings.Repeat("really ", 80) + "do this.\n")
	rules := ExtractRepoRules([]InstructionFile{{"web/AGENTS.md", near.String()}, {"CONTRIBUTING.md", long.String()}})
	if len(rules) != MaxRepoRules {
		t.Fatalf("%d rules, want the cap of %d", len(rules), MaxRepoRules)
	}
	nearN := 0
	for i, r := range rules {
		if r.ID != fmt.Sprintf("C%d", i+1) {
			t.Errorf("rule %d is numbered %s", i, r.ID)
		}
		if r.Source == "web/AGENTS.md" {
			nearN++
		}
		if n := utf8.RuneCountInString(r.Text); n > MaxRepoRuleLen {
			t.Errorf("%s is %d characters", r.ID, n)
		}
	}
	if nearN != 6 {
		t.Errorf("%d of the nearest file's 6 rules kept beside a file of 60", nearN)
	}
	if last := rules[5].Text; !strings.HasSuffix(last, "…") {
		t.Errorf("a rule past %d characters is not cut with a mark: %q", MaxRepoRuleLen, last)
	}
	if ExtractRepoRules(nil) != nil {
		t.Error("no files, and rules came back")
	}
}
