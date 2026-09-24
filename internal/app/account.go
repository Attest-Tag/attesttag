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
)

// Your own account: the profile the console shows you, the credentials that get you in, and the
// second factor that stands behind the password.
//
// Everything here acts on the person making the request and on nobody else. Managing other
// members lives in console_users.go, where it is a permission; there is deliberately no path in
// this file that takes a user id, so an admin cannot read or reset somebody else's second factor
// by aiming an endpoint at them.

// ---- the account screen ----

func (b *Bot) handleAccount(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	ctx := r.Context()
	u, _ := b.store.User(ctx, me.ID)
	if u == nil {
		writeJSON(w, 404, map[string]any{"error": "no such account"})
		return
	}
	// Identities carry their subject, which is what the disconnect button has to name. A Slack
	// subject is 'T…:U…' — the workspace and the person — and it is already the person's own,
	// shown only to them.
	ids, _ := b.store.Identities(ctx, me.ID)
	methods := map[string]bool{}
	for _, id := range ids {
		methods[id.Provider] = true
	}
	enrol, _ := b.store.TOTPEnrolment(ctx, me.ID)
	set := b.settings.Get(ctx, me.OrgID)
	writeJSON(w, 200, map[string]any{
		"user": u, "role": me.Role, "org": map[string]any{"id": me.OrgPublic, "name": me.OrgName, "slug": me.OrgSlug},
		"has_password":   u.HasPassword(),
		"identities":     ids,
		"methods":        methods,
		"signed_in_with": me.Via,
		"two_factor": map[string]any{
			"enabled":       enrol != nil && enrol.Confirmed,
			"pending":       enrol != nil && !enrol.Confirmed,
			"required":      set.RequireTwoFactor,
			"recovery_left": b.store.RecoveryCodesLeft(ctx, me.ID),
			// What enrolling will ask for. GET /api/org answers the same question for account
			// deletion; the enrolment screen needs it for the same reason — so it shows the
			// field that will actually be accepted rather than guessing.
			"proof": b.proofKind(ctx, me),
		},
		"auth_policy": set.AuthPolicy,
	})
}

// handleUpdateAccount changes what the console calls you. The address is not editable here: it
// is the join key for invitations and the one the verification mail went to, so moving it is a
// verified change of its own rather than a text field.
func (b *Bot) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	var in struct{ Name string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	name := strings.TrimSpace(in.Name)
	if name != "" {
		clean, err := cleanName(name, 120)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": "A name is up to 120 ordinary characters."})
			return
		}
		name = clean
	}
	if err := b.store.SetUserName(r.Context(), me.ID, name); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": name})
}

// ---- the second factor ----

// totpSecret unseals one account's enrolment. The secret is in memory for the length of one
// check and is never returned to a caller outside this file.
func (b *Bot) totpSecret(ctx context.Context, userID int64) (string, *TOTPEnrolment, error) {
	e, err := b.store.TOTPEnrolment(ctx, userID)
	if err != nil || e == nil {
		return "", nil, err
	}
	plain, err := b.sealer.Open(e.SecretEnc)
	if err != nil {
		return "", e, fmt.Errorf("this account's two-factor secret cannot be read with the current MASTER_KEY: %w", err)
	}
	return string(plain), e, nil
}

// handleTOTPStart mints a secret and hands back what an authenticator app needs to hold it. It
// is not a second factor yet: nothing asks for a code until handleTOTPConfirm proves the app and
// this server agree, so an enrolment abandoned halfway leaves the account exactly as it was.
func (b *Bot) handleTOTPStart(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	ctx := r.Context()
	var in struct{ Password, Code string }
	decode(r, &in) // a body is optional: an account whose proof is a recent session sends none
	if e, _ := b.store.TOTPEnrolment(ctx, me.ID); e != nil && e.Confirmed {
		writeJSON(w, 409, map[string]any{"error": "Two-factor is already on for this account. Turn it off first if you want to enrol a new device."})
		return
	}
	// The same proof turning the factor off asks for, and for the stronger reason. Enrolling was
	// the one door in this file that asked for nothing, and it led all the way through: somebody
	// holding a session they found could enrol their own authenticator, which makes proveIdentity
	// answer "code" from then on — so they could then set a first password with a code from their
	// own phone and keep the account for good. Every other exit here was already closed; this was
	// the entrance.
	if ok, why := b.proveIdentity(ctx, me, in.Password, in.Code); !ok {
		writeJSON(w, 403, map[string]any{"error": why, "proof": b.proofKind(ctx, me)})
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		fail(w, err)
		return
	}
	sealed, err := b.sealer.Seal([]byte(secret))
	if err != nil {
		fail(w, err)
		return
	}
	if err := b.store.StartTOTP(ctx, me.ID, sealed); err != nil {
		fail(w, err)
		return
	}
	issuer := nonEmpty(me.OrgName, "attest_tag")
	writeJSON(w, 200, map[string]any{"secret": secret, "uri": otpauthURI(issuer, me.Email, secret)})
}

