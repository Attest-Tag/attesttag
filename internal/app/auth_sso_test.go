package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Single sign-on, tested against an identity provider that answers the way a real one does.
//
// What these are about is the two halves of the trust in auth_sso.go: that a provider is inert
// until its domain is proved, and that a proved provider is authoritative for that domain and
// for nothing beyond it. Everything else here — discovery, PKCE, the token exchange — is
// plumbing that has to work for those two to be reachable.

// fakeIdP is an OpenID Connect provider in about forty lines: a discovery document, a token
// endpoint that mints an unsigned id_token for whoever it is told to, and userinfo.
type fakeIdP struct {
	srv *httptest.Server
	// Who the next sign-in is for. Set per test rather than per request, because what varies
	// between these cases is who the provider claims somebody is.
	sub, email, name string
	emailVerified    bool
	// Overrides, for the cases that are about a provider misbehaving.
	audience, issuer, nonce string
	lastForm                url.Values
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{sub: "idp-user-1", email: "new@acme-corp.test", name: "New Person", emailVerified: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 idp.srv.URL,
			"authorization_endpoint": idp.srv.URL + "/authorize",
			"token_endpoint":         idp.srv.URL + "/token",
			"userinfo_endpoint":      idp.srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		idp.lastForm = r.PostForm
		iss := idp.srv.URL
		if idp.issuer != "" {
			iss = idp.issuer
		}
		aud := r.PostFormValue("client_id")
		if idp.audience != "" {
			aud = idp.audience
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-1", "token_type": "Bearer",
			"id_token": idp.idToken(iss, aud),
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"sub": idp.sub, "email": idp.email, "email_verified": idp.emailVerified, "name": idp.name,
		})
	})
	// TLS, because registration refuses an issuer that is not https — an OIDC client secret and
	// an id_token over cleartext is not a thing to allow for the convenience of a test. The
	// client below is the one httptest builds to trust this server's certificate.
	idp.srv = httptest.NewTLSServer(mux)
	t.Cleanup(idp.srv.Close)

	// The outbound guard comes off for as long as this provider exists: it is there to refuse
	// loopback addresses, and a test's IdP is one. Both halves have to go together — the check
	// and the transport under it (see the note by ssoURLCheck) — so the client becomes the
	// server's own, which both trusts its certificate and will dial it.
	oldCheck, oldClient := ssoURLCheck, ssoClient
	ssoURLCheck = func(*url.URL) error { return nil }
	client := idp.srv.Client()
	client.Timeout = 10 * time.Second
	ssoClient = client
	t.Cleanup(func() {
		ssoURLCheck, ssoClient = oldCheck, oldClient
		discoveryMu.Lock()
		discoveryCache = map[string]discoveryEntry{}
		discoveryMu.Unlock()
	})
	return idp
}

// idToken builds the JWT. Unsigned, with an "alg":"none" header: what this package reads out of
// an id_token is claims it then checks against values it chose itself, and the token only ever
// arrives over the direct TLS call to the token endpoint — see the note on ssoIdentity.
func (f *fakeIdP) idToken(iss, aud string) string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	claims := map[string]any{
		"iss": iss, "sub": f.sub, "aud": aud, "exp": time.Now().Add(5 * time.Minute).Unix(),
		"email": f.email, "email_verified": f.emailVerified, "name": f.name,
	}
	if f.nonce != "" {
		claims["nonce"] = f.nonce
	}
	return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "."
}

// registerSSO puts a provider on the organisation and, unless asked not to, proves its domain
// the way handleSSOVerify would. Verification itself goes through DNS, which a test cannot
// publish to, so the flag is set directly — the tests that care about the unverified state
// simply do not call this with verify.
func registerSSO(t *testing.T, b *Bot, mux *http.ServeMux, st *Store, token, issuer, domain string, verify bool) *SSOProvider {
	t.Helper()
	code, body := authReq(t, mux, "POST", "/api/settings/sso", map[string]string{
		"domain": domain, "issuer": issuer, "client_id": "client-1", "client_secret": "shh",
	}, token)
	if code != 200 {
		t.Fatalf("registering a provider = %d: %v", code, body)
	}
	ctx := context.Background()
	u, _ := st.AdminSession(ctx, token)
	if verify {
		if err := st.MarkSSODomainVerified(ctx, u.OrgID); err != nil {
			t.Fatal(err)
		}
	}
	p, err := st.SSOProviderForOrg(ctx, u.OrgID)
	if err != nil || p == nil {
		t.Fatalf("no provider stored: %v", err)
	}
	return p
}

