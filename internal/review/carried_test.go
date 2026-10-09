package review

import (
	"encoding/json"
	"strings"
	"testing"
)

// What a review took from merged pull requests is said in its summary, and none of it is scored:
// the files it did not read again, linked to the pull requests that read them, and what those left
// open, linked to their threads — the worst of them named in the first sentence when it is a P0 or
// P1, since it ships with this pull request whatever this review found.
func TestSummaryListsWhatWasCarriedForward(t *testing.T) {
	s := SummaryState{ReviewID: testFindingID, FullCoverage: true, Reviews: 1, ReviewedSHA: testHead, HeadSHA: testHead,
		Types: []TypeRun{{Key: "general"}, {Key: "security", Summary: "Reads the new handler."}},
		Carried: []CarriedFile{
			{Path: "api/list.go", PR: 15},
			{Path: "api/items.go", PR: 12},
			{Path: "web/app.tsx", PR: 12, Types: []string{"general"}},
		},
		CarriedOpen: []CarriedFinding{
			{PR: 15, ID: "f2", Severity: P2, Type: "general", Title: "Name shadows the import", Path: "api/list.go", Line: 9},
			{PR: 12, ID: "f1", Severity: P1, Type: "security", Title: "Items query is not scoped [x](https://evil.example)",
				Path: "api/items.go", StartLine: 40, Line: 52, CommentURL: "https://github.com/acme/web/pull/12#discussion_r99"},
		},
	}
	got := RenderSummary(s, testRenderContext())
	for _, want := range []string{
		"Confidence 5/5",
		"No blocking issues found. Still open on the merged pull request this code came in with: **Items query is not scoped",
		"(P1, [#12](https://github.com/acme/web/pull/12)).",
		"<details><summary>Open from merged pull requests (2)</summary>",
		"(https://github.com/acme/web/pull/12#discussion_r99) · `api/items.go:L40-52` · [#12](https://github.com/acme/web/pull/12)",
		"**P2** · General · Name shadows the import · `api/list.go:L9` · [#15](https://github.com/acme/web/pull/15)",
		"<details><summary>Reviewed earlier, unchanged since (3 files, in #12, #15)</summary>",
		"Read by the review of [#12](https://github.com/acme/web/pull/12) and [#15](https://github.com/acme/web/pull/15)",
		"- `web/app.tsx` · [#12](https://github.com/acme/web/pull/12) · for General only",
		"- `api/list.go` · [#15](https://github.com/acme/web/pull/15)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "evil.example") || strings.Contains(got, "Not reviewed") {
		t.Errorf("the summary carries what it must not:\n%s", got)
	}
	if strings.Index(got, "Open from merged pull requests") > strings.Index(got, "Reviewed earlier") {
		t.Error("the open findings are listed after the files")
	}
}

// Only a P0 or a P1 left open on a merged pull request is named in the first sentence; a P2 is listed
// and nothing more. A review with nothing carried says nothing of it.
func TestCarriedRiskNamesOnlyWhatBlocks(t *testing.T) {
	s := SummaryState{ReviewID: testFindingID, FullCoverage: true, ReviewedSHA: testHead, HeadSHA: testHead,
		CarriedOpen: []CarriedFinding{{PR: 15, ID: "f2", Severity: P2, Title: "Name shadows the import", Path: "api/list.go", Line: 9}}}
	got := RenderSummary(s, testRenderContext())
	if strings.Contains(got, "Still open on the merged pull request") || !strings.Contains(got, "Open from merged pull requests (1)") {
		t.Errorf("a carried P2:\n%s", got)
	}
	s.CarriedOpen = append(s.CarriedOpen,
		CarriedFinding{PR: 15, ID: "f3", Severity: P1, Title: "Retry loops for ever", Path: "api/list.go", Line: 20},
		CarriedFinding{PR: 12, ID: "f4", Severity: P0, Title: "Token logged in plain text", Path: "api/auth.go", Line: 3})
	if got := RenderSummary(s, testRenderContext()); !strings.Contains(got,
		"Still open on the merged pull request this code came in with: **Token logged in plain text** (P0, [#12](https://github.com/acme/web/pull/12)) and 1 more.") {
		t.Errorf("two serious carried findings:\n%s", got)
	}
	s.CarriedOpen = nil
	if got := RenderSummary(s, testRenderContext()); strings.Contains(got, "merged pull request") || strings.Contains(got, "Reviewed earlier") {
		t.Errorf("nothing carried, and the summary speaks of it:\n%s", got)
	}
}

// The ceiling on automatic reviews is a setting: ten by default, 0 for none, at most
// MaxAutoPauseAfter, inherited like any single value, and outside the hash — it decides whether a
// review runs, not what it finds, so no cached review is missed for it.
func TestAutoPauseAfterIsASettingOutsideTheHash(t *testing.T) {
	base := Resolve(nil)
	if base.AutoPause() != DefaultAutoPauseAfter || DefaultAutoPauseAfter != 10 || base.Source["auto_pause_after"] != LevelDefault {
		t.Errorf("the built-in ceiling = %d from %s", base.AutoPause(), base.Source["auto_pause_after"])
	}
	if (Effective{}).AutoPause() != DefaultAutoPauseAfter {
		t.Error("an Effective built in code has no ceiling")
	}
	var never Settings
	if err := json.Unmarshal([]byte(`{"auto_pause_after":0}`), &never); err != nil || never.Validate() != nil {
		t.Fatalf("auto_pause_after 0: %v, %v", err, never.Validate())
	}
	e := Resolve([]LevelSettings{{LevelConnection, Settings{AutoPauseAfter: ptr(3)}}, {LevelRepo, never}})
	if e.AutoPause() != 0 || e.Source["auto_pause_after"] != LevelRepo {
		t.Errorf("the repository's 0 under a connection's 3 = %d from %s", e.AutoPause(), e.Source["auto_pause_after"])
	}
	if raw, _ := json.Marshal(e); !strings.Contains(string(raw), `"auto_pause_after":0`) {
		t.Errorf("never is left out of the effective settings: %s", raw)
	}
	if e.Hash() != base.Hash() {
		t.Error("the ceiling changed the settings' hash")
	}
	for _, bad := range []int{-1, MaxAutoPauseAfter + 1} {
		if err := (Settings{AutoPauseAfter: ptr(bad)}).Validate(); err == nil || !strings.Contains(err.Error(), "auto_pause_after") {
			t.Errorf("auto_pause_after %d: %v", bad, err)
		}
	}
	if got := ChangedFields(Settings{}, Settings{AutoPauseAfter: ptr(4)}); len(got) != 1 || got[0] != "auto_pause_after" {
		t.Errorf("changed fields = %v", got)
	}
}
