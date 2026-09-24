package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The rule this file exists to hold: an email address is a delivery address and a label, never a
// join key. Every test below is a way somebody might try to acquire an account they do not own.

// asConsole sends r the way a browser on the deployment's own address does: to the host
// ADMIN_BASE_URL names, over https behind the proxy. A sign-in is started only from there
// (startsOnPublicOrigin); httptest's default of http://example.com is somewhere else.
func asConsole(r *http.Request) *http.Request {
	if u, err := url.Parse(os.Getenv("ADMIN_BASE_URL")); err == nil && u.Host != "" {
		r.Host = u.Host
		if u.Scheme == "https" {
			r.Header.Set("X-Forwarded-Proto", "https")
		}
	}
	return r
}

func identityBot(t *testing.T) (*Bot, *http.ServeMux, *Store) {
	t.Helper()
	// Open registration, because that is the configuration these tests are about: several
	// organisations founded by strangers signing up, which is the only way the tenant boundary
	// gets exercised. A deployment defaults to first-run instead (signupAllowed).
	return identityBotMode(t, SignupOpen)
}

func identityBotMode(t *testing.T, mode string) (*Bot, *http.ServeMux, *Store) {
	t.Helper()
	fixedMasterKey(t)
	t.Setenv("ADMIN_BASE_URL", "https://console.example.com")
	t.Setenv("SLACK_CLIENT_ID", "cid")
	t.Setenv("SLACK_CLIENT_SECRET", "csecret")
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{SignupMode: mode}
	b := &Bot{cfg: cfg, store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, cfg), resolver: NewResolver(st)}
	b.slacks = NewChatRegistry(st, sealer)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	return b, mux, st
}

func post(t *testing.T, mux *http.ServeMux, path string, body any, cookies ...*http.Cookie) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// Signing up founds an organisation, and every signup founds its own. The second one is the
// interesting half: two strangers on one deployment must end up in two separate organisations,
// each an admin of theirs alone, with no membership of the other's.
func TestEachSignupFoundsItsOwnOrg(t *testing.T) {
	_, mux, st := identityBot(t)
	ctx := context.Background()

	code, body := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "founder@example.com", "password": "correct horse battery", "name": "Founder", "org": "Acme Ltd",
	})
	if code != 200 {
		t.Fatalf("first signup = %d: %v", code, body)
	}
	if body["org"] != "Acme Ltd" {
		t.Errorf("org = %v, want the name they typed", body["org"])
	}
	u, _ := st.UserByEmail(ctx, "founder@example.com")
	if u == nil || !u.HasPassword() {
		t.Fatal("the founder should have an account with a password")
	}
	ms, _ := st.MembershipsFor(ctx, u.ID)
	if len(ms) != 1 || ms[0].Role != RoleAdmin {
		t.Fatalf("the founder should be an admin of exactly one organisation: %+v", ms)
	}
	// The slug is derived, url-safe and unique.
	org, _ := st.Org(ctx, ms[0].OrgID)
	if org.Slug != "acme-ltd" {
		t.Errorf("slug = %q", org.Slug)
	}

	// A second, unrelated signup: allowed, and it founds a separate organisation.
	code, body = post(t, mux, "/api/auth/signup", map[string]string{
		"email": "stranger@example.com", "password": "correct horse battery", "org": "Beta Inc",
	})
	if code != 200 {
		t.Fatalf("second signup = %d: %v", code, body)
	}
	if n := st.OrgCount(ctx); n != 2 {
		t.Errorf("organisations = %d, want 2", n)
	}
	stranger, _ := st.UserByEmail(ctx, "stranger@example.com")
	sms, _ := st.MembershipsFor(ctx, stranger.ID)
	if len(sms) != 1 || sms[0].Role != RoleAdmin {
		t.Fatalf("the second founder should be an admin of exactly one organisation: %+v", sms)
	}
	if sms[0].OrgID == ms[0].OrgID {
		t.Fatal("the second signup joined the first organisation instead of founding its own")
	}
	// And neither can see the other: the founder gained nothing from the stranger's signup.
	if got, _ := st.MembershipsFor(ctx, u.ID); len(got) != 1 || got[0].OrgID != ms[0].OrgID {
		t.Errorf("the first founder's memberships changed: %+v", got)
	}
}

