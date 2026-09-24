package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Email and password: signing up, signing in, proving the address, and getting back in after
// forgetting it. Sign in with Slack lives in auth_slack.go and shares the sessions this mints.

const (
	verifyTTL  = 24 * time.Hour
	resetTTL   = time.Hour
	inviteTTL2 = 7 * 24 * time.Hour

	// bcrypt refuses anything longer, rather than silently truncating as older versions did, so
	// the limit is checked here and reported in words instead of surfacing a library error.
	maxPasswordBytes = 72
	minPasswordBytes = 10
)

// bcrypt at cost 10 is deliberately CPU-heavy, and the service runs on one vCPU with request
// concurrency of 20. Without a bound, a handful of simultaneous sign-ins would starve the Slack
// event loop that shares the process. Two at a time keeps a login honest without letting it
// become a denial of service against the bot.
var hashSlots = make(chan struct{}, 2)

func hashPassword(pw string) (string, error) {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func checkPassword(hash, pw string) bool {
	if hash == "" || pw == "" {
		return false
	}
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// passwordProblem states the rule in the words the form will show. Length in bytes, not runes:
// bcrypt's limit is a byte limit, and a password of emoji reaches it in eighteen characters.
//
// Length is where it starts and not where it ends: "1234567890" is ten characters and is also
// the guess after "123456789". password_strength.go holds the rest, and personal is what the
// caller knows about the account — the address, the name, the organisation — so that a password
// cannot be made out of the things written on the door it opens. Every path that sets a
// password comes through here: signing up, resetting from a mailed link, and changing it from
// inside the account.
func passwordProblem(pw string, personal ...string) string {
	switch n := len(pw); {
	case n < minPasswordBytes:
		return fmt.Sprintf("Use at least %d characters.", minPasswordBytes)
	case n > maxPasswordBytes:
		return fmt.Sprintf("That is longer than %d bytes, which is as much as the hashing algorithm accepts. Try a shorter one.", maxPasswordBytes)
	}
	return predictablePassword(pw, personal)
}

// How a deployment answers "who may create an account", in one place so that the answer to
// "what stops anyone signing up" is a line rather than a search. SIGNUP_MODE picks one.
const (
	// SignupOpen: anyone may sign up and found their own organisation. This is what a hosted
	// service running open registration wants, and what holds the door there is not the door —
	// it is per-organisation filtering. Every query that touches a tenant's data carries its
	// organisation, TestEveryPerOrgQueryIsScoped fails the build on one that does not, and the
	// two-organisation tests in isolation_test.go assert that lists, reads by id, writes,
	// attachments, scope resolution, sessions, alerts and spend all stop at the boundary.
	SignupOpen = "open"
	// SignupFirstRun: the first signup founds the deployment and everybody after it arrives by
	// invitation. The default, because the deployment that has not thought about this is a
	// self-host on a public address, and open registration is not what its owner meant.
	SignupFirstRun = "first-run"
	// SignupClosed: never, not even to bootstrap. For a deployment provisioned some other way.
	SignupClosed = "closed"
)

// signupModeOf reads a configured value defensively: anything unrecognised is first-run, the
// restrictive answer. A typo must not leave a deployment's front door open.
func signupModeOf(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case SignupOpen:
		return SignupOpen
	case SignupClosed:
		return SignupClosed
	default:
		return SignupFirstRun
	}
}

// signupAllowed is the whole gate on registration.
func (b *Bot) signupAllowed(ctx context.Context) bool {
	switch b.cfg.SignupMode {
	case SignupOpen:
		return true
	case SignupClosed:
		return false
	default: // first-run: open only while there is nobody here yet
		return b.store.OrgCount(ctx) == 0
	}
}

// signupRefusal is what a closed door says. One sentence, in one place, because it is returned
// from both the password flow and the Slack one and wording that drifts between them reads like
// two different products.
func signupRefusal() string {
	return "This deployment is not open for sign-ups. Ask somebody who already has an account to invite you."
}

// ---- signing up ----

func (b *Bot) handleSignup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email, Password, Name, Org, Invite string
		TZ                                 string // the browser's IANA zone, for a founding sign-up
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	// The invitation is a cookie, parked by /invite/{token} (startInvite). It is HttpOnly on
	// purpose — a token the page can read is a token that ends up in a screenshot, a log or an
	// analytics payload — so the form cannot put it in the body and the server reads it here.
	// A token in the body still wins, because that is what the tests and curl use.
	if in.Invite == "" {
		if c, err := r.Cookie(inviteCookie); err == nil {
			in.Invite = c.Value
		}
	}
	in.Email = normalEmail(in.Email)
	if !looksLikeEmail(in.Email) {
		writeJSON(w, 400, map[string]any{"error": "That does not look like an email address."})
		return
	}
	if p := passwordProblem(in.Password, in.Email, in.Name, in.Org); p != "" {
		writeJSON(w, 400, map[string]any{"error": p})
		return
	}
	// One throttle for every credential-guessing path, keyed on the address as well as the
	// caller: signing up repeatedly is how you find out which addresses already have accounts.
	ipKey, mailKey := "ip:"+clientIP(r), "mail:"+in.Email
	if d, yes := logins.locked(ipKey); yes {
		tooMany(w, d)
		return
	}
	// The per-address counter has to be read as well as written, or probing one address from
	// many addresses costs nothing.
	if d, yes := logins.locked(mailKey); yes {
		tooMany(w, d)
		return
	}
	// Founding accounts is throttled too: each one is an organisation with its own invite
	// endpoint, uploads and share of the model key, so unlimited signups from one place are
	// unlimited copies of every other limit.
	if ok, d := signups.allow("ip:"+clientIP(r), signupsPerIPPerHour, time.Hour); !ok {
		tooMany(w, d)
		return
	}
	if in.Name != "" {
		name, err := cleanName(in.Name, 120)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		in.Name = name
	}

	ctx := r.Context()
	var invite *EmailToken
	if in.Invite != "" {
		t, err := b.store.PeekEmailToken(ctx, TokenInvite, in.Invite)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		// An invitation addressed to one mailbox may be redeemed by that mailbox and no other:
		// otherwise a forwarded email becomes an account in somebody else's organisation. A
		// share link names nobody, so it asks the only question it can — whether the address is
		// in the domain its maker allowed, when they named one.
		if !t.AcceptsEmail(in.Email) {
			writeJSON(w, 400, map[string]any{"error": inviteRefusal(t)})
			return
		}
		invite = t
	} else if !b.signupAllowed(ctx) {
		writeJSON(w, 403, map[string]any{"error": signupRefusal()})
		return
	}
	// The organisation's name is settled here, before any row is written, rather than left to
	// CreateOrg further down. That one runs after CreateUser: a name it refuses is a 500 with an
	// account already made and no organisation to put it in, and the address that owns it can
	// never sign up again, because the second attempt is told it is taken. The refusal is
	// cheaper to say than the orphan is to clean up.
	if invite == nil {
		org, err := cleanName(in.Org, 80)
		if err != nil {
			msg := "Give your organisation a name."
			if strings.TrimSpace(in.Org) != "" {
				msg = "That organisation name will not do — " + err.Error() + "."
			}
			writeJSON(w, 400, map[string]any{"error": msg})
			return
		}
		in.Org = org
	}

	// An address that already has an account is told so plainly, because the alternative — a
	// response indistinguishable from success — means a real person who forgot they had signed
	// up sits waiting for an account that was never made.
	//
	// The cost is stated rather than hidden: with signup open, this reply tells an unauthenticated
	// caller whether an address has an account here. What bounds it is the throttle above, which
	// counts against the address as well as the caller, so the answer cannot be harvested in bulk.
	// If that trade stops being acceptable, the fix is to make signup always end in "check your
	// email" — identical for both cases — not to make this branch lie.
	if existing, _ := b.store.UserByEmail(ctx, in.Email); existing != nil {
		logins.fail(ipKey)
		logins.fail(mailKey)
		writeJSON(w, 409, map[string]any{"error": errEmailTaken.Error()})
		return
	}

	hash, err := hashPassword(in.Password)
	if err != nil {
		fail(w, err)
		return
	}
	u, err := b.store.CreateUser(ctx, in.Email, in.Name, hash)
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	if err := b.store.AddIdentity(ctx, u.ID, ProviderPassword, strconv.FormatInt(u.ID, 10)); err != nil {
		fail(w, err)
		return
	}

	// A domain link names no mailbox, so the address just typed is unproven. The join waits until
	// our verification mail to that address is answered: the verify token names the link, and
	// handleVerify redeems it when the link in the mailbox is clicked — spending one of its uses
	// then, not now, so a sign-up nobody confirms costs the link nothing. There is no organisation
	// to sign into yet, so no session is started — an attacker who typed an address they do not
	// hold never receives the mail and never joins.
	if invite != nil && invite.domainLink() {
		b.store.InvalidateTokens(ctx, u.ID, TokenVerify)
		raw, err := b.store.NewEmailToken(ctx, EmailToken{Kind: TokenVerify, UserID: u.ID, Email: u.Email,
			OrgID: invite.OrgID, Role: invite.Role, CreatedBy: invite.CreatedBy, InviteHash: hashEmailToken(in.Invite)}, verifyTTL)
		if err != nil {
			fail(w, err)
			return
		}
		if err := b.mail.Send(ctx, Mail{To: u.Email, Subject: "Confirm your email address",
			Body: verifyEmail(u.Name, b.baseURL(r)+"/admin/verify/?token="+raw)}); err != nil {
			slog.Error("verification email", "to", u.Email, "err", err)
		}
		http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: "", Path: "/", MaxAge: -1})
		slog.Info("signup: domain-link join awaiting verification", "user", u.ID, "org", invite.OrgID)
		writeJSON(w, 200, map[string]any{"ok": true, "verify_to_join": true, "email_configured": b.mail.Configured()})
		return
	}

	var org *Org
	if invite != nil {
		// Redeeming the invitation is what joins them, and it is consumed here so a second
		// signup with the same link cannot happen.
		if _, err := b.store.TakeEmailToken(ctx, TokenInvite, in.Invite); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		if err := b.store.AddMembership(ctx, u.ID, invite.OrgID, invite.Role, invite.CreatedBy); err != nil {
			fail(w, err)
			return
		}
		// An invitation sent to this address is already proof they hold it — the link only ever
		// reached that inbox. A share link proves nothing of the sort: it was handed around, and
		// whoever redeemed it typed the address themselves. They confirm it like anybody else.
		if invite.Email != "" {
			b.store.SetEmailVerified(ctx, u.ID, emailByInvite, invite.OrgID)
			u.EmailVerified = true
		}
		org, _ = b.store.Org(ctx, invite.OrgID)
	} else {
		org, err = b.store.CreateOrg(ctx, in.Org, u.ID)
		if err != nil {
			fail(w, err)
			return
		}
		b.seedTimezone(ctx, org.ID, in.TZ)
		b.seedEmailDomain(ctx, org.ID, in.Email)
	}
	if org == nil {
		fail(w, errors.New("the organisation could not be created"))
		return
	}

	if invite != nil {
		// Spent: the cookie must not follow them into a second sign-up.
		http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: "", Path: "/", MaxAge: -1})
	}
	if !u.EmailVerified {
		b.sendVerification(ctx, r, u)
	}
	// The first line of a new organisation's log is its founding, and it comes before the
	// sign-in that follows it; an invited sign-up is a joining, recorded the way accepting the
	// invitation from an existing account is.
	if invite != nil {
		b.auditAs(r, u, org.ID, "member.joined", AuditEvent{TargetKind: "member", TargetID: u.PublicID, TargetName: u.Email,
			Details: auditDetails(map[string]any{"role": invite.Role, "via": "signup"})})
	} else {
		b.auditAs(r, u, org.ID, "org.created", AuditEvent{TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name})
	}
	b.startSession(w, r, u, org.ID, "signup")
	slog.Info("signup", "user", u.ID, "email", u.Email, "org", org.ID, "invited", invite != nil)
	// Signed up on the way to connecting a workspace — from the site's Add to Slack button, or
	// from the Marketplace — so the install is where this ends, not the Overview. The invitation
	// is not consulted here: an invited sign-up has already joined, and has nowhere else to go.
	writeJSON(w, 200, map[string]any{"ok": true, "org": org.Name, "verified": u.EmailVerified,
		"email_configured": b.mail.Configured(), "next": b.installNext(w, r)})
}

