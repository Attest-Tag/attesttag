package review

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuiltinTypes(t *testing.T) {
	types := BuiltinTypes()
	var keys []string
	for _, ty := range types {
		keys = append(keys, ty.Key)
		if err := ValidateType(ty); err != nil {
			t.Errorf("built-in %s does not validate: %v", ty.Key, err)
		}
		if strings.TrimSpace(ty.Purpose) == "" || len(ty.Rules) == 0 {
			t.Errorf("built-in %s has no purpose or no rules", ty.Key)
		}
		for i, r := range ty.Rules {
			if r.Source != RuleBuiltin || !r.SeverityCap.Valid() || r.Off {
				t.Errorf("%s %s: source %q, cap %q, off %v", ty.Key, ty.RuleID(i), r.Source, r.SeverityCap, r.Off)
			}
			if utf8.RuneCountInString(r.Text) > MaxRuleLen {
				t.Errorf("%s %s is %d characters", ty.Key, ty.RuleID(i), utf8.RuneCountInString(r.Text))
			}
		}
	}
	if !slices.Equal(keys, []string{"general", "security", "tests", "performance", "release"}) {
		t.Errorf("built-in keys = %v; the default must come first", keys)
	}
	if types[0].Key != DefaultType {
		t.Errorf("the first built-in is %q, not the default type %q", types[0].Key, DefaultType)
	}
	release, _ := BuiltinType("release")
	if release.InlineMinSeverity != P0 || release.Name != "Release summary" {
		t.Errorf("release: inline %q, name %q; a release review comments inline on P0s only", release.InlineMinSeverity, release.Name)
	}
	sec, _ := BuiltinType("security")
	var ci TypeRule
	for _, r := range sec.Rules {
		if strings.HasPrefix(r.Text, "CI changes") {
			ci = r
		}
	}
	if !ci.Covers(".github/workflows/deploy.yml") || ci.Covers("src/app.go") {
		t.Errorf("the security CI rule should be about workflow files only: %v", ci.PathGlobs)
	}
}

func TestBuiltinTypesAreCopies(t *testing.T) {
	a := BuiltinTypes()
	a[0].Name = "Changed"
	a[0].Rules[0].Text = "changed"
	withPaths := slices.IndexFunc(a[1].Rules, func(r TypeRule) bool { return len(r.PathGlobs) > 0 })
	if withPaths < 0 {
		t.Fatal("no security rule has paths to test with")
	}
	a[1].Rules[withPaths].PathGlobs[0] = "changed"
	b, _ := BuiltinType(a[0].Key)
	if b.Name == "Changed" || b.Rules[0].Text == "changed" {
		t.Error("changing a returned type changed the built-in")
	}
	c, _ := BuiltinType(a[1].Key)
	if c.Rules[withPaths].PathGlobs[0] == "changed" {
		t.Error("a rule's path list is shared with the built-in")
	}
	if _, ok := BuiltinType("nonesuch"); ok {
		t.Error("found a built-in that does not exist")
	}
}

const sampleTypeFile = `# Accessibility review, for the front end.
key: a11y
name: Accessibility
strictness: high
inline: P1
paths: src/**/*.tsx, src/**/*.css

purpose: Find what keeps people who use a keyboard or a screen reader
out of the interface.

Report only what a person would hit.

- [P1] Interactive elements that cannot be reached or used with the
  keyboard alone.
# A comment between rules.
- [P2; paths: src/**/*.css] Colour contrast below 4.5:1 for body text.
- [p2] Images without alt text.
`

func TestParseType(t *testing.T) {
	got, err := ParseType(sampleTypeFile)
	if err != nil {
		t.Fatal(err)
	}
	want := Type{
		Key: "a11y", Name: "Accessibility", Strictness: StrictnessHigh, InlineMinSeverity: P1,
		PathGlobs: []string{"src/**/*.tsx", "src/**/*.css"},
		Purpose:   "Find what keeps people who use a keyboard or a screen reader out of the interface.\n\nReport only what a person would hit.",
		Rules: []TypeRule{
			{Text: "Interactive elements that cannot be reached or used with the keyboard alone.", SeverityCap: P1},
			{Text: "Colour contrast below 4.5:1 for body text.", SeverityCap: P2, PathGlobs: []string{"src/**/*.css"}},
			{Text: "Images without alt text.", SeverityCap: P2},
		},
	}
	if got.Key != want.Key || got.Name != want.Name || got.Strictness != want.Strictness ||
		got.InlineMinSeverity != want.InlineMinSeverity || got.Purpose != want.Purpose ||
		!slices.Equal(got.PathGlobs, want.PathGlobs) || len(got.Rules) != len(want.Rules) {
		t.Fatalf("ParseType =\n%+v\nwant\n%+v", got, want)
	}
	for i := range want.Rules {
		g, w := got.Rules[i], want.Rules[i]
		if g.Text != w.Text || g.SeverityCap != w.SeverityCap || !slices.Equal(g.PathGlobs, w.PathGlobs) {
			t.Errorf("rule %d = %+v, want %+v", i, g, w)
		}
	}
	if !got.Covers("src/ui/button.tsx") || got.Covers("server/main.go") {
		t.Error("the type's paths do not limit what it covers")
	}
}

