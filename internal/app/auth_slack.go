package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Sign in with Slack (OpenID Connect). The other way in is an email and a password, which lives
// in auth_password.go; both mint the same session.
//
// What a Slack sign-in proves is control of one Slack account in one workspace, and that is all
// it is ever used for. It is not matched to an existing account by email address: the address in
// Slack's OIDC response is set by the workspace's own admin (and by its SAML IdP), so adopting an
// account because the addresses agree would let a workspace admin sign in as anybody. Connecting
// Slack to an account you already hold is done from inside that account, in Settings.
//
// There is no environment credential. Authority is a membership joining a person to an
// organisation, and nothing set on the process outranks it.
//
// Sessions are opaque tokens in admin_sessions, sent as an HttpOnly cookie.

const (
	sessionCookie = "attest_admin"
	stateCookie   = "attest_oidc_state"
	sessionTTL    = 7 * 24 * time.Hour
)

// slackOIDC is Slack's OpenID Connect issuer. Tests point it at an httptest server.
var slackOIDC = "https://slack.com"

var oidcClient = &http.Client{Timeout: 15 * time.Second}

type ctxKey int

const userKey ctxKey = 1

func adminFromCtx(ctx context.Context) *AdminUser {
	u, _ := ctx.Value(userKey).(*AdminUser)
	return u
}

// baseURL is the origin for links in this response: ADMIN_BASE_URL when set, else the one
// the request arrived on. The sign-in redirect_uri derives from it, so the console works on
// whatever host it is reached through without configuration (public_origin.go).
func (b *Bot) baseURL(r *http.Request) string {
	if v := os.Getenv("ADMIN_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if u := publicBaseURL(r.Context(), b.store, b.cfg); u != "" {
		return u
	}
	// Nothing configured, nothing learned and no listen address to fall back on. A deployment
	// reached over loopback lands here, because a loopback origin is deliberately never learned
	// (public_origin.go) — and an empty base leaves a relative redirect_uri, which is not a
	// redirect_uri at all. The request in hand is the only thing that still knows where the
	// console is being reached, and it shapes the links in this one response and nothing else:
	// a forged Host buys its sender a broken sign-in, not a foothold in anyone else's.
	return requestOrigin(r)
}

// callbackURL is the redirect URL that must be registered on the Slack app.
func (b *Bot) callbackURL(r *http.Request) string { return b.baseURL(r) + "/api/auth/callback" }

// startsOnPublicOrigin keeps a sign-in on the address it will come back to. Slack and Microsoft
// send the browser back to baseURL, and the one-shot state cookie belongs to whichever host set
// it: a sign-in started anywhere else — the loopback address of a machine that also has a public
// one, a Cloud Run URL beside the custom domain — came back to a host without that cookie and
// failed as "That sign-in link expired or was already used", every time, however quickly it was
// retried. So a start on another address is sent to the same path on the public origin first,
// and the cookie is set there. It reports whether the caller should carry on.
//
// Only when the public origin is pinned or learned: the local fallback is this machine's own
// listen address, which is not where anybody else's browser should be sent. And only once —
// bounced marks the hop, so a proxy that rewrites Host cannot turn it into a loop.
func (b *Bot) startsOnPublicOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("bounced") != "" || !b.publicOriginKnown(r) {
		return true
	}
	base := b.baseURL(r)
	if sameOriginOf(base, requestOrigin(r)) {
		return true
	}
	q := r.URL.Query()
	q.Set("bounced", "1")
	http.Redirect(w, r, base+r.URL.Path+"?"+q.Encode(), http.StatusFound)
	return false
}

// publicOriginKnown reports whether baseURL names an address this deployment was told or taught,
// rather than falling back to its listen address or to the request.
func (b *Bot) publicOriginKnown(r *http.Request) bool {
	return os.Getenv("ADMIN_BASE_URL") != "" || b.store.PublicOrigin(r.Context()) != ""
}

// sameOriginOf is sameOrigin (outbound.go) for two origins written out: scheme, host and port, a
// default port written or left off meaning the same thing.
func sameOriginOf(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && sameOrigin(ua, ub)
}

func slackLoginConfigured() bool {
	return os.Getenv("SLACK_CLIENT_ID") != "" && os.Getenv("SLACK_CLIENT_SECRET") != ""
}

