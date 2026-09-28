package app

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Who may open the console, and what they see once they are in.
//
// Slack is the identity provider: a console user is a Slack user id. Workspace admins and owners
// sign themselves in — they can already read every channel the bot is in, so an invite for them
// would be ceremony. Everybody else needs one, and an accepted invite is the membership.

const (
	inviteCookie = "attest_invite"
	csrfCookie   = "attest_csrf"
	csrfHeader   = "X-CSRF-Token"
)

// permissionsFor resolves what this session may do, per request rather than at sign-in, so moving
// somebody between tiers takes effect on their next click instead of their next login.
// permissionsFor resolves what this session may do, per request rather than at sign-in, so moving
// somebody between tiers takes effect on their next click instead of their next login.
//
// Authority comes from the membership joining this user to the organisation on their session,
// and from nothing else: not an email domain, not a Slack workspace-admin flag, not an
// environment variable. No membership means no permissions at all — a refusal, not a tier to
// fall back to.
// orgOf is the organisation this request is acting in, taken from the session. It is the one
// place a handler asks, so "which organisation is this" has a single answer per request.
func orgOf(r *http.Request) int64 {
	if u := adminFromCtx(r.Context()); u != nil {
		return u.OrgID
	}
	return 0
}

func (b *Bot) permissionsFor(ctx context.Context, u *AdminUser) {
	if u == nil {
		return
	}
	u.Role, u.Permissions = "", map[Permission]bool{}
	if u.ID == 0 || u.OrgID == 0 {
		return
	}
	m, err := b.store.Membership(ctx, u.ID, u.OrgID)
	if err != nil || m == nil {
		return
	}
	u.Role = m.Role
	u.Permissions = permissionsForRole(m.Role, b.store.CustomRoleMap(ctx, u.OrgID))
}

// requirePerm gates one route on one permission. It is deliberately a wrapper rather than a check
// inside each handler: a permission that has to be remembered at the top of a function is one
// that eventually is not.
func (b *Bot) requirePerm(perm Permission, next http.HandlerFunc) http.HandlerFunc {
	return b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		u := adminFromCtx(r.Context())
		if u == nil || !u.Permissions[perm] {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": denialCopy(perm)})
			return
		}
		if why := b.needsVerifiedEmail(r.Context(), u, perm); why != "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": why, "verify_email": true})
			return
		}
		next(w, r)
	})
}

// needsVerifiedEmail is the one thing signup alone does not grant. Founding an organisation is
// open; reaching out from it is not: inviting people and minting an API key wait until the
// address behind the account has been proved — an invitation lands in a stranger's inbox, and a
// key lets a script spend in this organisation's name with nobody signed in.
//
// Connecting a workspace, and storing a credential, are deliberately not on the list. Neither
// reaches anybody else, and the install already asks for a stronger proof than a mail link: the
// person pressing Allow has to be an admin or owner of the workspace they are connecting, and
// what an organisation can spend is capped whoever founded it. Gating it only ever stopped
// somebody arriving from the Marketplace between Add to Slack and the consent screen, to go and
// find a mail. The setup page still asks them to confirm — an address nobody proved is an
// account nobody can reset the password for — it just does not hold the install on it.
//
// It only applies where verification mail can actually be sent — a deployment with no mailer
// cannot verify anyone, and locking every founder out would be the wrong way to say so.
func (b *Bot) needsVerifiedEmail(ctx context.Context, u *AdminUser, perm Permission) string {
	switch perm {
	case PermUsersManage, PermAPIKeysManage:
	default:
		return ""
	}
	if b.emailUnverified(ctx, u) {
		return "Confirm your email address first: the link is in your inbox, and Settings → General can send it again."
	}
	return ""
}

// emailUnverified says whether the address behind this account is still unproved, where proving
// it is possible at all: with no mailer configured nobody can be verified, so nobody is asked.
func (b *Bot) emailUnverified(ctx context.Context, u *AdminUser) bool {
	if b.mail == nil || !b.mail.Configured() {
		return false
	}
	acct, _ := b.store.User(ctx, u.ID)
	return acct != nil && !acct.EmailVerified
}

// denialCopy is product copy, not an enumeration of role names: it says what the person needs, in
// the vocabulary of the screen they are looking at.
func denialCopy(p Permission) string {
	switch p {
	case PermUsersManage:
		return "You need permission to manage console users for that."
	case PermRolesManage:
		return "You need permission to manage roles for that."
	case PermSettingsManage:
		return "Only someone who can change workspace settings can do that."
	case PermConnManage:
		return "Changing credentials needs the connections permission."
	case PermBundlesManage:
		return "You need permission to manage access bundles for that."
	case PermScopesManage:
		return "You need permission to manage channel settings for that."
	case PermApproversManage:
		return "You need permission to manage approval tiers for that."
	case PermAccessClose:
		return "You need permission to close access requests."
	case PermAuditView:
		return "You need permission to read the audit log for that."
	case PermAPIKeysManage:
		return "You need permission to manage API keys for that."
	case PermBillingManage:
		return "Only someone who can manage billing can do that."
	}
	return "You don't have permission to do that."
}

// ---- invites ----

// ---- CSRF ----
//
// The console's state-changing routes are cookie-authenticated JSON endpoints, and SameSite=Lax
// does not stop a cross-site POST that arrives as a top-level navigation. That was survivable
// while only workspace admins had sessions; once somebody can be invited, a forged request is
// worth more, and several of these routes spend real credentials.
//
// Double submit: a readable cookie the page echoes back in a header. An attacker on another
// origin can cause the cookie to be sent but cannot read it to set the header.

func (b *Bot) setCSRFCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: tok, Path: "/", HttpOnly: false,
		MaxAge: int(sessionTTL.Seconds()), SameSite: http.SameSiteLaxMode,
		Secure: b.secureCookies(r)})
}

// checkCSRF reports whether a state-changing request may proceed. Bearer-authenticated calls are
// exempt: they carry no cookie, so nothing can be ridden.
func checkCSRF(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return true
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	sent := r.Header.Get(csrfHeader)
	return sent != "" && subtle.ConstantTimeCompare([]byte(sent), []byte(c.Value)) == 1
}
