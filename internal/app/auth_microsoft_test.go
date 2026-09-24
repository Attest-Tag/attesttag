package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	contosoTenant  = "7a1b2c3d-0000-4000-8000-00000000c0de"
	fabrikamTenant = "7a1b2c3d-0000-4000-8000-0000000fab00"
	anaOID         = "8f3b1c2d-0000-4000-8000-00000000000a"
)

// microsoftBot is a bot whose deployment offers Sign in with Microsoft, against the fake.
func microsoftBot(t *testing.T, audience string) (*Bot, *http.ServeMux, *fakeMicrosoft, *Store) {
	t.Helper()
	b, mux, f, st := teamsTestBot(t, "unused")
	b.cfg.MSTeamsAppID, b.cfg.MSTeamsAppPassword, b.cfg.MSTeamsSignIn = fakeAppID, "secret", audience
	return b, mux, f, st
}

// microsoftSignIn walks Sign in with Microsoft through its real routes. The fake issues an id_token
// with the claims a real one would carry — this deployment as the audience, the nonce from the
// authorize URL, the tenant's own issuer — with over applied, and a nil in over removing a claim.
func microsoftSignIn(t *testing.T, mux *http.ServeMux, f *fakeMicrosoft, over map[string]any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, asConsole(httptest.NewRequest("GET", "/api/auth/microsoft/login", nil)))
	if w.Code != http.StatusFound {
		t.Fatalf("microsoft login = %d: %s", w.Code, w.Body.String())
	}
	authz, err := url.Parse(w.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(authz.String(), f.srv.URL+"/") || authz.Query().Get("code_challenge") == "" {
		t.Fatalf("the sign-in went to %q", w.Header().Get("Location"))
	}
	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == msLoginStateCookie {
			state = c
		}
	}
	if state == nil {
		t.Fatal("no state cookie was set")
	}
	tid := contosoTenant
	if v, ok := over["tid"].(string); ok {
		tid = v
	}
	claims := map[string]any{
		"iss": f.srv.URL + "/" + tid + "/v2.0", "aud": fakeAppID, "nonce": authz.Query().Get("nonce"),
		"exp": time.Now().Add(time.Hour).Unix(), "tid": tid, "oid": anaOID, "name": "Ana", "email": "ana@contoso.com",
	}
	for k, v := range over {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	f.mu.Lock()
	f.codes["the-code"] = claims
	f.mu.Unlock()
	cb := httptest.NewRequest("GET", "/api/auth/microsoft/callback?code=the-code&state="+url.QueryEscape(authz.Query().Get("state")), nil)
	cb.AddCookie(state)
	for _, c := range cookies {
		cb.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, cb)
	return rec
}

// A Microsoft sign-in is somebody in a tenant, and the account it makes is keyed by exactly that —
// the pair the bot knows them by in Teams — while the address Microsoft vouched for is left
// unconfirmed, because a tenant's own admin can set it to anything.
func TestMicrosoftSignInIsKeyedByTheTenantAndTheObjectID(t *testing.T) {
	_, mux, f, st := microsoftBot(t, msSignInAnyOrganisation)
	ctx := context.Background()
	rec := microsoftSignIn(t, mux, f, nil)
	me := signedInAs(t, st, rec)
	if me == nil {
		t.Fatalf("no session: %s", authError(rec))
	}
	if rec.Header().Get("Location") != "/admin/" {
		t.Errorf("signed in to %q", rec.Header().Get("Location"))
	}
	if u, _ := st.UserByIdentity(ctx, ProviderMicrosoft, contosoTenant+":"+anaOID); u == nil || u.ID != me.ID {
		t.Fatalf("the account is not keyed by its tenant and object id: %+v", u)
	}
	if u, _ := st.User(ctx, me.ID); u == nil || u.EmailVerified {
		t.Errorf("an address Microsoft vouched for was taken as confirmed: %+v", u)
	}
	// The same person again is the same account, whatever their address says now.
	again := microsoftSignIn(t, mux, f, map[string]any{"email": "ana.renamed@contoso.com"})
	if u := signedInAs(t, st, again); u == nil || u.ID != me.ID {
		t.Errorf("the second sign-in was %+v, want account %d", u, me.ID)
	}
}

