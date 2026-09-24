package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// What a developer key may and may not do.
//
// The claim these tests hold up is a narrow one: a key is its owner, in one organisation, until
// it is revoked. Every test below is a way somebody might try to make it more than that — use it
// as a console session, read a second organisation with it, keep using it after the person it
// belongs to is gone, or write past the permissions their role holds.

// mintKey creates a key through the console, exactly as the page does, and returns the raw key.
func mintKey(t *testing.T, mux *http.ServeMux, session, name string) (string, int64) {
	t.Helper()
	code, body := authReq(t, mux, "POST", "/api/api-keys", map[string]string{"name": name}, session)
	if code != 201 {
		t.Fatalf("create key = %d: %v", code, body)
	}
	raw, _ := body["raw"].(string)
	if raw == "" {
		t.Fatalf("no raw key in %v", body)
	}
	key, _ := body["key"].(map[string]any)
	id := int64(key["id"].(float64))
	return raw, id
}

func TestAKeyIsShownOnceAndAfterwardsOnlyItsPrefix(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, session := signedUp(t, b, mux, st, "founder@example.com")

	raw, _ := mintKey(t, mux, session, "nightly export")
	if !strings.HasPrefix(raw, apiKeyPrefix) {
		t.Fatalf("key %q does not carry the %q marker", raw, apiKeyPrefix)
	}

	code, body := authReq(t, mux, "GET", "/api/api-keys", nil, session)
	if code != 200 {
		t.Fatalf("list = %d: %v", code, body)
	}
	// The whole listing, as text: the secret must not be anywhere in it, under any field name.
	blob, _ := json.Marshal(body)
	if strings.Contains(string(blob), raw) {
		t.Fatalf("the raw key came back from the list: %s", blob)
	}
	keys := body["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("expected one key, got %d", len(keys))
	}
	row := keys[0].(map[string]any)
	prefix := row["prefix"].(string)
	if !strings.HasPrefix(raw, prefix) || len(prefix) >= len(raw) {
		t.Fatalf("prefix %q is not a proper prefix of the key", prefix)
	}
	// And what the console does show is not a credential.
	if code, _ := authReq(t, mux, "GET", "/v1/whoami", nil, prefix); code != 401 {
		t.Errorf("the displayed prefix authenticated: %d", code)
	}
}

func TestAKeyOpensTheAPIAndASessionDoesNot(t *testing.T) {
	b, mux, st := identityBot(t)
	_, orgID, session := signedUp(t, b, mux, st, "founder@example.com")
	raw, _ := mintKey(t, mux, session, "ci")

	code, body := authReq(t, mux, "GET", "/v1/whoami", nil, raw)
	if code != 200 {
		t.Fatalf("whoami with a key = %d: %v", code, body)
	}
	// The organisation is named by its public id, not its row number: /v1 is the surface a
	// customer's own scripts read, and a serial there would count the deployment's accounts.
	org := body["organisation"].(map[string]any)
	if want := orgPublic(t, st, orgID); org["id"] != want {
		t.Errorf("key acted in org %v, want %s", org["id"], want)
	}
	if body["role"] != RoleAdmin {
		t.Errorf("role = %v, want admin", body["role"])
	}

	// The two credentials are not interchangeable in either direction.
	if code, _ := authReq(t, mux, "GET", "/v1/whoami", nil, session); code != 401 {
		t.Errorf("a console session authenticated the developer API: %d", code)
	}
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, raw); code != 401 {
		t.Errorf("an API key opened the console: %d", code)
	}
}

