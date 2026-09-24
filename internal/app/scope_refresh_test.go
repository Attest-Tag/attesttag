package app

import (
	"context"
	"fmt"
	"testing"
)

// The refresh button beside a workspace: ask Slack again, and bring in the channels somebody has
// invited the bot to since the page was drawn.
func TestRefreshWorkspacePullsInNewChannels(t *testing.T) {
	h := newLeaveHarness(t)
	h.channels = `{"id":"C1","name":"eng","is_private":false},{"id":"C2","name":"support","is_private":true}`

	code, out := authReq(t, h.mux, "POST", "/api/teams/TA/sync", nil, h.token)
	if code != 200 {
		t.Fatalf("refreshing the workspace = %d: %v", code, out)
	}
	if out["added"] != float64(1) || out["channels"] != float64(2) {
		t.Errorf("refresh reported %v; want one channel added, two in total", out)
	}
	if got := h.listed(t); len(got) != 2 || got[1] != "C2" {
		t.Fatalf("rail lists %v, want the newly joined channel to have arrived", got)
	}
	sc, _ := h.st.ChannelScope(context.Background(), h.orgID, "TA", "C2")
	if sc == nil || !sc.IsPrivate {
		t.Errorf("the new channel came in as %+v; a private channel has to arrive marked private", sc)
	}

	// Nothing new the second time round, and the count still describes what is there.
	code, out = authReq(t, h.mux, "POST", "/api/teams/TA/sync", nil, h.token)
	if code != 200 || out["added"] != float64(0) || out["channels"] != float64(2) {
		t.Errorf("a second refresh = %d %v; want nothing added and the same two channels", code, out)
	}
}

// A refresh must not undo the removal that was just made: Slack can still report the bot as a
// member for a moment after it leaves, and the button sits right next to the channel that left.
func TestRefreshDoesNotUndoARemoval(t *testing.T) {
	h := newLeaveHarness(t)

	if code, out := authReq(t, h.mux, "POST", fmt.Sprintf("/api/scopes/%d/leave", h.channel.ID), nil, h.token); code != 200 {
		t.Fatalf("removing the channel = %d: %v", code, out)
	}
	if code, _ := authReq(t, h.mux, "POST", "/api/teams/TA/sync", nil, h.token); code != 200 {
		t.Fatalf("refreshing after a removal should still answer 200")
	}
	if got := h.listed(t); len(got) != 0 {
		t.Errorf("rail lists %v; a refresh pressed after a removal brought the channel back", got)
	}
}

// Whose workspace it is, and whether Slack will talk about it at all.
func TestRefreshWorkspaceRefusals(t *testing.T) {
	h := newLeaveHarness(t)
	ctx := context.Background()

	if code, _ := authReq(t, h.mux, "POST", "/api/teams/TNOPE/sync", nil, h.token); code != 404 {
		t.Errorf("refreshing a workspace that is not connected = %d, want 404", code)
	}

	// Another organisation's workspace, by its real id, with a valid session of our own.
	otherOrg, _, _ := seedOrgAs(t, h.st, "other@example.com", RoleAdmin)
	enc, _ := h.b.sealer.Seal([]byte("xoxb-theirs"))
	if err := h.st.SaveTeam(ctx, &Team{TeamID: "TB", OrgID: otherOrg, Name: "Theirs"}, enc); err != nil {
		t.Fatal(err)
	}
	if code, _ := authReq(t, h.mux, "POST", "/api/teams/TB/sync", nil, h.token); code != 404 {
		t.Errorf("refreshing another organisation's workspace = %d, want 404", code)
	}

	// A disconnected workspace has nothing to ask Slack with, and says so rather than failing quietly.
	if err := h.st.RevokeTeam(ctx, "TA", "disconnected in a test"); err != nil {
		t.Fatal(err)
	}
	code, out := authReq(t, h.mux, "POST", "/api/teams/TA/sync", nil, h.token)
	if code != 409 {
		t.Errorf("refreshing a disconnected workspace = %d, want 409: %v", code, out)
	}

	// A viewer holds no scopes.manage, so the button is not theirs to press.
	_, _, viewer := seedOrgAs(t, h.st, "viewer@example.com", RoleViewer)
	if code, _ := authReq(t, h.mux, "POST", "/api/teams/TA/sync", nil, viewer); code != 403 {
		t.Errorf("a viewer refreshing a workspace = %d, want 403", code)
	}
}
