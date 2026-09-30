package app

import (
	"context"
	"testing"
	"time"
)

// A password reset or a removal evicts the person's sessions, keys and MCP grants. An MCP
// authorization code approved just before, but not yet exchanged, must go too: it lives ten
// minutes and would otherwise still trade for a fresh 30-day grant after the reset.
func TestRevokingKeysAlsoSpendsUnusedMCPCodes(t *testing.T) {
	t.Setenv("MASTER_KEY", "")
	fixedMasterKey(t)
	st := testStore(t)
	ctx := context.Background()

	mk := func(hash string, org, user int64) {
		if err := st.CreateMCPCode(ctx, mcpCode{Hash: hash, ClientID: "c", OrgID: org, UserID: user,
			RedirectURI: "https://x/cb", Challenge: "ch", ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.DateTime)}); err != nil {
			t.Fatal(err)
		}
	}
	spent := func(hash string) bool {
		_, first, err := st.UseMCPCode(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		return !first // first==false means it was already used (revoked)
	}

	// Account-wide revoke (password reset/change) voids the held code.
	mk("h-reset", 1, 7)
	if err := st.RevokeAPIKeysForUser(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if !spent("h-reset") {
		t.Error("a held code survived an account-wide key revocation")
	}

	// Org-scoped revoke (member removed) voids the held code in that org.
	mk("h-remove", 2, 8)
	if err := st.RevokeAPIKeysOf(ctx, 2, 8); err != nil {
		t.Fatal(err)
	}
	if !spent("h-remove") {
		t.Error("a held code survived the member's removal")
	}

	// A code for a different, untouched user is left alone.
	mk("h-other", 2, 9)
	if err := st.RevokeAPIKeysOf(ctx, 2, 8); err != nil {
		t.Fatal(err)
	}
	if spent("h-other") {
		t.Error("revoking one member's access voided another member's code")
	}
}
