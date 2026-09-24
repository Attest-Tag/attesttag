package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func permSet(ps ...Permission) map[Permission]bool {
	m := map[Permission]bool{}
	for _, p := range ps {
		m[p] = true
	}
	return m
}

// A role key this build has never heard of must grant nothing, not fall through to a real tier.
// Rows outlive releases: an old custom role, a hand-edited database, a rollback.
func TestPermissionsFailClosed(t *testing.T) {
	if got := permissionsForRole("does-not-exist", nil); len(got) != 0 {
		t.Fatalf("unknown role granted %d permissions", len(got))
	}
	if got := permissionsForRole("", nil); len(got) != 0 {
		t.Fatalf("empty role granted %d permissions", len(got))
	}
	// Built-ins beat customs, so an install cannot redefine "admin" to mean less.
	custom := map[string][]Permission{"admin": {PermActivityView}}
	if got := permissionsForRole("admin", custom); len(got) != len(allPermissions) {
		t.Fatalf("a custom role shadowed the built-in admin: %d permissions", len(got))
	}
}

// The subset rule: you may only hand out access you hold. Stated this way it also covers roles
// nobody has seen before, which "only an admin may grant admin" never could.
func TestCanAssignRoleSubsetRule(t *testing.T) {
	custom := map[string][]Permission{
		"ops":  {PermUsersManage, PermConnManage},
		"docs": {PermDocsManage},
	}
	admin := permissionsForRole("admin", custom)
	editor := permissionsForRole("editor", custom)

	if !canAssignRole(admin, "editor", custom) {
		t.Error("an admin should be able to grant editor")
	}
	if canAssignRole(editor, "admin", custom) {
		t.Error("an editor granted admin")
	}
	if canAssignRole(editor, "ops", custom) {
		t.Error("an editor granted a custom role holding connections.manage they do not have")
	}
	if !canAssignRole(editor, "docs", custom) {
		t.Error("an editor holds documents.manage and should be able to grant a role that is only that")
	}
	// An unknown target is refused rather than granted as an empty role.
	if canAssignRole(admin, "ghost", custom) {
		t.Error("an unknown role was assignable")
	}
}

// A custom role written against a newer build must not smuggle in a permission as a bare string.
func TestSanitizePermissions(t *testing.T) {
	got := sanitizePermissions([]string{PermDocsManage, "secrets.exfiltrate", PermDocsManage, "  " + PermActivityView + "  "})
	if len(got) != 2 || got[0] != PermDocsManage || got[1] != PermActivityView {
		t.Fatalf("got %v, want the two known names once each", got)
	}
}

// Nobody may leave the console with no one able to manage users or credentials. The check is on
// the capability, not on the literal role "admin" — an "Ops" role holding users.manage is a
// perfectly good administrator, and an install of those has no admin to count.
// The invariant is on the capability, not the role name: a custom role holding users.manage is
// a perfectly good administrator, so an organisation of those must not be left without one.
func TestLastHolderGuard(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	b := &Bot{store: st, settings: newSettingsCache(st, Config{})}
	orgID, opsID, _ := seedOrg(t, st, RoleAdmin)
	st.UpsertConsoleRole(ctx, orgID, &ConsoleRole{Key: "ops", Label: "Ops",
		Permissions: []string{PermUsersManage, PermConnManage}})
	st.SetMemberRole(ctx, opsID, orgID, "ops")

	ed, _ := st.CreateUser(ctx, "editor@example.com", "Ed", "")
	st.AddMembership(ctx, ed.ID, orgID, RoleEditor, 0)

	if err := b.lastHolderGuard(ctx, orgID, opsID, "ops", RoleEditor); err == nil {
		t.Fatal("demoting the only holder of users.manage should have been refused")
	}
	if err := b.lastHolderGuard(ctx, orgID, opsID, "ops", ""); err == nil {
		t.Fatal("removing the only holder of users.manage should have been refused")
	}
	// A second holder makes it fine — and neither of them holds the built-in "admin" role.
	second, _ := st.CreateUser(ctx, "ops2@example.com", "Ops Two", "")
	st.AddMembership(ctx, second.ID, orgID, "ops", 0)
	if err := b.lastHolderGuard(ctx, orgID, opsID, "ops", RoleEditor); err != nil {
		t.Fatalf("with a second holder this should be allowed: %v", err)
	}
	// Demoting somebody who never held it is never blocked.
	if err := b.lastHolderGuard(ctx, orgID, ed.ID, RoleEditor, RoleViewer); err != nil {
		t.Fatalf("demoting a non-holder was blocked: %v", err)
	}
	// And the guard is per organisation: another org's admins are no help here.
	other, _ := st.CreateUser(ctx, "elsewhere@example.com", "Elsewhere", "")
	otherOrg, _ := st.CreateOrg(ctx, "Other", other.ID)
	if err := b.lastHolderGuard(ctx, otherOrg.ID, other.ID, RoleAdmin, ""); err == nil {
		t.Fatal("removing the only admin of another organisation should still be refused")
	}
}

