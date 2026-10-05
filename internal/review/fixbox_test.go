package review

import (
	"strings"
	"testing"
)

// A tick is the edit that turns the one fix item from unticked to ticked, and nothing else is: an
// App's own edit, an untick, a box ticked again, or a comment somebody edited into holding two.
func TestFixBoxTicked(t *testing.T) {
	open, ticked := "- [ ] "+FixBoxText, "- [x] "+FixBoxText
	body := func(item string) string {
		return "**Security · P1 · Tenant check missing**\n\nThe list query skips the tenant.\n\n" + item +
			"\n\n<sub>Reply here.</sub>\n\n<!-- attest_tag:finding=abc.def -->"
	}
	cases := []struct {
		name          string
		before, after string
		want          bool
	}{
		{"ticked", body(open), body(ticked), true},
		{"ticked with a capital X", body(open), body("- [X] " + FixBoxText), true},
		{"a star bullet", body("* [ ] " + FixBoxText), body("* [x] " + FixBoxText), true},
		{"CRLF, as GitHub may store it", strings.ReplaceAll(body(open), "\n", "\r\n"), strings.ReplaceAll(body(ticked), "\n", "\r\n"), true},
		{"unticked", body(ticked), body(open), false},
		{"ticked already", body(ticked), body(ticked), false},
		{"no change", body(open), body(open), false},
		{"text edited, box untouched", body(open), body(open) + "\nedited", false},
		{"no box before", body(""), body(ticked), false},
		{"the box removed", body(open), body(""), false},
		{"another item's text", body("- [ ] Fix something else"), body("- [x] Fix something else"), false},
		{"two boxes after", body(open), body(ticked + "\n" + ticked), false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FixBoxTicked(c.before, c.after); got != c.want {
				t.Errorf("FixBoxTicked = %v, want %v", got, c.want)
			}
		})
	}
}

// The box is on a finding's comment only where a fix can be asked for, unticked, and the hint below
// it names the command that does the same where a box cannot be ticked.
func TestRenderFindingOffersTheFixBox(t *testing.T) {
	f := Finding{Path: "store.go", Line: 12, Severity: P1, Category: CategoryBug, Title: "Tenant check missing",
		Scenario: "The list query skips the tenant."}
	without := RenderFinding(f, RenderContext{Repo: "acme/web", PR: 7, Slug: "attesttag"})
	if strings.Contains(without, FixBoxText) || strings.Contains(without, " fix</code>") {
		t.Errorf("a comment that cannot be fixed from offers a fix:\n%s", without)
	}
	with := RenderFinding(f, RenderContext{Repo: "acme/web", PR: 7, Slug: "attesttag", FixBox: true})
	if !strings.Contains(with, "\n- [ ] "+FixBoxText+"\n") || fixBoxState(with) != " " {
		t.Errorf("the fix box is missing or not on a line of its own:\n%s", with)
	}
	if !strings.Contains(with, "<code>@attesttag fix</code>") {
		t.Errorf("the hint does not name the command:\n%s", with)
	}
	if !FixBoxTicked(with, strings.Replace(with, "- [ ] ", "- [x] ", 1)) {
		t.Error("ticking the rendered box is not read as a tick")
	}
}
