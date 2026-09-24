package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// Removing a disconnected workspace.
//
// Disconnecting a workspace, next to this on the same card, is the reversible half: the token is
// handed back, the bot goes quiet, and every channel, instruction, budget and thread is kept so
// that reconnecting picks up where it left off. That is the right answer to "we are pausing
// this" and the wrong one to "this workspace was a mistake, or it is closed, or we are done with
// it" — for which the console had nothing at all, and the rail kept a dead row nobody could get
// rid of.
//
// So this is the other half: the workspace and everything recorded for it, gone. What it does
// not take is the account's own things — bundles, credentials, documents, skills and people
// belong to the account and are shared with its other workspaces — nor the audit log, which is
// the account's record that this happened.
//
// Three things stand in front of it. The workspace has to be disconnected first, so that ending
// the install and erasing what it did are two deliberate acts rather than one button that does
// both — and so that a slip stops at the reversible one. It needs the permission that covers
// credentials, the one Disconnect needed. And it asks for the workspace's name typed out: not
// security, since anybody who can press the button can type six letters, but the pause in which
// you read the name and notice it is the wrong workspace.

// handleDeleteTeam ends one workspace's life in this account.
func (b *Bot) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := orgOf(r)
	teamID := r.PathValue("team")
	// A workspace another organisation holds answers exactly as one that was never connected,
	// so this cannot be used to discover who has which workspace.
	t, _ := b.store.Team(ctx, teamID)
	if t == nil || t.OrgID != orgID {
		writeJSON(w, 404, map[string]any{"error": "That workspace is not connected."})
		return
	}
	// Disconnect first. It is the reversible half, it is one press away on the same card, and
	// requiring it means the token is already back with Slack by the time anything is erased.
	if t.Status == "active" {
		writeJSON(w, 409, map[string]any{"error": "Disconnect " + nonEmpty(t.Name, t.TeamID) +
			" first. Removing it erases everything recorded for it, so ending the install is its own step."})
		return
	}
	var in struct{ Confirm string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	// Slack does not always give a workspace a name, and a confirmation nobody can type is a
	// feature nobody can use: the id is what the console shows for such a row, so the id is what
	// it asks for.
	named := nonEmpty(t.Name, t.TeamID)
	if !sameName(in.Confirm, named) {
		writeJSON(w, 400, map[string]any{"error": "Type the workspace's name — " + named + " — to confirm."})
		return
	}

	// Stop what is still running in it. Disconnecting took the Slack token, not the worker
	// containers: a fix job carries on in one of its own, and one finishing after this would
	// push a branch for a workspace that no longer exists here, then write its result to rows
	// that have gone.
	if n := b.cancelJobsForTeam(ctx, orgID, teamID); n > 0 {
		slog.Info("cancelled jobs before removing a workspace", "team", teamID, "jobs", n)
	}
	erased, err := b.store.DeleteTeam(ctx, orgID, teamID)
	if err != nil {
		if err == errNotThisOrgsTeam {
			writeJSON(w, 404, map[string]any{"error": "That workspace is not connected."})
			return
		}
		fail(w, err)
		return
	}
	b.slacks.Evict(teamID)
	b.changed(ctx, orgID)
	// After the sweep, not before: audit_log survives a workspace removal, but a row written
	// ahead of the delete would carry this workspace's team_id and be swept away with it.
	b.audit(r, "workspace.removed", AuditEvent{TeamID: teamID, TargetKind: "workspace",
		TargetID: teamID, TargetName: t.Name, Details: auditDetails(map[string]any{"rows": erased.Rows})})
	slog.Warn("workspace removed", "org", orgID, "team", teamID, "name", t.Name, "rows", erased.Rows)
	writeJSON(w, 200, map[string]any{"ok": true, "rows": erased.Rows})
}

// cancelJobsForTeam stops the fix jobs this workspace has in flight, and leaves every other
// workspace's alone.
func (b *Bot) cancelJobsForTeam(ctx context.Context, orgID int64, teamID string) int {
	if b.jobs == nil {
		return 0
	}
	jobs, _ := b.store.ActiveJobsFor(ctx, orgID)
	n := 0
	for _, j := range jobs {
		if j.TeamID != teamID {
			continue
		}
		if b.jobs.cancel(ctx, &j, "", "the workspace was removed from the console") {
			n++
		}
	}
	return n
}

// sameName compares what somebody typed with the name they were asked to type. Case and
// surrounding space are forgiven — this is a confirmation that the right thing is in front of
// you, not a password — but nothing else is.
func sameName(typed, name string) bool {
	return strings.EqualFold(strings.TrimSpace(typed), strings.TrimSpace(name)) && strings.TrimSpace(name) != ""
}