// Signing up with an address that already has an account never merges into it — with or without
// an invitation. The account that holds the address is left exactly as it was: same id, same
// memberships, same password.
func TestSignupCannotClaimAnExistingAddress(t *testing.T) {
	_, mux, st := identityBot(t)
	ctx := context.Background()
	post(t, mux, "/api/auth/signup", map[string]string{
		"email": "owner@example.com", "password": "correct horse battery", "org": "Acme",
	})
	before, _ := st.UserByEmail(ctx, "owner@example.com")
	ms, _ := st.MembershipsFor(ctx, before.ID)

	// Plain signup on a taken address is refused as a duplicate, and founds nothing.
	taken, bodyA := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "owner@example.com", "password": "a different password", "org": "Not Acme",
	})
	if taken != 409 {
		t.Errorf("signup on a taken address = %d: %v", taken, bodyA)
	}
	if n := st.OrgCount(ctx); n != 1 {
		t.Errorf("organisations = %d — the refused signup founded one anyway", n)
	}

	// An invitation is not a way around it either: redeeming one for an address that already has
	// an account must not hand that account to whoever holds the link.
	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: ms[0].OrgID,
		Email: "owner@example.com", Role: RoleViewer, CreatedBy: before.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	code, body := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "owner@example.com", "password": "a different password", "invite": raw,
	})
	if code != 409 {
		t.Fatalf("re-signup with an invitation = %d: %v", code, body)
	}
	after, _ := st.UserByEmail(ctx, "owner@example.com")
	if after.ID != before.ID {
		t.Fatal("a second signup replaced the account that already held that address")
	}
	if got, _ := st.MembershipsFor(ctx, after.ID); len(got) != len(ms) {
		t.Errorf("the existing account gained a membership it never asked for: %+v", got)
	}
}

// The attack the whole design is shaped around: a Slack workspace admin can set any email
// address on any of their users, so a Slack sign-in must never adopt an account by address.
func TestSlackSignInCannotAdoptAPasswordAccount(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()

	post(t, mux, "/api/auth/signup", map[string]string{
		"email": "victim@example.com", "password": "correct horse battery", "org": "Victim Co",
	})
	victim, _ := st.UserByEmail(ctx, "victim@example.com")
	victimOrgs, _ := st.MembershipsFor(ctx, victim.ID)

	// An attacker who controls their own Slack workspace signs in, asserting the victim's address.
	id := &slackIdentity{UserID: "U_ATTACKER", TeamID: "T_ATTACKER", TeamName: "Attacker Inc",
		Name: "Mallory", Email: "victim@example.com"}
	_, err := b.slackSignup(ctx, id, "", "")
	if err == nil {
		t.Fatal("a Slack sign-in claiming an existing address must be refused")
	}
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("the refusal should say how to connect the two properly: %v", err)
	}

	// Nothing about the victim changed: no new identity, no new membership.
	if u, _ := st.UserByIdentity(ctx, ProviderSlack, slackSubject("T_ATTACKER", "U_ATTACKER")); u != nil {
		t.Fatal("the attacker's Slack identity was attached to an account")
	}
	after, _ := st.MembershipsFor(ctx, victim.ID)
	if len(after) != len(victimOrgs) {
		t.Error("the victim's memberships changed")
	}
}

// The same rule in the other direction, and the safe path that replaces it: linking happens from
// inside a session that already holds both credentials.
func TestSlackIdentityLinksOnlyFromInsideTheAccount(t *testing.T) {
	_, _, st := identityBot(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "person@example.com", "Person", "")
	st.AddIdentity(ctx, u.ID, ProviderPassword, "1")

	// Linking is an explicit write, and it is what the Settings flow performs.
	if err := st.AddIdentity(ctx, u.ID, ProviderSlack, slackSubject("T1", "U1")); err != nil {
		t.Fatalf("linking from inside the account should work: %v", err)
	}
	if got, _ := st.UserByIdentity(ctx, ProviderSlack, slackSubject("T1", "U1")); got == nil || got.ID != u.ID {
		t.Fatal("the linked identity does not resolve to the account")
	}
	// A Slack identity belongs to one account. Moving it is refused rather than silently reassigned.
	other, _ := st.CreateUser(ctx, "other@example.com", "Other", "")
	if err := st.AddIdentity(ctx, other.ID, ProviderSlack, slackSubject("T1", "U1")); err == nil {
		t.Fatal("the same Slack identity was attached to a second account")
	}
}

