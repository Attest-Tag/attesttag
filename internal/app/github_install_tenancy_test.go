package app

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A GitHub App installation id is not a secret and was never meant to be one: GitHub puts it in
// the settings URL of the account it belongs to, and this console puts it in the query string it
// redirects to when an install finishes. What makes it safe to accept is the row the install flow
// wrote saying whose it is — so every path that turns an id into a token has to read that row.
//
// Without it, naming somebody else's id is enough: the token is minted from this deployment's own
// app key, so the attacker needs no credential of the victim's at all. These tests each pin one
// door shut.

// seedInstall gives org a bound installation and returns its id.
func seedInstall(t *testing.T, st *Store, orgID, id int64, login string) int64 {
	t.Helper()
	if err := st.SaveGitHubInstall(context.Background(), &GitHubInstall{
		ID: id, OrgID: orgID, AccountLogin: login, AccountID: id, AccountType: "Organization",
		RepoSelection: "selected", AppSlug: "attesttag", InstalledBy: "admin@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// secondOrg makes an organisation that is not organisation 1, with an admin session for it.
func secondOrg(t *testing.T, st *Store) (orgID int64, token string) {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "other@example.com", "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(ctx, "Other Ltd", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, OrgID: org.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return org.ID, tok
}

// repoAuthFor takes the installation id straight out of a request body — /api/github/repos and
// /api/repos both hand it over — so it is the first place the claim has to be checked. Refusing
// here is what stops the listing endpoint being used to read another tenant's repository names
// back before anything is even stored.
func TestRepoAuthRefusesAnInstallationThisOrgDoesNotHold(t *testing.T) {
	b, _, st := installTestBot(t)
	ctx := context.Background()
	seedAdmin(t, st) // organisation 1
	otherOrg, _ := secondOrg(t, st)

	id := seedInstall(t, st, otherOrg, 1234567, "victim-co")

	if _, err := b.repoAuthFor(ctx, 1, "", 0, id); err == nil {
		t.Fatal("organisation 1 was handed an authorisation for another organisation's installation")
	} else if strings.Contains(err.Error(), "victim-co") {
		t.Errorf("the refusal names the account behind the id: %v", err)
	}

	// An id nobody has installed answers exactly the same, so the endpoint cannot be used to
	// learn which installations exist on this deployment.
	missing, err := b.repoAuthFor(ctx, 1, "", 0, 999999)
	unknownErr := err
	if err == nil {
		t.Fatalf("an installation id nobody holds was accepted: %+v", missing)
	}
	if _, err := b.repoAuthFor(ctx, 1, "", 0, id); err.Error() != unknownErr.Error() {
		t.Errorf("another org's installation and an unknown one answer differently:\n %q\n %q", err, unknownErr)
	}

	// The organisation that actually installed it is unaffected.
	auth, err := b.repoAuthFor(ctx, otherOrg, "", 0, id)
	if err != nil {
		t.Fatalf("the installing organisation was refused its own installation: %v", err)
	}
	if auth.installationID != id {
		t.Errorf("installationID = %d, want %d", auth.installationID, id)
	}

	// Disconnecting it here takes it out of use even though the row survives.
	if err := st.RevokeGitHubInstall(ctx, otherOrg, id, "disconnected"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.repoAuthFor(ctx, otherOrg, "", 0, id); err == nil {
		t.Error("a revoked installation still authorised a repository")
	}
}

// The connection endpoints take a free-form credential, and a github_app credential is nothing but
// an installation id. An app-backed connection built that way would also carry no repository, and
// a token minted with no repository scope reaches the whole installation — so these endpoints do
// not offer the credential type at all; connectRepo makes them, scoped, from an install this
// organisation holds.
func TestConnectionEndpointsRefuseAnAppInstallationFromTheBody(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	admin := seedAdmin(t, st)
	otherOrg, _ := secondOrg(t, st)
	id := seedInstall(t, st, otherOrg, 1234567, "victim-co")

	bundle, err := b.store.CreateBundle(ctx, 1, "Repositories", "")
	if err != nil {
		t.Fatal(err)
	}

	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	body := `{"name":"gh","preset":"github","cred_type":"github_app","secret":{"installation_id":1234567}}`
	for _, path := range []string{
		"/api/bundles/" + strconv.FormatInt(bundle.ID, 10) + "/connections",
		"/api/connections/test",
	} {
		if w := post(path, body); w.Code != 400 {
			t.Errorf("%s accepted a github_app credential from the body: %d %s", path, w.Code, w.Body.String())
		}
	}

	// Nothing was stored, so the victim's installation is not reachable from this organisation.
	conns, err := b.store.ConnectionsForBundle(ctx, 1, bundle.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		if c.CredType == "github_app" || c.GitHubInstallationID == id {
			t.Fatalf("an app-backed connection was created anyway: %+v", c)
		}
	}
}

// The proxy is the one place every path passes through — a stored connection, an unsaved one built
// while the console is still asking questions, a tool call — so the check is repeated here. And it
// comes before the token cache, which is keyed on the installation rather than on the connection:
// a token the rightful tenant minted a moment ago is sitting under exactly the key an impostor
// asks for.
func TestInstallationTokenRefusesAnotherOrgsInstallationBeforeTheCache(t *testing.T) {
	// Named rather than generated: NewSealer writes a fresh key into a dotenv file when the
	// environment has none, and a test has no business leaving one behind.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))

	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(sealer, st)
	_, b64, _ := testAppKey(t)
	app := &githubApp{id: "1234567", slug: "attesttag", clientID: "cid", clientSecret: "csecret"}
	if app.key, err = parseGitHubAppKey(b64, ""); err != nil {
		t.Fatal(err)
	}
	p.ghApp = app

	ctx := context.Background()
	u, err := st.CreateUser(ctx, "owner@example.com", "Owner", "")
	if err != nil {
		t.Fatal(err)
	}
	victim, err := st.CreateOrg(ctx, "Victim Ltd", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := seedInstall(t, st, victim.ID, 1234567, "victim-co")

	conn := &Connection{ID: 7, Name: "api", Preset: "github", CredType: "github_app",
		Repo: "victim-co/api", GitHubInstallationID: id}
	sec := &Secret{InstallationID: id}

	// The rightful tenant's token, already minted and cached.
	p.remember(installTokenKey(id, conn.Repo), "ghs_victimtoken", time.Hour)
	if tok, err := p.installationToken(ctx, victim.ID, conn, sec); err != nil || tok != "ghs_victimtoken" {
		t.Fatalf("the installing organisation could not spend its own installation: %q %v", tok, err)
	}

	// Another organisation naming the same id gets nothing — not the cached token, and no mint.
	tok, err := p.installationToken(ctx, victim.ID+1, conn, sec)
	if err == nil {
		t.Fatal("another organisation minted a token against an installation it does not hold")
	}
	if tok != "" {
		t.Fatalf("a token came back with the refusal: %q", tok)
	}
	if strings.Contains(err.Error(), "ghs_") {
		t.Errorf("the refusal carries the cached token: %v", err)
	}
}
