package review

import (
	"encoding/json"
	"strings"
	"testing"
)

// Concurrency and state ships like the other built-ins, and is the one that brings itself in: it
// has a pattern, and none of the others do.
func TestConcurrencyTypeIsBuiltIn(t *testing.T) {
	c, ok := BuiltinType("concurrency")
	if !ok {
		t.Fatal("no built-in concurrency type")
	}
	if c.Name != "Concurrency and state" || c.Strictness != StrictnessMedium || c.InlineMinSeverity != P2 || len(c.Rules) < 15 {
		t.Errorf("concurrency: name %q, strictness %q, inline %q, %d rules", c.Name, c.Strictness, c.InlineMinSeverity, len(c.Rules))
	}
	if c.Auto == "" || len(c.Auto) > MaxTypeAutoLen {
		t.Errorf("concurrency's pattern is %d characters, want 1 to %d", len(c.Auto), MaxTypeAutoLen)
	}
	for _, bt := range BuiltinTypes() {
		if bt.Key != "concurrency" && bt.Auto != "" {
			t.Errorf("%s brings itself into reviews; only concurrency should", bt.Key)
		}
	}
	// The kinds of bug it is for, across the interface, the backend and the data.
	for _, want := range []string{"out of order", "unique only within a parent", "debounce", "optimistic", "BaseException",
		"retry", "exception type", "check-then-act", "resolved twice"} {
		if !rulesMention(c, want) {
			t.Errorf("no concurrency rule mentions %q", want)
		}
	}
}

