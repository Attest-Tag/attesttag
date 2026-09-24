package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The console's two guards: a second factor behind the password, and the organisation's say in
// which credentials open it at all. Both are enforced on every request rather than at sign-in,
// so the tests below check the door as well as the lock.

func authReq(t *testing.T, mux *http.ServeMux, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// signupPassword is the password every account made by signedUp holds. Named, because enrolling
// a second factor now has to prove the account as well, and the proof is this.
const signupPassword = "correct horse battery"

// signedUp founds an organisation with a password account and hands back a session for it,
// signed in the way a password sign-in would have been.
func signedUp(t *testing.T, b *Bot, mux *http.ServeMux, st *Store, email string) (u *User, orgID int64, token string) {
	t.Helper()
	ctx := context.Background()
	if code, body := post(t, mux, "/api/auth/signup", map[string]string{
		"email": email, "password": signupPassword, "org": "Acme Ltd",
	}); code != 200 {
		t.Fatalf("signup = %d: %v", code, body)
	}
	u, _ = st.UserByEmail(ctx, email)
	ms, _ := st.MembershipsFor(ctx, u.ID)
	tok, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Name: u.Name, Email: u.Email, OrgID: ms[0].OrgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return u, ms[0].OrgID, tok
}

// enrol walks the real enrolment: the account proved, a secret, a code typed back, and the
// recovery codes that only exist in that one response. The proof is what turning the factor off
// and reissuing the codes have always asked for, and starting now asks for it too — enrolment
// was the entrance to the account, not a preference.
func enrol(t *testing.T, mux *http.ServeMux, token string) (secret string, recovery []string) {
	t.Helper()
	code, body := authReq(t, mux, "POST", "/api/account/totp/start", map[string]string{"password": signupPassword}, token)
	if code != 200 {
		t.Fatalf("totp start = %d: %v", code, body)
	}
	secret, _ = body["secret"].(string)
	if secret == "" {
		t.Fatalf("no secret in %v", body)
	}
	first, err := totpCodeAt(secret, totpStep(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	code, body = authReq(t, mux, "POST", "/api/account/totp/confirm", map[string]string{"code": first}, token)
	if code != 200 {
		t.Fatalf("totp confirm = %d: %v", code, body)
	}
	for _, c := range body["recovery_codes"].([]any) {
		recovery = append(recovery, c.(string))
	}
	if len(recovery) != recoveryCodeCount {
		t.Fatalf("got %d recovery codes", len(recovery))
	}
	return secret, recovery
}

func TestPasswordSignInOwesASecondFactor(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, token := signedUp(t, b, mux, st, "founder@example.com")
	secret, recovery := enrol(t, mux, token)

	// The password alone now buys a challenge, not a session.
	code, body := post(t, mux, "/api/auth/login", map[string]string{
		"email": "founder@example.com", "password": "correct horse battery",
	})
	if code != 200 || body["two_factor"] != true {
		t.Fatalf("login = %d: %v — a second factor should have been asked for", code, body)
	}
	challenge, _ := body["challenge"].(string)
	if challenge == "" {
		t.Fatal("no challenge token")
	}
	if _, ok := body["ok"]; ok {
		t.Error("the response looks like a completed sign-in")
	}

	if code, _ := post(t, mux, "/api/auth/two-factor", map[string]string{"challenge": challenge, "code": "000000"}); code != 401 {
		t.Errorf("a wrong code was answered with %d, want 401", code)
	}
	// A step ahead of the one enrolment spent: within the drift window, and not a code already
	// used — which is exactly what a phone a few seconds fast produces.
	next, _ := totpCodeAt(secret, totpStep(time.Now())+1)
	code, body = authFinish(t, mux, challenge, map[string]string{"code": next})
	if code != 200 || body["ok"] != true {
		t.Fatalf("the right code was refused: %d %v", code, body)
	}

	// The same code again, on a fresh challenge, is not a way in.
	_, body = post(t, mux, "/api/auth/login", map[string]string{"email": "founder@example.com", "password": "correct horse battery"})
	challenge, _ = body["challenge"].(string)
	if code, _ := post(t, mux, "/api/auth/two-factor", map[string]string{"challenge": challenge, "code": next}); code != 401 {
		t.Errorf("a code was accepted twice: %d", code)
	}

	// A recovery code is, once.
	code, body = authFinish(t, mux, challenge, map[string]string{"recovery": recovery[0]})
	if code != 200 || body["used"] != "recovery code" {
		t.Fatalf("recovery code refused: %d %v", code, body)
	}
	if left := st.RecoveryCodesLeft(context.Background(), 1); left != recoveryCodeCount-1 {
		t.Errorf("recovery codes left = %d, want %d", left, recoveryCodeCount-1)
	}
	_, body = post(t, mux, "/api/auth/login", map[string]string{"email": "founder@example.com", "password": "correct horse battery"})
	challenge, _ = body["challenge"].(string)
	if code, _ := post(t, mux, "/api/auth/two-factor", map[string]string{"challenge": challenge, "recovery": recovery[0]}); code != 401 {
		t.Errorf("a spent recovery code was accepted again: %d", code)
	}
}

// authFinish is the two-factor step of a sign-in, kept separate because it is the one call that
// turns a challenge into a session.
func authFinish(t *testing.T, mux *http.ServeMux, challenge string, fields map[string]string) (int, map[string]any) {
	t.Helper()
	body := map[string]string{"challenge": challenge}
	for k, v := range fields {
		body[k] = v
	}
	return post(t, mux, "/api/auth/two-factor", body)
}

func TestSignInPolicyRefusesTheWrongCredential(t *testing.T) {
	b, mux, st := identityBot(t)
	u, orgID, _ := signedUp(t, b, mux, st, "founder@example.com")
	ctx := context.Background()

	st.PutSetting(ctx, orgID, "auth_policy", AuthPolicySlack)
	b.settings.Invalidate(orgID)

	code, body := post(t, mux, "/api/auth/login", map[string]string{
		"email": "founder@example.com", "password": "correct horse battery",
	})
	if code != 403 {
		t.Fatalf("password sign-in under Slack-only = %d: %v", code, body)
	}
	// And the other way round: passwords only turns the Slack button away.
	st.PutSetting(ctx, orgID, "auth_policy", AuthPolicyPassword)
	b.settings.Invalidate(orgID)
	if _, err := b.orgForSignIn(ctx, u, "slack"); err == nil {
		t.Error("a Slack sign-in was allowed into a passwords-only organisation")
	}
	if _, err := b.orgForSignIn(ctx, u, "password"); err != nil {
		t.Errorf("a password was refused by its own policy: %v", err)
	}
}

// A live session made one way does not survive the organisation deciding on another. The
// account's own endpoints stay open, because that is where somebody fixes it.
func TestAPolicyChangeStopsTheSessionsItMeant(t *testing.T) {
	b, mux, st := identityBot(t)
	_, orgID, token := signedUp(t, b, mux, st, "founder@example.com")
	ctx := context.Background()

	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, token); code != 200 {
		t.Fatal("the session should work before the policy changes")
	}
	st.PutSetting(ctx, orgID, "auth_policy", AuthPolicySlack)
	b.settings.Invalidate(orgID)

	code, body := authReq(t, mux, "GET", "/api/settings", nil, token)
	if code != 403 || body["reauth"] != true {
		t.Errorf("a password session survived Slack-only: %d %v", code, body)
	}
	if code, _ := authReq(t, mux, "GET", "/api/account", nil, token); code != 200 {
		t.Error("the account screen should stay reachable: it is where they connect Slack")
	}
}

func TestRequiringTwoFactorHoldsTheDoorUntilEnrolled(t *testing.T) {
	b, mux, st := identityBot(t)
	_, orgID, token := signedUp(t, b, mux, st, "founder@example.com")
	ctx := context.Background()

	// An admin cannot require of everybody else what they have not done themselves.
	code, body := authReq(t, mux, "PUT", "/api/settings", map[string]string{"require_two_factor": "1"}, token)
	if code != 409 {
		t.Fatalf("requiring two-factor without holding one = %d: %v", code, body)
	}
	enrol(t, mux, token)
	if code, body := authReq(t, mux, "PUT", "/api/settings", map[string]string{"require_two_factor": "1"}, token); code != 200 {
		t.Fatalf("an enrolled admin could not turn the requirement on: %d %v", code, body)
	}

	// Somebody else in the same organisation, signed in with a password and no second factor.
	other, err := st.CreateUser(ctx, "member@example.com", "Member", "")
	if err != nil {
		t.Fatal(err)
	}
	st.AddMembership(ctx, other.ID, orgID, RoleEditor, 0)
	theirs, _ := st.CreateAdminSession(ctx, AdminUser{ID: other.ID, Email: other.Email, OrgID: orgID, Via: "password"}, time.Hour)

	code, body = authReq(t, mux, "GET", "/api/settings", nil, theirs)
	if code != 403 || body["enrol_two_factor"] != true {
		t.Errorf("an unenrolled member was let in: %d %v", code, body)
	}
	if code, _ := authReq(t, mux, "POST", "/api/account/totp/start", nil, theirs); code != 200 {
		t.Error("enrolment itself must stay reachable while the requirement holds")
	}

	// A Slack sign-in is asked too. It used to be exempt, on the reasoning that the workspace had
	// carried out its own sign-in — but linking a Slack identity is something any member can do
	// for themselves under Security, so the exemption made the organisation's setting voidable by
	// the very people it applies to: sign out, press Sign in with Slack, and the requirement is
	// gone. It requires everybody or it requires nobody.
	viaSlack, _ := st.CreateAdminSession(ctx, AdminUser{ID: other.ID, Email: other.Email, OrgID: orgID, Via: "slack"}, time.Hour)
	code, body = authReq(t, mux, "GET", "/api/settings", nil, viaSlack)
	if code != 403 || body["enrol_two_factor"] != true {
		t.Errorf("signing in with Slack side-stepped the two-factor requirement: %d %v", code, body)
	}
	// And enrolment stays reachable for them as well, or the requirement is a locked door with
	// the key inside.
	if code, _ := authReq(t, mux, "POST", "/api/account/totp/start", nil, viaSlack); code != 200 {
		t.Error("a Slack session cannot reach enrolment, so it can never satisfy the requirement")
	}
}

func TestSlackOnlyNeedsALinkedSlackAccountFirst(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, token := signedUp(t, b, mux, st, "founder@example.com")

	code, body := authReq(t, mux, "PUT", "/api/settings", map[string]string{"auth_policy": AuthPolicySlack}, token)
	if code != 409 {
		t.Fatalf("Slack-only without a linked Slack account = %d: %v", code, body)
	}
	ctx := context.Background()
	u, _ := st.UserByEmail(ctx, "founder@example.com")
	st.AddIdentity(ctx, u.ID, ProviderSlack, slackSubject("T1", "U1"))
	if code, body := authReq(t, mux, "PUT", "/api/settings", map[string]string{"auth_policy": AuthPolicySlack}, token); code != 200 {
		t.Fatalf("Slack-only with a linked account = %d: %v", code, body)
	}
}

// Turning the factor off is a deliberate act with proof attached, and it is refused outright
// while the organisation requires one.
func TestTurningTwoFactorOffNeedsProof(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, token := signedUp(t, b, mux, st, "founder@example.com")
	enrol(t, mux, token)

	if code, _ := authReq(t, mux, "POST", "/api/account/totp/disable", map[string]string{}, token); code != 403 {
		t.Error("two-factor came off with nothing to prove it was the account holder")
	}
	if code, _ := authReq(t, mux, "POST", "/api/account/totp/disable", map[string]string{"password": "not it"}, token); code != 403 {
		t.Error("a wrong password removed the second factor")
	}
	code, body := authReq(t, mux, "POST", "/api/account/totp/disable", map[string]string{"password": "correct horse battery"}, token)
	if code != 200 {
		t.Fatalf("the account holder could not remove it: %d %v", code, body)
	}
	if e, _ := st.TOTPEnrolment(context.Background(), 1); e != nil {
		t.Error("the enrolment is still there")
	}
}

// The invitation arrives as an HttpOnly cookie, so the sign-up form cannot put it in the body.
// A sign-up that carries only the cookie has to join the inviting organisation rather than ask
// for an organisation name — which is what it did before the handler read the cookie.
func TestAnInvitedSignupIsCarriedByItsCookie(t *testing.T) {
	b, mux, st := identityBot(t)
	_, orgID, _ := signedUp(t, b, mux, st, "founder@example.com")
	ctx := context.Background()

	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "joiner@example.com", Role: RoleEditor, CreatedBy: 1}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	code, body := post(t, mux, "/api/auth/signup",
		map[string]string{"email": "joiner@example.com", "password": "correct horse battery"},
		&http.Cookie{Name: inviteCookie, Value: raw})
	if code != 200 {
		t.Fatalf("invited sign-up with only the cookie = %d: %v", code, body)
	}
	u, _ := st.UserByEmail(ctx, "joiner@example.com")
	ms, _ := st.MembershipsFor(ctx, u.ID)
	if len(ms) != 1 || ms[0].OrgID != orgID || ms[0].Role != RoleEditor {
		t.Fatalf("they did not join the inviting organisation as its editor: %+v", ms)
	}
}

// An invitation names a mailbox. Holding the link is not the same as holding the mailbox — links
// are forwarded, and they sit in inboxes and chat logs — so the account accepting it has to be
// the one it was addressed to. Without this, anyone who saw the link joined the organisation.
func TestAnInvitationOnlyOpensForTheAccountItNames(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgA, _ := signedUp(t, b, mux, st, "owner@acme.example")
	_, _, strangerSession := signedUp(t, b, mux, st, "stranger@elsewhere.example")

	invited := "newhire@acme.example"
	raw, err := st.NewEmailToken(ctx, EmailToken{
		Kind: TokenInvite, OrgID: orgA, Email: invited, Role: "viewer", CreatedBy: 1,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// The stranger holds the link and is signed in as somebody else. Authenticate by bearer
	// token, not the cookie: a cookie write also has to carry a CSRF token, and a 403 from that
	// gate would make this test pass without ever reaching the check it is about.
	r := httptest.NewRequest("POST", "/api/auth/accept-invite", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+strangerSession)
	r.AddCookie(&http.Cookie{Name: inviteCookie, Value: raw})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 403 {
		t.Fatalf("a stranger accepted an invitation addressed to %s: %d %v", invited, w.Code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, invited) {
		t.Errorf("refused for the wrong reason (not the invite binding): %q", msg)
	}
	if ms, _ := st.MembershipsFor(ctx, 2); len(ms) != 1 {
		t.Errorf("the stranger joined the invited organisation anyway: %+v", ms)
	}

	// And the token is still there for the person it was actually sent to.
	if _, err := st.PeekEmailToken(ctx, TokenInvite, raw); err != nil {
		t.Errorf("the refused attempt spent the invitation: %v", err)
	}
}

// The second factor is a six-digit code, so the only thing standing between it and a brute force
// is a limit that outlives one challenge. The challenge's own try-cap did not: starting a fresh
// one costs a re-post of the password, which reset the count for free.
func TestTheSecondFactorIsRateLimitedAcrossChallenges(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	u, _, token := signedUp(t, b, mux, st, "owner@acme.example")
	enrol(t, mux, token)
	logins.reset("2fa:"+strconv.FormatInt(u.ID, 10), "2fa-ip:192.0.2.1")

	// Each round is a brand-new challenge, which is what made the per-challenge cap free.
	locked := false
	for i := 0; i < 40 && !locked; i++ {
		ch, err := st.NewLoginChallenge(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		code, _ := post(t, mux, "/api/auth/two-factor", map[string]string{"challenge": ch, "code": "000000"})
		switch code {
		case 429:
			locked = true
		case 401:
		default:
			t.Fatalf("attempt %d returned %d, want 401 or 429", i, code)
		}
	}
	if !locked {
		t.Error("forty wrong codes across fresh challenges were never rate-limited")
	}
	logins.reset("2fa:"+strconv.FormatInt(u.ID, 10), "2fa-ip:192.0.2.1")
}

// The settings screen tells an admin the functional shape of the deployment — the worker mode,
// which secrets are present — but not the raw infrastructure identifiers: the database path, the
// docs bucket, the GCP project/region/job name. The console never renders those, and keeping them
// out of an admin session's reach is one less thing a compromised session hands an attacker for
// reconnaissance. Booleans that only say a secret exists stay, because the console draws on them.
func TestSettingsHidesInfraIdentifiersFromAdmins(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, token := signedUp(t, b, mux, st, "founder@example.com")

	code, body := authReq(t, mux, "GET", "/api/settings", nil, token)
	if code != 200 {
		t.Fatalf("an admin could not read settings: %d %v", code, body)
	}
	env, ok := body["env"].(map[string]any)
	if !ok {
		t.Fatalf("settings carried no env object: %v", body)
	}
	// The founder is an owner, so the admin-only block ran: the presence flags prove it, and their
	// being here is what makes the absence of the identifiers below a real check rather than a
	// vacuous one.
	if _, present := env["secrets"]; !present {
		t.Fatal("secret-presence flags should be reported to an admin")
	}
	for _, k := range []string{"db_path", "docs_dir", "docs_bucket"} {
		if _, present := env[k]; present {
			t.Errorf("settings env still exposes the infrastructure identifier %q", k)
		}
	}
	if w, ok := env["worker"].(map[string]any); ok {
		for _, k := range []string{"project", "region", "job_name"} {
			if _, present := w[k]; present {
				t.Errorf("settings worker still exposes the infrastructure identifier %q", k)
			}
		}
	}
}
