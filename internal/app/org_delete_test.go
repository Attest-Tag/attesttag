package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Deleting an account. The tests here are about the two halves that can go wrong quietly: the
// rule about who may press it, and whether pressing it actually leaves nothing behind.

// ---- what is left afterwards ----

// The guard on the sweep. A table added later belongs to one of the four rules below, and this
// is where that decision is written down — so a new table cannot answer "which of these am I?"
// by being forgotten, and its rows outlive the customer who asked to be deleted.
func TestDeletingAnAccountReachesEveryTable(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	scoped, err := st.orgScopedTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]string{}
	for _, tb := range scoped {
		covered[tb] = "org_id"
	}
	for _, tb := range teamKeyedTables {
		covered[tb] = "team_id"
	}
	for _, tb := range accountTables {
		covered[tb] = "user_id"
	}
	for tb, why := range tablesThatSurvive {
		covered[tb] = why
	}

	rows, err := st.db.QueryContext(ctx, allTablesQuery(st.db.postgres()))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if covered[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d table(s) no account deletion would ever touch: %s\n"+
			"Give the table an org_id, or name it in teamKeyedTables, accountTables or "+
			"tablesThatSurvive with the reason.", len(missing), strings.Join(missing, ", "))
	}
}

func TestDeletingAnAccountLeavesNothingBehind(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	// Threads and turns are keyed by workspace rather than by organisation, which is the half of
	// the sweep an org_id predicate cannot reach.
	for _, team := range []string{"T_A", "T_B"} {
		if _, err := x.st.EnsureSession(ctx, team, "C1", "1.0", "thread", "m"); err != nil {
			t.Fatal(err)
		}
		if err := x.st.AddTurn(ctx, team, "C1", "1.0", "user", "U1", "what is our refund policy", "1.0", 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	// Somebody who is in both organisations, and so must survive the deletion of one.
	both, err := x.st.CreateUser(ctx, "both@example.com", "Both", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []int64{x.a, x.b} {
		if err := x.st.AddMembership(ctx, both.ID, org, RoleEditor, 0); err != nil {
			t.Fatal(err)
		}
	}

	erased, err := x.st.DeleteOrg(ctx, x.a)
	if err != nil {
		t.Fatal(err)
	}
	if erased.Rows == 0 {
		t.Fatal("the deletion reported no rows at all")
	}

	// Nothing of Acme's is left in any table that could hold it.
	scoped, err := x.st.orgScopedTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range scoped {
		var n int
		if err := x.st.db.QueryRowContext(ctx, fmt.Sprintf(`select count(*) from %q where org_id=?`, tb), x.a).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			t.Errorf("%s still holds %d row(s) of the deleted account", tb, n)
		}
		// ... and Beta still has everything it had. Every table seeded above holds at least one
		// of Beta's rows; the ones the fixture never touched hold none either way.
		if err := x.st.db.QueryRowContext(ctx, fmt.Sprintf(`select count(*) from %q where org_id=?`, tb), x.b).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	for _, tb := range teamKeyedTables {
		var n int
		if err := x.st.db.QueryRowContext(ctx, fmt.Sprintf(`select count(*) from %q where team_id=?`, tb), "T_A").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			t.Errorf("%s still holds %d row(s) from the deleted account's workspace", tb, n)
		}
	}
	if o, _ := x.st.Org(ctx, x.a); o != nil {
		t.Error("the organisation row survived its own deletion")
	}

	// Beta is untouched: its rows, its documents, its member.
	if o, _ := x.st.Org(ctx, x.b); o == nil {
		t.Fatal("deleting one account took the other with it")
	}
	if ds, _ := x.st.Documents(ctx, x.b); len(ds) != 1 {
		t.Errorf("Beta has %d documents left, want 1", len(ds))
	}
	if chunks, _ := x.st.AllChunks(ctx, x.b); len(chunks) != 1 {
		t.Errorf("Beta has %d retrieval chunks left, want 1", len(chunks))
	}
	var turns int
	x.st.db.QueryRowContext(ctx, `select count(*) from turns where team_id=?`, "T_B").Scan(&turns)
	if turns != 1 {
		t.Errorf("Beta has %d turns left, want 1", turns)
	}

	// The people: Acme's own founder had nowhere else to be and goes with it; the member of both
	// keeps their account and their other membership.
	if u, _ := x.st.UserByEmail(ctx, "a@example.com"); u != nil {
		t.Error("an account whose only organisation was deleted was left behind — it can sign in and belongs nowhere, and its address can never sign up again")
	}
	if erased.Accounts != 1 {
		t.Errorf("erased.Accounts = %d, want 1", erased.Accounts)
	}
	if u, _ := x.st.UserByEmail(ctx, "both@example.com"); u == nil {
		t.Fatal("deleting one organisation deleted somebody who belongs to another")
	}
	if ms, _ := x.st.MembershipsFor(ctx, both.ID); len(ms) != 1 || ms[0].OrgID != x.b {
		t.Errorf("the surviving member has memberships %+v, want Beta only", ms)
	}
	if u, _ := x.st.UserByEmail(ctx, "b@example.com"); u == nil {
		t.Error("the other organisation's founder was deleted")
	}
}

// ---- who may press it ----

// admin adds a second member to an organisation and hands back a session for them.
func member(t *testing.T, st *Store, orgID int64, email, role string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, email, email, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMembership(ctx, u.ID, orgID, role, 0); err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Email: u.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, tok
}

func TestOnlyTheOwnerDeletesTheAccount(t *testing.T) {
	b, mux, st := identityBot(t)
	_, orgID, owner := signedUp(t, b, mux, st, "founder@example.com")
	_, second := member(t, st, orgID, "admin2@example.com", RoleAdmin)
	_, editor := member(t, st, orgID, "editor@example.com", RoleEditor)

	// Another administrator is refused, and told whose it is.
	code, body := authReq(t, mux, "POST", "/api/org/delete", map[string]string{"confirm": "Acme Ltd"}, second)
	if code != 403 {
		t.Fatalf("a second admin deleted somebody else's account: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "founder@example.com") {
		t.Errorf("the refusal does not say who to ask: %q", msg)
	}
	// So is somebody who is not an administrator at all, and for the other reason.
	code, body = authReq(t, mux, "POST", "/api/org/delete", map[string]string{"confirm": "Acme Ltd"}, editor)
	if code != 403 {
		t.Fatalf("an editor deleted the account: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "administrator") {
		t.Errorf("an editor was refused for the wrong reason: %q", msg)
	}

	// The owner still has to name the account and prove who they are.
	if code, _ := authReq(t, mux, "POST", "/api/org/delete", map[string]string{
		"confirm": "Acme", "password": "correct horse battery"}, owner); code != 400 {
		t.Errorf("a half-typed name was accepted: %d", code)
	}
	if code, _ := authReq(t, mux, "POST", "/api/org/delete", map[string]string{
		"confirm": "Acme Ltd", "password": "hunter2"}, owner); code != 403 {
		t.Errorf("a wrong password deleted the account: %d", code)
	}
	if o, _ := st.Org(context.Background(), orgID); o == nil {
		t.Fatal("the account is gone after three refusals")
	}

	code, body = authReq(t, mux, "POST", "/api/org/delete", map[string]string{
		"confirm": " acme ltd ", "password": "correct horse battery"}, owner)
	if code != 200 {
		t.Fatalf("the owner was refused: %d %v", code, body)
	}
	if o, _ := st.Org(context.Background(), orgID); o != nil {
		t.Error("the account survived a successful delete")
	}
	// The session that did it is gone with everything else, so the next request is a sign-in.
	if code, _ := authReq(t, mux, "GET", "/api/org", nil, owner); code != 401 {
		t.Errorf("the deleting session still works: %d", code)
	}
}

func TestAnAdminInheritsTheAccountWhenTheOwnerHasGone(t *testing.T) {
	b, mux, st := identityBot(t)
	founder, orgID, _ := signedUp(t, b, mux, st, "founder@example.com")
	_, second := member(t, st, orgID, "admin2@example.com", RoleAdmin)
	ctx := context.Background()

	if code, _ := authReq(t, mux, "GET", "/api/org", nil, second); code != 200 {
		t.Fatal("a member cannot read the account")
	}
	// The founder leaves and is removed from Users. Without this rule the account would now be
	// one nobody could ever close.
	if err := st.RemoveMember(ctx, founder.ID, orgID); err != nil {
		t.Fatal(err)
	}
	_, body := authReq(t, mux, "GET", "/api/org", nil, second)
	if body["can_delete"] != true {
		t.Fatalf("the remaining admin was not offered the delete: %v", body)
	}
	// They have no password of their own; the session is minutes old, which is the proof
	// proveIdentity accepts from an account that has neither password nor second factor.
	if body["proof"] != "recent" {
		t.Errorf("proof = %v, want recent", body["proof"])
	}
	code, body := authReq(t, mux, "POST", "/api/org/delete", map[string]string{"confirm": "Acme Ltd"}, second)
	if code != 200 {
		t.Fatalf("the inheriting admin was refused: %d %v", code, body)
	}
	if o, _ := st.Org(ctx, orgID); o != nil {
		t.Error("the account survived")
	}
}

func TestTheAccountScreenNamesWhoMadeIt(t *testing.T) {
	b, mux, st := identityBot(t)
	_, orgID, owner := signedUp(t, b, mux, st, "founder@example.com")
	_, editor := member(t, st, orgID, "editor@example.com", RoleEditor)

	code, body := authReq(t, mux, "GET", "/api/org", nil, owner)
	if code != 200 {
		t.Fatalf("GET /api/org = %d: %v", code, body)
	}
	who, _ := body["owner"].(map[string]any)
	if who == nil || who["email"] != "founder@example.com" || who["is_you"] != true || who["is_member"] != true {
		t.Errorf("owner = %v", who)
	}
	if body["can_delete"] != true || body["members"].(float64) != 2 {
		t.Errorf("the owner's view of the account is wrong: %v", body)
	}
	if body["proof"] != "password" {
		t.Errorf("proof = %v, want password", body["proof"])
	}

	_, body = authReq(t, mux, "GET", "/api/org", nil, editor)
	if body["can_delete"] != false {
		t.Error("an editor was offered the delete")
	}
	if why, _ := body["why_not"].(string); !strings.Contains(why, "administrator") {
		t.Errorf("why_not = %q", why)
	}
}
