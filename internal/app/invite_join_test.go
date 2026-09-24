package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// The whole of "a second person from one company signs up on their own": the walk tells them the
// workspace is taken before they press the button, the bot asks the person who connected it for
// an invitation, and accepting that invitation leaves the empty organisation behind.

// slackSignedIn gives an account a Slack identity in teamID, which is what makes the walk able
// to say anything about the workspace behind their sign-in.
func slackSignedIn(t *testing.T, st *Store, userID int64, teamID, slackUser string) {
	t.Helper()
	if err := st.AddIdentity(context.Background(), userID, ProviderSlack, slackSubject(teamID, slackUser)); err != nil {
		t.Fatal(err)
	}
}

func TestWalkSaysTheWorkspaceIsTakenBeforeTheButtonIsPressed(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()

	// Alice's organisation holds the workspace, and the install granted im:write.
	alice, err := st.CreateOrg(ctx, "Acme (Alice)", 0)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := b.sealer.Seal([]byte("xoxb-alice"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_ACME", OrgID: alice.ID, Name: "Acme",
		InstalledBy: "U_ALICE", DMScope: true}, enc); err != nil {
		t.Fatal(err)
	}

	// Bob signed up on his own, from the same Slack workspace.
	bobOrg, bobID, bob := seedOrg(t, st, RoleAdmin)
	slackSignedIn(t, st, bobID, "T_ACME", "U_BOB")

	got := readOnboarding(t, mux, bob)
	if got.Step != stepInstall {
		t.Fatalf("step = %q, want the install step", got.Step)
	}
	if got.Taken == nil {
		t.Fatal("the walk offered Add to Slack for a workspace that would refuse it")
	}
	if got.Taken.Name != "Acme" || got.Taken.Asked {
		t.Errorf("taken = %+v", got.Taken)
	}
	// It says a workspace is taken and who in Bob's own workspace took it — both things he can
	// read in Slack — and nothing about the organisation holding it.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "Acme (Alice)") {
		t.Errorf("the walk named the other organisation: %s", raw)
	}
	_ = bobOrg

	// A workspace nobody holds says nothing.
	if err := st.RevokeTeam(ctx, "T_ACME", "test"); err != nil {
		t.Fatal(err)
	}
	if got := readOnboarding(t, mux, bob); got.Taken != nil {
		t.Errorf("a disconnected workspace still reads as taken: %+v", got.Taken)
	}
}

// Somebody who signs in with a password says nothing about a Slack workspace, so the walk cannot
// and does not guess at one.
func TestWalkSaysNothingAboutAWorkspaceItCannotKnow(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	alice, _ := st.CreateOrg(ctx, "Acme (Alice)", 0)
	enc, _ := b.sealer.Seal([]byte("xoxb-alice"))
	st.SaveTeam(ctx, &Team{TeamID: "T_ACME", OrgID: alice.ID, Name: "Acme", InstalledBy: "U_ALICE", DMScope: true}, enc)

	_, _, bob := seedOrg(t, st, RoleAdmin) // no Slack identity
	if got := readOnboarding(t, mux, bob); got.Taken != nil {
		t.Errorf("the walk guessed at a workspace: %+v", got.Taken)
	}
}

func TestAskInviteMessagesTheInstallerOnceADay(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()

	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/chat.postMessage" {
			r.ParseForm()
			posted = append(posted, r.Form.Get("channel")+"|"+r.Form.Get("blocks")+r.Form.Get("text"))
		}
		w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1.0"}`))
	}))
	defer srv.Close()

	alice, _ := st.CreateOrg(ctx, "Acme (Alice)", 0)
	enc, _ := b.sealer.Seal([]byte("xoxb-alice"))
	st.SaveTeam(ctx, &Team{TeamID: "T_ACME", OrgID: alice.ID, Name: "Acme", InstalledBy: "U_ALICE", DMScope: true}, enc)
	b.slacks = testRegistry(&Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))},
		TeamID: "T_ACME", OrgID: alice.ID, BotUserID: "UBOT"})

	_, bobID, bob := seedOrg(t, st, RoleAdmin)
	slackSignedIn(t, st, bobID, "T_ACME", "U_BOB")

	if w := do(t, mux, "POST", "/api/onboarding/ask-invite", bob); w.Code != 200 {
		t.Fatalf("ask = %d: %s", w.Code, w.Body.String())
	}
	if len(posted) != 1 {
		t.Fatalf("sent %d messages, want one", len(posted))
	}
	// It goes to the person who connected the workspace, and names who is asking so they can act.
	if !strings.HasPrefix(posted[0], "U_ALICE|") {
		t.Errorf("message went to %q, want the installer", posted[0])
	}
	if !strings.Contains(posted[0], "admin@example.com") {
		t.Errorf("the message does not say who is asking: %q", posted[0])
	}

	// Pressing it again is one person's repeated click, not a second message.
	if w := do(t, mux, "POST", "/api/onboarding/ask-invite", bob); w.Code != 200 {
		t.Fatalf("second ask = %d: %s", w.Code, w.Body.String())
	}
	if len(posted) != 1 {
		t.Errorf("a reload sent another message: %d in total", len(posted))
	}
	if got := readOnboarding(t, mux, bob); got.Taken == nil || !got.Taken.Asked {
		t.Errorf("the walk does not say the message went: %+v", got.Taken)
	}

	// And it is refused outright for somebody whose workspace nobody else holds.
	_, carolID, carol := seedOrgAs(t, st, "carol@example.com", RoleAdmin)
	slackSignedIn(t, st, carolID, "T_OTHER", "U_CAROL")
	if w := do(t, mux, "POST", "/api/onboarding/ask-invite", carol); w.Code != 400 {
		t.Errorf("ask with nothing to ask about = %d, want 400", w.Code)
	}
}

