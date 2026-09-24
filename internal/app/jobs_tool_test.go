package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// start_fix_job is offered only where a repository is connected, composes the brief from the
// thread with secrets redacted, holds it for a human, and the confirmation dispatches it without
// a model turn.
func TestFixJobToolHoldsAndConfirmDispatches(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	b := h.b
	a := &Agent{cfg: b.cfg, slacks: b.slacks, store: b.store, proxy: b.proxy, settings: b.settings, jobs: b.jobs, tools: map[string]Tool{}}
	conn, _ := b.store.Connection(ctx, orgID, h.connID)
	acc := &Access{Rules: []Rule{{Conn: conn, Rank: 2}}, DefaultRepo: "acme/app", ToolPacks: map[string]bool{}}
	c := &Call{OrgID: orgID, TeamID: "T1", SL: a.slacks.Any(ctx), Channel: "C1", ThreadTS: "1.1", UserID: "U1", Access: acc, Session: &Session{}}

	tools := a.toolsFor(ctx, c)
	tool, ok := tools["start_fix_job"]
	if !ok {
		t.Fatal("start_fix_job not offered in a channel with a repository")
	}
	if _, ok := a.toolsFor(ctx, &Call{Channel: "C1", Access: &Access{ToolPacks: map[string]bool{}}})["start_fix_job"]; ok {
		t.Error("start_fix_job offered without a repository")
	}
	if _, err := tool.Run(ctx, c, json.RawMessage(`{"title":"x"}`)); err == nil {
		t.Error("a brief without a requirement must be refused")
	}

	out, err := tool.Run(ctx, c, json.RawMessage(`{"title":"Null ticket id","requirement":"Guard the retry path against a missing ticket id.","acceptance":["tests pass"],"ticket":"86d472qu4"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "needs a human OK") || c.pendingID == 0 || !strings.Contains(c.pendingSummary, "acme/app") {
		t.Fatalf("hold: out=%q pending=%d summary=%q", out, c.pendingID, c.pendingSummary)
	}
	_, raw, err := b.store.TakePendingWrite(ctx, orgID, "T1", "C1", "1.1")
	if err != nil || raw == "" {
		t.Fatalf("pending write: %q %v", raw, err)
	}
	var held struct {
		Job *JobSpec `json:"job"`
	}
	if err := json.Unmarshal([]byte(raw), &held); err != nil || held.Job == nil {
		t.Fatalf("held request is not a job: %s", raw)
	}
	sp := held.Job
	if sp.ConnectionID != h.connID || sp.Repo != "acme/app" || sp.BaseBranch != "main" || sp.Ticket != "86d472qu4" || len(sp.Acceptance) != 1 {
		t.Errorf("spec: %+v", sp)
	}
	if !strings.Contains(sp.ThreadText, "[redacted-secret]") || strings.Contains(sp.ThreadText, "ghp_") {
		t.Errorf("thread text not redacted: %q", sp.ThreadText)
	}
	if len(h.fd.launches) != 0 {
		t.Fatal("holding must not dispatch")
	}

	// Confirmation (button or typed) runs the dispatch; the launch carries only id, URL, token.
	b.runPending(ctx, b.slacks.Any(ctx), "channel", "C1", "1.1", "U2", "U2", raw, &Session{})
	jobs, _ := b.store.Jobs(ctx, orgID, JobFilter{})
	if len(jobs) != 1 || jobs[0].Status != jobStarting || jobs[0].ApprovedBy != "U2" || jobs[0].Approval != "confirm" || jobs[0].Title != "Null ticket id" {
		t.Fatalf("after confirm: %+v", jobs)
	}
	if len(h.fd.launches) != 1 || h.fd.launches[0].JobID != jobs[0].ID {
		t.Fatalf("launches: %+v", h.fd.launches)
	}
	if _, err := tool.Run(ctx, c, json.RawMessage(`{"title":"again","requirement":"again"}`)); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("a second job in the thread should be refused while one runs, got %v", err)
	}

	// The default branch comes from GitHub when the caller names none.
	h2 := newJobHarness(t, func(r *http.Request) (int, string) { return 200, `{"default_branch":"develop"}` })
	a2 := &Agent{cfg: h2.b.cfg, slacks: h2.b.slacks, store: h2.b.store, proxy: h2.b.proxy, settings: h2.b.settings, jobs: h2.b.jobs, tools: map[string]Tool{}}
	conn2, _ := h2.b.store.Connection(ctx, orgID, h2.connID)
	c2 := &Call{TeamID: "T1", SL: a2.slacks.Any(ctx), Channel: "C9", ThreadTS: "9.9", UserID: "U1", Session: &Session{}, Access: &Access{Rules: []Rule{{Conn: conn2, Rank: 2}}, ToolPacks: map[string]bool{}}}
	if _, err := a2.toolsFor(ctx, c2)["start_fix_job"].Run(ctx, c2, json.RawMessage(`{"title":"t","requirement":"r"}`)); err != nil {
		t.Fatal(err)
	}
	_, raw2, _ := h2.b.store.TakePendingWrite(ctx, orgID, "T1", "C9", "9.9")
	json.Unmarshal([]byte(raw2), &held)
	if held.Job == nil || held.Job.BaseBranch != "develop" {
		t.Errorf("default branch: %+v", held.Job)
	}
}
