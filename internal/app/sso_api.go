package app

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Settings → Security → Single sign-on, from the console's side.
//
// Four routes and one shape: read what is registered, register one, prove the domain, remove
// it. Everything that decides who gets in lives in auth_sso.go; this file is the form around it.

// ssoView is what the console renders: the row, plus the two things an admin has to copy into
// their IdP and their DNS. Never the client secret — it goes in and is not read back out.
type ssoView struct {
	Provider    *SSOProvider `json:"provider"`
	RedirectURI string       `json:"redirect_uri,omitempty"`
	RecordName  string       `json:"record_name,omitempty"`
	RecordValue string       `json:"record_value,omitempty"`
}

func (b *Bot) handleSSOGet(w http.ResponseWriter, r *http.Request) {
	p, err := b.store.SSOProviderForOrg(r.Context(), orgOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	if p == nil {
		writeJSON(w, http.StatusOK, ssoView{})
		return
	}
	writeJSON(w, http.StatusOK, ssoView{
		Provider: p, RedirectURI: b.ssoCallbackURL(r, p.ProviderID),
		RecordName: ssoTXTName(p.Domain), RecordValue: p.DomainToken,
	})
}

func (b *Bot) handleSSORegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind         string `json:"kind"`
		Domain       string `json:"domain"`
		Issuer       string `json:"issuer"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	ctx := r.Context()
	orgID := orgOf(r)

	if k := strings.ToLower(strings.TrimSpace(in.Kind)); k != "" && k != "oidc" {
		bad(w, errors.New("Only OpenID Connect is supported at the moment. Most identity providers — Okta, Entra, Google Workspace, Auth0, JumpCloud — offer it alongside SAML."))
		return
	}
	domain := ssoNormalDomain(in.Domain)
	if !emailDomainRe.MatchString(domain) {
		bad(w, errors.New("That does not look like a domain. Use the bare form, like example.com."))
		return
	}
	issuer := strings.TrimRight(strings.TrimSpace(in.Issuer), "/")
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		bad(w, errors.New("The issuer has to be an https:// URL, like https://your-org.okta.com."))
		return
	}
	clientID, clientSecret := strings.TrimSpace(in.ClientID), strings.TrimSpace(in.ClientSecret)
	if clientID == "" || clientSecret == "" {
		bad(w, errors.New("The client ID and client secret both come from the application you created in your identity provider."))
		return
	}
	if existing, _ := b.store.SSOProviderForOrg(ctx, orgID); existing != nil {
		bad(w, errors.New("This organisation already has an identity provider. Remove it before registering another."))
		return
	}
	// Discovery before the row is written, so a typed issuer fails here — where the form is
	// still open and the admin can see the field — rather than at the first sign-in attempt.
	if _, err := oidcDiscover(ctx, issuer); err != nil {
		bad(w, err)
		return
	}

	// Derived rather than typed. The provider id goes in our own callback URL and is unique
	// across every organisation, so letting an admin choose it invites both a collision and a
	// URL with a space in it. The organisation's public id, never the serial (store_identity.go).
	org, err := b.store.Org(ctx, orgID)
	if err != nil || org == nil {
		fail(w, err)
		return
	}
	p := &SSOProvider{
		OrgID: orgID, ProviderID: "org-" + org.PublicID, Kind: "oidc", Domain: domain,
		Issuer: issuer, ClientID: clientID, DomainToken: randomToken(),
	}
	if u := adminFromCtx(ctx); u != nil {
		p.CreatedBy = u.ID
	}
	if err := b.store.CreateSSOProvider(ctx, b.sealer, p, clientSecret); err != nil {
		bad(w, err)
		return
	}
	b.handleSSOGet(w, r)
}

// handleSSOVerify looks for the TXT record and switches sign-in on when it finds it.
func (b *Bot) handleSSOVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := b.store.SSOProviderForOrg(ctx, orgOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	if p == nil {
		bad(w, errors.New("Register an identity provider first."))
		return
	}
	if p.DomainVerified {
		b.handleSSOGet(w, r)
		return
	}
	if !checkSSODomain(ctx, p.Domain, p.DomainToken) {
		writeJSON(w, http.StatusOK, map[string]any{
			"verified": false,
			"error": "We could not find that TXT record on " + p.Domain + " yet. DNS can take a while to " +
				"spread — add the record and try again in a few minutes.",
		})
		return
	}
	if err := b.store.MarkSSODomainVerified(ctx, p.OrgID); err != nil {
		bad(w, err)
		return
	}
	b.handleSSOGet(w, r)
}

// handleSSODelete stops new sign-ins through the IdP. It offboards nobody: everyone provisioned
// through it keeps their account and their membership, which is a distinction worth stating
// because "remove SSO" reads like it might do both.
func (b *Bot) handleSSODelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := orgOf(r)
	// The same rule the other two sign-in settings follow (selfLockout, account.go): you may
	// only remove the way in that you are not relying on. An organisation on the SSO-only
	// policy that deletes its provider has nothing left to sign in with.
	if b.settings.Get(ctx, orgID).AuthPolicy == AuthPolicySSO {
		bad(w, errors.New("This organisation signs in with single sign-on only. Change Sign-in methods first, or removing this would leave nobody a way in."))
		return
	}
	if err := b.store.DeleteSSOProvider(ctx, orgID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ssoView{})
}

// ssoNormalDomain takes what was pasted and leaves the bare domain: a scheme, a path, a leading
// @ and stray case are all things people paste, and none of them change what was meant.
func ssoNormalDomain(raw string) string {
	d := strings.ToLower(strings.TrimSpace(raw))
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	d = strings.TrimPrefix(d, "@")
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if i := strings.LastIndex(d, "@"); i >= 0 {
		d = d[i+1:]
	}
	return strings.TrimSuffix(d, ".")
}