// ssoStart asks for a sign-in the way the console's login page does, from the console's own address.
func ssoStart(t *testing.T, mux *http.ServeMux, email string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"email": email})
	r := asConsole(httptest.NewRequest("POST", "/api/auth/sso/start", strings.NewReader(string(raw))))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// ssoRoundTrip walks a browser through the flow: start, carry the state cookie to the provider
// and back, and land on the callback. It returns the callback's response.
func ssoRoundTrip(t *testing.T, mux *http.ServeMux, idp *fakeIdP, providerID, email string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"email": email})
	r := asConsole(httptest.NewRequest("POST", "/api/auth/sso/start", strings.NewReader(string(raw))))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("sso start = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)

	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == ssoStateCookie {
			state = c
		}
	}
	if state == nil {
		t.Fatal("no state cookie was set")
	}
	// The nonce the server minted rides in the authorize URL; a real IdP echoes it into the
	// id_token, so the fake one is told what it is.
	authz, err := url.Parse(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	idp.nonce = authz.Query().Get("nonce")

	cb := httptest.NewRequest("GET", "/api/auth/sso/callback/"+providerID+
		"?code=auth-code&state="+url.QueryEscape(authz.Query().Get("state")), nil)
	cb.AddCookie(state)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, cb)
	return rec
}

// signedInAs reads the session the callback set, if it set one.
func signedInAs(t *testing.T, st *Store, rec *httptest.ResponseRecorder) *AdminUser {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			u, _ := st.AdminSession(context.Background(), c.Value)
			return u
		}
	}
	return nil
}

// authError pulls the reason out of the redirect the failure path sends the browser to.
func authError(rec *httptest.ResponseRecorder) string {
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		return ""
	}
	return u.Query().Get("auth_error")
}

// Registering claims a domain; it does not open it. Until the TXT record is published the
// provider routes nobody at all — which is what stops a stranger on an open-registration
// deployment from claiming a domain they do not own and collecting its sign-ins.
func TestSSORegistrationIsInertUntilTheDomainIsProved(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	_, _, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", false)

	if p.DomainVerified {
		t.Fatal("a freshly registered provider must not be verified")
	}
	// The sign-in lookup cannot see it, so the start endpoint has nothing to route to.
	if got, _ := st.SSOProviderByDomain(context.Background(), "acme-corp.test"); got != nil {
		t.Error("an unverified provider was returned by the sign-in lookup")
	}
	code, body := ssoStart(t, mux, "someone@acme-corp.test")
	if code != 404 {
		t.Fatalf("start against an unverified provider = %d: %v", code, body)
	}

	// And once proved, the same address routes.
	if err := st.MarkSSODomainVerified(context.Background(), p.OrgID); err != nil {
		t.Fatal(err)
	}
	if code, body := ssoStart(t, mux, "someone@acme-corp.test"); code != 200 {
		t.Fatalf("start against a verified provider = %d: %v", code, body)
	}
}

// The ordinary path: somebody in the directory who has never been here signs in, and arrives as
// a member of the provider's organisation with the least the console has.
func TestSSOSignsInAndProvisionsAViewer(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	_, orgID, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", true)
	ctx := context.Background()

	rec := ssoRoundTrip(t, mux, idp, p.ProviderID, "new@acme-corp.test")
	me := signedInAs(t, st, rec)
	if me == nil {
		t.Fatalf("no session was created: %d %s", rec.Code, authError(rec))
	}
	if me.OrgID != orgID {
		t.Errorf("signed into org %d, want the provider's %d", me.OrgID, orgID)
	}
	if me.Via != "sso" {
		t.Errorf("session recorded via %q, want sso", me.Via)
	}
	m, err := st.Membership(ctx, me.ID, orgID)
	if err != nil || m == nil {
		t.Fatalf("no membership was created: %v", err)
	}
	if m.Role != RoleViewer {
		t.Errorf("provisioned as %q, want %q: a directory listing is not a decision about permissions", m.Role, RoleViewer)
	}
	// PKCE actually happened, rather than the parameter being sent and forgotten.
	if idp.lastForm.Get("code_verifier") == "" {
		t.Error("the token exchange carried no PKCE verifier")
	}

	// Signing in again is the same person, not a second account.
	rec2 := ssoRoundTrip(t, mux, idp, p.ProviderID, "new@acme-corp.test")
	again := signedInAs(t, st, rec2)
	if again == nil || again.ID != me.ID {
		t.Errorf("a second sign-in made a different account: %v", again)
	}
}