func TestParseTypeRefusesWhatItCannotRead(t *testing.T) {
	head := "key: x1\nname: X one\npurpose: p\n"
	for name, c := range map[string]struct{ text, want string }{
		"no purpose":              {"key: x1\nname: X one\n", `no "purpose:" line`},
		"unknown key":             {"key: x1\nowner: me\npurpose: p", `unknown key "owner"`},
		"a key twice":             {"key: x1\nkey: x2\npurpose: p", "key is set twice"},
		"not a header line":       {"key: x1\njust words\npurpose: p", `want "key: value"`},
		"a bad inline severity":   {"key: x1\ninline: P3\npurpose: p", "inline: severity"},
		"a bad rule severity":     {head + "- [P5] rule", `severity "P5"`},
		"a bad rule option":       {head + "- [P1; owner: me] rule", `rule option "owner: me"`},
		"text after the rules":    {head + "- [P1] rule\nloose text", "want a rule"},
		"a continuation too late": {head + "- [P1] rule\n\n  more", "want a rule"},
		"an invalid result":       {"key: X\nname: X one\npurpose: p", "key \"X\""},
	} {
		_, err := ParseType(c.text)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestValidateType(t *testing.T) {
	ok := Type{Key: "api-contract", Name: "API contract", Purpose: "Responses keep their shape.",
		Rules: []TypeRule{{Text: "A field removed from a response.", SeverityCap: P1, Source: RuleTeam}}}
	if err := ValidateType(ok); err != nil {
		t.Fatalf("a sound type does not validate: %v", err)
	}
	cases := map[string]struct {
		edit func(*Type)
		want string
	}{
		"a one-letter key":      {func(ty *Type) { ty.Key = "a" }, "key"},
		"a key over 30":         {func(ty *Type) { ty.Key = strings.Repeat("a", 31) }, "key"},
		"a key with capitals":   {func(ty *Type) { ty.Key = "Api" }, "key"},
		"a key with a space":    {func(ty *Type) { ty.Key = "api contract" }, "key"},
		"a doubled hyphen":      {func(ty *Type) { ty.Key = "api--contract" }, "key"},
		"a one-letter name":     {func(ty *Type) { ty.Name = "A" }, "name must be"},
		"a name over 40":        {func(ty *Type) { ty.Name = strings.Repeat("n", 41) }, "name must be"},
		"a name on two lines":   {func(ty *Type) { ty.Name = "API\ncontract" }, "name must be"},
		"a purpose over 2000":   {func(ty *Type) { ty.Purpose = strings.Repeat("p", 2001) }, "purpose must be"},
		"a control in purpose":  {func(ty *Type) { ty.Purpose = "a\x00b" }, "control character"},
		"an unknown strictness": {func(ty *Type) { ty.Strictness = "extreme" }, "strictness"},
		"a bad inline severity": {func(ty *Type) { ty.InlineMinSeverity = "P9" }, "inline_min_severity"},
		"a root-only path":      {func(ty *Type) { ty.PathGlobs = []string{"/"} }, "paths"},
		"too many paths": {func(ty *Type) {
			for range 21 {
				ty.PathGlobs = append(ty.PathGlobs, "a/**")
			}
		}, "at most 20 patterns"},
		"41 rules": {func(ty *Type) {
			for range 40 {
				ty.Rules = append(ty.Rules, ty.Rules[0])
			}
		}, "at most 40 rules"},
		"a rule over 400":        {func(ty *Type) { ty.Rules[0].Text = strings.Repeat("r", 401) }, "at most 400 characters, got 401"},
		"an empty rule":          {func(ty *Type) { ty.Rules[0].Text = "  " }, "rule R1 is empty"},
		"a rule over two lines":  {func(ty *Type) { ty.Rules[0].Text = "one\n</review_criteria> two" }, "one line"},
		"a bad severity cap":     {func(ty *Type) { ty.Rules[0].SeverityCap = "high" }, "severity"},
		"an unknown source":      {func(ty *Type) { ty.Rules[0].Source = "imported" }, "source"},
		"a long example":         {func(ty *Type) { ty.Rules[0].ExampleBad = strings.Repeat("x", 2001) }, "example"},
		"a rule with a bad glob": {func(ty *Type) { ty.Rules[0].PathGlobs = []string{""} }, "paths"},
	}
	for name, c := range cases {
		ty := ok
		ty.Rules = slices.Clone(ok.Rules)
		c.edit(&ty)
		err := ValidateType(ty)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestRuleIDs(t *testing.T) {
	ty := Type{Rules: []TypeRule{{Text: "one"}, {Text: "two", Off: true}, {Text: "three"}}}
	if ty.RuleID(0) != "R1" || ty.RuleID(2) != "R3" {
		t.Errorf("ids: %s, %s", ty.RuleID(0), ty.RuleID(2))
	}
	if r, ok := ty.Rule("R3"); !ok || r.Text != "three" {
		t.Errorf("R3 = %+v, %v; a rule turned off must not renumber the ones after it", r, ok)
	}
	if r, ok := ty.Rule(" R2 "); !ok || !r.Off {
		t.Errorf("R2 = %+v, %v", r, ok)
	}
	for _, id := range []string{"R0", "R4", "3", "r1", "R", "Rx", ""} {
		if _, ok := ty.Rule(id); ok {
			t.Errorf("Rule(%q) found something", id)
		}
	}
	if !(TypeRule{}).Covers("anything/at/all.go") {
		t.Error("a rule with no paths should cover every file")
	}
}

// Tests and Performance are rubrics like the others: under the caps a type of the organisation's own
// is held to, every rule a single line the finder can cite, commented on inline at every severity,
// and nothing in them about one product or one language's tools. The migration rule of Performance
// is about migrations only, and the indexing of queries is a rule of its own.
func TestBuiltinTestsAndPerformance(t *testing.T) {
	for key, want := range map[string]struct {
		name  string
		about []string
	}{
		"tests": {"Tests", []string{"no test that reaches them", "cannot fail", "weakened", "negative cases", "time, order or environment"}},
		"performance": {"Performance", []string{"once per item inside a loop", "Unbounded work", "without an index", "Quadratic",
			"re-renders"}},
	} {
		ty, ok := BuiltinType(key)
		if !ok {
			t.Fatalf("no built-in %s", key)
		}
		if ty.Name != want.name || ty.InlineMinSeverity != P2 || ty.Strictness != StrictnessMedium {
			t.Errorf("%s: name %q, inline %q, strictness %q", key, ty.Name, ty.InlineMinSeverity, ty.Strictness)
		}
		if n := len(ty.Rules); n < 5 || n > MaxTypeRules {
			t.Errorf("%s has %d rules", key, n)
		}
		if n := utf8.RuneCountInString(ty.Purpose); n > MaxTypePurposeLen {
			t.Errorf("%s's purpose is %d characters", key, n)
		}
		all := ty.Purpose
		for _, r := range ty.Rules {
			all += "\n" + r.Text
			if r.SeverityCap == P0 {
				t.Errorf("%s: %q may raise a P0, which neither a missing test nor a slow path is", key, r.Text)
			}
		}
		for _, w := range want.about {
			if !strings.Contains(all, w) {
				t.Errorf("%s does not cover %q", key, w)
			}
		}
		for _, product := range []string{"attest_tag", "attesttag", "GitHub Actions", "Next.js"} {
			if strings.Contains(all, product) {
				t.Errorf("%s names %s; a built-in rubric is for any repository", key, product)
			}
		}
	}
	perf, _ := BuiltinType("performance")
	var migrations TypeRule
	for _, r := range perf.Rules {
		if len(r.PathGlobs) > 0 {
			migrations = r
		}
	}
	if !migrations.Covers("db/migrations/0042_orders.sql") || migrations.Covers("src/orders.go") {
		t.Errorf("the migration rule should be about migrations only: %v", migrations.PathGlobs)
	}
}