// ---- signing in ----

func (b *Bot) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	in.Email = normalEmail(in.Email)
	ipKey, mailKey := "ip:"+clientIP(r), "mail:"+in.Email
	if d, yes := logins.locked(ipKey); yes {
		slog.Warn("sign-in refused: locked out", "ip", clientIP(r), "email", truncate(in.Email, 64), "for", d.Round(time.Second))
		// Attributed to the account the address names, when it names one: its organisation
		// should be able to see that somebody was hammering on one of its members' doors.
		u, _ := b.store.UserByEmail(r.Context(), in.Email)
		b.auditSignInFailed(r, u, "locked_out")
		tooMany(w, d)
		return
	}
	// An address under attack is slowed, not locked: a hard lock on the address alone let
	// anyone keep its owner out of the console with eight wrong guesses every quarter hour.
	// The caller's own lock above still stops a single source; the delay makes a distributed
	// guess cost seconds per attempt while the right password still gets in.
	if _, yes := logins.locked(mailKey); yes {
		time.Sleep(2 * time.Second)
	}

	ctx := r.Context()
	u, _ := b.store.UserByEmail(ctx, in.Email)
	// One message and one shape of failure whether the address is unknown, the password is
	// wrong, or the account only has a Slack sign-in: none of those should be discoverable
	// from outside.
	if u == nil || u.Status != "active" || !checkPassword(u.passwordHash, in.Password) {
		logins.fail(ipKey)
		logins.fail(mailKey)
		slog.Warn("sign-in failed", "ip", clientIP(r), "email", truncate(in.Email, 64))
		b.auditSignInFailed(r, u, "password")
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "That email and password do not match an account."})
		return
	}
	// Reset the account's counter, not the address's. Clearing the IP key on success was a free
	// pardon for a sprayer: one correct guess against any account — including an account they
	// created themselves a moment earlier — wiped the record of every failure from that address,
	// so the per-IP limit never actually bound.
	logins.reset(mailKey)

	// The password was right; whether it is a way in here is a separate question, and the
	// organisation's policy answers it before any session exists.
	orgID, err := b.orgForSignIn(ctx, u, "password")
	if err != nil {
		b.auditSignInFailed(r, u, "policy")
		writeJSON(w, 403, map[string]any{"error": err.Error()})
		return
	}
	// A second factor turns the answer into "not yet": a challenge instead of a session, so a
	// stolen password on its own buys five minutes of typing wrong codes.
	e, err := b.store.TOTPEnrolment(ctx, u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if e != nil && e.Confirmed {
		tok, err := b.store.NewLoginChallenge(ctx, u.ID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"two_factor": true, "challenge": tok,
			"recovery_left": b.store.RecoveryCodesLeft(ctx, u.ID)})
		return
	}
	b.startSession(w, r, u, orgID, "password")
	// Signed in from an invitation link: the cookie startInvite parked is still in hand, and the
	// point of following that link was to join, not to land in whatever organisation this
	// account already had. The join screen is where it is confirmed — including the offer to
	// leave behind an empty organisation founded on the way here — so say where to go rather
	// than joining silently behind their back.
	writeJSON(w, 200, map[string]any{"ok": true, "verified": u.EmailVerified, "next": b.signInNext(w, r)})
}

