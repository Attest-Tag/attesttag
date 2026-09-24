package app

import (
	"context"
	"testing"
	"time"
)

func newTestAgent() *Agent { return &Agent{runs: map[int64]*runHandle{}} }

func TestStopRunsCancelsTheTurn(t *testing.T) {
	a := newTestAgent()
	c := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1.0", UserID: "U1"}
	ctx, end := a.beginRun(context.Background(), c)

	if n := a.StopRuns(1, "T1", "C1", "1.0", "U9"); n != 1 {
		t.Fatalf("stopped %d runs, want 1", n)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stopping a run did not cancel its context")
	}
	if by, stopped := c.stoppedBy(); !stopped || by != "U9" {
		t.Fatalf("stoppedBy() = %q, %v; want U9, true", by, stopped)
	}
	end()
	if n := a.StopRuns(1, "T1", "C1", "1.0", "U9"); n != 0 {
		t.Errorf("stopped %d runs after the turn ended, want 0", n)
	}
}

func TestStopRunsLeavesOtherThreadsAlone(t *testing.T) {
	a := newTestAgent()
	mine := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1.0"}
	theirs := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "2.0"}
	_, end1 := a.beginRun(context.Background(), mine)
	defer end1()
	otherCtx, end2 := a.beginRun(context.Background(), theirs)
	defer end2()

	if n := a.StopRuns(1, "T1", "C1", "1.0", "U9"); n != 1 {
		t.Fatalf("stopped %d runs, want 1", n)
	}
	if _, stopped := theirs.stoppedBy(); stopped {
		t.Error("stopped a run in another thread")
	}
	if otherCtx.Err() != nil {
		t.Error("cancelled another thread's context")
	}
}

func TestCallWithoutARunIsNeverStopped(t *testing.T) {
	if by, stopped := (&Call{}).stoppedBy(); stopped || by != "" {
		t.Errorf("stoppedBy() = %q, %v; want \"\", false", by, stopped)
	}
}

func TestStopWord(t *testing.T) {
	for _, s := range []string{"stop", "Stop", "stop!", "STOP.", "stop it", "please stop", "cancel", "cancel that", "abort", "halt", "nevermind", "never mind"} {
		if !stopWordRe.MatchString(s) {
			t.Errorf("%q should ask me to stop", s)
		}
	}
	for _, s := range []string{"stop the deploy", "can you stop the routine?", "why did it stop", "cancel the meeting for me", "", "halting problem"} {
		if stopWordRe.MatchString(s) {
			t.Errorf("%q is a question, not an interruption", s)
		}
	}
}

// A Slack Connect channel carries the same channel id in both workspaces, so matching a run on
// channel and thread alone let "stop" in one organisation cancel another organisation's turn.
func TestStopRunsDoesNotCrossOrganisations(t *testing.T) {
	a := newTestAgent()
	acme := &Call{OrgID: 1, TeamID: "T_A", Channel: "C_SHARED", ThreadTS: "1.0"}
	beta := &Call{OrgID: 2, TeamID: "T_B", Channel: "C_SHARED", ThreadTS: "1.0"}
	_, endA := a.beginRun(context.Background(), acme)
	defer endA()
	betaCtx, endB := a.beginRun(context.Background(), beta)
	defer endB()

	if n := a.StopRuns(1, "T_A", "C_SHARED", "1.0", "U_A"); n != 1 {
		t.Fatalf("Acme stopped %d runs, want only its own", n)
	}
	select {
	case <-betaCtx.Done():
		t.Fatal("Acme's stop cancelled Beta's run in the same channel id")
	default:
	}
	if _, stopped := beta.stoppedBy(); stopped {
		t.Error("Beta's run was marked stopped by another organisation")
	}
}