// handleTOTPConfirm accepts the first code and, with it, the recovery codes. They are shown
// once: this response is the only time the plain codes exist outside the person's own notes.
func (b *Bot) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	ctx := r.Context()
	var in struct{ Code string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	secret, enrol, err := b.totpSecret(ctx, me.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if enrol == nil {
		writeJSON(w, 400, map[string]any{"error": "Start the enrolment again: there is no secret waiting to be confirmed."})
		return
	}
	if enrol.Confirmed {
		writeJSON(w, 409, map[string]any{"error": "Two-factor is already on for this account."})
		return
	}
	step, ok := checkTOTP(secret, in.Code, time.Now(), enrol.LastStep)
	if !ok {
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, 400, map[string]any{"error": "That code is not right. Check your phone's clock if it keeps failing."})
		return
	}
	if err := b.store.ConfirmTOTP(ctx, me.ID, step); err != nil {
		fail(w, err)
		return
	}
	if err := b.store.MarkSessionMFA(ctx, sessionTokenOf(r), me.ID); err != nil {
		fail(w, err)
		return
	}
	codes, err := b.issueRecoveryCodes(ctx, me.ID)
	if err != nil {
		fail(w, err)
		return
	}
	slog.Info("two-factor enrolled", "user", me.ID, "org", me.OrgID)
	b.audit(r, "auth.two_factor_enrolled", AuditEvent{TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email})
	writeJSON(w, 200, map[string]any{"ok": true, "recovery_codes": codes})
}

func (b *Bot) issueRecoveryCodes(ctx context.Context, userID int64) ([]string, error) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := b.store.PutRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// proveIdentity is the check in front of every change that would let a stolen session keep the
// account: turning the second factor off, reissuing its recovery codes, setting a first password.
// The account's own password when it has one; otherwise a fresh code from the enrolled
// authenticator; otherwise — an account with neither, which is a Slack sign-in that has never
// set a password — only a session minted in the last ten minutes, because a thief holding a
// cookie found on a desk cannot show that. A code that checks out is spent, so the one just
// typed at sign-in is not good again here; failures count towards a lockout like a sign-in's.
func (b *Bot) proveIdentity(ctx context.Context, me *AdminUser, password, code string) (bool, string) {
	key := proofKey(me.ID)
	if d, locked := logins.locked(key); locked {
		return false, fmt.Sprintf("Too many failed attempts. Try again in %d seconds.", int(d.Seconds())+1)
	}
	acct, _ := b.store.User(ctx, me.ID)
	if acct == nil {
		return false, "no such account"
	}
	secret, enrol, err := b.totpSecret(ctx, me.ID)
	if err != nil {
		return false, err.Error()
	}
	enrolled := enrol != nil && enrol.Confirmed
	switch {
	case acct.HasPassword() && password != "":
		if checkPassword(acct.passwordHash, password) {
			return true, ""
		}
	case enrolled && code != "":
		if step, ok := checkTOTP(secret, code, time.Now(), enrol.LastStep); ok {
			b.store.SpendTOTPStep(ctx, me.ID, step)
			return true, ""
		}
	case !acct.HasPassword() && !enrolled:
		if age := me.sessionAge(); age >= 0 && age <= 10*time.Minute {
			return true, ""
		}
		return false, "Sign in again to do this: it needs a session that is only a few minutes old."
	}
	logins.fail(key)
	time.Sleep(400 * time.Millisecond)
	switch {
	case acct.HasPassword() && enrolled:
		return false, "That did not check out. Enter your password, or a current code from your authenticator app."
	case acct.HasPassword():
		return false, "That did not check out. Enter your password."
	default:
		return false, "That did not check out. Enter a current code from your authenticator app."
	}
}