// inviteNext is "/join" when this request carries an unspent invitation, and "" otherwise.
func inviteNext(r *http.Request) string {
	if c, err := r.Cookie(inviteCookie); err == nil && c.Value != "" {
		return "/admin/join/"
	}
	return ""
}

// passwordUsableBy says whether a password is still a way into any organisation this person
// belongs to. Somebody with no membership at all keeps their password: they are mid-invitation,
// and no organisation has said otherwise.
func (b *Bot) passwordUsableBy(ctx context.Context, u *User) bool {
	ms, err := b.store.MembershipsFor(ctx, u.ID)
	if err != nil || len(ms) == 0 {
		return true
	}
	for _, m := range ms {
		if b.settings.Get(ctx, m.OrgID).allowsPassword() {
			return true
		}
	}
	return false
}

// startSession is the one place a session is minted, so the cookie flags, the CSRF token and the
// audit line cannot drift between the three ways in.
func (b *Bot) startSession(w http.ResponseWriter, r *http.Request, u *User, orgID int64, via string, mfa ...bool) {
	tok, err := b.store.CreateAdminSession(r.Context(), AdminUser{
		ID: u.ID, Name: u.Name, Email: u.Email, OrgID: orgID, Via: via, MFAVerified: len(mfa) > 0 && mfa[0],
		UserID: b.slackIDOf(r.Context(), u, orgID),
	}, sessionTTL)
	if err != nil {
		fail(w, err)
		return
	}
	b.setSessionCookie(w, r, tok)
	b.setCSRFCookie(w, r, randomToken())
	b.store.TouchUser(r.Context(), u.ID)
	slog.Info("console sign-in", "via", via, "user", u.ID, "email", u.Email, "org", orgID)
	// Every sign-in, however it was made, lands here — so this is the one place it is written
	// into the organisation's own record, with the credential used and the address it came from.
	b.auditAs(r, u, orgID, "auth.sign_in", AuditEvent{TargetKind: "session",
		Details: auditDetails(map[string]any{"via": via, "two_factor": len(mfa) > 0 && mfa[0]})})
}