// An invitation is addressed to one mailbox. A forwarded email must not become an account in
// somebody else's organisation.
func TestInviteIsBoundToItsAddress(t *testing.T) {
	_, mux, st := identityBot(t)
	ctx := context.Background()
	post(t, mux, "/api/auth/signup", map[string]string{
		"email": "boss@example.com", "password": "correct horse battery", "org": "Acme",
	})
	boss, _ := st.UserByEmail(ctx, "boss@example.com")
	ms, _ := st.MembershipsFor(ctx, boss.ID)
	orgID := ms[0].OrgID

	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "colleague@example.com", Role: RoleEditor, CreatedBy: boss.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Somebody the mail was forwarded to tries to use it.
	code, body := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "outsider@example.com", "password": "correct horse battery", "invite": raw,
	})
	if code != 400 {
		t.Fatalf("redeeming with another address = %d: %v", code, body)
	}
	if n := st.OrgCount(ctx); n != 1 {
		t.Errorf("organisations = %d", n)
	}

	// The person it was addressed to can, and their address counts as proved by the delivery.
	code, body = post(t, mux, "/api/auth/signup", map[string]string{
		"email": "colleague@example.com", "password": "correct horse battery", "invite": raw,
	})
	if code != 200 {
		t.Fatalf("redeeming as the invitee = %d: %v", code, body)
	}
	joined, _ := st.UserByEmail(ctx, "colleague@example.com")
	if !joined.EmailVerified {
		t.Error("an invitation that reached the mailbox proves the address")
	}
	got, _ := st.MembershipsFor(ctx, joined.ID)
	if len(got) != 1 || got[0].OrgID != orgID || got[0].Role != RoleEditor {
		t.Fatalf("membership = %+v, want editor of the inviting organisation", got)
	}
	// And it is spent.
	code, _ = post(t, mux, "/api/auth/signup", map[string]string{
		"email": "colleague@example.com", "password": "correct horse battery", "invite": raw,
	})
	if code == 200 {
		t.Error("the invitation was redeemable twice")
	}
}

// Signing in must not say which addresses have accounts, and a wrong password must look exactly
// like an unknown address.
func TestLoginIsNotAnAccountOracle(t *testing.T) {
	_, mux, _ := identityBot(t)
	post(t, mux, "/api/auth/signup", map[string]string{
		"email": "real@example.com", "password": "correct horse battery", "org": "Acme",
	})

	wrongPw, bodyA := post(t, mux, "/api/auth/login", map[string]string{
		"email": "real@example.com", "password": "not the password",
	})
	unknown, bodyB := post(t, mux, "/api/auth/login", map[string]string{
		"email": "nobody@example.com", "password": "not the password",
	})
	if wrongPw != unknown {
		t.Errorf("status differs: %d for a wrong password, %d for an unknown address", wrongPw, unknown)
	}
	if bodyA["error"] != bodyB["error"] {
		t.Errorf("message differs:\n  %v\n  %v", bodyA["error"], bodyB["error"])
	}
	if wrongPw != 401 {
		t.Errorf("want 401, got %d", wrongPw)
	}
}

// Asking for a reset says the same thing either way, for the same reason.
func TestForgotPasswordIsNotAnAccountOracle(t *testing.T) {
	_, mux, _ := identityBot(t)
	post(t, mux, "/api/auth/signup", map[string]string{
		"email": "real@example.com", "password": "correct horse battery", "org": "Acme",
	})
	a, bodyA := post(t, mux, "/api/auth/forgot", map[string]string{"email": "real@example.com"})
	b2, bodyB := post(t, mux, "/api/auth/forgot", map[string]string{"email": "nobody@example.com"})
	if a != b2 || a != 200 {
		t.Errorf("statuses %d and %d", a, b2)
	}
	if len(bodyA) != len(bodyB) {
		t.Errorf("bodies differ: %v vs %v", bodyA, bodyB)
	}
}

// A reset is what somebody does when they think the account is compromised, so it has to end the
// attacker's session as well as change the secret.
func TestResetEndsEveryOtherSession(t *testing.T) {
	_, mux, st := identityBot(t)
	ctx := context.Background()
	post(t, mux, "/api/auth/signup", map[string]string{
		"email": "person@example.com", "password": "correct horse battery", "org": "Acme",
	})
	u, _ := st.UserByEmail(ctx, "person@example.com")
	stolen, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Email: u.Email, OrgID: 1}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.AdminSession(ctx, stolen); got == nil {
		t.Fatal("the session should exist before the reset")
	}

	raw, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenReset, UserID: u.ID, Email: u.Email}, time.Hour)
	code, body := post(t, mux, "/api/auth/reset", map[string]string{"token": raw, "password": "a brand new password"})
	if code != 200 {
		t.Fatalf("reset = %d: %v", code, body)
	}
	if got, _ := st.AdminSession(ctx, stolen); got != nil {
		t.Fatal("a session survived the password reset")
	}
	// The new password works and the old one does not.
	if code, _ := post(t, mux, "/api/auth/login", map[string]string{
		"email": "person@example.com", "password": "correct horse battery"}); code == 200 {
		t.Error("the old password still works")
	}
	if code, _ := post(t, mux, "/api/auth/login", map[string]string{
		"email": "person@example.com", "password": "a brand new password"}); code != 200 {
		t.Error("the new password does not work")
	}
}