// An IdP is authoritative for the domain its organisation proved and for nothing else. This is
// the check that keeps one organisation's identity provider from minting an identity for
// somebody at another — including the address of an admin somewhere else on the deployment.
func TestSSORefusesAnAddressOutsideTheProvedDomain(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	_, _, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", true)

	// A second organisation, whose founder the first one's IdP now tries to speak for.
	victim, _, _ := signedUp(t, b, mux, st, "victim@other-company.test")

	// The flow is started at the attacker's own domain, which is the only one that routes to
	// their provider — and the provider then answers with somebody else's address. That is the
	// shape of the attack: the IdP is reached legitimately and lies about who signed in.
	idp.sub, idp.email = "attacker", "victim@other-company.test"
	rec := ssoRoundTrip(t, mux, idp, p.ProviderID, "attacker@acme-corp.test")
	if me := signedInAs(t, st, rec); me != nil {
		t.Fatalf("an IdP signed in somebody at a domain it had not proved: user %d", me.ID)
	}
	if msg := authError(rec); !strings.Contains(msg, "acme-corp.test") {
		t.Errorf("the refusal should name the domain it is for, got %q", msg)
	}
	// And the account it aimed at is untouched: no identity was attached to it.
	ids, _ := st.Identities(context.Background(), victim.ID)
	for _, id := range ids {
		if id.Provider == ProviderSSO {
			t.Fatal("an SSO identity was attached to an account at another domain")
		}
	}
}

// The provider is taken from the one-shot cookie, not from the path. They have to agree: the
// path is attacker-chosen, and a callback that trusted it would let a code issued by one
// organisation's IdP be redeemed against another's registration.
func TestSSOCallbackRefusesAProviderItDidNotStart(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	_, _, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", true)

	raw, _ := json.Marshal(map[string]string{"email": "new@acme-corp.test"})
	r := asConsole(httptest.NewRequest("POST", "/api/auth/sso/start", strings.NewReader(string(raw))))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == ssoStateCookie {
			state = c
		}
	}
	var out struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	authz, _ := url.Parse(out.URL)

	cb := httptest.NewRequest("GET", "/api/auth/sso/callback/org-somebody-else?code=c&state="+
		url.QueryEscape(authz.Query().Get("state")), nil)
	cb.AddCookie(state)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, cb)
	if me := signedInAs(t, st, rec); me != nil {
		t.Fatal("a callback completed against a provider it was not started for")
	}

	// A callback with no state cookie at all is refused the same way.
	bare := httptest.NewRequest("GET", "/api/auth/sso/callback/"+p.ProviderID+"?code=c&state=anything", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, bare)
	if me := signedInAs(t, st, rec2); me != nil {
		t.Fatal("a callback with no state cookie created a session")
	}
}

// An account that already exists is adopted only when it is already a member here. The
// organisation has proved the domain and the address is inside it, which is exactly what an IdP
// is for; an account somewhere else on the deployment is not its to speak for.
func TestSSOAdoptsAMemberAndRefusesAStranger(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	founder, orgID, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", true)
	ctx := context.Background()

	idp.sub, idp.email = "idp-founder", "founder@acme-corp.test"
	rec := ssoRoundTrip(t, mux, idp, p.ProviderID, "founder@acme-corp.test")
	me := signedInAs(t, st, rec)
	if me == nil || me.ID != founder.ID {
		t.Fatalf("the founder was not adopted onto their own account: %v (%s)", me, authError(rec))
	}
	if m, _ := st.Membership(ctx, founder.ID, orgID); m == nil || m.Role != RoleAdmin {
		t.Error("adopting an account must not change the role it already had")
	}

	// Somebody at the same domain with an account that is not a member of this organisation.
	// Contrived — it takes a second organisation holding the address — but it is the case the
	// membership check exists for, and it must not be provisioned over.
	outsider, err := st.CreateUser(ctx, "outsider@acme-corp.test", "Outsider", "")
	if err != nil {
		t.Fatal(err)
	}
	idp.sub, idp.email = "idp-outsider", "outsider@acme-corp.test"
	rec = ssoRoundTrip(t, mux, idp, p.ProviderID, "outsider@acme-corp.test")
	if signedInAs(t, st, rec) != nil {
		t.Fatal("an account that is not a member of this organisation was signed into it")
	}
	if m, _ := st.Membership(ctx, outsider.ID, orgID); m != nil {
		t.Fatal("a membership was created for an account that was refused")
	}
}