// auditSignInFailed records a refused sign-in against every organisation the account belongs
// to. Only for an address that names an account: the sign-in form answers identically whether
// or not one exists, and the log must not be the place that difference leaks.
func (b *Bot) auditSignInFailed(r *http.Request, u *User, reason string) {
	if u == nil {
		return
	}
	b.auditEverywhere(r, u, "auth.sign_in_failed", AuditEvent{Outcome: auditDenied, TargetKind: "session",
		Details: auditDetails(map[string]any{"reason": reason})})
}

// slackIDOf finds the Slack user id this account signs in with, in a workspace this organisation
// has connected. It is what "created by" and "denied by" record, and those are read back into
// Slack as <@U…> mentions — so an account with no Slack identity gets "", and the callers render
// the person's name instead of a mention that would resolve to nobody.
//
// The org matters: one person can hold Slack identities in several workspaces, and the id worth
// recording is the one their colleagues here would recognise.
func (b *Bot) slackIDOf(ctx context.Context, u *User, orgID int64) string {
	ids, err := b.store.Identities(ctx, u.ID)
	if err != nil || len(ids) == 0 {
		return ""
	}
	teams, _ := b.store.Teams(ctx, orgID)
	mine := map[string]bool{}
	for _, t := range teams {
		mine[t.TeamID] = true
	}
	fallback := ""
	for _, i := range ids {
		team, user, ok := chatAccount(i) // a Slack identity, or a Microsoft one for Teams
		if !ok {
			continue
		}
		if mine[team] {
			return user
		}
		if fallback == "" {
			fallback = user
		}
	}
	return fallback
}

