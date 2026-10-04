package review

import (
	"strings"
	"testing"
)

func TestSkillLinksAreValidatedAsStored(t *testing.T) {
	base, _ := BuiltinType(DefaultType)
	ok := []SkillLink{
		{Path: "skills/review"},
		{Repo: "acme/skills", Path: "review/go"},
		{Repo: "acme/skills", Path: "review/go/SKILL.md", Ref: "main"},
		{Repo: "skill-co/agent.skills", Path: "a b/c", Ref: "release/v2"},
		{Repo: "acme/skills", Path: "x", Ref: "0123456789abcdef0123456789abcdef01234567"},
	}
	ty := base
	ty.Skills = ok
	if err := ValidateType(ty); err != nil {
		t.Fatalf("valid links refused: %v", err)
	}
	for _, c := range []struct {
		links []SkillLink
		want  string
	}{
		{[]SkillLink{{Path: ""}}, "path"},
		{[]SkillLink{{Path: "a/../b"}}, "path"},
		{[]SkillLink{{Path: "skills/*"}}, "path"},
		{[]SkillLink{{Path: ".git/config"}}, "path"},
		{[]SkillLink{{Path: "skills/review/"}}, "not written plainly"},
		{[]SkillLink{{Repo: "acme", Path: "x"}}, "owner/name"},
		{[]SkillLink{{Path: "x", Ref: "main"}}, "base commit"},
		{[]SkillLink{{Repo: "a/b", Path: "x", Ref: "a..b"}}, "branch, tag or commit"},
		{[]SkillLink{{Repo: "a/b", Path: "x", Ref: "-x"}}, "branch, tag or commit"},
		{[]SkillLink{{Path: "x"}, {Path: "x"}}, "linked twice"},
		{[]SkillLink{{Path: "a"}, {Path: "b"}, {Path: "c"}, {Path: "d"}, {Path: "e"}, {Path: "f"}}, "at most 5"},
	} {
		ty := base
		ty.Skills = c.links
		err := ValidateType(ty)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want it to say %q", c.links, err, c.want)
		}
	}
}

func TestSkillURLsAreReadAsLinks(t *testing.T) {
	for in, want := range map[string]SkillLink{
		"https://github.com/anthropics/skills/tree/main/skills/frontend-design": {Repo: "anthropics/skills", Ref: "main", Path: "skills/frontend-design"},
		"https://github.com/acme/skills/blob/v2/review/SKILL.md":                {Repo: "acme/skills", Ref: "v2", Path: "review/SKILL.md"},
		"https://github.com/acme/skills/tree/main/our%20skills/review/?tab=x":   {Repo: "acme/skills", Ref: "main", Path: "our skills/review"},
		" https://www.github.com/acme/skills.git/tree/abc/x/ ":                  {Repo: "acme/skills", Ref: "abc", Path: "x"},
	} {
		got, err := ParseSkillURL(in)
		if err != nil || got != want {
			t.Errorf("%q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"https://github.com/acme/skills", "https://gitlab.com/acme/skills/tree/main/x",
		"https://github.com/acme/skills/tree/main/../x", "skills/review"} {
		if l, err := ParseSkillURL(in); err == nil {
			t.Errorf("%q read as %+v", in, l)
		}
	}
}

func TestSkillNamesAndIDs(t *testing.T) {
	for p, want := range map[string]string{"skills/review": "review", "skills/review/SKILL.md": "review", "REVIEW_GUIDE.md": "REVIEW_GUIDE.md",
		"docs/standards.md": "standards.md"} {
		if got := (SkillLink{Path: p}).Name(); got != want {
			t.Errorf("Name(%q) = %q, want %q", p, got, want)
		}
	}
	ty := Type{Skills: []SkillLink{{Path: "a"}, {Repo: "o/r", Path: "b", Ref: "main"}}}
	if l, i, ok := ty.Skill("S2"); !ok || i != 1 || l.String() != "o/r@main:b" {
		t.Errorf("S2 = %+v %d %v", l, i, ok)
	}
	for _, id := range []string{"S0", "S3", "R1", "S", "2"} {
		if _, _, ok := ty.Skill(id); ok {
			t.Errorf("%s names a skill", id)
		}
	}
}

// A finding citing a skill says which, where it lives; the summary names the skills the review
// followed, and leaves out a private one on a public repository.
func TestSkillsAreNamedWhereTheReviewIsPosted(t *testing.T) {
	ty, _ := BuiltinType(DefaultType)
	ty.Skills = []SkillLink{{Repo: "acme/skills", Path: "review/go", Ref: "main"}}
	ctx := RenderContext{Repo: "acme/web", PR: 7, HeadSHA: strings.Repeat("a", 40), Types: []Type{ty}}
	f := Finding{Path: "x.go", Side: Right, Line: 3, Severity: P1, Category: CategoryConcurrency, Title: "Lock is not held",
		Scenario: "Two writers race.", RuleIDs: []string{"S1", "R2"}}
	why := ctx.whyFlagged(f, ctx.policy())
	if !strings.Contains(why, "Skill `S1`: `go`, `acme/skills@main:review/go`") || !strings.Contains(why, "Rule `R2`") {
		t.Errorf("why flagged:\n%s", why)
	}

	st := SummaryState{ReviewID: "r1", Summary: "Adds a lock.", Types: []TypeRun{{Key: DefaultType}}, Reviews: 1,
		ReviewedSHA: ctx.HeadSHA, HeadSHA: ctx.HeadSHA, FullCoverage: true,
		Skills: []SkillRead{{Name: "go-review", Source: "acme/skills@abcdef1:review/go"}, {Name: "secret-standards", Source: "acme/private@1234567:x", Private: true}}}
	out := RenderSummary(st, ctx)
	if !strings.Contains(out, "Followed skills: `go-review` (`acme/skills@abcdef1:review/go`) · `secret-standards`") {
		t.Errorf("the summary does not name the skills:\n%s", out)
	}
	ctx.PublicRepo = true
	if out := RenderSummary(st, ctx); strings.Contains(out, "secret-standards") || !strings.Contains(out, "`go-review`") {
		t.Errorf("a public repository's summary names a private skill, or drops the public one:\n%s", out)
	}
}
