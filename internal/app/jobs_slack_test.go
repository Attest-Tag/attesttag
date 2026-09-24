package app

import (
	"strings"
	"testing"
)

// The checklist is what people watch while the worker runs, so each step's glyph must follow the
// phase events, the last log line must show while active, and a failure must be visible.
func TestJobChecklist(t *testing.T) {
	j := &Job{ID: 12, Repo: "acme/app", Branch: "fix-12-x-attest_tag", Status: jobRunning, Phase: "engine", Title: "Fix x", CreatedAt: "2026-09-02 10:00:00"}
	events := []JobEvent{
		{Kind: JobKindPhase, Phase: "clone", Status: "ok"},
		{Kind: JobKindPhase, Phase: "setup", Status: "ok"},
		{Kind: JobKindPhase, Phase: "build_before", Status: "ok"},
		{Kind: JobKindPhase, Phase: "test_before", Status: "ok"},
		{Kind: JobKindPhase, Phase: "engine", Status: "started"},
		{Kind: JobKindLog, Message: "editing parser.py"},
	}
	md := jobChecklist(j, events)
	for _, want := range []string{"Fix job #12", "`acme/app`", "`fix-12-x-attest_tag`", "● clone", "● set up and check", "◐ fix", "○ build and test", "○ pull request", "_editing parser.py_", "running: engine"} {
		if !strings.Contains(md, want) {
			t.Errorf("checklist lacks %q:\n%s", want, md)
		}
	}
	if f := jobFooter(j); !strings.Contains(f, "stop") {
		t.Errorf("active footer should say how to cancel: %q", f)
	}

	// Commit and push done, PR still in flight: the folded step shows in progress.
	events = append(events, JobEvent{Kind: JobKindPhase, Phase: "engine", Status: "ok"},
		JobEvent{Kind: JobKindPhase, Phase: "build_after", Status: "ok"}, JobEvent{Kind: JobKindPhase, Phase: "test_after", Status: "ok"},
		JobEvent{Kind: JobKindPhase, Phase: "commit", Status: "ok"}, JobEvent{Kind: JobKindPhase, Phase: "push", Status: "ok"}, JobEvent{Kind: JobKindPhase, Phase: "pr", Status: "started"})
	if md = jobChecklist(j, events); !strings.Contains(md, "● build and test → ◐ pull request") {
		t.Errorf("folded step should be in progress:\n%s", md)
	}

	// A failed phase marks its step, and a failed job shows the error and no cancel hint.
	j.Status, j.Error, j.FinishedAt = JobFailed, "tests_failed: 2 failed", "2026-09-02 10:14:00"
	events = append(events[:4], JobEvent{Kind: JobKindPhase, Phase: "engine", Status: "ok"}, JobEvent{Kind: JobKindPhase, Phase: "test_after", Status: "failed"})
	md = jobChecklist(j, events)
	for _, want := range []string{"✕ build and test", "_failed_", "tests_failed: 2 failed"} {
		if !strings.Contains(md, want) {
			t.Errorf("failed checklist lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "editing parser.py") {
		t.Error("a finished job should not show the last log line")
	}
	if f := jobFooter(j); strings.Contains(f, "stop") || !strings.Contains(f, "14m") {
		t.Errorf("finished footer: %q", f)
	}
	if got := jobStateLine(&Job{Status: JobCancelled, CancelBy: "U2", CancelReason: "user"}); !strings.Contains(got, "<@U2>") {
		t.Errorf("cancel state line: %q", got)
	}
}

func TestJobConfirmSummaryAndDescribe(t *testing.T) {
	spec := JobSpec{Repo: "acme/app", BaseBranch: "main", Title: "Null ticket id", Requirement: "Guard the retry path against a missing ticket id.",
		Acceptance: []string{"tests pass"}, Ticket: "86d472qu4", Constraints: JobConstraints{BranchSuffix: "attest_tag"}}
	s := jobConfirmSummary(spec)
	for _, want := range []string{"acme/app", "`main`", "fix-<n>-null-ticket-id-attest_tag", "Guard the retry path", "• tests pass", "86d472qu4", "never merges"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	if len(s) > 2900 {
		t.Errorf("summary too long for a card: %d", len(s))
	}
	// The branch on the card is the branch the worker will push, house convention and all.
	withPrefix := spec
	withPrefix.Kind, withPrefix.Constraints.BranchPrefix = jobKindBugfix, "bugfix/"
	if card := jobConfirmSummary(withPrefix); !strings.Contains(card, "`bugfix/fix-<n>-null-ticket-id-attest_tag`") {
		t.Errorf("the card does not promise the branch the convention requires:\n%s", card)
	}
	d := describeJobDispatch(spec)
	if !strings.Contains(d, "acme/app") || !strings.Contains(d, "NEW branch") || !strings.Contains(d, "against main") {
		t.Errorf("describe: %q", d)
	}
}

// The report's last line is what people read to see what a job cost, so it must carry the money,
// the wall time and the tokens — and say plainly when no price came back rather than "$0.00",
// which read as "this was free" on every job before the worker metered its own spend.
func TestJobSpendLine(t *testing.T) {
	j := &Job{CostUSD: 0.8312, TokensIn: 3_993_012, TokensOut: 29_027,
		CreatedAt: "2026-09-02 10:00:00", FinishedAt: "2026-09-02 10:14:00"}
	got := jobSpendLine(j)
	for _, want := range []string{"$0.831", "14m", "4M in", "29k out"} {
		if !strings.Contains(got, want) {
			t.Errorf("spend line %q lacks %q", got, want)
		}
	}

	j.CostUSD = 0
	if got := jobSpendLine(j); !strings.Contains(got, "not reported") || strings.Contains(got, "$0.00") {
		t.Errorf("an unpriced job should say so, got %q", got)
	}

	// Nothing ran: no tokens, no claim about a price.
	if got := jobSpendLine(&Job{}); got != "$0.00" {
		t.Errorf("empty job spend line = %q, want $0.00", got)
	}
}
