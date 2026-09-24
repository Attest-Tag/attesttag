package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sign in with Microsoft: a work or school account, through the app registration the Teams bot
// already has.
//
// What it proves is who somebody is in their Microsoft 365 organisation: the tenant (tid) and
// their object id in it (oid) — the same pair the bot knows them by in Teams, which is what lets
// the person signed in to the console and the person the bot talks to be one person, as a Slack
// sign-in does for Slack.
//
// What it does not prove is an email address. The address in a Microsoft token is one the
// person's own tenant admin can set to anything, including somebody else's mailbox — the account
// takeover that came to be called nOAuth — so an address from here never matches, links or
// verifies an account. The sign-in is looked up by (tid, oid) and by nothing else, and connecting
// it to an existing account is done from inside that account, the way a Slack sign-in is.

const (
	ProviderMicrosoft       = "microsoft"
	msLoginStateCookie      = "attest_ms_state"
	msSignInAnyOrganisation = "organizations"
	// Microsoft's consumer directory: a personal Microsoft account. Not a Teams organisation, and
	// the "organizations" authority keeps it out; refused by id as well, in case one is named.
	msConsumerTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"
)

// microsoftSubject is the identity a Microsoft sign-in proves. An object id is unique only inside
// its tenant, so the tenant is part of the key — the shape slackSubject has, for the same reason.
func microsoftSubject(tenantID, objectID string) string {
	return strings.ToLower(tenantID) + ":" + strings.ToLower(objectID)
}

func (b *Bot) microsoftLoginConfigured() bool {
	return b.cfg.MSTeamsSignIn != "" && b.slacks != nil && b.slacks.msteams != nil
}

func (b *Bot) msCallbackURL(r *http.Request) string {
	return b.baseURL(r) + "/api/auth/microsoft/callback"
}

// msAuthorityURL is an endpoint of the authority this deployment signs people in through.
func (b *Bot) msAuthorityURL(path string) string {
	return msLoginBase + "/" + url.PathEscape(b.cfg.MSTeamsSignIn) + "/oauth2/v2.0/" + path
}