// mayLinkIdentity guards adding a new way to sign in (a second Slack or Microsoft identity) to an
// account that is already signed in. Linking is what turns a stolen session into a permanent
// takeover — the thief attaches their own identity and comes back through it after the owner
// resets the password — so it is held to the same bar as the other account-keeping changes
// proveIdentity stands in front of. An OAuth redirect carries no password or code to check, so
// the test is the one proveIdentity accepts for an account with neither: a session minted in the
// last ten minutes. A thief holding a stale cookie cannot produce one, because minting a session
// needs the very credential they lack; a real owner has just signed in to reach the page.
func (b *Bot) mayLinkIdentity(me *AdminUser) bool {
	age := me.sessionAge()
	return age >= 0 && age <= 10*time.Minute
}

// proofKey is the lockout counter proveIdentity fails against: one per account, named here so
// that the check and anything that has to clear it cannot drift apart.
func proofKey(userID int64) string { return "proof:" + strconv.FormatInt(userID, 10) }

// proofKind names what proveIdentity will ask this account for, so a screen that is about to
// need proof can put up the field that will actually be accepted rather than guessing. The three
// answers are the three branches of the switch above: "password", "code", "recent".
func (b *Bot) proofKind(ctx context.Context, me *AdminUser) string {
	if acct, _ := b.store.User(ctx, me.ID); acct != nil && acct.HasPassword() {
		return "password"
	}
	if e, _ := b.store.TOTPEnrolment(ctx, me.ID); e != nil && e.Confirmed {
		return "code"
	}
	return "recent"
}