// An invitation is bearer authority, so it has to be single-use and dead once revoked or expired.
// Two people opening the same link must not produce two memberships.
func TestInviteIsSingleUse(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	orgID, byID, _ := seedOrg(t, st, RoleAdmin)

	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "new@example.com", Role: RoleEditor, CreatedBy: byID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Peeking does not consume it: the sign-up page has to be able to say who it is for.
	if got, err := st.PeekEmailToken(ctx, TokenInvite, raw); err != nil || got.Email != "new@example.com" {
		t.Fatalf("peek = %+v, %v", got, err)
	}
	if got, err := st.TakeEmailToken(ctx, TokenInvite, raw); err != nil || got.Role != RoleEditor {
		t.Fatalf("first use = %+v, %v", got, err)
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err == nil {
		t.Error("an invitation must not be usable twice")
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, "never-issued"); err == nil {
		t.Error("an unknown token must be refused")
	}
	// The kind is part of the key: a reset link must not work as an invitation.
	reset, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenReset, UserID: byID}, time.Hour)
	if _, err := st.TakeEmailToken(ctx, TokenInvite, reset); err == nil {
		t.Error("a reset token must not be redeemable as an invitation")
	}
}

func TestRevokedAndExpiredInvites(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	orgID, byID, _ := seedOrg(t, st, RoleAdmin)

	// Expired.
	old, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "late@example.com", Role: RoleViewer, CreatedBy: byID}, -time.Minute)
	if _, err := st.TakeEmailToken(ctx, TokenInvite, old); err == nil {
		t.Error("an expired invitation must be refused")
	}

	// Revoked.
	live, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "gone@example.com", Role: RoleViewer, CreatedBy: byID}, time.Hour)
	if err := st.RevokeInviteByID(ctx, orgID, idOfInvite(t, st, orgID, "gone@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, live); err == nil {
		t.Error("a revoked invitation must be refused")
	}

	// One organisation cannot revoke another's.
	otherUser, _ := st.CreateUser(ctx, "other@example.com", "Other", "")
	otherOrg, _ := st.CreateOrg(ctx, "Other Org", otherUser.ID)
	mine, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "mine@example.com", Role: RoleViewer, CreatedBy: byID}, time.Hour)
	if err := st.RevokeInviteByID(ctx, otherOrg.ID, idOfInvite(t, st, orgID, "mine@example.com")); err == nil {
		t.Error("another organisation revoked our invitation")
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, mine); err != nil {
		t.Errorf("another organisation revoked our invitation: %v", err)
	}

	// Only a live, unaccepted invitation is listed as pending: the expired one, the revoked one
	// and the one just redeemed above are all gone from the Users page.
	if _, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "waiting@example.com", Role: RoleViewer, CreatedBy: byID}, time.Hour); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingInvitesFor(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Email != "waiting@example.com" {
		t.Errorf("pending = %+v, want only waiting@example.com", pending)
	}
}