// handleMicrosoftLogin starts the sign-in: a one-shot cookie holding the state, the PKCE verifier
// and the nonce, then off to Microsoft's account picker.
func (b *Bot) handleMicrosoftLogin(w http.ResponseWriter, r *http.Request) {
	if !b.microsoftLoginConfigured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Sign in with Microsoft is not configured: " +
			"set MSTEAMS_SIGNIN and register " + b.msCallbackURL(r) + " as a Web redirect URI on the bot's app registration."})
		return
	}
	if !b.startsOnPublicOrigin(w, r) {
		return
	}
	state, verifier, nonce := randomToken(), randomToken(), randomToken()
	tz := r.URL.Query().Get("tz")
	if !validTimezone(tz) {
		tz = ""
	}
	http.SetCookie(w, &http.Cookie{Name: msLoginStateCookie, Value: strings.Join([]string{state, verifier, nonce, tz}, "|"),
		Path: "/api/auth/", HttpOnly: true, MaxAge: 600, SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	q := url.Values{
		"client_id": {b.cfg.MSTeamsAppID}, "response_type": {"code"}, "response_mode": {"query"},
		"redirect_uri": {b.msCallbackURL(r)}, "scope": {"openid profile email"},
		"state": {state}, "nonce": {nonce}, "prompt": {"select_account"},
		"code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, b.msAuthorityURL("authorize")+"?"+q.Encode(), http.StatusFound)
}

func (b *Bot) handleMicrosoftCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, _ := r.Cookie(msLoginStateCookie)
	http.SetCookie(w, &http.Cookie{Name: msLoginStateCookie, Value: "", Path: "/api/auth/", MaxAge: -1}) // single use
	if c == nil || c.Value == "" {
		loginFailed(w, r, "That sign-in link expired or was already used. Try again.")
		return
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 4 || parts[0] == "" || q.Get("state") != parts[0] {
		loginFailed(w, r, "That sign-in link expired or was already used. Try again.")
		return
	}
	verifier, nonce, browserTZ := parts[1], parts[2], parts[3]
	if e := q.Get("error"); e != "" { // cancelled on Microsoft's screen, or refused by the tenant
		loginFailed(w, r, "Microsoft sign-in did not finish ("+e+").")
		return
	}
	code := q.Get("code")
	if code == "" {
		loginFailed(w, r, "Microsoft did not return a sign-in code. Try again.")
		return
	}
	ctx := r.Context()
	id, err := b.microsoftIdentity(ctx, code, nonce, verifier, b.msCallbackURL(r))
	if err != nil {
		slog.Warn("microsoft sign-in failed", "err", err)
		loginFailed(w, r, err.Error())
		return
	}
	subject := microsoftSubject(id.TenantID, id.ObjectID)
	acct, err := b.store.UserByIdentity(ctx, ProviderMicrosoft, subject)
	if err != nil {
		loginFailed(w, r, err.Error())
		return
	}

	// Somebody already signed in is connecting Microsoft to the account they are holding — the one
	// safe moment to join two ways of signing in, because they have just proved they hold both.
	if me := b.authenticate(r); me != nil && me.ID != 0 {
		if acct != nil && acct.ID != me.ID {
			loginFailed(w, r, "That Microsoft account is already connected to a different attest_tag account.")
			return
		}
		if acct == nil {
			// Attaching a new sign-in method is what makes a stolen session outlast a password
			// reset, so it needs a session the thief cannot have — one minted minutes ago.
			if !b.mayLinkIdentity(me) {
				b.audit(r, "auth.identity_link_refused", AuditEvent{ActorID: me.ID, ActorPublic: me.PublicID, Outcome: "refused", TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email})
				http.Redirect(w, r, "/admin/settings/?tab=security&relink=microsoft", http.StatusFound)
				return
			}
			if err := b.store.AddIdentity(ctx, me.ID, ProviderMicrosoft, subject); err != nil {
				loginFailed(w, r, err.Error())
				return
			}
			slog.Info("microsoft identity linked", "user", me.ID, "tenant", id.TenantID)
			b.audit(r, "auth.identity_linked", AuditEvent{ActorID: me.ID, ActorPublic: me.PublicID, TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email, Details: json.RawMessage(`{"provider":"microsoft"}`)})
		}
		http.Redirect(w, r, "/admin/settings/?tab=security", http.StatusFound)
		return
	}

	invite := ""
	if c, err := r.Cookie(inviteCookie); err == nil {
		invite = c.Value
	}
	joinScreen, joinAfterCode := false, false
	if acct == nil {
		acct, err = b.microsoftSignup(ctx, id, invite, browserTZ)
		if err != nil {
			slog.Warn("microsoft sign-up refused", "tenant", id.TenantID, "err", err)
			loginFailed(w, r, err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: "", Path: "/", MaxAge: -1})
	} else if invite != "" {
		// A known account opening an invitation joins that organisation too, on the terms the join
		// screen applies (acceptInvitation): addressed to this account, and for a domain-limited
		// link, an address proved the way the link asks — never by what a tenant put in the token —
		// and not before a second factor it owes is in.
		if joinScreen, joinAfterCode, err = b.joinOnSignIn(w, r, acct, invite); err != nil {
			loginFailed(w, r, "Could not verify account security. Try again.")
			return
		}
	}

	// An account in no organisation at all, holding an invitation it will redeem once its code is
	// in, is still asked for the code: that invitation is where it signs in to.
	orgID, err := b.orgForSignIn(ctx, acct, ProviderMicrosoft)
	if err != nil && !(joinAfterCode && b.inNoOrganisation(ctx, acct.ID)) {
		loginFailed(w, r, err.Error())
		return
	}
	e, err := b.store.TOTPEnrolment(ctx, acct.ID)
	if err != nil {
		loginFailed(w, r, "Could not verify account security. Try again.")
		return
	}
	if e != nil && e.Confirmed {
		tok, err := b.store.NewLoginChallenge(ctx, acct.ID, ProviderMicrosoft)
		if err != nil {
			loginFailed(w, r, "Could not start two-factor sign-in.")
			return
		}
		http.Redirect(w, r, "/admin/login/#challenge="+url.QueryEscape(tok), http.StatusFound)
		return
	}
	b.startSession(w, r, acct, orgID, ProviderMicrosoft)
	// The invitation this sign-in arrived holding was refused, and the join screen says why.
	if joinScreen {
		http.Redirect(w, r, b.signInNext(w, r), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// microsoftIdentity is who a Microsoft sign-in says signed in.
type microsoftIdentity struct {
	TenantID, ObjectID string
	Name, Email        string // Email is a claim, never a fact: see the top of this file
}

// microsoftIdentity redeems the code and reads the id_token. Its signature is not checked, on the
// ground OIDC Core §3.1.3.7 gives and the SSO path next door relies on: the token arrives over TLS
// in a direct, client-authenticated call to Microsoft's own token endpoint. What is checked is
// what that call cannot vouch for — that the token names this application, this browser's nonce
// and a tenant whose issuer it carries, and that it is not stale.
func (b *Bot) microsoftIdentity(ctx context.Context, code, nonce, verifier, redirectURI string) (*microsoftIdentity, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"code_verifier": {verifier}, "client_id": {b.cfg.MSTeamsAppID}, "client_secret": {b.cfg.MSTeamsAppPassword},
		"scope": {"openid profile email"},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", b.msAuthorityURL("token"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := b.slacks.msteams.http.Do(req)
	if err != nil {
		return nil, errors.New("Could not reach Microsoft to finish signing in. Try again.")
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken   string `json:"id_token"`
		Error     string `json:"error"`
		ErrorDesc string `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok)
	if tok.Error != "" || tok.IDToken == "" {
		slog.Warn("microsoft token exchange refused", "status", resp.StatusCode, "err", tok.Error, "desc", firstLine(tok.ErrorDesc))
		return nil, errors.New("Microsoft would not complete the sign-in. Try again, or ask whoever runs this deployment to check its app registration.")
	}
	var claims struct {
		Issuer            string `json:"iss"`
		Audience          any    `json:"aud"`
		Nonce             string `json:"nonce"`
		Expiry            int64  `json:"exp"`
		TenantID          string `json:"tid"`
		ObjectID          string `json:"oid"`
		Name              string `json:"name"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) != 3 || decodeJWTPart(parts[1], &claims) != nil {
		return nil, errors.New("Microsoft returned a token we could not read.")
	}
	tid, oid := strings.ToLower(claims.TenantID), strings.ToLower(claims.ObjectID)
	switch {
	case !isEntraObjectID(tid) || !isEntraObjectID(oid):
		return nil, errors.New("Microsoft did not say which organisation and account signed in.")
	case tid == msConsumerTenant:
		return nil, errors.New("That is a personal Microsoft account. Sign in with a work or school account.")
	case b.cfg.MSTeamsSignIn != msSignInAnyOrganisation && tid != b.cfg.MSTeamsSignIn:
		return nil, errors.New("This deployment accepts Microsoft accounts from one organisation only, and that is not it.")
	case !strings.EqualFold(strings.TrimRight(claims.Issuer, "/"), msLoginBase+"/"+tid+"/v2.0"):
		return nil, errors.New("That sign-in was not issued by the organisation it names.")
	case !(oidcClaims{Audience: claims.Audience}).audienceIs(b.cfg.MSTeamsAppID):
		return nil, errors.New("That sign-in was issued for a different application.")
	case claims.Nonce != nonce:
		return nil, errors.New("That sign-in could not be matched to this browser. Try again.")
	case claims.Expiry > 0 && time.Now().After(time.Unix(claims.Expiry, 0).Add(2*time.Minute)):
		return nil, errors.New("That sign-in took too long. Try again.")
	}
	email := strings.TrimSpace(claims.Email)
	if email == "" && strings.Contains(claims.PreferredUsername, "@") {
		email = strings.TrimSpace(claims.PreferredUsername)
	}
	return &microsoftIdentity{TenantID: tid, ObjectID: oid, Name: strings.TrimSpace(claims.Name), Email: email}, nil
}

// microsoftSignup makes the account for a Microsoft identity seen for the first time. It joins an
// organisation only by an invitation addressed to it, or by founding one, and its address starts
// unconfirmed whatever Microsoft said about it.
func (b *Bot) microsoftSignup(ctx context.Context, id *microsoftIdentity, invite, browserTZ string) (*User, error) {
	email := normalEmail(id.Email)
	if email == "" {
		return nil, errors.New("Microsoft did not give us an email address for your account, so we cannot create one. Sign up with an email and password instead.")
	}
	// An address that already has an account cannot be claimed this way. Connecting the two is
	// done from inside that account, in Settings, where the person has proved they hold it.
	if existing, _ := b.store.UserByEmail(ctx, email); existing != nil {
		return nil, errors.New("An account already uses that email address. Sign in the way you usually do, then connect Microsoft from Settings.")
	}
	var token *EmailToken
	if invite != "" {
		t, err := b.store.PeekEmailToken(ctx, TokenInvite, invite)
		if err != nil {
			return nil, err
		}
		if !t.AcceptsEmail(email) {
			return nil, errors.New(inviteRefusal(t))
		}
		// A domain link admits a proven address in its domain. A Microsoft profile address is
		// asserted by whichever tenant signed in — one the redeemer may control — so it is not
		// that proof. An addressed invitation to the mailbox is; ask for one.
		if t.domainLink() {
			return nil, errors.New("This link is limited to the " + t.Domain + " domain. Ask for an invitation addressed to you, or sign up with your email address and confirm it.")
		}
		token = t
	} else if !b.signupAllowed(ctx) {
		return nil, errors.New(signupRefusal())
	}
	u, err := b.store.CreateUser(ctx, email, id.Name, "") // no password: Microsoft is their sign-in
	if err != nil {
		return nil, err
	}
	if err := b.store.AddIdentity(ctx, u.ID, ProviderMicrosoft, microsoftSubject(id.TenantID, id.ObjectID)); err != nil {
		return nil, err
	}
	if token != nil {
		if _, err := b.store.TakeEmailToken(ctx, TokenInvite, invite); err != nil {
			return nil, err
		}
		if err := b.store.AddMembership(ctx, u.ID, token.OrgID, token.Role, token.CreatedBy); err != nil {
			return nil, err
		}
		if token.Email != "" && normalEmail(token.Email) == email {
			b.store.SetEmailVerified(ctx, u.ID, emailByInvite, token.OrgID) // the invitation reached this mailbox
		}
	} else {
		// Founding an organisation. A Microsoft token carries no organisation name, so it is named
		// for the address's domain until somebody renames it.
		org, err := b.store.CreateOrg(ctx, nonEmpty(emailDomainOf(email), email), u.ID)
		if err != nil {
			return nil, err
		}
		b.seedTimezone(ctx, org.ID, "", browserTZ)
		b.seedEmailDomain(ctx, org.ID, email)
	}
	slog.Info("microsoft sign-up", "user", u.ID, "email", email, "tenant", id.TenantID, "invited", token != nil)
	return u, nil
}