func parseEmailDomains(s string) []string {
	var out []string
	for _, d := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' }) {
		d = strings.ToLower(strings.TrimPrefix(d, "@"))
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// sameSiteOnly refuses a state-changing request a browser marks as coming from another site. The
// pre-authentication endpoints — sign-in, sign-up, sign-out, the reset and verification posts —
// establish or clear a session and so sit outside the CSRF double-submit that requireAdmin applies
// to authenticated writes; without this a page on another origin could auto-submit a form that
// logs the visitor into the attacker's account (or forces a sign-out). A browser states the
// request's provenance in Sec-Fetch-Site, and older ones in Origin; a non-browser caller sends
// neither and is left alone, so the JSON API and the tests are unaffected.
func sameSiteOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "none":
			// the browser vouches the request began on this origin, or from the user themselves
		case "":
			// no Fetch Metadata: fall back to Origin when the browser sent one
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "cross-site request refused", http.StatusForbidden)
					return
				}
			}
		default: // cross-site, same-site
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// requireAdmin wraps API handlers.
func (b *Bot) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := b.authenticate(r); u != nil {
			b.store.LearnOrigin(r.Context(), requestOrigin(r))
			// Checked here rather than per route: a cookie-authenticated write that skipped this
			// is exactly the one somebody would forget to wrap.
			if !checkCSRF(r) {
				// A session from before the token existed has no cookie yet. Mint one on the way
				// out, so the retry the message asks for (and the client's automatic one) succeeds.
				b.ensureCSRFCookie(w, r)
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "This request is missing its CSRF token. Reload the console and try again."})
				return
			}
			// Two checks that outrank every permission, in the one place every authenticated
			// route passes through. Both are re-read per request rather than trusted from the
			// session: a policy that took effect only at sign-in would leave exactly the people
			// it was aimed at still holding a live console.
			//
			// Everything under /api/auth/ and /api/account is let through regardless — those are
			// how somebody fixes the thing they are being held on (enrol, connect Slack, set a
			// password, sign out). Refusing them would be a locked door with the key inside.
			if !strings.HasPrefix(r.URL.Path, "/api/auth/") && r.URL.Path != "/api/account" && !strings.HasPrefix(r.URL.Path, "/api/account/") {
				ctx := r.Context()
				if !b.settings.Get(ctx, u.OrgID).allowsVia(u.Via) {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": "This organisation has changed which sign-in methods it accepts. Sign out and sign in again.", "reauth": true})
					return
				}
				if b.twoFactorOwed(ctx, u) {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": "This organisation requires two-factor authentication. Set it up to carry on.", "enrol_two_factor": true})
					return
				}
			}
			// Every write that gets past this point is recorded (audit.go): as the named event
			// the handler writes, or as a plain row naming the route when it writes none.
			b.auditedWrites(next)(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required", "login": "/api/auth/login"})
	}
}

// authenticate resolves the request to a person acting in an organisation. There is exactly one
// way to be authenticated — a session in admin_sessions — reachable either as a bearer token
// (curl, tests) or as the browser's cookie. No environment variable grants access: a static
// credential that outranks every membership is the thing a multi-tenant console must not have.
func (b *Bot) authenticate(r *http.Request) *AdminUser {
	tok := sessionTokenOf(r)
	if tok == "" {
		return nil
	}
	u, _ := b.store.AdminSession(r.Context(), tok)
	if u == nil {
		return nil
	}
	enrol, err := b.store.TOTPEnrolment(r.Context(), u.ID)
	if err != nil || (enrol != nil && enrol.Confirmed && !u.MFAVerified) {
		return nil
	}
	// The session says which organisation it is acting in; membership is what says it still may.
	// Removing somebody used to leave their cookie working: permissionsFor would hand them an
	// empty permission set, which closes every requirePerm route but none of the requireAdmin
	// ones — so an ex-member kept reading the documents, bundles, scopes and memories of the
	// organisation that removed them until the session expired a week later.
	//
	// A session that names no organisation is not a session. Org 0 is the deployment's own
	// settings namespace, and the only rows that could carry it are ones from before there were
	// organisations — so nothing may act there on a cookie.
	if u.OrgID == 0 {
		return nil
	}
	if m, err := b.store.Membership(r.Context(), u.ID, u.OrgID); err != nil || m == nil {
		return nil
	}
	b.permissionsFor(r.Context(), u)
	return u
}

