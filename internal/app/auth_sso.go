package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"crypto/sha256"
)

// Single sign-on: an organisation points at its own OpenID Connect provider and its staff sign
// in there instead of here.
//
// The three ways into this console are now a password (auth_password.go), Slack
// (auth_slack.go), and this. All three mint the same session; what differs is what they prove.
//
// What an SSO sign-in proves is that the IdP at a DNS-verified domain vouches for an address at
// that same domain. Both halves are load-bearing:
//
//   - DNS-verified. Registration alone lets an organisation claim any domain it can type, and
//     on a deployment with open registration that would be a stranger claiming yours. The TXT
//     record is what turns a claim into a fact, and SSOProviderByDomain will not return an
//     unproved row, so nothing before verification opens any door.
//   - At that same domain. An IdP can put whatever it likes in an assertion, including an
//     address it has no business speaking for. Refusing an email outside the proved domain is
//     what keeps one organisation's IdP from minting an identity for somebody at another.
//
// Together they mean the worst an organisation's own IdP admin can do is impersonate their own
// colleagues, which is a power they already had.

const (
	ssoStateCookie = "attest_sso_state"
	ssoStateTTL    = 10 * time.Minute
	// How long a discovery document is trusted before it is fetched again. Endpoints move
	// rarely, and a sign-in is not the moment to find out that an IdP's well-known endpoint is
	// briefly down.
	ssoDiscoveryTTL = time.Hour
)

// ---- discovery ----

type oidcConfig struct {
	Issuer        string   `json:"issuer"`
	AuthURL       string   `json:"authorization_endpoint"`
	TokenURL      string   `json:"token_endpoint"`
	UserInfoURL   string   `json:"userinfo_endpoint"`
	JWKSURL       string   `json:"jwks_uri"`
	PKCEMethods   []string `json:"code_challenge_methods_supported"`
	ScopesSupport []string `json:"scopes_supported"`
}

// Every call below goes to a URL an admin typed into a settings form, so it goes out through
// the guarded transport the rest of the deployment uses for admin-supplied URLs: no loopback, no
// link-local, no RFC1918. Without it, "Issuer URL" is a hole straight to the metadata service.
//
// Both are variables for the same reason mcpURLCheck is: a test's IdP listens on loopback, and
// relaxing the check alone is not enough — the transport refuses to dial it too.
var (
	ssoURLCheck = checkURL
	ssoClient   = &http.Client{Transport: publicTransport(), Timeout: 20 * time.Second, CheckRedirect: rejectRedirect}
)

var (
	discoveryMu    sync.Mutex
	discoveryCache = map[string]discoveryEntry{}
)

type discoveryEntry struct {
	cfg *oidcConfig
	at  time.Time
}

// discover reads an issuer's well-known document, cached.
//
// The issuer is checked against the document's own `issuer` claim, which is the spec's own
// anti-mixup rule: a URL that serves somebody else's metadata would otherwise let an admin
// register a provider whose tokens are issued by a party the admin named and the document
// disowns.
func oidcDiscover(ctx context.Context, issuer string) (*oidcConfig, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	discoveryMu.Lock()
	if e, ok := discoveryCache[issuer]; ok && time.Since(e.at) < ssoDiscoveryTTL {
		discoveryMu.Unlock()
		return e.cfg, nil
	}
	discoveryMu.Unlock()

	well, err := url.Parse(issuer + "/.well-known/openid-configuration")
	if err != nil {
		return nil, fmt.Errorf("that issuer is not a URL we can reach: %w", err)
	}
	if err := ssoURLCheck(well); err != nil {
		return nil, fmt.Errorf("that issuer is not a public address we will fetch: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", well.String(), nil)
	resp, err := ssoClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", issuer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s answered %d for its OpenID configuration; check the issuer URL", issuer, resp.StatusCode)
	}
	var cfg oidcConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("could not read the OpenID configuration at %s: %w", issuer, err)
	}
	if cfg.AuthURL == "" || cfg.TokenURL == "" {
		return nil, fmt.Errorf("%s published an OpenID configuration with no authorization or token endpoint", issuer)
	}
	if got := strings.TrimRight(cfg.Issuer, "/"); got != "" && got != issuer {
		return nil, fmt.Errorf("that URL publishes configuration for a different issuer (%s); use %s instead", got, got)
	}
	discoveryMu.Lock()
	discoveryCache[issuer] = discoveryEntry{cfg: &cfg, at: time.Now()}
	discoveryMu.Unlock()
	return &cfg, nil
}