// nOAuth: somebody who controls a tenant can put anybody's address in their own token. That must
// never reach an account held by that address — not by signing in, and not by founding a new one.
func TestAMicrosoftSignInNeverTakesAnAccountByItsEmail(t *testing.T) {
	_, mux, f, st := microsoftBot(t, msSignInAnyOrganisation)
	ctx := context.Background()
	victim, err := st.CreateUser(ctx, "boss@contoso.com", "Boss", "not-a-real-hash")
	if err != nil {
		t.Fatal(err)
	}
	rec := microsoftSignIn(t, mux, f, map[string]any{"tid": fabrikamTenant, "email": "boss@contoso.com"})
	if u := signedInAs(t, st, rec); u != nil {
		t.Fatalf("a token claiming somebody's address signed in as %+v", u)
	}
	if !strings.Contains(authError(rec), "already uses that email") {
		t.Errorf("refused with %q", authError(rec))
	}
	if ids, _ := st.Identities(ctx, victim.ID); len(ids) != 0 {
		t.Errorf("the victim's account gained %v", ids)
	}
}

// What a token has to be to be believed: for this application, for this browser's sign-in, from
// the tenant it names, a work account — and, where the deployment names one directory, from it.
func TestAMicrosoftTokenIsRefusedUnlessEveryPartOfItHolds(t *testing.T) {
	_, mux, f, st := microsoftBot(t, msSignInAnyOrganisation)
	for name, over := range map[string]map[string]any{
		"another application":        {"aud": "22222222-2222-2222-2222-222222222222"},
		"another browser's sign-in":  {"nonce": "somebody-else"},
		"a tenant it does not issue": {"iss": f.srv.URL + "/" + fabrikamTenant + "/v2.0"},
		"a personal account":         {"tid": msConsumerTenant},
		"no object id":               {"oid": nil},
		"an expired token":           {"exp": time.Now().Add(-time.Hour).Unix()},
	} {
		if u := signedInAs(t, st, microsoftSignIn(t, mux, f, over)); u != nil {
			t.Errorf("%s signed in as %+v", name, u)
		}
	}

	_, mux, f, st = microsoftBot(t, contosoTenant)
	if u := signedInAs(t, st, microsoftSignIn(t, mux, f, map[string]any{"tid": fabrikamTenant})); u != nil {
		t.Errorf("a deployment for one directory let another's account in: %+v", u)
	}
	if u := signedInAs(t, st, microsoftSignIn(t, mux, f, nil)); u == nil {
		t.Error("a deployment for one directory refused that directory's own account")
	}
}

// Somebody signed in who signs in with Microsoft is connecting it to the account they hold — the
// one moment two ways in can be joined, because they have just proved both.
func TestSigningInWithMicrosoftWhileSignedInConnectsIt(t *testing.T) {
	b, mux, f, st := microsoftBot(t, msSignInAnyOrganisation)
	ctx := context.Background()
	u, _, token := signedUp(t, b, mux, st, "ana@contoso.com")
	rec := microsoftSignIn(t, mux, f, nil, &http.Cookie{Name: sessionCookie, Value: token})
	if rec.Header().Get("Location") != "/admin/settings/?tab=security" {
		t.Fatalf("connecting went to %q (%s)", rec.Header().Get("Location"), authError(rec))
	}
	if got, _ := st.UserByIdentity(ctx, ProviderMicrosoft, contosoTenant+":"+anaOID); got == nil || got.ID != u.ID {
		t.Fatalf("the Microsoft identity is on %+v, want account %d", got, u.ID)
	}
	// And it now stands for her Teams account, which is what the console's personal notes and its
	// "who did this" read.
	teamID, userID, ok := chatAccount(Identity{Provider: ProviderMicrosoft, Subject: contosoTenant + ":" + anaOID})
	if !ok || teamID != "msteams:"+contosoTenant || userID != anaOID {
		t.Errorf("chatAccount = %q %q %v", teamID, userID, ok)
	}
}