func TestCSRF(t *testing.T) {
	withCookie := func(method, header string) *http.Request {
		r := httptest.NewRequest(method, "/api/settings", nil)
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok-abc"})
		if header != "" {
			r.Header.Set(csrfHeader, header)
		}
		return r
	}
	if !checkCSRF(withCookie("GET", "")) {
		t.Error("reads must not need a token")
	}
	if checkCSRF(withCookie("PUT", "")) {
		t.Error("a write with no header was allowed")
	}
	if checkCSRF(withCookie("POST", "wrong")) {
		t.Error("a write with a mismatched header was allowed")
	}
	if !checkCSRF(withCookie("POST", "tok-abc")) {
		t.Error("a matching header should be allowed")
	}
	// No cookie at all is a refusal, not a pass.
	if checkCSRF(httptest.NewRequest("POST", "/api/settings", nil)) {
		t.Error("a write with no CSRF cookie was allowed")
	}
	// Bearer auth carries no cookie, so there is nothing to ride.
	bearer := httptest.NewRequest("POST", "/api/settings", nil)
	bearer.Header.Set("Authorization", "Bearer abc")
	if !checkCSRF(bearer) {
		t.Error("bearer-authenticated calls should be exempt")
	}
}

// A session that predates the CSRF cookie gets one on its next /api/me, and a write that arrives
// without one is refused but leaves the cookie behind, so the retry succeeds without a new login.
func TestCSRFCookieIssuedForExistingSession(t *testing.T) {
	t.Setenv("ADMIN_BASE_URL", "")
	st := testStore(t)
	b := &Bot{store: st, settings: newSettingsCache(st, Config{})}
	sess := seedAdmin(t, st)
	csrfOf := func(rec *httptest.ResponseRecorder) string {
		for _, c := range rec.Result().Cookies() {
			if c.Name == csrfCookie {
				return c.Value
			}
		}
		return ""
	}
	withSession := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		return r
	}

	// /api/me with only the session cookie: a CSRF cookie comes back.
	rec := httptest.NewRecorder()
	b.handleMe(rec, withSession("GET", "/api/me"))
	tok := csrfOf(rec)
	if tok == "" {
		t.Fatal("/api/me did not issue a CSRF cookie to a session without one")
	}
	// Already holding one: nothing new is issued.
	r := withSession("GET", "/api/me")
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: tok})
	rec = httptest.NewRecorder()
	b.handleMe(rec, r)
	if csrfOf(rec) != "" {
		t.Error("a session that has a CSRF cookie was given another")
	}
	// Signed out: nothing is issued.
	rec = httptest.NewRecorder()
	b.handleMe(rec, httptest.NewRequest("GET", "/api/me", nil))
	if csrfOf(rec) != "" {
		t.Error("an anonymous request was given a CSRF cookie")
	}

	// A write without the cookie is refused, but the cookie is set so the retry works.
	h := b.requireAdmin(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	rec = httptest.NewRecorder()
	h(rec, withSession("POST", "/api/console/invites"))
	if rec.Code != http.StatusForbidden || csrfOf(rec) == "" {
		t.Fatalf("write without a token: %d, cookie %q", rec.Code, csrfOf(rec))
	}
	tok = csrfOf(rec)
	r = withSession("POST", "/api/console/invites")
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: tok})
	r.Header.Set(csrfHeader, tok)
	rec = httptest.NewRecorder()
	h(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("retry with the token: %d", rec.Code)
	}
}

// idOfInvite finds the row a pending invitation was written to, which is how the console
// addresses one for revoking.
func idOfInvite(t *testing.T, st *Store, orgID int64, email string) int64 {
	t.Helper()
	pending, err := st.PendingInvitesFor(context.Background(), orgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.Email == email {
			return p.ID
		}
	}
	t.Fatalf("no pending invitation for %s in %+v", email, pending)
	return 0
}