// ---- the sign-in flow ----

// ssoCallbackURL is the redirect URI an organisation registers on its IdP. Per provider, so one
// organisation's registration cannot be used to complete another's sign-in.
func (b *Bot) ssoCallbackURL(r *http.Request, providerID string) string {
	return b.baseURL(r) + "/api/auth/sso/callback/" + url.PathEscape(providerID)
}

// handleSSOStart turns an email address into a redirect to that organisation's IdP.
//
// It answers the same way whether or not a provider exists, because this endpoint takes an
// unauthenticated address and would otherwise say which domains have SSO and which do not —
// a free directory of who runs what. The caller gets a URL or a flat "no SSO for that address".
func (b *Bot) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	// The identity provider sends the browser back to the public origin, and the state cookie
	// set below would stay on this address (startsOnPublicOrigin). This start is a POST the page
	// makes, so it cannot be redirected there: say where to sign in instead of failing afterwards
	// as an expired link. Before the provider lookup, so it is the same answer for every domain.
	if base := b.baseURL(r); b.publicOriginKnown(r) && !sameOriginOf(base, requestOrigin(r)) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "Single sign-on comes back to " + base + ", not to this address. Open " + base + "/admin/ and sign in there."})
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	domain := emailDomainOf(in.Email)
	if domain == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Type the email address you use at work."})
		return
	}
	p, err := b.store.SSOProviderByDomain(r.Context(), domain)
	if err != nil || p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "No single sign-on is set up for " + domain + ". Sign in with your password, or ask an admin to set it up."})
		return
	}
	cfg, err := oidcDiscover(r.Context(), p.Issuer)
	if err != nil {
		slog.Warn("sso discovery failed", "org", p.OrgID, "issuer", p.Issuer, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "Your identity provider did not answer. Try again, or tell an admin the setup needs checking."})
		return
	}

	state, verifier, nonce := randomToken(), randomToken(), randomToken()
	http.SetCookie(w, &http.Cookie{
		Name: ssoStateCookie, Value: strings.Join([]string{state, verifier, nonce, p.ProviderID}, "|"),
		Path: "/api/auth/", HttpOnly: true, MaxAge: int(ssoStateTTL.Seconds()),
		SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r),
	})

	q := url.Values{
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"client_id":     {p.ClientID},
		"redirect_uri":  {b.ssoCallbackURL(r, p.ProviderID)},
		"state":         {state},
		"nonce":         {nonce},
		// PKCE on a confidential client is belt and braces, and it is what stops an
		// authorisation code stolen in the redirect from being spent by anybody but the browser
		// that started the flow.
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
		// The address they typed, so the IdP can skip its own "who are you" step. A hint and
		// nothing more: what comes back is checked against the domain regardless.
		"login_hint": {normalEmail(in.Email)},
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": cfg.AuthURL + "?" + q.Encode()})
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// emailDomainOf takes the domain from an address, or from a bare domain typed instead of one.
func emailDomainOf(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if !strings.Contains(s, ".") {
		return ""
	}
	return s
}

// ssoIdentity is what an IdP told us about the person who just signed in.
type ssoIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

func (b *Bot) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, _ := r.Cookie(ssoStateCookie)
	http.SetCookie(w, &http.Cookie{Name: ssoStateCookie, Value: "", Path: "/api/auth/", MaxAge: -1}) // single use

	if c == nil || c.Value == "" {
		loginFailed(w, r, "That sign-in link expired or was already used. Try again.")
		return
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 4 || parts[0] == "" || q.Get("state") != parts[0] {
		loginFailed(w, r, "That sign-in link expired or was already used. Try again.")
		return
	}
	verifier, nonce, providerID := parts[1], parts[2], parts[3]
	// The provider is taken from the cookie, not from the path. They must agree: the path is
	// attacker-chosen, and a callback that trusted it would let a code issued by one
	// organisation's IdP be redeemed against another's registration.
	if providerID != r.PathValue("provider") {
		loginFailed(w, r, "That sign-in was started for a different organisation. Try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		loginFailed(w, r, "Your identity provider refused the sign-in ("+e+").")
		return
	}
	code := q.Get("code")
	if code == "" {
		loginFailed(w, r, "Your identity provider did not return a sign-in code. Try again.")
		return
	}

	ctx := r.Context()
	p, err := b.store.SSOProviderByID(ctx, providerID)
	if err != nil || p == nil {
		loginFailed(w, r, "That identity provider is no longer registered.")
		return
	}
	if !p.DomainVerified {
		loginFailed(w, r, "Single sign-on for "+p.Domain+" is not switched on yet: the domain has not been verified.")
		return
	}
	// The organisation's sign-in policy is asked before anything is exchanged or written. Asked
	// after, as it was, the refusal came too late: ssoAccount had already given somebody new an
	// account and a viewer's membership, or connected single sign-on to a member's account, in an
	// organisation that does not accept single sign-on at all. The organisation is the provider's,
	// not "whichever membership came first": somebody who belongs to two organisations and signs
	// in through this one's IdP means this one.
	if st := b.settings.Get(ctx, p.OrgID); !st.allowsVia("sso") {
		loginFailed(w, r, "This organisation does not accept single sign-on. "+signInHint(st.AuthPolicy))
		return
	}

	id, err := b.ssoIdentity(ctx, p, code, nonce, verifier, b.ssoCallbackURL(r, p.ProviderID))
	if err != nil {
		slog.Warn("sso sign-in failed", "org", p.OrgID, "provider", p.ProviderID, "err", err)
		loginFailed(w, r, err.Error())
		return
	}

	acct, err := b.ssoAccount(ctx, p, id)
	if err != nil {
		slog.Warn("sso sign-in refused", "org", p.OrgID, "subject", id.Subject, "err", err)
		loginFailed(w, r, err.Error())
		return
	}

	e, err := b.store.TOTPEnrolment(ctx, acct.ID)
	if err != nil {
		loginFailed(w, r, "Could not verify account security. Try again.")
		return
	}
	if e != nil && e.Confirmed {
		tok, err := b.store.NewLoginChallenge(ctx, acct.ID, "sso")
		if err != nil {
			loginFailed(w, r, "Could not start two-factor sign-in.")
			return
		}
		http.Redirect(w, r, "/admin/login/#challenge="+url.QueryEscape(tok), http.StatusFound)
		return
	}
	b.startSession(w, r, acct, p.OrgID, "sso")
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// ssoIdentity exchanges the code and reads back who signed in.
//
// The identity claims are taken from the userinfo endpoint rather than from the id_token
// alone, which is the same shape as the Slack path next door and avoids this package growing a
// hand-rolled JWT verifier. That is sound because both the token response and the userinfo
// response arrive over TLS in direct, client-authenticated calls to endpoints named by the
// issuer's own discovery document — OIDC Core §3.1.3.7 says signature checking may be skipped
// on exactly that basis. The id_token is still read, for three checks a userinfo response
// cannot make: the issuer, the audience, and the nonce that binds this response to this
// browser's request. And the two `sub` values must agree, as the spec requires, so a userinfo
// response cannot describe a different person from the token that fetched it.
func (b *Bot) ssoIdentity(ctx context.Context, p *SSOProvider, code, nonce, verifier, redirectURI string) (*ssoIdentity, error) {
	cfg, err := oidcDiscover(ctx, p.Issuer)
	if err != nil {
		return nil, errors.New("Your identity provider did not answer. Try again in a moment.")
	}
	secret, err := b.store.SSOClientSecret(ctx, b.sealer, p.ID)
	if err != nil {
		return nil, errors.New("This organisation's single sign-on needs to be registered again.")
	}

	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier},
		"client_id": {p.ClientID}, "client_secret": {secret},
	}
	tokenURL, err := url.Parse(cfg.TokenURL)
	if err != nil || ssoURLCheck(tokenURL) != nil {
		return nil, errors.New("Your identity provider's token endpoint is not an address we will call.")
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", cfg.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic as well as _post: some IdPs (Entra among them) register a client as
	// one or the other, and sending both is what every library does rather than making an admin
	// find out which by failing.
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(secret))
	resp, err := ssoClient.Do(req)
	if err != nil {
		return nil, errors.New("Could not reach your identity provider to finish signing in. Try again.")
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok)
	if tok.Error != "" || tok.IDToken == "" {
		slog.Warn("sso token exchange refused", "org", p.OrgID, "status", resp.StatusCode,
			"err", tok.Error, "desc", tok.ErrorDesc)
		return nil, errors.New("Your identity provider would not complete the sign-in. Try again, or ask an admin to check the client ID and secret.")
	}

	claims, err := idTokenClaims(tok.IDToken)
	if err != nil {
		return nil, errors.New("Your identity provider returned a token we could not read.")
	}
	if got := strings.TrimRight(claims.Issuer, "/"); got != strings.TrimRight(p.Issuer, "/") {
		return nil, errors.New("That sign-in came from a different identity provider than the one registered here.")
	}
	if !claims.audienceIs(p.ClientID) {
		return nil, errors.New("That sign-in was issued for a different application.")
	}
	if claims.Nonce != "" && claims.Nonce != nonce {
		return nil, errors.New("That sign-in could not be matched to this browser. Try again.")
	}
	if claims.Expiry > 0 && time.Now().After(time.Unix(claims.Expiry, 0).Add(2*time.Minute)) {
		return nil, errors.New("That sign-in took too long. Try again.")
	}
	if claims.Subject == "" {
		return nil, errors.New("Your identity provider did not say who signed in.")
	}

	id := &ssoIdentity{Subject: claims.Subject, Email: strings.TrimSpace(claims.Email),
		EmailVerified: claims.EmailVerified, Name: strings.TrimSpace(claims.Name)}

	if cfg.UserInfoURL != "" && tok.AccessToken != "" {
		if ui, err := ssoUserInfo(ctx, cfg.UserInfoURL, tok.AccessToken); err != nil {
			slog.Warn("sso userinfo failed; falling back to the id_token", "org", p.OrgID, "err", err)
		} else if ui.Subject != "" && ui.Subject != claims.Subject {
			// The spec's own rule. A mismatch means the access token and the id_token describe
			// different people, which is a sign of a broken or hostile provider either way.
			return nil, errors.New("Your identity provider returned two different identities for one sign-in.")
		} else {
			if ui.Email != "" {
				id.Email, id.EmailVerified = strings.TrimSpace(ui.Email), ui.EmailVerified
			}
			if ui.Name != "" {
				id.Name = strings.TrimSpace(ui.Name)
			}
		}
	}
	if id.Email == "" {
		return nil, errors.New("Your identity provider did not give us an email address. Ask an admin to grant the application the email scope.")
	}
	return id, nil
}

type oidcClaims struct {
	Issuer        string `json:"iss"`
	Subject       string `json:"sub"`
	Audience      any    `json:"aud"`
	Nonce         string `json:"nonce"`
	Expiry        int64  `json:"exp"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

// audienceIs handles both shapes the claim is allowed to take: one string, or a list.
func (c oidcClaims) audienceIs(clientID string) bool {
	switch v := c.Audience.(type) {
	case string:
		return v == clientID
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

// idTokenClaims reads the payload of a JWT. It does not check the signature: see the note on
// ssoIdentity for why that is sound here, and note that every claim read from it is checked
// against something this server chose (the registered issuer, the registered client id, the
// nonce minted for this request).
func idTokenClaims(raw string) (oidcClaims, error) {
	var c oidcClaims
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return c, errors.New("not a JWT")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return c, err
	}
	return c, nil
}

func ssoUserInfo(ctx context.Context, endpoint, accessToken string) (*oidcClaims, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if err := ssoURLCheck(u); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := ssoClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("userinfo answered %d", resp.StatusCode)
	}
	var c oidcClaims
	// A provider may answer userinfo as a signed JWT rather than as JSON. Reading the payload
	// covers that without a second code path; the sub check above is what makes it safe.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return &c, nil
	}
	c, err = idTokenClaims(strings.TrimSpace(string(raw)))
	return &c, err
}

// ssoAccount resolves an assertion to the account it may sign in as, making one if this is the
// first time the person has arrived.
//
// The domain check comes first and applies to all three paths below. An IdP is authoritative
// for the domain its organisation proved and for nothing else, so an assertion carrying
// somebody@another-company.com is refused however well-formed it is.
func (b *Bot) ssoAccount(ctx context.Context, p *SSOProvider, id *ssoIdentity) (*User, error) {
	email := normalEmail(id.Email)
	if emailDomainOf(email) != p.Domain {
		return nil, fmt.Errorf("Single sign-on here is for addresses at %s, and your identity provider signed you in as %s.", p.Domain, email)
	}

	subject := ssoSubject(p.ProviderID, id.Subject)
	acct, err := b.store.UserByIdentity(ctx, ProviderSSO, subject)
	if err != nil {
		return nil, errors.New("Could not look up your account. Try again.")
	}
	if acct != nil {
		// A known SSO identity. The address may have changed at the IdP since last time, and
		// the IdP is authoritative for its own domain, so take its word for it.
		if !acct.EmailVerified && id.EmailVerified {
			b.store.SetEmailVerified(ctx, acct.ID, emailBySSO, 0)
			acct.EmailVerified = true
		}
		if m, err := b.store.Membership(ctx, acct.ID, p.OrgID); err == nil && m == nil {
			// Removed from the organisation but still in the directory. Re-provisioning them
			// would undo a removal an admin meant, so it is refused and said plainly.
			return nil, errors.New("Your account is no longer a member of this organisation. Ask an admin to invite you back.")
		}
		return acct, nil
	}

	// No SSO identity yet. An account may still exist — the founder who set this up signed up
	// with a password, and everybody who joined before SSO did too.
	existing, err := b.store.UserByEmail(ctx, email)
	if err != nil {
		return nil, errors.New("Could not look up your account. Try again.")
	}
	if existing != nil {
		// Adopting an existing account on the strength of an assertion is exactly what the
		// Slack path refuses, and for the same reason — except that here two things are true
		// that are not true there: the organisation has proved by DNS that it controls this
		// domain, and the address is inside it. Vouching for its own people at its own domain
		// is the whole of what an IdP is for. It still has to be one of its own people: an
		// account that is not a member of this organisation is not adopted.
		m, err := b.store.Membership(ctx, existing.ID, p.OrgID)
		if err != nil {
			return nil, errors.New("Could not look up your membership. Try again.")
		}
		if m == nil {
			return nil, errors.New("An account already uses that email address somewhere else. Sign in with your password, then ask an admin to add you here.")
		}
		if err := b.store.AddIdentity(ctx, existing.ID, ProviderSSO, subject); err != nil {
			return nil, errors.New("Could not connect your account to single sign-on. Ask an admin for help.")
		}
		if !existing.EmailVerified && id.EmailVerified {
			b.store.SetEmailVerified(ctx, existing.ID, emailBySSO, 0)
			existing.EmailVerified = true
		}
		slog.Info("sso identity linked to an existing account", "user", existing.ID, "org", p.OrgID)
		return existing, nil
	}

	// Nobody yet: the directory is the roster, so they are provisioned into the organisation
	// that owns the provider. As a viewer — the least the console has — because who should be
	// able to change settings is a decision for an admin on the Users tab, not for whoever
	// happens to be listed in a directory.
	u, err := b.store.CreateUser(ctx, email, id.Name, "") // no password: their IdP is their sign-in
	if err != nil {
		return nil, errors.New("Could not create your account. Try again.")
	}
	if err := b.store.AddIdentity(ctx, u.ID, ProviderSSO, subject); err != nil {
		return nil, errors.New("Could not connect your account to single sign-on. Try again.")
	}
	if err := b.store.AddMembership(ctx, u.ID, p.OrgID, RoleViewer, p.CreatedBy); err != nil {
		return nil, errors.New("Could not add you to the organisation. Ask an admin for help.")
	}
	if id.EmailVerified {
		// The IdP confirmed the mailbox at a domain its organisation proved it owns. Making
		// them click a link in that same mailbox to prove it again would strand a
		// just-provisioned user on step one of the setup walk.
		b.store.SetEmailVerified(ctx, u.ID, emailBySSO, 0)
		u.EmailVerified = true
	}
	slog.Info("sso sign-up", "user", u.ID, "org", p.OrgID, "domain", p.Domain)
	return u, nil
}

// ssoSubject is the identity an SSO sign-in proves: one directory account at one provider. The
// provider id is part of the key because `sub` is only unique within its issuer.
func ssoSubject(providerID, sub string) string { return providerID + ":" + sub }

// ---- domain verification ----

// ssoTXTName is the host the record goes on. Under a name of ours so that publishing it cannot
// be mistaken for, or collide with, any other vendor's proof.
func ssoTXTName(domain string) string { return "_attest-tag-verification." + domain }

var ssoResolver = &net.Resolver{}

// checkSSODomain looks for the TXT record. A miss is the ordinary case — DNS takes a while —
// so the caller words it as "not yet", not as a failure.
func checkSSODomain(ctx context.Context, domain, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	records, err := ssoResolver.LookupTXT(ctx, ssoTXTName(domain))
	if err != nil {
		return false
	}
	for _, rec := range records {
		if strings.TrimSpace(rec) == token {
			return true
		}
	}
	return false
}