// handleTOTPRecovery reissues the ten codes, retiring the old set. It asks for the same proof as
// turning the factor off: ten fresh codes are ten complete bypasses of it, and the old set stops
// working the moment the new one exists — so without proof a console left signed in was a way to
// take the account and lock its owner's printed codes at the same time.
func (b *Bot) handleTOTPRecovery(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	ctx := r.Context()
	var in struct{ Password, Code string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if e, _ := b.store.TOTPEnrolment(ctx, me.ID); e == nil || !e.Confirmed {
		writeJSON(w, 400, map[string]any{"error": "Turn two-factor on first."})
		return
	}
	if ok, why := b.proveIdentity(ctx, me, in.Password, in.Code); !ok {
		writeJSON(w, 403, map[string]any{"error": why})
		return
	}
	codes, err := b.issueRecoveryCodes(ctx, me.ID)
	if err != nil {
		fail(w, err)
		return
	}
	slog.Info("recovery codes reissued", "user", me.ID, "org", me.OrgID)
	b.audit(r, "auth.recovery_codes_reissued", AuditEvent{TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email})
	writeJSON(w, 200, map[string]any{"ok": true, "recovery_codes": codes})
}

// handleTOTPDisable turns the second factor off, and asks for proof first: the account password
// when there is one, otherwise a current code. Without that, anybody who found a console left
// signed in could quietly remove the factor and keep the account.
func (b *Bot) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	ctx := r.Context()
	var in struct{ Password, Code string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if b.settings.Get(ctx, me.OrgID).RequireTwoFactor {
		writeJSON(w, 403, map[string]any{"error": "This organisation requires two-factor, so it cannot be turned off. An admin can lift the requirement in Settings → Security."})
		return
	}
	enrol, err := b.store.TOTPEnrolment(ctx, me.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if enrol == nil {
		writeJSON(w, 200, map[string]any{"ok": true}) // already off: nothing to prove, nothing to do
		return
	}
	if ok, why := b.proveIdentity(ctx, me, in.Password, in.Code); !ok {
		writeJSON(w, 403, map[string]any{"error": why})
		return
	}
	if err := b.store.DisableTOTP(ctx, me.ID); err != nil {
		fail(w, err)
		return
	}
	slog.Info("two-factor removed", "user", me.ID, "org", me.OrgID)
	b.audit(r, "auth.two_factor_disabled", AuditEvent{TargetKind: "account", TargetID: me.PublicID, TargetName: me.Email})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- the sign-in challenge ----

// handleTwoFactorLogin finishes a password sign-in that owed a second factor. It is
// unauthenticated by necessity — there is no session yet — and the challenge token is the whole
// authority: single account, five minutes, six wrong answers and it is gone.
func (b *Bot) handleTwoFactorLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Challenge, Code, Recovery string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	ctx := r.Context()
	userID, err := b.store.LoginChallenge(ctx, in.Challenge)
	if err != nil {
		fail(w, err)
		return
	}
	if userID == 0 {
		writeJSON(w, 401, map[string]any{"error": "That sign-in expired or was tried too many times. Start again."})
		return
	}
	u, _ := b.store.User(ctx, userID)
	if u == nil || u.Status != "active" {
		writeJSON(w, 401, map[string]any{"error": "That sign-in expired or was tried too many times. Start again."})
		return
	}
	// Throttle the account and the address, not just the challenge. A challenge carries its own
	// small try limit, but starting a fresh one costs nothing more than re-posting the password,
	// so the cap reset for free and a six-digit code was reachable by brute force. These keys
	// outlive any single challenge.
	totpKey, ipKey := "2fa:"+strconv.FormatInt(userID, 10), "2fa-ip:"+clientIP(r)
	for _, k := range []string{totpKey, ipKey} {
		if d, yes := logins.locked(k); yes {
			slog.Warn("second factor refused: locked out", "user", userID, "ip", clientIP(r), "for", d.Round(time.Second))
			tooMany(w, d)
			return
		}
	}
	secret, enrol, err := b.totpSecret(ctx, userID)
	if err != nil {
		fail(w, err)
		return
	}
	if enrol == nil || !enrol.Confirmed {
		writeJSON(w, 400, map[string]any{"error": "This account has no second factor. Sign in again."})
		return
	}

	used := ""
	if code := strings.TrimSpace(in.Code); code != "" {
		if step, ok := checkTOTP(secret, code, time.Now(), enrol.LastStep); ok {
			b.store.SpendTOTPStep(ctx, userID, step)
			used = "code"
		}
	}
	if used == "" && strings.TrimSpace(in.Recovery) != "" {
		if ok, _ := b.store.SpendRecoveryCode(ctx, userID, hashRecoveryCode(in.Recovery)); ok {
			used = "recovery code"
		}
	}
	if used == "" {
		logins.fail(totpKey)
		logins.fail(ipKey)
		b.auditSignInFailed(r, u, "two_factor")
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, 401, map[string]any{"error": "That code is not right."})
		return
	}
	via, err := b.store.loginChallengeVia(ctx, in.Challenge)
	if err != nil {
		fail(w, err)
		return
	}
	logins.reset(totpKey)
	b.store.DeleteLoginChallenge(ctx, in.Challenge)

	// A Slack or Microsoft sign-in that owed this code left the invitation it arrived holding
	// alone (joinOnSignIn). With an organisation to land in, the join screen is where it is
	// accepted, as after a password — signInNext below sends it there. With none, the invitation
	// is the only place to sign in to, so it is redeemed here, on the join screen's terms, now that
	// the account is proved; refused, the reason is what the person needs to hear.
	if c, err := r.Cookie(inviteCookie); err == nil && c.Value != "" && b.inNoOrganisation(ctx, u.ID) {
		if _, status, err := b.acceptInvitation(r, u, c.Value); err != nil && status == http.StatusForbidden {
			writeJSON(w, 403, map[string]any{"error": err.Error()})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: "", Path: "/", MaxAge: -1})
	}
	orgID, err := b.orgForSignIn(ctx, u, via)
	if err != nil {
		writeJSON(w, 403, map[string]any{"error": err.Error()})
		return
	}
	slog.Info("two-factor accepted", "user", u.ID, "with", used)
	b.startSession(w, r, u, orgID, via, true)
	left := b.store.RecoveryCodesLeft(ctx, userID)
	// An invitation followed here survives the second factor: the sign-in it belongs to is only
	// finishing now, so this is where the browser is told to go and join.
	writeJSON(w, 200, map[string]any{"ok": true, "verified": u.EmailVerified, "used": used,
		"recovery_left": left, "next": b.signInNext(w, r)})
}

// ---- policy ----