// bcrypt refuses more than 72 bytes rather than truncating, so the limit is checked in words
// before hashing — and counted in bytes, because emoji reach it in eighteen characters.
func TestPasswordLengthRules(t *testing.T) {
	if p := passwordProblem("short"); p == "" {
		t.Error("a short password should be refused")
	}
	if p := passwordProblem("a perfectly ordinary password"); p != "" {
		t.Errorf("an ordinary password was refused: %s", p)
	}
	if p := passwordProblem(strings.Repeat("x", 73)); p == "" {
		t.Error("73 bytes should be refused, since bcrypt errors rather than truncating")
	}
	// 25 emoji is 100 bytes but only 25 characters: the check must count bytes.
	if p := passwordProblem(strings.Repeat("😀", 25)); p == "" {
		t.Error("a password over 72 bytes should be refused however few characters it is")
	}
	// And a hash of an accepted password round-trips.
	h, err := hashPassword("a perfectly ordinary password")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(h, "a perfectly ordinary password") {
		t.Error("the password does not verify against its own hash")
	}
	if checkPassword(h, "a perfectly ordinary passworD") {
		t.Error("a different password verified")
	}
	if checkPassword("", "anything") || checkPassword(h, "") {
		t.Error("an empty hash or password must never verify")
	}
}

// Authority is the membership. A session naming an organisation the user does not belong to
// holds nothing, rather than falling back to a role.
func TestPermissionsComeFromMembershipAlone(t *testing.T) {
	b, _, st := identityBot(t)
	ctx := context.Background()
	orgID, userID, _ := seedOrg(t, st, RoleAdmin)

	me := &AdminUser{ID: userID, OrgID: orgID}
	b.permissionsFor(ctx, me)
	if me.Role != RoleAdmin || !me.Permissions[PermUsersManage] {
		t.Fatalf("an admin of the organisation should hold its permissions: %+v", me)
	}

	// Same person, an organisation they are not in.
	outsider, _ := st.CreateUser(ctx, "outsider@example.com", "Outsider", "")
	otherOrg, _ := st.CreateOrg(ctx, "Somebody Else", outsider.ID)
	me2 := &AdminUser{ID: userID, OrgID: otherOrg.ID}
	b.permissionsFor(ctx, me2)
	if me2.Role != "" || len(me2.Permissions) != 0 {
		t.Fatalf("a session naming an organisation they do not belong to holds nothing: %+v", me2)
	}

	// And a session with no organisation at all.
	me3 := &AdminUser{ID: userID}
	b.permissionsFor(ctx, me3)
	if len(me3.Permissions) != 0 {
		t.Fatalf("a session with no organisation holds nothing: %+v", me3)
	}
}