func TestAKeyStopsAtItsOwnOrganisation(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgA, sessionA := signedUp(t, b, mux, st, "a@example.com")
	_, orgB, sessionB := signedUp(t, b, mux, st, "b@example.com")

	// One artifact in each organisation, and a workspace belonging to B.
	if err := st.SaveTeam(ctx, &Team{TeamID: "TB", OrgID: orgB, Name: "B workspace"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	artA := &Artifact{Title: "A's report", Kind: "md", Content: "a"}
	artB := &Artifact{Title: "B's report", Kind: "md", Content: "b"}
	if err := st.AddArtifact(ctx, orgA, artA); err != nil {
		t.Fatal(err)
	}
	if err := st.AddArtifact(ctx, orgB, artB); err != nil {
		t.Fatal(err)
	}

	keyA, _ := mintKey(t, mux, sessionA, "a's key")
	keyB, _ := mintKey(t, mux, sessionB, "b's key")

	code, body := authReq(t, mux, "GET", "/v1/artifacts", nil, keyA)
	if code != 200 {
		t.Fatalf("artifacts = %d: %v", code, body)
	}
	arts := body["artifacts"].([]any)
	if len(arts) != 1 || arts[0].(map[string]any)["title"] != "A's report" {
		t.Fatalf("A's key saw %v", arts)
	}

	// B's artifact by id is not a 403 but a 404: a key must not be able to tell a row it may not
	// have from one that does not exist.
	if code, _ := authReq(t, mux, "GET", "/v1/artifacts/"+itoa(artB.ID), nil, keyA); code != 404 {
		t.Errorf("A's key reading B's artifact = %d, want 404", code)
	}
	if code, _ := authReq(t, mux, "GET", "/v1/artifacts/"+itoa(artA.ID), nil, keyB); code != 404 {
		t.Errorf("B's key reading A's artifact = %d, want 404", code)
	}

	// Nor may it write into B's workspace by naming it.
	code, body = authReq(t, mux, "POST", "/v1/memories",
		map[string]string{"scope": "team:TB", "text": "planted"}, keyA)
	if code != 404 {
		t.Fatalf("A's key wrote a memory into B's workspace: %d %v", code, body)
	}
	ms, _ := st.AllMemories(ctx, orgB)
	if len(ms) != 0 {
		t.Fatalf("B's organisation gained a memory: %v", ms)
	}
}

func TestRevokedAndExpiredKeysAreRefused(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, session := signedUp(t, b, mux, st, "founder@example.com")

	revoked, id := mintKey(t, mux, session, "to be revoked")
	if code, body := authReq(t, mux, "DELETE", "/api/api-keys/"+itoa(id), nil, session); code != 200 {
		t.Fatalf("revoke = %d: %v", code, body)
	}
	if code, body := authReq(t, mux, "GET", "/v1/whoami", nil, revoked); code != 401 {
		t.Errorf("a revoked key still worked: %d %v", code, body)
	}
	// Revoking twice is not a second act, and says so.
	if code, _ := authReq(t, mux, "DELETE", "/api/api-keys/"+itoa(id), nil, session); code != 404 {
		t.Errorf("re-revoking = %d, want 404", code)
	}

	// An expiry is checked at the request, not swept in the background, so a key that lapsed a
	// second ago is already refused.
	lapsed := mintKey1(t, ctx, st, orgID, session, b, mux, time.Now().UTC().Add(-time.Second))
	if code, body := authReq(t, mux, "GET", "/v1/whoami", nil, lapsed); code != 401 {
		t.Errorf("an expired key still worked: %d %v", code, body)
	}
}

// mintKey1 makes a key and back-dates its expiry, which the console will not do — the API refuses
// an expiry in the past, and rightly.
func mintKey1(t *testing.T, ctx context.Context, st *Store, orgID int64, session string, b *Bot, mux *http.ServeMux, at time.Time) string {
	t.Helper()
	raw, id := mintKey(t, mux, session, "lapsed")
	if _, err := st.db.ExecContext(ctx, `update api_keys set expires_at=? where org_id=? and id=?`,
		at.Format(time.DateTime), orgID, id); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAKeyCannotOutliveItsOwnersMembership(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	u, orgID, session := signedUp(t, b, mux, st, "leaver@example.com")
	raw, _ := mintKey(t, mux, session, "their integration")

	if code, _ := authReq(t, mux, "GET", "/v1/whoami", nil, raw); code != 200 {
		t.Fatalf("the key did not work to begin with: %d", code)
	}
	if err := st.RemoveMember(ctx, u.ID, orgID); err != nil {
		t.Fatal(err)
	}
	if code, body := authReq(t, mux, "GET", "/v1/whoami", nil, raw); code != 401 {
		t.Errorf("the key outlived the membership: %d %v", code, body)
	}
}

func TestAKeyHoldsNoMoreThanItsOwnersRole(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, adminSession := signedUp(t, b, mux, st, "founder@example.com")

	// A role that may mint keys and nothing else — the narrowest thing that can still create one.
	if err := st.UpsertConsoleRole(ctx, orgID, &ConsoleRole{Key: "integrator", Label: "Integrator",
		Permissions: []string{PermAPIKeysManage}}); err != nil {
		t.Fatal(err)
	}
	mate, err := st.CreateUser(ctx, "mate@example.com", "Mate", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMembership(ctx, mate.ID, orgID, "integrator", 0); err != nil {
		t.Fatal(err)
	}
	theirs, err := st.CreateAdminSession(ctx, AdminUser{ID: mate.ID, Email: mate.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	narrow, _ := mintKey(t, mux, theirs, "narrow")

	// It authenticates, and reports exactly what it holds.
	code, body := authReq(t, mux, "GET", "/v1/whoami", nil, narrow)
	if code != 200 {
		t.Fatalf("whoami = %d: %v", code, body)
	}
	perms := body["permissions"].([]any)
	if len(perms) != 1 || perms[0] != PermAPIKeysManage {
		t.Fatalf("permissions = %v", perms)
	}

	// And it is refused where the role is.
	if code, _ := authReq(t, mux, "POST", "/v1/memories",
		map[string]string{"scope": "team:T1", "text": "x"}, narrow); code != 403 {
		t.Errorf("a key wrote past its owner's role: %d", code)
	}
	if code, _ := authReq(t, mux, "GET", "/v1/activity", nil, narrow); code != 403 {
		t.Errorf("a key read activity its owner cannot: %d", code)
	}
	// The admin's own key still may, so the refusal above is the role and not the door.
	adminKey, _ := mintKey(t, mux, adminSession, "admin's key")
	if code, _ := authReq(t, mux, "GET", "/v1/activity", nil, adminKey); code != 200 {
		t.Errorf("the admin's key was refused activity: %d", code)
	}
}

func TestOnlySomebodyWhoMayManageKeysCanMintThem(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, _ := signedUp(t, b, mux, st, "founder@example.com")

	watcher, err := st.CreateUser(ctx, "watcher@example.com", "Watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMembership(ctx, watcher.ID, orgID, RoleViewer, 0); err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateAdminSession(ctx, AdminUser{ID: watcher.ID, Email: watcher.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := authReq(t, mux, "POST", "/api/api-keys", map[string]string{"name": "sneaky"}, session); code != 403 {
		t.Errorf("a viewer minted a key: %d %v", code, body)
	}
}

func TestAKeyIsRefusedWithoutAName(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, session := signedUp(t, b, mux, st, "founder@example.com")
	if code, _ := authReq(t, mux, "POST", "/api/api-keys", map[string]string{"name": "  "}, session); code != 400 {
		t.Errorf("an unnamed key was created: %d", code)
	}
	if code, _ := authReq(t, mux, "POST", "/api/api-keys",
		map[string]string{"name": "yesterday", "expires_on": "2020-01-01"}, session); code != 400 {
		t.Errorf("a key expiring in the past was created: %d", code)
	}
}

func TestKeyRateLimitCountsPerKeyPerMinute(t *testing.T) {
	reset := func() {
		apiRates.Lock()
		apiRates.m = map[int64]*rateWindow{}
		apiRates.Unlock()
	}
	reset()
	// The window is process-wide and keyed by row id, and every test database starts its ids
	// at 1 — so the 240 hits spent here would otherwise land on the next test's first key.
	t.Cleanup(reset)

	for i := 0; i < apiKeyRateLimit; i++ {
		if ok, _ := allowKeyRequest(1); !ok {
			t.Fatalf("request %d of %d was refused", i+1, apiKeyRateLimit)
		}
	}
	ok, retry := allowKeyRequest(1)
	if ok {
		t.Fatalf("request %d was allowed past the limit", apiKeyRateLimit+1)
	}
	if retry <= 0 || retry > 61 {
		t.Errorf("retry-after = %ds, want something inside the minute", retry)
	}
	// A second key has its own budget: one runaway script does not stop everybody else's.
	if ok, _ := allowKeyRequest(2); !ok {
		t.Errorf("another key was caught by the first one's limit")
	}
}