// The organisation's sign-in policy is asked about a Microsoft sign-in by name: a passwords-only
// organisation turns it away and says how it is signed in to, a Microsoft-only one takes it, and a
// second factor finishes the sign-in it was owed for rather than a password one.
func TestTheSignInPolicyKnowsMicrosoft(t *testing.T) {
	b, mux, f, st := microsoftBot(t, msSignInAnyOrganisation)
	ctx := context.Background()
	u, orgID, _ := signedUp(t, b, mux, st, "ana@contoso.com")
	if err := st.AddIdentity(ctx, u.ID, ProviderMicrosoft, contosoTenant+":"+anaOID); err != nil {
		t.Fatal(err)
	}

	st.PutSetting(ctx, orgID, "auth_policy", AuthPolicyPassword)
	b.settings.Invalidate(orgID)
	rec := microsoftSignIn(t, mux, f, nil)
	if signedInAs(t, st, rec) != nil || !strings.Contains(authError(rec), "email and password") {
		t.Errorf("a passwords-only organisation answered a Microsoft sign-in with %q", authError(rec))
	}

	st.PutSetting(ctx, orgID, "auth_policy", AuthPolicyMicrosoft)
	b.settings.Invalidate(orgID)
	if me := signedInAs(t, st, microsoftSignIn(t, mux, f, nil)); me == nil || me.Via != ProviderMicrosoft {
		t.Errorf("a Microsoft-only organisation refused Microsoft: %+v", me)
	}
	if _, err := b.orgForSignIn(ctx, u, "password"); err == nil || !strings.Contains(err.Error(), "Sign in with Microsoft") {
		t.Errorf("a password under Microsoft-only was told %v", err)
	}

	for _, via := range []string{"slack", ProviderMicrosoft, "sso"} {
		tok, err := st.NewLoginChallenge(ctx, u.ID, via)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := st.loginChallengeVia(ctx, tok); got != via {
			t.Errorf("a %s sign-in owing a second factor finishes as %q", via, got)
		}
	}
	tok, _ := st.NewLoginChallenge(ctx, u.ID, "anything-else")
	if got, _ := st.loginChallengeVia(ctx, tok); got != "password" {
		t.Errorf("an unknown method finishes as %q, want password", got)
	}
}

// Neither the Teams package nor Sign in with Microsoft names a host of its own: both are made from
// the address the deployment is served at. The one binary at app.attesttag.com hands out a package
// whose domain is app.attesttag.com and sends Microsoft back to app.attesttag.com, and on a test
// host does the same for that host. What does differ per host is kept at Microsoft: the app
// registration's redirect URIs and the bot's messaging endpoint (guide/msteams.md).
func TestTheTeamsPackageAndSignInNameTheHostTheyAreServedFrom(t *testing.T) {
	for _, host := range []string{"app.attesttag.com", "mini.example.ts.net"} {
		t.Run(host, func(t *testing.T) {
			_, mux, f, _ := microsoftBot(t, msSignInAnyOrganisation)
			t.Setenv("ADMIN_BASE_URL", "https://"+host)

			// Started where the deployment is served, as a sign-in is: one started on another
			// address is first sent here (startsOnPublicOrigin).
			start := httptest.NewRequest("GET", "/api/auth/microsoft/login", nil)
			start.Host = host
			start.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, start)
			authz, err := url.Parse(w.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := authz.Query().Get("redirect_uri"), "https://"+host+"/api/auth/microsoft/callback"; got != want {
				t.Errorf("Microsoft is asked to send the sign-in back to %q, want %q", got, want)
			}

			var session *http.Cookie
			for _, c := range microsoftSignIn(t, mux, f, nil).Result().Cookies() {
				if c.Name == sessionCookie && c.Value != "" {
					session = c
				}
			}
			if session == nil {
				t.Fatal("the sign-in made no session")
			}
			r := httptest.NewRequest("GET", "/api/workspaces/msteams/package", nil)
			r.AddCookie(session)
			pkg := httptest.NewRecorder()
			mux.ServeHTTP(pkg, r)
			if pkg.Code != http.StatusOK {
				t.Fatalf("package = %d: %s", pkg.Code, pkg.Body.String())
			}
			var manifest struct {
				ValidDomains []string `json:"validDomains"`
			}
			if err := json.Unmarshal(fileInZip(t, pkg.Body.Bytes(), "manifest.json"), &manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.ValidDomains) != 1 || manifest.ValidDomains[0] != host {
				t.Errorf("the package served at %s names %v", host, manifest.ValidDomains)
			}
		})
	}
}

func fileInZip(t *testing.T, zipped []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			raw, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
	}
	t.Fatalf("%s is not in the package", name)
	return nil
}
