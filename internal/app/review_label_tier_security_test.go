package app

import (
	"slices"
	"testing"
)

// A label rule chooses nothing (review.MatchRule passes over it), so the tier check must not count
// one as a branch rule. Counted, it stood in for the rule it replaced: an editor turned the rule
// that kept main in shadow into a label rule on main, the list read the same, and main's pull
// requests fell through to the live rule below — live posting without connections.manage.
func TestReviewAPILabelRuleDoesNotStandInForABranchRule(t *testing.T) {
	settings := func(s map[string]any) map[string]any { return map[string]any{"settings": s} }
	rig := newReviewAPIRig(t, `{"mode":"shadow","branch_rules":[{"base":"main","post":"shadow"},{"post":"live"}]}`)
	conn := "/api/review-settings/" + rig.connID()

	code, out := rig.call("PUT", conn, rig.editor, settings(map[string]any{"mode": "shadow", "branch_rules": []map[string]any{
		{"base": "main", "labels": []string{"zz"}, "types": []string{"general"}}, {"post": "live"}}}))
	if code != 403 || !slices.Contains(fieldsOf(out), "branch_rules") {
		t.Fatalf("an editor turning main's shadow rule into a label rule = %d %v, want 403 naming branch_rules", code, out)
	}
	d := rig.must(200, "GET", conn+"?base=main&head=feature", rig.viewer, nil)
	if mode := d["rule"].(map[string]any)["effective"].(map[string]any)["mode"]; mode != "shadow" {
		t.Errorf("a pull request into main is %v after the refusal, want shadow", mode)
	}

	// Adding a label rule beside the branch rules moves no branch: still the editor's.
	rig.must(200, "PUT", conn, rig.editor, settings(map[string]any{"mode": "shadow", "branch_rules": []map[string]any{
		{"labels": []string{"perf"}, "types": []string{"general"}}, {"base": "main", "post": "shadow"}, {"post": "live"}}}))
}