// orgForSignIn picks the organisation a new session starts in: the first one this person belongs
// to whose policy accepts the credential they just used. Somebody in two organisations, one of
// which has moved to Slack-only, still signs in with a password — into the other one.
func (b *Bot) orgForSignIn(ctx context.Context, u *User, via string) (int64, error) {
	ms, err := b.store.MembershipsFor(ctx, u.ID)
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 {
		return 0, errors.New("Your account is not in any organisation. Ask for an invitation.")
	}
	for _, m := range ms {
		if b.settings.Get(ctx, m.OrgID).allowsVia(via) {
			return m.OrgID, nil
		}
	}
	// Say what the organisation does accept, which is the one thing the person can act on. It used
	// to assume the only two ways in were Slack and a password, and told somebody refused for
	// signing in through their identity provider to use Slack.
	return 0, errors.New("Your organisation does not accept that way of signing in. " +
		signInHint(b.settings.Get(ctx, ms[0].OrgID).AuthPolicy))
}

// signInHint is how an organisation with this policy is signed in to.
func signInHint(policy string) string {
	switch policy {
	case AuthPolicyPassword:
		return "Sign in with your email and password instead."
	case AuthPolicySlack:
		return "Use Sign in with Slack instead."
	case AuthPolicyMicrosoft:
		return "Use Sign in with Microsoft instead."
	case AuthPolicySSO:
		return "Sign in through your organisation's identity provider instead: type your work address and choose single sign-on."
	}
	return "Try another way of signing in."
}

// twoFactorOwed says whether this request is from somebody the organisation requires a second
// factor from who has not enrolled one.
//
// Signing in with Slack used to be exempt, on the reasoning that the workspace had already done
// its own sign-in. That turned the setting into a suggestion: any member could sign out, press
// Sign in with Slack, and be back in an organisation that requires two-factor without one — and
// linking a Slack identity is something they can do for themselves under Security. An
// organisation that switches the requirement on means everybody, or it means nothing.
//
// The enrolment routes stay reachable regardless (requireAdmin lets /api/account/* through), so
// somebody held here can always enrol and carry on.
func (b *Bot) twoFactorOwed(ctx context.Context, me *AdminUser) bool {
	if me == nil || !b.settings.Get(ctx, me.OrgID).RequireTwoFactor {
		return false
	}
	e, err := b.store.TOTPEnrolment(ctx, me.ID)
	return err != nil || e == nil || !e.Confirmed || !me.MFAVerified
}

// selfLockout refuses the two settings changes that could leave nobody able to sign in and undo
// them. The rule is the same in both cases: you may only impose what you yourself already
// satisfy, so whoever turns it on is proof that at least one admin can still get in.
func (b *Bot) selfLockout(ctx context.Context, me *AdminUser, key, value string) error {
	if me == nil {
		return nil
	}
	switch {
	case key == "auth_policy" && value == AuthPolicySlack:
		ids, _ := b.store.Identities(ctx, me.ID)
		for _, id := range ids {
			if id.Provider == ProviderSlack {
				return nil
			}
		}
		return errors.New("Connect your own Slack account first, under Security → Sign-in methods. Otherwise this setting locks you out of the console the moment you sign out.")
	case key == "auth_policy" && value == AuthPolicyMicrosoft:
		ids, _ := b.store.Identities(ctx, me.ID)
		for _, id := range ids {
			if id.Provider == ProviderMicrosoft {
				return nil
			}
		}
		return errors.New("Connect your own Microsoft account first, under Security → Sign-in methods. Otherwise this setting locks you out of the console the moment you sign out.")
	case key == "auth_policy" && value == AuthPolicySSO:
		p, _ := b.store.SSOProviderForOrg(ctx, me.OrgID)
		if p == nil || !p.DomainVerified {
			return errors.New("Register your identity provider and verify its domain first, under Security → Single sign-on. Until that is done nobody can sign in through it, including you.")
		}
		ids, _ := b.store.Identities(ctx, me.ID)
		for _, id := range ids {
			if id.Provider == ProviderSSO {
				return nil
			}
		}
		return errors.New("Sign in through your identity provider once first. Otherwise this setting locks you out of the console the moment you sign out.")
	case key == "auth_policy" && value == AuthPolicyPassword:
		u, _ := b.store.User(ctx, me.ID)
		if u == nil || !u.HasPassword() {
			return errors.New("Set a password for your own account first. Without one, allowing passwords only would leave you nothing to sign in with.")
		}
	case key == "require_two_factor" && value == "1":
		e, _ := b.store.TOTPEnrolment(ctx, me.ID)
		if e == nil || !e.Confirmed {
			return errors.New("Turn on two-factor for your own account first. It is not a requirement anybody should be the exception to.")
		}
	}
	return nil
}
