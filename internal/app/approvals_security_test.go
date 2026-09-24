package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// An approver is a person in a workspace; a Confirm press belongs to one channel and one
// requester; what a card shows is what will run. Each test asserts the safe behaviour of one of
// the review's findings.

func approvalOrg(t *testing.T, st *Store) (orgID int64, roleID int64) {
	t.Helper()
	ctx := context.Background()
	orgID, _, _ = seedOrg(t, st, RoleAdmin)
	r := &ApprovalRole{Name: "Approver", Rank: 1}
	if err := st.AddApprovalRole(ctx, orgID, r); err != nil {
		t.Fatal(err)
	}
	return orgID, r.ID
}

func TestApproversAreBoundToTheirWorkspace(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	orgID, roleID := approvalOrg(t, st)
	for _, team := range []string{"T1", "T2"} {
		if err := st.SaveTeam(ctx, &Team{TeamID: team, OrgID: orgID, Name: team}, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	// alice was resolved, from the console, in T1.
	if err := st.AddResolvedApprovalMember(ctx, orgID, roleID, "alice@example.com", "T1", "U_ALICE"); err != nil {
		t.Fatal(err)
	}
	// bob was written as a bare address before entries carried a workspace.
	if err := st.AddApprovalMember(ctx, orgID, roleID, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	a := &Agent{store: st}
	members := func(team string) []string {
		var out []string
		for _, tier := range a.tiers(ctx, orgID, &Chat{TeamID: team, OrgID: orgID}) {
			out = append(out, tier.Members...)
		}
		return out
	}
	if got := members("T1"); len(got) != 1 || got[0] != "U_ALICE" {
		t.Fatalf("T1 approvers = %v, want alice's account there", got)
	}
	// From T2 — a second connected workspace, whose directory could say anything about
	// alice@example.com — she is nobody, and bob's untied address resolves nowhere either.
	if got := members("T2"); len(got) != 0 {
		t.Fatalf("T2 approvers = %v, want none", got)
	}
	if a.rankOf(ctx, orgID, &Chat{TeamID: "T2", OrgID: orgID}, "U_ALICE") >= 0 {
		t.Fatal("alice's T1 account held a rank when pressing from T2")
	}
}

func TestMayApproveRefusesAnotherWorkspace(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	orgID, roleID := approvalOrg(t, st)
	st.AddResolvedApprovalMember(ctx, orgID, roleID, "U_APP", "T1", "U_APP")
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), agent: &Agent{store: st}}
	r := &AccessRequest{OrgID: orgID, TeamID: "T1", Requester: "U_ASK"}
	if ok, why := b.mayApprove(ctx, &Chat{TeamID: "T2", BotUserID: "UBOT", OrgID: orgID}, "U_APP", r); ok || !strings.Contains(why, "different workspace") {
		t.Fatalf("a press from another workspace was allowed: %v %q", ok, why)
	}
}

func TestConfirmPressIsRequesterOrApprover(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	orgID, roleID := approvalOrg(t, st)
	st.AddResolvedApprovalMember(ctx, orgID, roleID, "U_APP", "T1", "U_APP")
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), agent: &Agent{store: st}}
	sl := &Chat{TeamID: "T1", OrgID: orgID}
	if !b.mayConfirm(ctx, sl, "U_ASK", "U_ASK") {
		t.Error("the requester could not confirm their own write")
	}
	if b.mayConfirm(ctx, sl, "U_BYSTANDER", "U_ASK") {
		t.Error("anybody who could see the card could run it")
	}
	if !b.mayConfirm(ctx, sl, "U_APP", "U_ASK") {
		t.Error("an approver in this workspace could not confirm")
	}
	if b.mayConfirm(ctx, &Chat{TeamID: "T2", OrgID: orgID}, "U_APP", "U_ASK") {
		t.Error("an approver's authority followed them into another workspace")
	}
	if b.mayConfirm(ctx, sl, "U_ASK", "") {
		t.Error("a write with no requester could be confirmed")
	}
}

func TestConfirmSummariesEscapeModelText(t *testing.T) {
	// A link written as <evil|safe> shows the safe URL and goes somewhere else; a body holding
	// ``` closes the code block and whatever follows renders as card structure.
	s := httpConfirmSummary(&Connection{Name: "GitHub <admin>"}, ProxyRequest{Method: "post",
		URL:  "<https://evil.example|https://api.github.com/orgs/acme/invitations>",
		Body: "```\n<https://evil.example|api.github.com> *approved*"})
	if strings.Contains(s, "<https://evil") || strings.Contains(s, "<admin>") {
		t.Fatalf("model text reached the card unescaped:\n%s", s)
	}
	if n := strings.Count(s, "```"); n != 2 {
		t.Fatalf("the body broke out of its code block (%d fences):\n%s", n, s)
	}
	m := mcpConfirmSummary(&Connection{Name: "Jira"}, "create_issue <b>", map[string]any{"summary": "```x"})
	if strings.Contains(m, "<b>") || strings.Count(m, "```") != 2 {
		t.Fatalf("MCP card:\n%s", m)
	}
	steps := renderSteps([]json.RawMessage{json.RawMessage(`{"method":"POST","url":"https://api.example.com/x","body":"` + "```" + `<https://evil|safe>"}`)})
	if strings.Contains(steps, "<https://evil") || strings.Count(steps, "```") != 2 {
		t.Fatalf("access card steps:\n%s", steps)
	}
}

func TestUserByEmailCacheExpires(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"ok":true,"user":{"id":"U_NEW"}}`)
	}))
	defer srv.Close()
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, TeamID: "T1"}
	// A stale hit — an address that moved to another account an hour ago — is looked up again.
	sl.byEmail.Store("alice@example.com", emailHit{id: "U_OLD", at: time.Now().Add(-time.Hour)})
	if id, err := sl.UserByEmail(context.Background(), "alice@example.com"); err != nil || id != "U_NEW" {
		t.Fatalf("stale entry served: %q %v", id, err)
	}
	if id, _ := sl.UserByEmail(context.Background(), "alice@example.com"); id != "U_NEW" || calls != 1 {
		t.Fatalf("fresh entry not cached: %q, %d calls", id, calls)
	}
}
