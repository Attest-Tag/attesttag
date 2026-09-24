package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Only a share link narrowed to a domain and addressed to nobody is a domain link: the one whose
// holder types the address themselves, so it proves nothing about the mailbox and has to wait for
// our own verification mail before it joins an account. An invitation addressed to a mailbox or a
// Slack id is proof by delivery; a share link with no domain admits anyone by design.
func TestDomainLinkPredicate(t *testing.T) {
	cases := []struct {
		t    EmailToken
		want bool
	}{
		{EmailToken{Domain: "corp.com"}, true},
		{EmailToken{Email: "a@corp.com", Domain: "corp.com"}, false},
		{EmailToken{SlackUserID: "U1", Domain: "corp.com"}, false},
		{EmailToken{}, false},
	}
	for _, c := range cases {
		if got := c.t.domainLink(); got != c.want {
			t.Errorf("domainLink(%+v) = %v, want %v", c.t, got, c.want)
		}
	}
}

// domainLinkFor mints a share link into org that takes addresses at domain, the way the Users tab
// makes one, and returns the raw token a browser would carry in the invitation cookie.
func domainLinkFor(t *testing.T, st *Store, org, by int64, domain string) string {
	t.Helper()
	raw, err := st.NewEmailToken(context.Background(), EmailToken{Kind: TokenInvite, OrgID: org, Role: RoleEditor,
		Domain: domain, MaxUses: UsesUnlimited, CreatedBy: by}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// linkUses is how many memberships a link has produced so far.
func linkUses(t *testing.T, st *Store, raw string) int {
	t.Helper()
	tok, err := st.PeekEmailToken(context.Background(), TokenInvite, raw)
	if err != nil {
		t.Fatalf("the link is gone: %v", err)
	}
	return tok.Uses
}

// clearsInvite reports whether a response told the browser to drop the invitation cookie.
func clearsInvite(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == inviteCookie && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// slackSignIn walks Sign in with Slack through the real routes against the fake, carrying any
// cookies the browser already had — an invitation, above all.
func slackSignIn(t *testing.T, mux *http.ServeMux, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, asConsole(httptest.NewRequest("GET", "/api/auth/login", nil)))
	loc, err := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || err != nil {
		t.Fatalf("slack login = %d: %s", w.Code, w.Body.String())
	}
	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == stateCookie {
			state = c
		}
	}
	if state == nil {
		t.Fatal("no state cookie was set")
	}
	cb := httptest.NewRequest("GET", "/api/auth/callback?code=good-code&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	cb.AddCookie(state)
	for _, c := range cookies {
		cb.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, asConsole(cb))
	return rec
}

// invitePreview is what the join screen reads, for a session holding the invitation cookie.
func invitePreview(t *testing.T, mux *http.ServeMux, token string, invite *http.Cookie) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/auth/invite", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.AddCookie(invite)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// The attack the domain rule on the join screen was never reaching: sign up with a password as
// somebody at the victim's domain — nothing checks the address at that point — connect your own
// Slack account to it, then sign in with Slack holding a leaked domain-limited link. The link asks
// for a confirmed address wherever an existing account redeems it, and a refusal leaves both the
// link and the cookie alone, so the sign-in lands on the join screen, which says why.
func TestADomainLinkNeedsAConfirmedAddressOnASlackSignIn(t *testing.T) {
	b, mux, st := identityBot(t)
	fs := &fakeSlack{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	old := slackOIDC
	slackOIDC = srv.URL
	t.Cleanup(func() { slackOIDC = old })
	ctx := context.Background()

	founder, victimOrg, _ := signedUp(t, b, mux, st, "founder@victim.test")
	raw := domainLinkFor(t, st, victimOrg, founder.ID, "victim.test")
	invite := &http.Cookie{Name: inviteCookie, Value: raw}

	// The attacker's account, at the victim's domain and never confirmed, with their own Slack
	// account connected — whose workspace knows them by an address of their own.
	attacker, _, token := signedUp(t, b, mux, st, "x@victim.test")
	if err := st.AddIdentity(ctx, attacker.ID, ProviderSlack, slackSubject("T9", "U9")); err != nil {
		t.Fatal(err)
	}
	fs.info = map[string]any{"ok": true, "sub": "U9", "name": "X", "email": "x@attacker.test", "email_verified": true,
		"https://slack.com/team_id": "T9", "https://slack.com/user_id": "U9"}

	rec := slackSignIn(t, mux, invite)
	if signedInAs(t, st, rec) == nil {
		t.Fatalf("the sign-in itself should still go through: %s", authError(rec))
	}
	if m, _ := st.Membership(ctx, attacker.ID, victimOrg); m != nil {
		t.Fatal("an unconfirmed address joined through a domain-limited link by signing in with Slack")
	}
	if got := rec.Header().Get("Location"); got != "/admin/join/" {
		t.Errorf("a refused invitation should finish on the join screen, which says why; went to %q", got)
	}
	if clearsInvite(rec) {
		t.Error("the refusal threw the invitation cookie away, so the join screen has nothing to explain")
	}
	if n := linkUses(t, st, raw); n != 0 {
		t.Errorf("a refusal spent %d of the link's uses", n)
	}
	// The join screen asks the same question before the button is pressed, and answers with the
	// step that puts it right — a confirmation mail — rather than an organisation to join.
	code, body := invitePreview(t, mux, token, invite)
	if code != 200 || body["confirm_email"] != true || body["org"] != nil {
		t.Errorf("the join screen offered the link to an unconfirmed address: %d %v", code, body)
	}

	// Confirmed, the same sign-in joins, spends one use and lets the cookie go.
	if err := st.SetEmailVerified(ctx, attacker.ID, emailByMail, 0); err != nil {
		t.Fatal(err)
	}
	if code, body := invitePreview(t, mux, token, invite); code != 200 {
		t.Errorf("a confirmed address was refused by the join screen: %d %v", code, body)
	}
	rec = slackSignIn(t, mux, invite)
	if m, _ := st.Membership(ctx, attacker.ID, victimOrg); m == nil || m.Role != RoleEditor {
		t.Fatalf("a confirmed address did not join as the link's role: %+v (%s)", m, authError(rec))
	}
	if n := linkUses(t, st, raw); n != 1 {
		t.Errorf("the join spent %d uses, want 1", n)
	}
	if !clearsInvite(rec) {
		t.Error("a spent invitation should not follow the browser around")
	}
	if got := rec.Header().Get("Location"); got == "/admin/join/" {
		t.Error("a sign-in that joined was still sent to the join screen")
	}
}

// The same door through Microsoft, where the address a token carries is whatever the tenant's own
// admin set — so it is not what confirms anything here either.
func TestADomainLinkNeedsAConfirmedAddressOnAMicrosoftSignIn(t *testing.T) {
	b, mux, f, st := microsoftBot(t, msSignInAnyOrganisation)
	ctx := context.Background()
	founder, victimOrg, _ := signedUp(t, b, mux, st, "founder@victim.test")
	raw := domainLinkFor(t, st, victimOrg, founder.ID, "victim.test")
	invite := &http.Cookie{Name: inviteCookie, Value: raw}

	// A tenant that says its user is x@victim.test: the account is made, founding an organisation
	// of its own, with the address unconfirmed.
	first := microsoftSignIn(t, mux, f, map[string]any{"email": "x@victim.test"})
	me := signedInAs(t, st, first)
	if me == nil {
		t.Fatalf("no account: %s", authError(first))
	}

	rec := microsoftSignIn(t, mux, f, nil, invite)
	if signedInAs(t, st, rec) == nil {
		t.Fatalf("the sign-in itself should still go through: %s", authError(rec))
	}
	if m, _ := st.Membership(ctx, me.ID, victimOrg); m != nil {
		t.Fatal("an address a tenant asserted joined through a domain-limited link")
	}
	if got := rec.Header().Get("Location"); got != "/admin/join/" || clearsInvite(rec) {
		t.Errorf("a refused invitation should finish on the join screen with its cookie: went to %q", got)
	}
	if n := linkUses(t, st, raw); n != 0 {
		t.Errorf("a refusal spent %d of the link's uses", n)
	}

	if err := st.SetEmailVerified(ctx, me.ID, emailByMail, 0); err != nil {
		t.Fatal(err)
	}
	rec = microsoftSignIn(t, mux, f, nil, invite)
	if m, _ := st.Membership(ctx, me.ID, victimOrg); m == nil {
		t.Fatalf("a confirmed address did not join: %s", authError(rec))
	}
	if n := linkUses(t, st, raw); n != 1 || !clearsInvite(rec) {
		t.Errorf("after joining: %d uses, cookie cleared %v", n, clearsInvite(rec))
	}
}

// signUpThroughLink signs somebody up with a password while holding an invitation, the way the
// form does, and returns the token from the confirmation mail they were sent.
func signUpThroughLink(t *testing.T, mux *http.ServeMux, caught *catchMailer, email, raw string) string {
	t.Helper()
	code, out := post(t, mux, "/api/auth/signup", map[string]string{
		"email": email, "password": "a quiet harbour lantern", "name": "Colleague"},
		&http.Cookie{Name: inviteCookie, Value: raw})
	if code != 200 || out["verify_to_join"] != true {
		t.Fatalf("a domain-link sign-up for %s = %d %v, want one waiting on its mail", email, code, out)
	}
	body := caught.sent[len(caught.sent)-1].Body
	_, rest, ok := strings.Cut(body, "/admin/verify/?token=")
	if !ok {
		t.Fatalf("no confirmation link in the mail:\n%s", body)
	}
	return strings.TrimRightFunc(rest[:48], func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) })
}

// answer clicks the confirmation link.
func answer(t *testing.T, mux *http.ServeMux, token string) map[string]any {
	t.Helper()
	code, out := post(t, mux, "/api/auth/verify", map[string]string{"token": token})
	if code != 200 {
		t.Fatalf("confirming an address = %d %v", code, out)
	}
	return out
}

// A password sign-up through a domain link joins when its confirmation mail is answered, and that
// is when the link is redeemed — spent, and asked whether it still allows the join. It used to
// join from a copy of the link written onto the confirmation, so a link for one person let in
// everybody who signed up through it, and withdrawing or letting it expire stopped nobody who had
// already filled the form in.
func TestADomainLinkSignUpRedeemsTheLinkWhenTheMailIsAnswered(t *testing.T) {
	b, mux, st := identityBot(t)
	caught := &catchMailer{}
	b.mail = caught
	ctx := context.Background()
	founder, org, _ := signedUp(t, b, mux, st, "founder@victim.test")
	member := func(email string) *Membership {
		t.Helper()
		u, _ := st.UserByEmail(ctx, email)
		if u == nil {
			t.Fatalf("no account for %s", email)
		}
		m, _ := st.Membership(ctx, u.ID, org)
		return m
	}

	// A link for one person, and two people who sign up through it before either confirms.
	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: org, Role: RoleEditor,
		Domain: "victim.test", MaxUses: 1, CreatedBy: founder.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first := signUpThroughLink(t, mux, caught, "first@victim.test", raw)
	second := signUpThroughLink(t, mux, caught, "second@victim.test", raw)
	if n := linkUses(t, st, raw); n != 0 {
		t.Fatalf("signing up spent %d uses before anybody confirmed anything", n)
	}
	if out := answer(t, mux, first); out["joined"] != true {
		t.Fatalf("the first to confirm did not join: %v", out)
	}
	if m := member("first@victim.test"); m == nil || m.Role != RoleEditor {
		t.Fatalf("joined as %+v, want the link's role", m)
	}
	out := answer(t, mux, second)
	if msg, _ := out["join_error"].(string); out["joined"] != false || !strings.Contains(msg, "have not joined") {
		t.Errorf("a link for one person let in a second: %v", out)
	}
	if member("second@victim.test") != nil {
		t.Fatal("the link's use limit did not bind")
	}
	if u, _ := st.UserByEmail(ctx, "second@victim.test"); u == nil || !u.EmailVerified {
		t.Error("a refused join should still leave the address confirmed; the mail was answered")
	}

	// Withdrawn after the form was filled in, and let run out: neither lets anybody in.
	resetLimiters() // five sign-ups an hour from one address, and this test is all one address
	open := domainLinkFor(t, st, org, founder.ID, "victim.test")
	withdrawn := signUpThroughLink(t, mux, caught, "withdrawn@victim.test", open)
	pending, _ := st.PendingInvitesFor(ctx, org)
	for _, p := range pending {
		if err := st.RevokeInviteByID(ctx, org, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if out := answer(t, mux, withdrawn); out["joined"] != false || member("withdrawn@victim.test") != nil {
		t.Errorf("a join confirmed after its link was withdrawn still happened: %v", out)
	}
	stale := domainLinkFor(t, st, org, founder.ID, "victim.test")
	expired := signUpThroughLink(t, mux, caught, "expired@victim.test", stale)
	if _, err := st.db.ExecContext(ctx, `update email_tokens set expires_at=? where token_hash=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.DateTime), hashEmailToken(stale)); err != nil {
		t.Fatal(err)
	}
	if out := answer(t, mux, expired); out["joined"] != false || member("expired@victim.test") != nil {
		t.Errorf("a join confirmed after its link expired still happened: %v", out)
	}

	// A confirmation parked before it named its link has nothing to redeem, so it joins nothing.
	u, err := st.CreateUser(ctx, "parked@victim.test", "Parked", "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenVerify, UserID: u.ID, Email: u.Email,
		OrgID: org, Role: RoleAdmin, CreatedBy: founder.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if out := answer(t, mux, old); out["joined"] != false || member("parked@victim.test") != nil {
		t.Errorf("a confirmation that names no link joined from what it remembered: %v", out)
	}
}

// Which confirmations a domain-limited link accepts. Every source that sets email_verified is here,
// so a new one has to be decided on rather than inherited.
func TestADomainLinkAcceptsOnlyProofItsRedeemerCannotArrange(t *testing.T) {
	const org = 7
	for _, c := range []struct {
		name string
		u    User
		want bool
	}{
		{"never confirmed", User{}, false},
		{"a link we mailed", User{EmailVerified: true, emailVerifiedBy: emailByMail}, true},
		{"single sign-on at the address's own proved domain", User{EmailVerified: true, emailVerifiedBy: emailBySSO}, true},
		{"an invitation from the link's own organisation", User{EmailVerified: true, emailVerifiedBy: emailByInvite, emailVerifiedOrg: org}, true},
		{"an invitation from any other organisation", User{EmailVerified: true, emailVerifiedBy: emailByInvite, emailVerifiedOrg: 8}, false},
		{"Slack's word for it", User{EmailVerified: true, emailVerifiedBy: emailBySlack}, false},
		{"confirmed before the source was recorded", User{EmailVerified: true}, true},
	} {
		if got := confirmedForDomainLink(&c.u, org); got != c.want {
			t.Errorf("%s: accepted = %v, want %v", c.name, got, c.want)
		}
	}
}

// What confirmed an address is only ever upgraded, to our own mail — never replaced by something
// weaker that happens to run later.
func TestAMailedConfirmationIsNeverDowngraded(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "dana@victim.test", "Dana", "")
	if err != nil {
		t.Fatal(err)
	}
	source := func() (string, int64) {
		t.Helper()
		got, _ := st.User(ctx, u.ID)
		return got.emailVerifiedBy, got.emailVerifiedOrg
	}
	st.SetEmailVerified(ctx, u.ID, emailBySlack, 0)
	if by, _ := source(); by != emailBySlack {
		t.Fatalf("recorded %q, want slack", by)
	}
	st.SetEmailVerified(ctx, u.ID, emailByMail, 0)
	st.SetEmailVerified(ctx, u.ID, emailByInvite, 9)
	if by, org := source(); by != emailByMail || org != 0 {
		t.Errorf("a later, weaker confirmation replaced our mail's: %q from org %d", by, org)
	}
}

// mailedVerifyToken is the token in the last confirmation mail sent to an address.
func mailedVerifyToken(t *testing.T, caught *catchMailer, to string) string {
	t.Helper()
	for i := len(caught.sent) - 1; i >= 0; i-- {
		if caught.sent[i].To != to {
			continue
		}
		if _, rest, ok := strings.Cut(caught.sent[i].Body, "/admin/verify/?token="); ok {
			return rest[:48]
		}
	}
	t.Fatalf("no confirmation mail went to %s", to)
	return ""
}

// acceptInvite presses Accept on the join screen for a session holding the invitation cookie.
func acceptInvite(t *testing.T, mux *http.ServeMux, token string, invite *http.Cookie) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/auth/accept-invite", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(invite)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// The cheapest way round the domain rule needed nothing but an account: found an organisation,
// invite the victim's address into it, and redeem the link yourself — the inviter is always handed
// the link — and the address was "confirmed". It still is, for everything that only asks whether
// somebody stands behind it; a domain link now asks how, and sends its owner a mail that settles it.
func TestASelfIssuedInvitationDoesNotOpenADomainLink(t *testing.T) {
	b, mux, st := identityBot(t)
	caught := &catchMailer{}
	b.mail = caught
	ctx := context.Background()
	founder, victimOrg, _ := signedUp(t, b, mux, st, "founder@victim.test")
	invite := &http.Cookie{Name: inviteCookie, Value: domainLinkFor(t, st, victimOrg, founder.ID, "victim.test")}

	attacker, _, attackerSession := signedUp(t, b, mux, st, "attacker@elsewhere.test")
	st.SetEmailVerified(ctx, attacker.ID, emailByMail, 0) // their own, real, address
	code, body := authReq(t, mux, "POST", "/api/console/invites", map[string]string{"email": "x@victim.test"}, attackerSession)
	link, _ := body["link"].(string)
	if code != 200 || link == "" {
		t.Fatalf("inviting = %d %v", code, body)
	}
	resetLimiters()
	if code, body := post(t, mux, "/api/auth/signup", map[string]string{"email": "x@victim.test",
		"password": "a quiet harbour lantern", "invite": link[strings.LastIndex(link, "/")+1:]}); code != 200 {
		t.Fatalf("redeeming it = %d %v", code, body)
	}
	x, _ := st.UserByEmail(ctx, "x@victim.test")
	if !x.EmailVerified {
		t.Fatal("an addressed invitation still confirms an address everywhere else; this test assumes it")
	}
	ms, _ := st.MembershipsFor(ctx, x.ID)
	session, err := st.CreateAdminSession(ctx, AdminUser{ID: x.ID, Email: x.Email, OrgID: ms[0].OrgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if code, body := invitePreview(t, mux, session, invite); code != 200 || body["confirm_email"] != true {
		t.Errorf("the join screen should ask for a mailed confirmation: %d %v", code, body)
	}
	if code, body := acceptInvite(t, mux, session, invite); code != 403 {
		t.Fatalf("an address confirmed by its own redeemer's invitation joined through a domain link: %d %v", code, body)
	}
	if m, _ := st.Membership(ctx, x.ID, victimOrg); m != nil {
		t.Fatal("the refusal still made a membership")
	}

	// The way on: the mail is sent though the address counts as confirmed, and answering it is
	// proof the link takes. Here the mailbox is the attacker's in name only — the test stands in
	// for the person who really holds it.
	if code, body := authReq(t, mux, "POST", "/api/auth/resend-verification", nil, session); code != 200 {
		t.Fatalf("resend = %d %v", code, body)
	}
	answer(t, mux, mailedVerifyToken(t, caught, "x@victim.test"))
	if code, body := acceptInvite(t, mux, session, invite); code != 200 {
		t.Fatalf("an address proved by our mail was refused: %d %v", code, body)
	}
}

// Slack's claim that the workspace confirmed an address is whatever that workspace's admin or SAML
// IdP says, and the workspace can be the redeemer's own.
func TestASlackClaimDoesNotOpenADomainLink(t *testing.T) {
	b, mux, st := identityBot(t)
	fs := &fakeSlack{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	old := slackOIDC
	slackOIDC = srv.URL
	t.Cleanup(func() { slackOIDC = old })
	ctx := context.Background()
	founder, victimOrg, _ := signedUp(t, b, mux, st, "founder@victim.test")
	invite := &http.Cookie{Name: inviteCookie, Value: domainLinkFor(t, st, victimOrg, founder.ID, "victim.test")}

	x, _, _ := signedUp(t, b, mux, st, "x@victim.test")
	st.AddIdentity(ctx, x.ID, ProviderSlack, slackSubject("T9", "U9"))
	fs.info = map[string]any{"ok": true, "sub": "U9", "name": "X", "email": "x@victim.test", "email_verified": true,
		"https://slack.com/team_id": "T9", "https://slack.com/user_id": "U9"}
	slackSignIn(t, mux)
	if u, _ := st.User(ctx, x.ID); !u.EmailVerified || u.emailVerifiedBy != emailBySlack {
		t.Fatalf("Slack's claim should still confirm the address, as Slack's: %+v", u)
	}
	rec := slackSignIn(t, mux, invite)
	if m, _ := st.Membership(ctx, x.ID, victimOrg); m != nil {
		t.Fatal("an address confirmed only by a Slack workspace's claim joined through a domain link")
	}
	if got := rec.Header().Get("Location"); got != "/admin/join/" {
		t.Errorf("the refusal should finish on the join screen, which offers the mail; went to %q", got)
	}
}