func tooMany(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": fmt.Sprintf("Too many attempts. Try again in %d minutes.", int(d.Minutes())+1)})
}

// ---- proving the address ----

func (b *Bot) sendVerification(ctx context.Context, r *http.Request, u *User) {
	b.store.InvalidateTokens(ctx, u.ID, TokenVerify)
	raw, err := b.store.NewEmailToken(ctx, EmailToken{Kind: TokenVerify, UserID: u.ID, Email: u.Email}, verifyTTL)
	if err != nil {
		slog.Error("verification token", "err", err)
		return
	}
	link := b.baseURL(r) + "/admin/verify/?token=" + raw
	if err := b.mail.Send(ctx, Mail{To: u.Email, Subject: "Confirm your email address", Body: verifyEmail(u.Name, link)}); err != nil {
		slog.Error("verification email", "to", u.Email, "err", err)
	}
}

func (b *Bot) handleVerify(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	t, err := b.store.TakeEmailToken(r.Context(), TokenVerify, in.Token)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if err := b.store.SetEmailVerified(r.Context(), t.UserID, emailByMail, 0); err != nil {
		fail(w, err)
		return
	}
	u, _ := b.store.User(r.Context(), t.UserID)
	if u != nil {
		b.auditEverywhere(r, u, "auth.email_verified", AuditEvent{TargetKind: "account", TargetID: u.PublicID, TargetName: u.Email})
	}
	// A domain-link signup parked its join on this token (see handleSignup): the address is now
	// proved, so the join it was waiting on is made, if the link still allows it. A plain
	// verification token carries no org, so this is a no-op for it. Re-checking membership keeps a
	// join made some other way in the meantime from spending one of the link's uses.
	out := map[string]any{"ok": true, "joined": false}
	if t.OrgID != 0 && u != nil {
		if ms, _ := b.store.MembershipsFor(r.Context(), u.ID); !hasMembership(ms, t.OrgID) {
			refused, err := b.joinParkedInvite(r, t, u)
			if err != nil {
				fail(w, err)
				return
			}
			if refused != "" {
				slog.Info("domain-link join refused on verification", "user", u.ID, "org", t.OrgID, "why", refused)
				out["join_error"] = refused
			} else {
				out["joined"] = true
			}
		}
	}
	writeJSON(w, 200, out)
}

// joinParkedInvite makes the join a domain-link sign-up left waiting on its verification link,
// now that the address it typed is proved, and returns why not when the link no longer allows it.
//
// The link is read and spent here, as if it were being redeemed now, because it is. Joining from
// what the verification link remembered of it meant its use limit never bound — nothing was spent
// at sign-up and nothing when the mail was answered — and a link withdrawn or expired in between
// still let somebody in. Taking it here asks every question taking a link asks: still live, a use
// left, and a maker still able to grant the role it gives.
func (b *Bot) joinParkedInvite(r *http.Request, t *EmailToken, u *User) (string, error) {
	ctx := r.Context()
	gone := "Your address is confirmed, but the invitation link you signed up with has expired, been withdrawn or run out of uses, so you have not joined. Ask whoever shared it for a new one."
	// Parked before verification links named their share link: there is no link left to redeem,
	// so there is no join to make.
	if t.InviteHash == "" {
		return gone, nil
	}
	// The organisation and the domain are asked again because it is the same link being redeemed,
	// and what it may join is its own to say, not the verification link's.
	inv, err := b.store.PeekEmailTokenHash(ctx, TokenInvite, t.InviteHash)
	if err != nil || inv.OrgID != t.OrgID || !inv.AcceptsEmail(u.Email) {
		return gone, nil
	}
	if inv, err = b.store.TakeEmailTokenHash(ctx, TokenInvite, t.InviteHash); errors.Is(err, errBadEmailToken) {
		return gone, nil
	} else if err != nil {
		// Its maker has left the organisation, or no longer holds what the link hands out.
		return "Your address is confirmed, but the invitation link you signed up with is no longer valid (" + err.Error() + "), so you have not joined. Ask for a new one.", nil
	}
	if err := b.store.AddMembership(ctx, u.ID, inv.OrgID, inv.Role, inv.CreatedBy); err != nil {
		return "", err
	}
	b.auditAs(r, u, inv.OrgID, "member.joined", AuditEvent{TargetKind: "member", TargetID: u.PublicID, TargetName: u.Email,
		Details: auditDetails(map[string]any{"role": inv.Role, "via": "domain-link"})})
	return "", nil
}