// Switching organisation is checked against the membership, not taken on trust from the body.
func TestSwitchOrgRequiresMembership(t *testing.T) {
	_, mux, st := identityBot(t)
	ctx := context.Background()
	_, userID, token := seedOrg(t, st, RoleAdmin)
	outsider, _ := st.CreateUser(ctx, "outsider@example.com", "Outsider", "")
	theirs, _ := st.CreateOrg(ctx, "Not Yours", outsider.ID)

	r := httptest.NewRequest("POST", "/api/auth/switch-org", strings.NewReader(`{"org_id":"`+theirs.PublicID+`"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("switching into an organisation they do not belong to = %d, want 403", w.Code)
	}
	// The session is untouched.
	if u, _ := st.AdminSession(ctx, token); u == nil || u.OrgID == theirs.ID {
		t.Error("the refused switch changed the session anyway")
	}
	_ = userID
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The zone a person signs up from becomes their organisation's, because a workspace's "today"
// should be theirs and not the deployment's. The second half is the one that bites: somebody
// accepting an invitation must not move the clock for everybody already there.
func TestFoundingSignupTakesItsTimeZoneFromTheBrowser(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()

	code, body := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "founder@example.com", "password": "correct horse battery", "org": "Acme Ltd",
		"tz": "America/New_York",
	})
	if code != 200 {
		t.Fatalf("signup = %d: %v", code, body)
	}
	u, _ := st.UserByEmail(ctx, "founder@example.com")
	ms, _ := st.MembershipsFor(ctx, u.ID)
	orgID := ms[0].OrgID
	if got := b.settings.Get(ctx, orgID).Timezone; got != "America/New_York" {
		t.Errorf("timezone = %q, want the zone the browser reported", got)
	}

	// Joining is not founding: the invitation's organisation keeps the zone it was founded with.
	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "joiner@example.com", Role: RoleEditor, CreatedBy: u.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	code, body = post(t, mux, "/api/auth/signup", map[string]string{
		"email": "joiner@example.com", "password": "correct horse battery", "invite": raw,
		"tz": "Asia/Tokyo",
	})
	if code != 200 {
		t.Fatalf("invited signup = %d: %v", code, body)
	}
	b.settings.Invalidate(orgID)
	if got := b.settings.Get(ctx, orgID).Timezone; got != "America/New_York" {
		t.Errorf("timezone = %q: an invited member moved the organisation's clock", got)
	}
}

// A zone we cannot load is not stored: the setting stays unset and TZ_NAME answers for it,
// which is what a browser that says nothing (or something made up) should cost.
func TestASignupWithAnUnknownZoneFallsBack(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	b.settings.cfg = Config{Timezone: "Asia/Kathmandu"}

	post(t, mux, "/api/auth/signup", map[string]string{
		"email": "founder@example.com", "password": "correct horse battery", "org": "Acme Ltd",
		"tz": "Mars/Olympus_Mons",
	})
	u, _ := st.UserByEmail(ctx, "founder@example.com")
	ms, _ := st.MembershipsFor(ctx, u.ID)
	if got := b.settings.Get(ctx, ms[0].OrgID).Timezone; got != "Asia/Kathmandu" {
		t.Errorf("timezone = %q, want the configured default", got)
	}
}

// SIGNUP_MODE is the door. The default is first-run: whoever installs the deployment signs up
// once and founds it, and everybody after that arrives by invitation. It matters because the
// deployment nobody configured is a self-host on a public address, and until this existed the
// only answer the code had was "open".
func TestSignupModeDecidesWhoMayFound(t *testing.T) {
	signups = newRateLimiter()
	founder := func(mux *http.ServeMux, email string) int {
		code, _ := post(t, mux, "/api/auth/signup", map[string]string{
			"email": email, "password": "correct horse battery", "org": "Org"})
		return code
	}

	t.Run("first-run founds once", func(t *testing.T) {
		signups = newRateLimiter()
		b, mux, _ := identityBotMode(t, SignupFirstRun)
		if code := founder(mux, "first@example.com"); code != 200 {
			t.Fatalf("the founding sign-up was refused: %d", code)
		}
		// And the door shuts behind them: the second stranger is turned away, and /api/me says
		// so before the form is drawn rather than after it is filled in.
		if code := founder(mux, "second@example.com"); code != 403 {
			t.Errorf("a second stranger founded an organisation on a first-run deployment: %d", code)
		}
		if b.signupAllowed(context.Background()) {
			t.Error("signup_open stayed true after the deployment was founded")
		}
	})

	t.Run("closed refuses even the first", func(t *testing.T) {
		signups = newRateLimiter()
		_, mux, _ := identityBotMode(t, SignupClosed)
		if code := founder(mux, "first@example.com"); code != 403 {
			t.Errorf("a closed deployment accepted a sign-up: %d", code)
		}
	})

	t.Run("open lets strangers keep founding", func(t *testing.T) {
		signups = newRateLimiter()
		_, mux, _ := identityBotMode(t, SignupOpen)
		if code := founder(mux, "first@example.com"); code != 200 {
			t.Fatalf("open refused the first sign-up: %d", code)
		}
		if code := founder(mux, "second@example.com"); code != 200 {
			t.Errorf("open refused the second sign-up: %d", code)
		}
	})

	// A typo must not open the door. Unrecognised values read as first-run, the restrictive one.
	for _, v := range []string{"", "opne", "OPEN", "yes", "true", "first-run", "CLOSED"} {
		want := SignupFirstRun
		switch strings.ToLower(v) {
		case "open":
			want = SignupOpen
		case "closed":
			want = SignupClosed
		}
		if got := signupModeOf(v); got != want {
			t.Errorf("signupModeOf(%q) = %q, want %q", v, got, want)
		}
	}
}
