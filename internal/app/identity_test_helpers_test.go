package app

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

// seedOrg makes a real organisation with a real member and a real session, and hands back the
// session token. Every HTTP test authenticates this way now: there is no environment credential
// that outranks a membership, so a test that wants admin has to hold one — which is the point,
// because a test signed in through a bypass would exercise a path production does not have.
func seedOrg(t *testing.T, st *Store, role string) (orgID int64, userID int64, token string) {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "admin@example.com", "Test Admin", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	org, err := st.CreateOrg(ctx, "Test Org", u.ID)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// The founder is made admin by CreateOrg; a test wanting a lesser role changes it. It has
	// to be a role change: AddMembership no longer overwrites an existing membership's role,
	// which is the rule that stops a stale invitation from re-promoting somebody.
	if role != RoleAdmin {
		if err := st.SetMemberRole(ctx, u.ID, org.ID, role); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	tok, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Name: u.Name, Email: u.Email, OrgID: org.ID}, time.Hour)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return org.ID, u.ID, tok
}

// seedAdmin is the common case: an admin of a fresh organisation.
func seedAdmin(t *testing.T, st *Store) string {
	t.Helper()
	_, _, tok := seedOrg(t, st, RoleAdmin)
	return tok
}

// fixedMasterKey pins MASTER_KEY so NewSealer does not mint a throwaway and append it to a
// dotenv file in the working tree.
func fixedMasterKey(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
}

// Most tests exercise one organisation and do not care which, so they name this one. It is 1
// because that is the schema's default for org_id, which means a row written without naming an
// organisation lands here — and a test that accidentally relies on that default is therefore
// visible as a test that mentions orgID rather than one that silently passes.
//
// A test that cares about isolation shadows it with its own `orgID, _, _ := seedOrg(...)`.
var orgID int64 = 1