// sessionTokenOf is the credential a request carries: a bearer token (curl, tests) or the
// browser's cookie, in that order. Every read of the session token goes through here so the
// bearer form and the cookie form cannot drift.
func sessionTokenOf(r *http.Request) string {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimPrefix(v, "Bearer ")
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// secureCookies decides the Secure flag once, from where the console is reachable rather than
// from the headers of the request in hand. Deriving it per request meant one plain-http request
// — a direct hit on the container, a second ingress, a load balancer that forgot the forwarded
// proto — re-issued the session cookie without the flag, after which the browser would send it
// in the clear. Only a loopback origin (a laptop run) gets a cleartext cookie; INSECURE_COOKIES=1
// forces it for a LAN test and nothing else.
func (b *Bot) secureCookies(r *http.Request) bool {
	if os.Getenv("INSECURE_COOKIES") == "1" {
		return false
	}
	if managedDeployment() {
		return true
	}
	u, err := url.Parse(b.baseURL(r))
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1"))
}

// handleMe tells the console who is signed in and which sign-in methods exist.
func (b *Bot) handleMe(w http.ResponseWriter, r *http.Request) {
	u := b.authenticate(r)
	if u != nil {
		b.ensureCSRFCookie(w, r) // every console load calls this, so an old session heals here
	}
	out := map[string]any{
		"user": u, "signed_in": u != nil,
		"slack_login":    slackLoginConfigured(),
		"password_login": true, // email and password is always available; there is no env account
		"signup_open":    b.signupAllowed(r.Context()),
		// Whether this deployment can connect Microsoft Teams at all. The console has no build-time
		// settings, so what the deployment is configured for has to arrive on this response.
		"msteams": b.slacks != nil && b.slacks.msteams != nil,
		// And whether it offers Sign in with Microsoft, which needs MSTEAMS_SIGNIN on top of that.
		"microsoft_login": b.microsoftLoginConfigured(),
		// Where this deployment's public pages live, if it has any: the Get started page reads
		// its walkthrough library from there. The server says it rather than the console baking
		// it in at build time, and says it through the same normaliser secureHeaders uses for
		// connect-src — so the origin the page asks and the origin the browser permits are one
		// value and cannot drift. Empty on a deployment that set no SITE_URL, and the page then
		// says where the guides live instead of fetching them from a domain it does not own.
		"site_url": siteOrigin(b.cfg.SiteURL),
		// Set while a browser has an install parked behind the sign-in it is looking at, so the
		// form can say where signing in leads. The cookie is HttpOnly; this is how the page learns.
		"install_pending": u == nil && installPending(r),
	}
	if u != nil {
		// The organisations this person can switch to, so the console can offer the switcher
		// without a second round trip on every load.
		if ms, err := b.store.MembershipsFor(r.Context(), u.ID); err == nil {
			out["orgs"] = ms
		}
		set := b.settings.Get(r.Context(), u.OrgID)
		e, _ := b.store.TOTPEnrolment(r.Context(), u.ID)
		out["two_factor"] = map[string]any{
			"enabled":  e != nil && e.Confirmed,
			"required": set.RequireTwoFactor,
			// The console is held at the door until this is false: it is the same question
			// requireAdmin answers on every other route, asked once so the shell can show the
			// enrolment screen rather than a wall of failed requests.
			"owed": b.twoFactorOwed(r.Context(), u),
		}
		out["auth_policy"] = set.AuthPolicy
		// What the account may spend this month and what it has. The console draws a banner
		// when the bot has stopped for the month, and that has to be true on every page rather
		// than only the one with the number on it, so it rides along with the session the shell
		// already asks for on each load.
		spend, _ := budgetSpend(r.Context(), b.store, u.OrgID, set)
		plan := map[string]any{
			"plan": set.Plan, "budget_usd": set.EffectiveBudget(), "spend_usd": spend,
			"requested_at":    b.store.Setting(r.Context(), u.OrgID, planRequestKey),
			"support_email":   b.cfg.SupportEmail,
			"billing_enabled": b.cfg.BillingEnabled(),
			// Credit is not what an organisation on its own key spends, so the console does not
			// draw it as if it were.
			"credit_enabled": (set.CreditEnforced || set.AllowanceActive) && !set.OwnKey.Active(),
			"paused_by":      pausedBy(set, false, 0, spend),
			"own_model_key":  set.OwnKey.Active(),
		}
		// The balance only when it means something, and read live rather than off the cached
		// settings: it moves every turn. paused_by is computed here, once, rather than left to
		// the console to derive from two numbers — that would be a second copy of the rule the
		// bot actually applies, and the two would drift.
		if set.CreditEnforced || set.AllowanceActive {
			spendable, metered, _ := b.store.SpendableCredit(r.Context(), u.OrgID)
			plan["credit_balance_usd"] = microsToUSD(spendable)
			plan["paused_by"] = pausedBy(set, metered, spendable, spend)
		}
		// warn_by is paused_by's earlier sibling: the limit this account is about to reach while
		// there is still something to do about it. Computed here for the same reason paused_by is
		// — the console must not derive it — and only where plans are sold, because a self-host
		// with no billing has neither a credit balance nor a size to be near the end of.
		//
		// Both reads are on the account's own row and its own indexed count, and /api/me is the
		// one endpoint every console page already waits for, so the banner costs no extra
		// round trip on the page that is most likely to be the one somebody is looking at.
		if b.cfg.BillingEnabled() {
			acct, err := b.store.BillingAccountOf(r.Context(), u.OrgID)
			if err == nil {
				active := b.activeUsers(r.Context(), u.OrgID, false)
				plan["warn_by"] = warnedBy(set, acct, b.cfg, active.Users, active.Limit)
				plan["users_active"] = active.Users
				plan["users_limit"] = active.Limit
				plan["credit_allowance_usd"] = microsToUSD(acct.SpendableAllowanceMicros())
				plan["credit_month_allowance_usd"] = microsToUSD(acct.AllowanceGrantedMicros)
			}
		}
		out["plan"] = plan
		// The organisation's own model key, as far as every page needs it: whether there is one,
		// and whether the bot can answer on it. The banner that says it cannot is on every page,
		// for the same reason the budget one is. Where it points is for settings.manage, on its
		// own route.
		if k := set.OwnKey; k.Present || k.Unknown {
			mk := map[string]any{"own": k.Active(), "failing": k.Ref.Failing() && k.Active(), "blocked": k.Blocked()}
			if err := k.refusal(); err != nil {
				mk["refusal"] = err.Error()
			}
			out["model_key"] = mk
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ensureCSRFCookie gives a cookie-authenticated session the double-submit cookie when it has
// none: a session created before the token existed, or a browser that dropped it. The token is
// not bound to the session, so minting a fresh one is as good as the one login would have set.
// Bearer calls carry no session cookie and need no token.
func (b *Bot) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(sessionCookie); err != nil {
		return
	}
	if c, err := r.Cookie(csrfCookie); err == nil && c.Value != "" {
		return
	}
	b.setCSRFCookie(w, r, randomToken())
}

func (b *Bot) setSessionCookie(w http.ResponseWriter, r *http.Request, tok string) {
	b.store.LearnOrigin(r.Context(), requestOrigin(r)) // a fresh sign-in: this is the console's origin
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, MaxAge: int(sessionTTL.Seconds()),
		SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
}

// handleLogin starts Sign in with Slack: a one-shot state cookie, then off to Slack's consent
// screen pinned to our workspace (team=) so members of other workspaces are refused early.
func (b *Bot) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !slackLoginConfigured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Sign in with Slack is not configured: set SLACK_CLIENT_ID and SLACK_CLIENT_SECRET and register " + b.callbackURL(r) + " as a redirect URL on the Slack app, or use the password login."})
		return
	}
	if !b.startsOnPublicOrigin(w, r) {
		return
	}
	state := randomToken()
	// The browser's zone rides along in the state so the callback can stamp a newly founded
	// organisation with it. Slack echoes the state back unchanged and it is still compared
	// whole, so carrying a passenger costs the check nothing; it holds no secret either way.
	if tz := r.URL.Query().Get("tz"); validTimezone(tz) {
		state += "|" + tz
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/api/auth/", HttpOnly: true, MaxAge: 600,
		SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	q := url.Values{
		"response_type": {"code"}, "scope": {"openid profile email"}, "client_id": {os.Getenv("SLACK_CLIENT_ID")},
		"redirect_uri": {b.callbackURL(r)}, "state": {state},
	}
	http.Redirect(w, r, slackOIDC+"/openid/connect/authorize?"+q.Encode(), http.StatusFound)
}

// loginFailed sends the browser back to the login card, which shows the reason.
func loginFailed(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin/?auth_error="+url.QueryEscape(msg), http.StatusFound)
}

func (b *Bot) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st, _ := r.Cookie(stateCookie)
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/api/auth/", MaxAge: -1}) // single use
	if st == nil || st.Value == "" || q.Get("state") != st.Value {
		loginFailed(w, r, "That sign-in link expired or was already used. Try again.")
		return
	}
	if e := q.Get("error"); e != "" { // the user pressed Cancel on Slack's consent screen
		loginFailed(w, r, "Slack sign-in was cancelled ("+e+").")
		return
	}
	code := q.Get("code")
	if code == "" {
		loginFailed(w, r, "Slack did not return a sign-in code. Try again.")
		return
	}
	id, err := b.slackIdentity(r.Context(), code, b.callbackURL(r))
	if err != nil {
		slog.Warn("slack sign-in failed", "err", err)
		loginFailed(w, r, err.Error())
		return
	}
	ctx := r.Context()
	subject := slackSubject(id.TeamID, id.UserID)

	// A Slack sign-in is looked up by the Slack identity and by nothing else. It is NEVER
	// matched to an existing account by email: the address in Slack's OIDC response is set by
	// the workspace's own admin (and by its SAML IdP), so adopting an account because the
	// addresses agree would let any workspace admin mint an assertion for anybody's mailbox.
	acct, err := b.store.UserByIdentity(ctx, ProviderSlack, subject)
	if err != nil {
		loginFailed(w, r, err.Error())
		return
	}

	// Somebody already signed in is connecting Slack to the account they are holding — the only
	// safe moment to link two sign-in methods, because they have proved they hold both.
	if me := b.authenticate(r); me != nil && me.ID != 0 {
		if acct != nil && acct.ID != me.ID {
			loginFailed(w, r, "That Slack account is already connected to a different attest_tag account.")
			return
		}
		if acct == nil {
			// Attaching a new sign-in method is what makes a stolen session outlast a password
			// reset, so it needs a session the thief cannot have — one minted minutes ago.
			if !b.mayLinkIdentity(me) {
				b.audit(r, "auth.identity_link_refused", AuditEvent{ActorID: me.ID, ActorPublic: me.PublicID, Outcome: "refused", TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email})
				http.Redirect(w, r, "/admin/settings/?tab=security&relink=slack", http.StatusFound)
				return
			}
			if err := b.store.AddIdentity(ctx, me.ID, ProviderSlack, subject); err != nil {
				loginFailed(w, r, err.Error())
				return
			}
			slog.Info("slack identity linked", "user", me.ID, "team", id.TeamID)
			b.audit(r, "auth.identity_linked", AuditEvent{ActorID: me.ID, ActorPublic: me.PublicID, TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email, Details: json.RawMessage(`{"provider":"slack"}`)})
			if acct, _ := b.store.User(ctx, me.ID); acct != nil {
				b.trustSlackEmail(ctx, acct, id)
			}
		}
		http.Redirect(w, r, "/admin/settings/?tab=security", http.StatusFound)
		return
	}

	invite := ""
	if c, err := r.Cookie(inviteCookie); err == nil {
		invite = c.Value
	}
	_, browserTZ, _ := strings.Cut(st.Value, "|")

	joinScreen, joinAfterCode := false, false
	if acct == nil {
		// First time this Slack account has been seen. It becomes a new account, and joins an
		// organisation only through an invitation or by founding one.
		acct, err = b.slackSignup(ctx, id, invite, browserTZ)
		if err != nil {
			slog.Warn("slack sign-up refused", "user", id.UserID, "team", id.TeamID, "err", err)
			loginFailed(w, r, err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: "", Path: "/", MaxAge: -1})
	} else if invite != "" {
		// A known account opening an invitation link joins that organisation as well, on the terms
		// the join screen applies (acceptInvitation): addressed to this account, and for a
		// domain-limited link, an address proved the way the link asks — and not before a second
		// factor it owes is in.
		if joinScreen, joinAfterCode, err = b.joinOnSignIn(w, r, acct, invite); err != nil {
			loginFailed(w, r, "Could not verify account security. Try again.")
			return
		}
	}

	// Slack has already confirmed this mailbox, if it says so, so the account does not have to.
	// Applied on every sign-in rather than at sign-up alone: an account made before we started
	// asking, or one whose address the workspace confirmed later, is proved the next time its
	// holder comes through here.
	b.trustSlackEmail(ctx, acct, id)

	// Which organisation this session opens in is the policy's answer, not simply the first
	// membership: one that has moved to passwords only does not accept a Slack sign-in, even
	// from a member whose Slack account is properly linked.
	//
	// An account in no organisation at all, holding an invitation it will redeem once its code is
	// in, is still asked for the code: that invitation is where it signs in to.
	orgID, err := b.orgForSignIn(ctx, acct, "slack")
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
		tok, err := b.store.NewLoginChallenge(ctx, acct.ID, "slack")
		if err != nil {
			loginFailed(w, r, "Could not start two-factor sign-in.")
			return
		}
		http.Redirect(w, r, "/admin/login/#challenge="+url.QueryEscape(tok), http.StatusFound)
		return
	}
	b.startSession(w, r, acct, orgID, "slack")
	// The invitation this sign-in arrived holding was refused, and the join screen says why.
	if joinScreen {
		http.Redirect(w, r, b.signInNext(w, r), http.StatusFound)
		return
	}
	// Signing in with Slack and putting the app in Slack are two different grants, so Slack asks
	// for them on two different consent screens — sign-in is OpenID Connect, the install is
	// oauth/v2, and neither endpoint will carry the other's scopes. What can be joined is the
	// walk between them: somebody who just proved they are in a workspace, and whose
	// organisation has none connected, goes straight on to the install rather than landing on an
	// Overview of eight zeroes and having to find the button. Anyone who cannot finish that —
	// a viewer, an unverified address, a workspace already connected — goes to the console,
	// where the onboarding walk explains where they stand.
	//
	// And somebody who was on their way to the install when they were sent to sign in goes back
	// to it whether or not their organisation needs one: that is what they came to do, and
	// /slack/install is where a workspace already connected gets explained. Only the install
	// intent is read here — an invitation was spent above, on this same sign-in.
	me := &AdminUser{ID: acct.ID, Name: acct.Name, Email: acct.Email, OrgID: orgID, Via: "slack"}
	b.permissionsFor(ctx, me)
	if next := b.installNext(w, r); next != "" {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	if b.needsInstall(ctx, me) {
		http.Redirect(w, r, "/slack/install", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// trustSlackEmail takes Slack's word for an address Slack says the workspace has confirmed.
// Signing in with Slack proves the account; email_verified on the OIDC response says that
// account's mailbox was proved too, at the workspace, before we ever saw it. Sending somebody
// who arrived that way off to click a link in mail they did not ask for is asking them to prove
// the same thing a second time, and it strands them on step one of the setup walk to do it.
//
// It is the claim that is trusted, never the mere fact of a Slack sign-in: an address a
// workspace admin typed and nobody confirmed comes back unverified, and stays gated. And only
// for the address Slack itself named — an account whose console email was changed afterwards is
// a different mailbox, about which a Slack identity has nothing to say.
func (b *Bot) trustSlackEmail(ctx context.Context, u *User, id *slackIdentity) {
	if u == nil || u.EmailVerified || !id.EmailVerified {
		return
	}
	if !strings.EqualFold(normalEmail(u.Email), normalEmail(id.Email)) {
		return
	}
	if err := b.store.SetEmailVerified(ctx, u.ID, emailBySlack, 0); err != nil {
		slog.Warn("could not record Slack's confirmation of an address", "user", u.ID, "err", err)
		return
	}
	u.EmailVerified = true
	slog.Info("email confirmed by slack", "user", u.ID, "team", id.TeamID)
}

// slackSignup makes an account for a Slack identity nobody has seen before.
//
// What it deliberately does not do is look for an existing user with the same email address.
// The Slack account is the credential; the address is a label copied across for display and for
// sending mail. Whether it counts as proved is not this function's to decide — trustSlackEmail
// reads that off Slack's own claim, for sign-ups and for every sign-in after.
func (b *Bot) slackSignup(ctx context.Context, id *slackIdentity, invite, browserTZ string) (*User, error) {
	email := normalEmail(id.Email)
	if email == "" {
		return nil, errors.New("Slack did not give us an email address for your account, so we cannot create one. Ask an admin to add users:read.email to the Slack app, or sign up with an email and password.")
	}
	// An address that already has an account cannot be claimed by a Slack sign-in. Connecting
	// the two is done from inside that account, in Settings, where the person has proved they
	// hold it.
	if existing, _ := b.store.UserByEmail(ctx, email); existing != nil {
		return nil, errors.New("An account already uses that email address. Sign in with your password, then connect Slack from Settings.")
	}

	var token *EmailToken
	if invite != "" {
		t, err := b.store.PeekEmailToken(ctx, TokenInvite, invite)
		if err != nil {
			return nil, err
		}
		// The invitation names a mailbox, and Slack has just told us this account's. If they
		// disagree, the link was forwarded: founding an account is fine, joining somebody's
		// organisation on the strength of a link addressed to another person is not. A share
		// link names no mailbox and so asks only about the domain, if it was given one.
		if !t.AcceptsEmail(email) {
			return nil, errors.New(inviteRefusal(t))
		}
		// A domain link admits a proven address in its domain. A Slack profile address is set by
		// whichever workspace signed in — one the redeemer may control — so it is not that proof.
		// An addressed invitation to the mailbox is; ask for one.
		if t.domainLink() {
			return nil, errors.New("This link is limited to the " + t.Domain + " domain. Ask for an invitation addressed to you, or sign up with your email address and confirm it.")
		}
		token = t
	} else if !b.signupAllowed(ctx) {
		return nil, errors.New(signupRefusal())
	}

	u, err := b.store.CreateUser(ctx, email, id.Name, "") // no password: Slack is their sign-in
	if err != nil {
		return nil, err
	}
	if err := b.store.AddIdentity(ctx, u.ID, ProviderSlack, slackSubject(id.TeamID, id.UserID)); err != nil {
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
		// Founding an organisation. Named after the Slack workspace, because that is the only
		// name we have and it is the one they will recognise.
		name := nonEmpty(id.TeamName, email)
		org, err := b.store.CreateOrg(ctx, name, u.ID)
		if err != nil {
			return nil, err
		}
		// The zone on their Slack profile is a choice they made; the browser's is only where
		// they happen to be sitting, so it is the fallback.
		b.seedTimezone(ctx, org.ID, id.TZ, browserTZ)
		b.seedEmailDomain(ctx, org.ID, email)
	}
	slog.Info("slack sign-up", "user", u.ID, "email", email, "team", id.TeamID, "invited", token != nil)
	return u, nil
}

// slackIdentity is what Slack tells us about who just signed in: the OIDC identity plus the
// admin/owner flags from users.info, read with the bot token so the user cannot vouch for
// themselves.
type slackIdentity struct {
	UserID, TeamID, TeamName, Name, Email string
	TZ                                    string // the zone on their Slack profile, when the workspace is installed
	IsAdmin, IsOwner                      bool
	// Whether the workspace has confirmed that address — Slack's own email_verified claim, not
	// a guess of ours. It is what saves a Slack sign-up from proving the same mailbox twice.
	EmailVerified bool
}

func (b *Bot) slackIdentity(ctx context.Context, code, redirectURL string) (*slackIdentity, error) {
	form := url.Values{"client_id": {os.Getenv("SLACK_CLIENT_ID")}, "client_secret": {os.Getenv("SLACK_CLIENT_SECRET")},
		"code": {code}, "grant_type": {"authorization_code"}, "redirect_uri": {redirectURL}}
	req, _ := http.NewRequestWithContext(ctx, "POST", slackOIDC+"/api/openid.connect.token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oidcClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack token exchange: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		OK          bool   `json:"ok"`
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok)
	if !tok.OK || tok.AccessToken == "" {
		return nil, fmt.Errorf("slack token exchange failed: %s", nonEmpty(tok.Error, "no access token"))
	}

	req, _ = http.NewRequestWithContext(ctx, "GET", slackOIDC+"/api/openid.connect.userInfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp2, err := oidcClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack userinfo: %w", err)
	}
	defer resp2.Body.Close()
	var info struct {
		OK            bool   `json:"ok"`
		Error         string `json:"error"`
		Name          string `json:"name"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		TeamID        string `json:"https://slack.com/team_id"`
		UserID        string `json:"https://slack.com/user_id"`
	}
	json.NewDecoder(io.LimitReader(resp2.Body, 1<<20)).Decode(&info)
	if !info.OK || info.UserID == "" {
		return nil, fmt.Errorf("could not read your Slack identity: %s", nonEmpty(info.Error, "empty response"))
	}
	id := &slackIdentity{UserID: info.UserID, TeamID: info.TeamID, Name: info.Name,
		Email: strings.TrimSpace(info.Email), EmailVerified: info.EmailVerified}
	// The admin/owner flags are read with that workspace's own bot token, never from the
	// identity token: otherwise a user would be vouching for themselves.
	sl, err := b.slacks.For(ctx, id.TeamID)
	if err != nil {
		return id, nil // a foreign workspace: users.info would fail for its members anyway
	}
	api, err := sl.slackAPI()
	if err != nil {
		return id, nil // not a Slack workspace, so it has no users.info to vouch for anyone
	}
	su, err := api.GetUserInfoContext(ctx, info.UserID)
	if err != nil {
		return nil, fmt.Errorf("could not check your workspace role (users.info): %w", err)
	}
	id.IsAdmin, id.IsOwner = su.IsAdmin, su.IsOwner || su.IsPrimaryOwner
	id.TZ = su.TZ
	if id.Name == "" {
		id.Name = nonEmpty(su.RealName, su.Name)
	}
	if id.Email == "" {
		// users.info as a fallback for a missing address, but it says nothing about whether the
		// workspace confirmed it, so an address found this way stays unproved.
		id.Email, id.EmailVerified = su.Profile.Email, false
	}
	return id, nil
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- login throttling ----

// On Cloud Run the console is reachable from the internet, so failed password logins are
// counted and locked out. The 800 ms delay below only ever slowed one connection; with
// --concurrency 20 an attacker who opens many gets a steady stream of guesses, and a hit is
// full admin. Two keys are counted: the client address, and the account name — the address is
// what a distributed guessing run varies, the name is what it cannot.
//
// Locking on the address means someone can lock a person out by guessing at them. That is a
// deliberate trade: the lockout is minutes rather than permanent, and Sign in with Slack is
// unaffected, so somebody with a linked Slack account always has another way in.
const (
	loginMaxFails = 8
	loginWindow   = 15 * time.Minute
	loginLockout  = 15 * time.Minute
)

type loginFails struct {
	mu sync.Mutex
	at map[string]*failRun
}

type failRun struct {
	n     int
	first time.Time
	until time.Time // locked out until
}

var logins = &loginFails{at: map[string]*failRun{}}

// locked reports how much longer a key is locked out, if it is.
func (l *loginFails) locked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.at[key]
	if f == nil {
		return 0, false
	}
	if d := time.Until(f.until); d > 0 {
		return d, true
	}
	return 0, false
}

func (l *loginFails) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.at) > 10000 { // a guessing run rotating addresses must not grow this without bound
		for k, f := range l.at {
			if time.Since(f.first) > loginWindow && time.Until(f.until) <= 0 {
				delete(l.at, k)
			}
		}
	}
	f := l.at[key]
	if f == nil || time.Since(f.first) > loginWindow {
		f = &failRun{first: time.Now()}
		l.at[key] = f
	}
	f.n++
	if f.n >= loginMaxFails {
		f.until = time.Now().Add(loginLockout)
		f.n, f.first = 0, time.Now()
	}
}

func (l *loginFails) reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.at, k)
	}
}

// clientIP is who a throttle is held against: the sign-in lockout, the signup limiter, the
// operator's attempts. X-Forwarded-For is worth reading only when something in front of us wrote
// it, and the peer is what says so. A proxy on the same network — Cloud Run's front end, Caddy on
// a Docker bridge, a Kubernetes ingress — reaches us from a private address. A client talking to
// a container published straight to the internet reaches us from a public one and can put
// whatever it likes in that header; believing it there meant every guess arrived from a fresh
// address and no lockout ever fired.
//
// The last element is the one to read, never the first: each hop appends the peer it heard from,
// so the rightmost is what our own proxy wrote and everything to the left of it is the caller's
// to invent.
//
// Not defaulting to "never trust the header" is deliberate. Behind a proxy that would collapse
// every caller onto one front-end address and one shared lockout, which locks out a whole
// deployment the first time anybody mistypes a password — worse than the thing being fixed here.
func clientIP(r *http.Request) string {
	peer := peerIP(r)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" && trustProxy(peer) {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	return peer
}

// peerIP is the address the connection actually came from, which nothing but the network can
// forge.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// trustProxy decides whether X-Forwarded-For from this peer means anything. TRUST_PROXY settles
// the two cases the peer's address cannot: a reverse proxy that reaches us from a public address,
// and a private network nobody wants trusted.
func trustProxy(peer string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRUST_PROXY"))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	// Cloud Run always terminates in front of the container, whatever the peer looks like, and
	// K_SERVICE is how the process knows it is there (the same tell crypto.go uses).
	if os.Getenv("K_SERVICE") != "" {
		return true
	}
	ip := net.ParseIP(strings.Trim(peer, "[]"))
	// An address that will not parse is a unix socket or a test, neither of which is the open
	// internet. publicIP (outbound.go) is the same judgement the proxy makes about where it may
	// dial, read the other way round.
	return ip == nil || !publicIP(ip)
}

func (b *Bot) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		// Who is leaving, read before the session goes: the sign-out is the last line of a
		// session in the audit log and it has to name the person.
		if u, _ := b.store.AdminSession(r.Context(), c.Value); u != nil {
			b.audit(r, "auth.sign_out", AuditEvent{OrgID: u.OrgID, ActorID: u.ID, ActorPublic: u.PublicID,
				ActorEmail: u.Email, ActorName: u.Name, ActorSlack: u.UserID})
		}
		b.store.DeleteAdminSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/admin/", http.StatusFound)
}