// The policy and the provider are two settings that can contradict each other, and the console
// refuses the move that would leave nobody a way in.
func TestSSOPolicyAndRemovalCannotLockEverybodyOut(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	_, orgID, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	ctx := context.Background()

	// SSO-only before there is an SSO provider at all.
	if code, _ := authReq(t, mux, "PUT", "/api/settings", map[string]string{"auth_policy": AuthPolicySSO}, token); code == 200 {
		t.Fatal("an organisation with no identity provider was allowed to require one")
	}
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", true)
	// Verified, but this admin has never signed in through it.
	if code, _ := authReq(t, mux, "PUT", "/api/settings", map[string]string{"auth_policy": AuthPolicySSO}, token); code == 200 {
		t.Fatal("an admin with no SSO identity of their own was allowed to require SSO")
	}

	idp.sub, idp.email = "idp-founder", "founder@acme-corp.test"
	if signedInAs(t, st, ssoRoundTrip(t, mux, idp, p.ProviderID, "founder@acme-corp.test")) == nil {
		t.Fatal("the founder could not sign in through the provider")
	}
	if code, body := authReq(t, mux, "PUT", "/api/settings", map[string]string{"auth_policy": AuthPolicySSO}, token); code != 200 {
		t.Fatalf("an admin who holds an SSO identity was refused: %d %v", code, body)
	}
	b.settings.Invalidate(orgID)

	// And now the provider cannot be removed out from under the policy that depends on it.
	if code, _ := authReq(t, mux, "DELETE", "/api/settings/sso", nil, token); code == 200 {
		t.Fatal("the only way in was removed while the policy required it")
	}
	if got, _ := st.SSOProviderForOrg(ctx, orgID); got == nil {
		t.Fatal("the provider was deleted by a refused request")
	}
}

// Two organisations cannot both hold the same domain once one of them has proved it — the claim
// is what routes a sign-in, so a shared one would be a coin toss over whose staff go where.
func TestSSODomainIsExclusiveOnlyOnceProved(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	_, _, first := signedUp(t, b, mux, st, "founder@acme-corp.test")
	_, secondOrg, second := signedUp(t, b, mux, st, "other@elsewhere.test")

	registerSSO(t, b, mux, st, first, idp.srv.URL, "acme-corp.test", true)

	// An unproved claim on the same domain is allowed to exist...
	code, body := authReq(t, mux, "POST", "/api/settings/sso", map[string]string{
		"domain": "acme-corp.test", "issuer": idp.srv.URL, "client_id": "c", "client_secret": "s",
	}, second)
	if code != 200 {
		t.Fatalf("a second, unproved claim was refused: %d %v", code, body)
	}
	// ...and proving it is where it stops.
	if err := st.MarkSSODomainVerified(context.Background(), secondOrg); err == nil {
		t.Fatal("two organisations both proved the same domain")
	}
	if got, _ := st.SSOProviderByDomain(context.Background(), "acme-corp.test"); got == nil {
		t.Fatal("the original provider stopped routing")
	} else if got.OrgID == secondOrg {
		t.Fatal("the domain was taken over by the second claim")
	}
}

// A provider whose token names a different audience or a different issuer is refused: those are
// the two claims that say a token was minted for somebody else, and a mix-up is how an
// assertion meant for another application ends up accepted here.
func TestSSORefusesATokenMintedForSomebodyElse(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	idp := newFakeIdP(t)
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", true)

	for _, tc := range []struct {
		name  string
		apply func()
	}{
		{"another application's audience", func() { idp.audience = "some-other-client" }},
		{"another provider's issuer", func() { idp.issuer = "https://issuer.invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp.audience, idp.issuer = "", ""
			tc.apply()
			rec := ssoRoundTrip(t, mux, idp, p.ProviderID, "new@acme-corp.test")
			if me := signedInAs(t, st, rec); me != nil {
				t.Fatalf("a token with %s was accepted", tc.name)
			}
		})
	}
	idp.audience, idp.issuer = "", ""
}

