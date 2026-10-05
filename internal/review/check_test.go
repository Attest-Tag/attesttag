package review

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

const testSummaryURL = "https://github.com/acme/web/pull/7#issuecomment-555"

// A posted review's check: the score and the open findings for the title, and for the summary what
// stands in the way of merging, the findings worst first with their threads and lines, and the way to
// the summary comment — the same findings the summary comment scores, in the same order however they
// were loaded.
func TestRenderCheckGolden(t *testing.T) {
	ctx := testRenderContext()
	s := doneSummary()
	title, summary := RenderCheck(s, ctx, testSummaryURL)
	golden(t, "check_done", title+"\n\n"+summary)

	if title != "Confidence 1/5 · 5 open findings" {
		t.Errorf("title = %q", title)
	}
	for range 5 {
		shuffled := s
		shuffled.Findings = slices.Clone(s.Findings)
		rand.Shuffle(len(shuffled.Findings), func(i, j int) {
			shuffled.Findings[i], shuffled.Findings[j] = shuffled.Findings[j], shuffled.Findings[i]
		})
		if t2, s2 := RenderCheck(shuffled, ctx, testSummaryURL); t2 != title || s2 != summary {
			t.Fatal("the check depends on the order of its findings")
		}
	}
	if len(ParseMarkers(summary)) != 0 {
		t.Error("a check carries a marker, which nothing reads there")
	}
	if !strings.Contains(summary, "[summary comment]("+testSummaryURL+")") {
		t.Errorf("the check does not link the summary comment:\n%s", summary)
	}
	// A summary comment on another repository is no link of ours to make.
	if _, other := RenderCheck(s, ctx, "https://github.com/evil/web/pull/7#issuecomment-1"); strings.Contains(other, "evil/web") {
		t.Error("the check linked a summary comment on another repository")
	}
}

// Nothing open is said in the title, and a score capped short of 5 says why.
func TestRenderCheckWithNothingOpen(t *testing.T) {
	s := doneSummary()
	s.Findings = nil
	title, summary := RenderCheck(s, testRenderContext(), testSummaryURL)
	if title != "Confidence 4/5 · no open findings" || !strings.Contains(summary, "No blocking issues found.") ||
		!strings.Contains(summary, "capped at 4 because not every changed line was reviewed") {
		t.Errorf("title %q, summary:\n%s", title, summary)
	}
	s.FullCoverage = true
	if title, _ := RenderCheck(s, testRenderContext(), ""); title != "Confidence 5/5 · no open findings" {
		t.Errorf("title = %q", title)
	}
}

// Everything in a check that a model wrote passes the sanitiser the summary comment's text does: a
// title cannot mention anybody, carry an image, or link anywhere but the pull request's repository.
func TestRenderCheckHostileTitle(t *testing.T) {
	f := validFinding()
	f.Title = "**Ping** @octocat and @octo-org/admins: [log in](https://evil.example/login) <img src=x>"
	s := SummaryState{ReviewedSHA: testHead, Findings: []SummaryFinding{{Finding: f, ID: "f1", Status: FindingOpen,
		Placement: PlacementInline, CommentURL: "https://github.com/acme/web/pull/7#discussion_r9"}}}
	title, summary := RenderCheck(s, testRenderContext(), testSummaryURL)
	for _, bad := range []string{"](https://evil", "<img", "@octocat", "@octo-org"} {
		if strings.Contains(title+summary, bad) {
			t.Errorf("%q reached the check:\n%s", bad, summary)
		}
	}
}

// A long list stops at maxCheckRows and counts the rest, which the summary comment lists.
func TestRenderCheckListIsCapped(t *testing.T) {
	var fs []SummaryFinding
	for i := range maxCheckRows + 4 {
		f := validFinding()
		f.Title, f.Line = fmt.Sprintf("Finding %02d", i), 10+i
		fs = append(fs, SummaryFinding{Finding: f, ID: fmt.Sprintf("f%d", i), Status: FindingOpen, Placement: PlacementInline})
	}
	_, summary := RenderCheck(SummaryState{Findings: fs, FullCoverage: true}, testRenderContext(), testSummaryURL)
	if n := strings.Count(summary, "\n- **"); n != maxCheckRows {
		t.Errorf("%d findings listed, want %d", n, maxCheckRows)
	}
	if !strings.Contains(summary, "- …and 4 more in the summary comment") {
		t.Errorf("the rest are not counted:\n%s", summary)
	}
}

// While it runs the check says the commit and the types; once a review ends unposted it says how,
// never the error itself, and how to ask again — the command in code, so nobody is mentioned.
func TestCheckStartAndEnded(t *testing.T) {
	ctx := testRenderContext()
	ctx.Types = []Type{{Key: "general", Name: "General"}, {Key: "custom", Name: "Ask @octocat"}}
	title, summary := CheckStart(ctx, testHead, []string{"general", "security", "custom"})
	if title != "Reviewing c03ddb1" || !strings.HasPrefix(summary, "Reviewing `c03ddb1` as General, Security and ") ||
		strings.Contains(summary, "@octocat") {
		t.Errorf("start: %q / %q", title, summary)
	}
	if title, _ := CheckStart(ctx, "main", nil); title != "Reviewing" {
		t.Errorf("a start on no commit: %q", title)
	}

	for _, c := range []struct {
		status string
		reason FailReason
		title  string
		has    string
	}{
		{"failed", FailModel, "Review failed: the model provider did not answer", "To ask for another: `@attest-tag-test review`."},
		{"failed", FailReason("ECONNREFUSED 10.0.0.7:5432"), "Review failed: an internal error; the console has the details", "did not finish"},
		{"cancelled", "", "Review cancelled", "was cancelled before it was posted"},
		{"superseded", "", "Superseded by a newer commit", "its newer head has a review of its own"},
		{"skipped", "", "Review not run", "stopped before it ran"},
	} {
		title, summary := CheckEnded(c.status, testHead, c.reason, ctx.Slug)
		if title != c.title || !strings.Contains(summary, c.has) || strings.Contains(summary, "10.0.0.7") ||
			!strings.Contains(summary, "`c03ddb1`") {
			t.Errorf("%s (%s): %q / %q", c.status, c.reason, title, summary)
		}
	}
	if _, summary := CheckEnded("failed", testHead, FailModel, "not a slug!"); strings.Contains(summary, "@") {
		t.Errorf("a slug that is not one was offered as a command: %q", summary)
	}

	title, summary = CheckStanding(testHead, 3, testSummaryURL, ctx)
	if title != "Confidence 3/5 · already reviewed" || !strings.Contains(summary, "("+testSummaryURL+")") {
		t.Errorf("standing: %q / %q", title, summary)
	}
	if title, _ := CheckStanding(testHead, -1, "", ctx); title != "Already reviewed" {
		t.Errorf("standing with no score: %q", title)
	}
}