// hasMembership reports whether one of the memberships is in org.
func hasMembership(ms []Membership, org int64) bool {
	for _, m := range ms {
		if m.OrgID == org {
			return true
		}
	}
	return false
}

// handleResendVerification runs under requireAdmin like every other signed-in write: it sends
// mail and invalidates the outstanding link, and a cross-site page must not be able to do that.
//
// An address confirmed some other way still gets the mail when it asks. A domain-limited link
// takes only certain proofs (confirmedForDomainLink), and an address Slack vouched for, say, is
// confirmed everywhere else and refused there — this is how its owner proves it the way the link
// asks. Only an address our own mail has already proved has nothing to gain.
func (b *Bot) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	u := adminFromCtx(r.Context())
	acct, _ := b.store.User(r.Context(), u.ID)
	if acct == nil || acct.EmailVerified && acct.emailVerifiedBy == emailByMail {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	for _, key := range []string{"verify-user:" + strconv.FormatInt(acct.ID, 10), "verify-email:" + normalEmail(acct.Email)} {
		if d, locked := logins.locked(key); locked {
			tooMany(w, d)
			return
		}
		logins.fail(key)
	}
	b.sendVerification(r.Context(), r, acct)
	writeJSON(w, 200, map[string]any{"ok": true, "email_configured": b.mail.Configured()})
}

// ---- forgetting it ----