// The registration form's own refusals, which are the ones an admin will actually meet.
func TestSSORegistrationRefusesWhatCannotWork(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	_, _, token := signedUp(t, b, mux, st, "founder@acme-corp.test")

	for _, tc := range []struct {
		name string
		body map[string]string
	}{
		{"a domain that is not one", map[string]string{"domain": "not a domain", "issuer": idp.srv.URL, "client_id": "c", "client_secret": "s"}},
		{"an issuer that is not https", map[string]string{"domain": "acme-corp.test", "issuer": "ftp://okta.example", "client_id": "c", "client_secret": "s"}},
		{"no client secret", map[string]string{"domain": "acme-corp.test", "issuer": idp.srv.URL, "client_id": "c"}},
		{"an issuer that publishes nothing", map[string]string{"domain": "acme-corp.test", "issuer": "https://nothing.invalid", "client_id": "c", "client_secret": "s"}},
		{"a protocol we do not speak", map[string]string{"kind": "saml", "domain": "acme-corp.test", "issuer": idp.srv.URL, "client_id": "c", "client_secret": "s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := authReq(t, mux, "POST", "/api/settings/sso", tc.body, token); code == 200 {
				t.Fatalf("%s was accepted: %v", tc.name, body)
			}
		})
	}

	// The client secret goes in and does not come back out.
	registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", false)
	_, body := authReq(t, mux, "GET", "/api/settings/sso", nil, token)
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "shh") {
		t.Fatalf("the client secret was returned to the console: %s", raw)
	}
	if fmt.Sprint(body["record_name"]) != ssoTXTName("acme-corp.test") {
		t.Errorf("the DNS record to publish was not offered: %v", body["record_name"])
	}
}

// The sign-in policy is an organisation's answer to "may single sign-on open this console", and it
// is asked before the sign-in does anything. It used to be asked after ssoAccount, so a refusal
// came with an account and a viewer's membership already made for somebody new, or single sign-on
// already connected to a member's account, in an organisation that had said no to it.
func TestSSORefusedByThePolicyLeavesNothingBehind(t *testing.T) {
	b, mux, st := identityBot(t)
	idp := newFakeIdP(t)
	founder, orgID, token := signedUp(t, b, mux, st, "founder@acme-corp.test")
	p := registerSSO(t, b, mux, st, token, idp.srv.URL, "acme-corp.test", true)
	ctx := context.Background()
	if code, body := authReq(t, mux, "PUT", "/api/settings", map[string]string{"auth_policy": AuthPolicyPassword}, token); code != 200 {
		t.Fatalf("moving to passwords only = %d %v", code, body)
	}
	b.settings.Invalidate(orgID)

	rec := ssoRoundTrip(t, mux, idp, p.ProviderID, "new@acme-corp.test")
	if signedInAs(t, st, rec) != nil {
		t.Fatal("single sign-on opened an organisation that accepts passwords only")
	}
	if msg := authError(rec); !strings.Contains(msg, "does not accept single sign-on") || !strings.Contains(msg, "password") {
		t.Errorf("the refusal should say what the organisation does accept: %q", msg)
	}
	if u, _ := st.UserByEmail(ctx, "new@acme-corp.test"); u != nil {
		t.Fatal("a refused sign-in made an account")
	}
	if ms, _ := st.MembersOf(ctx, orgID); len(ms) != 1 {
		t.Fatalf("a refused sign-in left %d members, want the founder alone", len(ms))
	}

	idp.sub, idp.email = "idp-founder", "founder@acme-corp.test"
	rec = ssoRoundTrip(t, mux, idp, p.ProviderID, "founder@acme-corp.test")
	if signedInAs(t, st, rec) != nil {
		t.Fatal("a member was signed in by single sign-on under a passwords-only policy")
	}
	ids, _ := st.Identities(ctx, founder.ID)
	for _, id := range ids {
		if id.Provider == ProviderSSO {
			t.Fatal("a refused sign-in connected single sign-on to the member's account")
		}
	}
	if idp.lastForm != nil {
		t.Error("the authorization code was exchanged for a sign-in the policy refuses")
	}
}
