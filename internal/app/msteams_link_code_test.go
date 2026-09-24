package app

import (
	"context"
	"strings"
	"testing"
)

// A link code was spent before the membership check, so a guest who tried it used it up — and the
// refusal told them to try the same code again, which could not work for them or for anyone. The
// code is now spent only once the link stands: the member it was meant for can still use it.
func TestARefusedTeamsLinkLeavesTheCodeForAMember(t *testing.T) {
	_, mux, f, st := teamsTestBot(t, "should never be said")
	ctx := context.Background()
	org, err := st.CreateOrg(ctx, "Fabrikam", 0)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := st.NewLinkCode(ctx, org.ID, platformMSTeams, 0)
	if err != nil {
		t.Fatal(err)
	}

	f.addMember(msMember{ID: "29:gus", Name: "Gus", AADObjectID: "8f3b1c2d-0000-4000-8000-00000000000b", TenantID: teamsOrg, Role: "guest"})
	deliverTeams(t, mux, f.sign(nil), f.activity("link "+code, map[string]any{
		"from":         map[string]any{"id": "29:gus", "name": "Gus", "aadObjectId": "8f3b1c2d-0000-4000-8000-00000000000b"},
		"conversation": map[string]any{"id": "a:chat-gus", "conversationType": "personal", "tenantId": teamsOrg},
	}))
	posts := f.waitForMessages(1)
	if got := posts[len(posts)-1].Activity.Text; !strings.Contains(got, "Only a member") {
		t.Fatalf("the guest was told %q, want the membership refusal", got)
	}
	if team, _ := st.Team(ctx, "msteams:"+teamsOrg); team != nil {
		t.Fatalf("a refused link left the tenant connected: %+v", team)
	}
	if _, err := st.PeekLinkCode(ctx, code, platformMSTeams); err != nil {
		t.Fatalf("the guest's refused attempt used the code up: %v", err)
	}

	// The member the code was meant for runs the same code.
	deliverTeams(t, mux, f.sign(nil), f.activity("link "+code, nil))
	posts = f.waitForMessages(2)
	if got := posts[len(posts)-1].Activity.Text; !strings.Contains(got, "Connected") {
		t.Fatalf("the member was told %q, want the tenant connected", got)
	}
	if team, _ := st.Team(ctx, "msteams:"+teamsOrg); team == nil || team.OrgID != org.ID {
		t.Fatalf("the member's link did not stand: %+v", team)
	}
	if _, err := st.PeekLinkCode(ctx, code, platformMSTeams); err == nil {
		t.Error("the code is still unspent after the link it made")
	}
}

// And a refused attempt on a tenant that is already connected to the same account undoes only
// itself. The refusal used to delete the tenant, which erases everything kept for it.
func TestARefusedTeamsLinkDoesNotUnlinkAConnectedTenant(t *testing.T) {
	_, mux, f, st := teamsTestBot(t, "should never be said")
	ctx := context.Background()
	org := linkTeams(t, st, mux, f)
	before := len(f.messages())
	code, _, err := st.NewLinkCode(ctx, org.ID, platformMSTeams, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.addMember(msMember{ID: "29:gus", Name: "Gus", AADObjectID: "8f3b1c2d-0000-4000-8000-00000000000b", TenantID: teamsOrg, Role: "guest"})
	deliverTeams(t, mux, f.sign(nil), f.activity("link "+code, map[string]any{
		"from":         map[string]any{"id": "29:gus", "name": "Gus", "aadObjectId": "8f3b1c2d-0000-4000-8000-00000000000b"},
		"conversation": map[string]any{"id": "a:chat-gus", "conversationType": "personal", "tenantId": teamsOrg},
	}))
	posts := f.waitForMessages(before + 1)
	if got := posts[len(posts)-1].Activity.Text; !strings.Contains(got, "Only a member") {
		t.Fatalf("the guest was told %q, want the membership refusal", got)
	}
	team, _ := st.Team(ctx, "msteams:"+teamsOrg)
	if team == nil || team.OrgID != org.ID || team.Status != "active" {
		t.Fatalf("a guest's refused code unlinked the tenant: %+v", team)
	}
}