func (b *Bot) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	in.Email = normalEmail(in.Email)
	// Asking is itself rate-limited: this endpoint sends mail on demand, and each ask
	// invalidates the link before it. Counted per caller and per address, in a namespace of
	// its own — eight of these must not lock the caller out of signing in.
	if ok, d := invites.allow("forgot-ip:"+clientIP(r), 8, 15*time.Minute); !ok {
		tooMany(w, d)
		return
	}
	if ok, d := invites.allow("forgot-mail:"+in.Email, 3, time.Hour); !ok {
		tooMany(w, d)
		return
	}

	ctx := r.Context()
	// A reset link is only sent where a password is still a way in. Under Slack-only the link
	// would mail somebody a credential their organisation refuses — a live reset token for an
	// account that cannot use it, which is worth nothing to them and something to an attacker.
	if u, _ := b.store.UserByEmail(ctx, in.Email); u != nil && u.Status == "active" && b.passwordUsableBy(ctx, u) {
		b.store.InvalidateTokens(ctx, u.ID, TokenReset)
		if raw, err := b.store.NewEmailToken(ctx, EmailToken{Kind: TokenReset, UserID: u.ID, Email: u.Email}, resetTTL); err == nil {
			link := b.baseURL(r) + "/admin/reset/?token=" + raw
			if err := b.mail.Send(ctx, Mail{To: u.Email, Subject: "Reset your attest_tag password", Body: resetEmail(u.Name, link)}); err != nil {
				slog.Error("reset email", "to", u.Email, "err", err)
			}
		}
		// A reset link going out is the first step of an account takeover as often as of a
		// forgotten password, and the organisation is owed the line either way.
		b.auditEverywhere(r, u, "auth.password_reset_requested", AuditEvent{TargetKind: "account", TargetID: u.PublicID, TargetName: u.Email})
	}
	// The same answer either way. Telling somebody an address has no account turns this form
	// into a way of enumerating who has one.
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (b *Bot) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Password string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	ctx := r.Context()
	// Peeked rather than taken: the account behind the link says which words the new password
	// may not be built from, and a password refused for that must not have burnt the link on
	// the way. Peeking says nothing to a caller who does not already hold the token.
	var personal []string
	if t, err := b.store.PeekEmailToken(ctx, TokenReset, in.Token); err == nil {
		if u, _ := b.store.User(ctx, t.UserID); u != nil {
			personal = append(personal, u.Email, u.Name)
		}
	}
	if p := passwordProblem(in.Password, personal...); p != "" {
		writeJSON(w, 400, map[string]any{"error": p})
		return
	}
	t, err := b.store.TakeEmailToken(ctx, TokenReset, in.Token)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		fail(w, err)
		return
	}
	if err := b.store.SetPassword(ctx, t.UserID, hash); err != nil {
		fail(w, err)
		return
	}
	// Holding the reset link proves control of the mailbox, so the address is verified too.
	b.store.SetEmailVerified(ctx, t.UserID, emailByMail, 0)
	// Every other session belonging to this account goes: a reset is what somebody does when
	// they think the account is compromised, and leaving the attacker signed in defeats it.
	b.store.DeleteSessionsFor(ctx, t.UserID)
	// And every developer key: a key an attacker minted while holding the session survives a
	// cookie sweep otherwise, so the reset would not actually evict them.
	b.store.RevokeAPIKeysForUser(ctx, t.UserID)
	slog.Info("password reset", "user", t.UserID)
	if u, _ := b.store.User(ctx, t.UserID); u != nil {
		b.auditEverywhere(r, u, "auth.password_reset", AuditEvent{TargetKind: "account", TargetID: u.PublicID, TargetName: u.Email})
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- from inside the account ----

// handleChangePassword is the only way to set a password while signed in, and it asks for the
// current one first: a stolen session should not be able to lock the owner out of their account.
func (b *Bot) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	var in struct{ Current, Password string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if p := passwordProblem(in.Password, me.Email, me.Name, me.OrgName); p != "" {
		writeJSON(w, 400, map[string]any{"error": p})
		return
	}
	acct, _ := b.store.User(r.Context(), me.ID)
	if acct == nil {
		writeJSON(w, 404, map[string]any{"error": "no such account"})
		return
	}
	// Everyone with a password proves they know the current one. Somebody who signed in with
	// Slack and has never set one has nothing to re-enter — but a first password is a second
	// way into the account that outlives the session it was set from, so it still needs proof:
	// a code from the authenticator when there is one, otherwise a session only minutes old.
	// Without that, any console left signed in was a way to keep the account for good.
	if acct.HasPassword() {
		if !checkPassword(acct.passwordHash, in.Current) {
			time.Sleep(400 * time.Millisecond)
			writeJSON(w, 403, map[string]any{"error": "That is not your current password."})
			return
		}
	} else if ok, why := b.proveIdentity(r.Context(), me, "", in.Current); !ok {
		writeJSON(w, 403, map[string]any{"error": why})
		return
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		fail(w, err)
		return
	}
	if err := b.store.SetPassword(r.Context(), me.ID, hash); err != nil {
		fail(w, err)
		return
	}
	if !acct.HasPassword() {
		b.store.AddIdentity(r.Context(), me.ID, ProviderPassword, strconv.FormatInt(me.ID, 10))
	}
	// A changed password ends every other session, as a reset does. This one stays.
	b.store.DeleteOtherSessionsFor(r.Context(), me.ID, sessionTokenOf(r))
	// Developer keys go too: the same argument as the reset path — a key minted from a stolen
	// session would otherwise outlive the change made to shut that session out.
	b.store.RevokeAPIKeysForUser(r.Context(), me.ID)
	slog.Info("password changed", "user", me.ID)
	b.audit(r, "auth.password_changed", AuditEvent{TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email,
		Details: auditDetails(map[string]any{"first_password": !acct.HasPassword()})})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- invitations ----

// startInvite is where an invitation link lands. It parks the token in a short-lived cookie and
// sends the browser to the sign-up form, which reads it back — so the token never sits in the
// address bar of a page that might be shared or logged.
func (b *Bot) startInvite(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("token")
	t, err := b.store.PeekEmailToken(r.Context(), TokenInvite, raw)
	if err != nil {
		http.Redirect(w, r, "/admin/?auth_error="+urlEscape(err.Error()), http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: raw, Path: "/", HttpOnly: true, MaxAge: 900,
		SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	// Somebody already signed in is joining a second organisation rather than making an account.
	if u := b.authenticate(r); u != nil {
		http.Redirect(w, r, "/admin/join/", http.StatusFound)
		return
	}
	// A share link names nobody, so there is no address to fill in and none to look up. It goes
	// to the sign-up form as an invitation all the same — an organisation is being joined rather
	// than founded — but the person types their own address, and the form says which addresses
	// the link will take so they find out before filling it in rather than after.
	if t.Shareable() {
		to := "/admin/signup/?invite=open"
		if t.Domain != "" {
			to += "&domain=" + urlEscape(t.Domain)
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	// An address that already has an account cannot sign up again, so sending it to the sign-up
	// form is a dead end that ends in "that address is taken". Send it to sign in instead; the
	// cookie rides along, and the login hands the browser on to the join screen.
	if existing, _ := b.store.UserByEmail(r.Context(), normalEmail(t.Email)); t.Email != "" && existing != nil {
		http.Redirect(w, r, "/admin/login/?invited="+urlEscape(t.Email), http.StatusFound)
		return
	}
	// An invitation to a Slack account whose workspace withholds email lands here with nothing
	// to prefill either. The direct message was the delivery, so the person holding it is the
	// person it was for; they still type the address their account will use.
	if t.Email == "" {
		http.Redirect(w, r, "/admin/signup/?invite=open", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/signup/?invited="+urlEscape(t.Email), http.StatusFound)
}

// handleAcceptInvite joins an already-signed-in account to the inviting organisation.
func (b *Bot) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	// Whether to leave behind the organisation they founded on sign-up. Absent means no: an
	// older console, or a caller that never saw the offer, must not have somebody detached from
	// an organisation on its behalf.
	var in struct {
		LeaveOwn bool `json:"leave_own"`
	}
	decode(r, &in)
	c, err := r.Cookie(inviteCookie)
	if err != nil || c.Value == "" {
		writeJSON(w, 400, map[string]any{"error": "That invitation link has expired. Ask for a new one."})
		return
	}
	acct, _ := b.store.User(r.Context(), me.ID)
	if acct == nil {
		writeJSON(w, 401, map[string]any{"error": "sign in required"})
		return
	}
	// The terms are acceptInvitation's, shared with a Slack or Microsoft sign-in made while holding
	// the same link. A refusal does not spend the link, so one wrong click by the wrong account
	// does not destroy the invitation for the right one.
	t, status, err := b.acceptInvitation(r, acct, c.Value)
	if status == http.StatusInternalServerError {
		fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: "", Path: "/", MaxAge: -1})
	org, _ := b.store.Org(r.Context(), t.OrgID)
	left := ""
	if in.LeaveOwn {
		if own := b.leavableOwnOrg(r, me, t.OrgID); own != nil && own.Leavable {
			b.leaveOwnOrg(w, r, me, t.OrgID)
			left = own.Name
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "org": org, "left": left})
}

// ---- switching organisation ----

func (b *Bot) handleSwitchOrg(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	var in struct {
		// The organisation as the switcher listed it: opaque and public. It is resolved here
		// and then has to survive the membership check below, so naming one is not reaching it.
		OrgID string `json:"org_id"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	org, err := b.store.OrgByPublicID(r.Context(), in.OrgID)
	if err != nil || org == nil {
		writeJSON(w, 403, map[string]any{"error": "You are not a member of that organisation."})
		return
	}
	m, err := b.store.Membership(r.Context(), me.ID, org.ID)
	if err != nil || m == nil {
		writeJSON(w, 403, map[string]any{"error": "You are not a member of that organisation."})
		return
	}
	// Membership is not enough: the destination decides which credentials it accepts, and this
	// session was made with one of them. Otherwise switching would be the way around a policy.
	if !b.settings.Get(r.Context(), org.ID).allowsVia(me.Via) {
		writeJSON(w, 403, map[string]any{"error": "That organisation does not accept the way you signed in. Sign out and sign in again to open it."})
		return
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		writeJSON(w, 401, map[string]any{"error": "sign in required"})
		return
	}
	if err := b.store.SetSessionOrg(r.Context(), c.Value, org.ID); err != nil {
		fail(w, err)
		return
	}
	slog.Info("organisation switched", "user", me.ID, "org", org.ID)
	// Recorded where they arrived: to that organisation it is the start of a session, with the
	// credential the session was originally made with.
	b.audit(r, "auth.org_switched", AuditEvent{OrgID: org.ID, TargetKind: "session",
		Details: auditDetails(map[string]any{"via": me.Via, "from_org": me.OrgPublic})})
	writeJSON(w, 200, map[string]any{"ok": true, "org": m})
}

// lastHolderGuard refuses a change that would leave an organisation with nobody able to undo it.
// The invariant is on the capability rather than on the role name, because a custom role can hold
// user management too — see console_roles.go.
func (b *Bot) lastHolderGuard(ctx context.Context, orgID, userID int64, curRole, nextRole string) error {
	custom := b.store.CustomRoleMap(ctx, orgID)
	after := permissionsForRole(nextRole, custom) // empty when they are being removed
	for _, p := range criticalPermissions {
		if !permissionsForRole(curRole, custom)[p] || after[p] {
			continue // they did not hold it, or they still will
		}
		n, err := b.store.CountMembersHolding(ctx, orgID, p, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("that would leave nobody who can %s — give someone else that access first",
				strings.TrimSuffix(strings.ReplaceAll(string(p), ".", " "), " manage"))
		}
	}
	return nil
}

// lastHolderGuardForRole is lastHolderGuard for the other way a capability can vanish: deleting
// a custom role demotes every member holding it at once. Members on that role fall back to no
// permissions, so if the role was the only thing granting a critical permission, the
// organisation loses the ability to administer itself with nobody having been removed.
func (b *Bot) lastHolderGuardForRole(ctx context.Context, orgID int64, key string) error {
	custom := b.store.CustomRoleMap(ctx, orgID)
	if custom[key] == nil {
		return nil // not a custom role; the built-ins cannot be deleted
	}
	granted := permissionsForRole(key, custom)
	for _, p := range criticalPermissions {
		if !granted[p] {
			continue
		}
		n, err := b.store.CountMembersHoldingExcludingRole(ctx, orgID, p, key)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("deleting %q would leave nobody who can %s — move those members to another role first",
				key, strings.TrimSuffix(strings.ReplaceAll(string(p), ".", " "), " manage"))
		}
	}
	return nil
}