func rulesMention(t Type, s string) bool {
	for _, r := range t.Rules {
		if strings.Contains(strings.ToLower(r.Text), strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// fileWith is a changed file of one hunk holding the given lines, each written "+text", "-text" or
// " text" as a patch writes them.
func fileWith(path string, lines ...string) File {
	h := Hunk{OldStart: 1, NewStart: 1}
	for i, l := range lines {
		h.Lines = append(h.Lines, Line{Kind: l[0], Text: l[1:], Old: i + 1, New: i + 1})
	}
	return File{Path: path, Status: "modified", Hunks: []Hunk{h}}
}

// The pattern is matched against what changed — added lines, deleted ones, and the paths — and not
// against context, which did not.
func TestConcurrencyAutoMatch(t *testing.T) {
	c, _ := BuiltinType("concurrency")
	for _, tc := range []struct {
		name string
		f    File
		want bool
	}{
		{"an await added", fileWith("src/save.ts", " function save() {", "+  await api.put(field)", " }"), true},
		{"a lock taken away", fileWith("src/totals.go", " func (t *Totals) Add(n int) {", "-\tt.mu.Lock()", "-\tdefer t.mu.Unlock()", " \tt.value += n"), true},
		{"a debounce", fileWith("src/search.tsx", "+const onType = debounce(search, 300)"), true},
		{"an effect", fileWith("src/Doc.tsx", "+  useEffect(() => load(id), [id])"), true},
		{"asyncio", fileWith("app/jobs.py", "+        await asyncio.gather(*tasks)"), true},
		{"a goroutine", fileWith("cmd/run.go", "+\tgo func() {"), true},
		{"a saga", fileWith("src/store/doc.ts", "+  yield takeLatest(LOAD, load)"), true},
		{"a retry", fileWith("lib/client.rb", "+    retry_count += 1"), true},
		{"a worker by its path", fileWith("services/workers/send.py", "+    return len(items)"), true},
		{"a queue by its path", fileWith("src/queue/drain.go", "+\treturn n"), true},
		{"plain logic", fileWith("src/sum.go", "+\treturn a + b", "-\treturn a - b"), false},
		{"await only in context", fileWith("src/save.ts", " await api.put(field)", "+  const x = 1"), false},
		{"a stylesheet's select", fileWith("src/form.css", "+select {", "+  color: red;", "+}"), false},
		{"prose", fileWith("README.md", "+Fix a typo in the install steps."), false},
	} {
		if got := c.AutoMatch(tc.f); got != tc.want {
			t.Errorf("%s: AutoMatch = %v, want %v", tc.name, got, tc.want)
		}
	}
	renamed := fileWith("src/plain.go", "+\treturn n")
	renamed.PrevPath = "src/worker/plain.go"
	if !c.AutoMatch(renamed) {
		t.Error("a file moved out of a worker's folder did not count by the path it came from")
	}
	general, _ := BuiltinType(DefaultType)
	if general.AutoMatch(fileWith("src/save.ts", "+await save()")) {
		t.Error("a type with no pattern matched")
	}
	scoped := c
	scoped.PathGlobs = []string{"web/**"}
	if scoped.AutoMatch(fileWith("api/save.ts", "+await save()")) {
		t.Error("a type matched a file outside its paths")
	}
}

// auto may be given on several lines, which are one pattern; each line is checked where it is written.
func TestParseTypeAuto(t *testing.T) {
	ty, err := ParseType("key: shell\nname: Shell\nauto: \\bexec\\.Command\\b\nauto: \\bsubprocess\\b\npurpose: Commands.\n\n- [P1] Quote arguments.\n")
	if err != nil {
		t.Fatal(err)
	}
	if ty.Auto != `\bexec\.Command\b|\bsubprocess\b` {
		t.Errorf("auto = %q, want the lines joined as alternatives", ty.Auto)
	}
	if !ty.AutoMatch(fileWith("tools/run.py", "+import subprocess")) || ty.AutoMatch(fileWith("tools/run.py", "+import os")) {
		t.Error("the joined pattern does not match as each of its lines would")
	}
	_, err = ParseType("key: shell\nname: Shell\nauto: (unclosed\npurpose: Commands.\n\n- [P1] Quote arguments.\n")
	if err == nil || !strings.Contains(err.Error(), "line 3: auto") {
		t.Errorf("a pattern that does not compile: %v", err)
	}
	if _, err := ParseType("key: shell\nname: Shell\nkey: other\npurpose: Commands.\n\n- [P1] Quote arguments.\n"); err == nil {
		t.Error("only auto may be given twice")
	}
	ty.Auto = strings.Repeat("a|", MaxTypeAutoLen)
	if err := ValidateType(ty); err == nil || !strings.Contains(err.Error(), "auto must be at most") {
		t.Errorf("a pattern over the cap: %v", err)
	}
	ty.Auto = "a("
	if err := ValidateType(ty); err == nil || !strings.Contains(err.Error(), "auto is not a regular expression") {
		t.Errorf("a pattern that does not compile: %v", err)
	}
}

// A type the diff brought in is named as one in the summary, so nobody looks for the rule that added
// it; the check says the same.
func TestSummaryMarksAutomaticTypes(t *testing.T) {
	s := SummaryState{ReviewID: "r", FullCoverage: true,
		Types: []TypeRun{{Key: "general"}, {Key: "security"}, {Key: "concurrency", Auto: true}}}
	got := RenderSummary(s, testRenderContext())
	if !strings.Contains(got, "Reviewed as General, Security and Concurrency and state (auto)") {
		t.Errorf("the summary does not mark the automatic type:\n%s", got)
	}
	if _, summary := RenderCheck(s, testRenderContext(), ""); !strings.Contains(summary, "Concurrency and state (auto)") {
		t.Errorf("the check does not mark the automatic type:\n%s", summary)
	}
	s.Types[2].Auto = false
	if got := RenderSummary(s, testRenderContext()); strings.Contains(got, "(auto)") {
		t.Errorf("a type a rule chose is marked automatic:\n%s", got)
	}
}

// auto_types is on unless a level turns it off, is inherited like any single value, and is in the
// hash only when off: the built-in settings hash what they did before it existed (pinned in
// TestEffectiveHashIsPinned), and turning it off is another review.
func TestAutoTypesSetting(t *testing.T) {
	on := Resolve(nil)
	if !on.AutoTypes || on.Source["auto_types"] != LevelDefault {
		t.Errorf("auto_types resolved to %v from %q, want on by default", on.AutoTypes, on.Source["auto_types"])
	}
	off := Resolve([]LevelSettings{{LevelConnection, Settings{AutoTypes: ptr(false)}}})
	if off.AutoTypes || off.Source["auto_types"] != LevelConnection {
		t.Errorf("a connection turning them off: %v from %q", off.AutoTypes, off.Source["auto_types"])
	}
	back := Resolve([]LevelSettings{{LevelConnection, Settings{AutoTypes: ptr(false)}}, {LevelRepo, Settings{AutoTypes: ptr(true)}}})
	if !back.AutoTypes || back.Source["auto_types"] != LevelRepo {
		t.Errorf("a repository turning them back on: %v from %q", back.AutoTypes, back.Source["auto_types"])
	}
	if on.Hash() == off.Hash() || on.Hash() != back.Hash() {
		t.Error("the hash does not follow whether automatic types run")
	}
	if got := on.WithRule(BranchRule{Strictness: StrictnessHigh}); !got.AutoTypes {
		t.Error("a branch rule switched automatic types off")
	}
	var s Settings
	if err := json.Unmarshal([]byte(`{"auto_types":false}`), &s); err != nil || s.AutoTypes == nil || *s.AutoTypes {
		t.Errorf("auto_types from JSON = %v (%v)", s.AutoTypes, err)
	}
	if got := ChangedFields(Settings{}, s); len(got) != 1 || got[0] != "auto_types" {
		t.Errorf("ChangedFields = %v", got)
	}
}