// Accepting an invitation offers to leave behind the empty organisation founded on the way here,
// and leaving means detaching: the organisation and everything in it stay where they are.
func TestAcceptingAnInvitationCanLeaveTheEmptyOrgBehind(t *testing.T) {
	_, mux, st := installTestBot(t)
	ctx := context.Background()

	alice, aliceOrg := seedFounder(t, st, "alice@acme.com", "Acme")
	bobOrg, bobID, bob := seedOrg(t, st, RoleAdmin)

	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: aliceOrg,
		Email: "admin@example.com", Role: RoleEditor, CreatedBy: alice}, inviteTTL2)
	if err != nil {
		t.Fatal(err)
	}
	with := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bob)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: inviteCookie, Value: raw})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	// The preview says what accepting would mean, and does not spend the invitation saying it.
	w := with("GET", "/api/auth/invite", "")
	if w.Code != 200 {
		t.Fatalf("preview = %d: %s", w.Code, w.Body.String())
	}
	var pv struct {
		Org struct{ Name string } `json:"org"`
		Own *struct {
			ID       string
			Name     string
			Leavable bool
		} `json:"own"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	if pv.Org.Name != "Acme" || pv.Own == nil || !pv.Own.Leavable || pv.Own.ID != orgPublic(t, st, bobOrg) {
		t.Fatalf("preview = %+v (own %+v)", pv.Org, pv.Own)
	}

	if w := with("POST", "/api/auth/accept-invite", `{"leave_own":true}`); w.Code != 200 {
		t.Fatalf("accept = %d: %s", w.Code, w.Body.String())
	}

	// He is in Acme, out of his own, and his account is untouched: one login, now opening Acme.
	ms, err := st.MembershipsFor(ctx, bobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].OrgID != aliceOrg {
		t.Fatalf("memberships = %+v, want Acme alone", ms)
	}
	if u, _ := st.User(ctx, bobID); u == nil || u.Email != "admin@example.com" {
		t.Error("the account was destroyed; leaving an organisation is not leaving attest_tag")
	}
	// Detached, not deleted: the organisation is still there to be given back.
	if org, _ := st.Org(ctx, bobOrg); org == nil {
		t.Error("the organisation was deleted; leaving is supposed to detach")
	}
	// And the next sign-in opens Acme, because it is the only one left.
	u, _ := st.User(ctx, bobID)
	if got, err := (&Bot{store: st, settings: newSettingsCache(st, Config{})}).orgForSignIn(ctx, u, "password"); err != nil || got != aliceOrg {
		t.Errorf("signing in again opens org %d (%v), want Acme", got, err)
	}
}

// An organisation with anything in it is not offered, however much its owner would like to be rid
// of it: leaving would strand rows that somebody may still need.
func TestLeavingIsNotOfferedForAnOrgWithAnythingInIt(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	alice, aliceOrg := seedFounder(t, st, "alice@acme.com", "Acme")
	bobOrg, _, bob := seedOrg(t, st, RoleAdmin)

	// Bob's own organisation has a workspace of its own connected.
	enc, _ := b.sealer.Seal([]byte("xoxb-bob"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_BOB", OrgID: bobOrg, Name: "Bob's"}, enc); err != nil {
		t.Fatal(err)
	}

	raw, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: aliceOrg,
		Email: "admin@example.com", Role: RoleEditor, CreatedBy: alice}, inviteTTL2)
	r := httptest.NewRequest("GET", "/api/auth/invite", nil)
	r.Header.Set("Authorization", "Bearer "+bob)
	r.AddCookie(&http.Cookie{Name: inviteCookie, Value: raw})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var pv struct {
		Own *struct {
			Leavable bool
			Reason   string
		} `json:"own"`
	}
	json.Unmarshal(w.Body.Bytes(), &pv)
	if pv.Own == nil || pv.Own.Leavable {
		t.Fatalf("an organisation with a workspace in it was offered up: %+v", pv.Own)
	}
	if pv.Own.Reason == "" {
		t.Error("no reason given, so the screen can only be mysterious about it")
	}

	// And asking for it anyway does not do it: the choice is re-checked, not trusted.
	r = httptest.NewRequest("POST", "/api/auth/accept-invite", strings.NewReader(`{"leave_own":true}`))
	r.Header.Set("Authorization", "Bearer "+bob)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: inviteCookie, Value: raw})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("accept = %d: %s", w.Code, w.Body.String())
	}
	if m, _ := st.Membership(ctx, mustUser(t, st, "admin@example.com"), bobOrg); m == nil {
		t.Error("a request that asked to leave an organisation it may not leave was obeyed")
	}
}

func mustUser(t *testing.T, st *Store, email string) int64 {
	t.Helper()
	u, err := st.UserByEmail(context.Background(), email)
	if err != nil || u == nil {
		t.Fatalf("no user %s: %v", email, err)
	}
	return u.ID
}

// seedOrgAs is seedOrg for a second account in the same test.
func seedOrgAs(t *testing.T, st *Store, email, role string) (orgID, userID int64, token string) {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, email, email, "")
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(ctx, email+"'s org", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleAdmin {
		st.SetMemberRole(ctx, u.ID, org.ID, role)
	}
	tok, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Name: u.Name, Email: u.Email, OrgID: org.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return org.ID, u.ID, tok
}

// seedFounder makes an account and the organisation it founds, and returns both ids. An
// invitation names who sent it and is refused if that person has since gone, so a test that
// mints one needs a real inviter behind it.
func seedFounder(t *testing.T, st *Store, email, orgName string) (userID, orgID int64) {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, email, email, "")
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(ctx, orgName, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, org.ID
}

// Following an invitation link when you already have an account. The link used to land on the
// sign-up form for everybody, which for an address that already has an account is a dead end
// reading "that address is taken" — and the invitation would sit in its cookie, unspent, while
// they signed in and arrived back in the organisation they were trying to leave behind.
func TestInvitationLinkSendsAnExistingAccountToSignIn(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	alice, aliceOrg := seedFounder(t, st, "alice@acme.com", "Acme")

	link := func(email string) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: aliceOrg,
			Email: email, Role: RoleViewer, CreatedBy: alice}, inviteTTL2)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/invite/"+raw, nil))
		return w
	}

	// Nobody by that name yet: sign up, as before.
	if loc := link("newcomer@acme.com").Header().Get("Location"); !strings.HasPrefix(loc, "/admin/signup/") {
		t.Errorf("a new address goes to %q, want the sign-up form", loc)
	}

	// An address that already has an account: sign in.
	if _, err := st.CreateUser(ctx, "bob@acme.com", "Bob", ""); err != nil {
		t.Fatal(err)
	}
	if loc := link("bob@acme.com").Header().Get("Location"); !strings.HasPrefix(loc, "/admin/login/") {
		t.Errorf("an existing address goes to %q, want the sign-in form", loc)
	}

	// And the sign-in that follows says where to go next, so the invitation is not left unspent
	// in a cookie while the console opens on whatever organisation they already had.
	pw, err := hashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.UserByEmail(ctx, "bob@acme.com")
	if err := st.SetPassword(ctx, u.ID, pw); err != nil {
		t.Fatal(err)
	}
	st.AddIdentity(ctx, u.ID, ProviderPassword, strconv.FormatInt(u.ID, 10))
	bobOrg, err := st.CreateOrg(ctx, "Bob's", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = bobOrg

	raw, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: aliceOrg,
		Email: "bob@acme.com", Role: RoleViewer, CreatedBy: alice}, inviteTTL2)
	r := httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"email":"bob@acme.com","password":"correct-horse-battery"}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: inviteCookie, Value: raw})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("sign-in = %d: %s", w.Code, w.Body.String())
	}
	var out struct{ Next string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Next != "/admin/join/" {
		t.Errorf("next = %q, want the join screen", out.Next)
	}
	_ = b

	// A sign-in with no invitation in hand still goes to the console.
	r = httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"email":"bob@acme.com","password":"correct-horse-battery"}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	out.Next = ""
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Next != "" {
		t.Errorf("an ordinary sign-in was sent to %q", out.Next)
	}
}
