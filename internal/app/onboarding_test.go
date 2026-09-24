package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"time"
)

// The setup walk over its real route. What is under test is that each step is derived from
// something that actually happened — an install, a channel, a turn — rather than from a flag
// somebody remembered to set, because a flag is what would let the console hold a finished
// organisation on a screen it has already been through.

func readOnboarding(t *testing.T, mux *http.ServeMux, token string) onboardingState {
	t.Helper()
	w := do(t, mux, "GET", "/api/onboarding", token)
	if w.Code != 200 {
		t.Fatalf("GET /api/onboarding = %d: %s", w.Code, w.Body.String())
	}
	var got onboardingState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOnboardingWalksTheSteps(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	orgID, _, token := seedOrg(t, st, RoleAdmin)

	// Nothing connected: the first step, and the button to do it.
	got := readOnboarding(t, mux, token)
	if got.Step != stepInstall || got.Done {
		t.Fatalf("a fresh organisation = %+v, want the install step", got)
	}
	if !got.CanInstall || !got.InstallConfigured || got.InstallURL != "/slack/install" {
		t.Errorf("an admin on a configured deployment must be offered the install: %+v", got)
	}
	// The empty collections are what the console maps over; nil would render as a crash.
	if got.Teams == nil || got.Channels == nil {
		t.Errorf("teams and channels must be empty lists, not null: %+v", got)
	}

	// A workspace, but the bot is in no channel yet.
	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: orgID, Name: "Acme"}, enc); err != nil {
		t.Fatal(err)
	}
	got = readOnboarding(t, mux, token)
	if got.Step != stepChannel {
		t.Fatalf("with a workspace connected = %q, want the channel step", got.Step)
	}
	if len(got.Teams) != 1 || got.Teams[0].Name != "Acme" {
		t.Errorf("the connected workspace has to be named back: %+v", got.Teams)
	}

	// Invited to a channel, but nobody has spoken to it.
	if _, err := st.UpsertChannelScope(ctx, orgID, "TA", "C1", "#eng", false); err != nil {
		t.Fatal(err)
	}
	got = readOnboarding(t, mux, token)
	if got.Step != stepMention || got.Turns != 0 {
		t.Fatalf("with a channel but no turns = %+v, want the mention step", got)
	}
	if len(got.Channels) != 1 || got.Channels[0].Name != "#eng" {
		t.Errorf("the channel it was let into has to be named back: %+v", got.Channels)
	}

	// A turn: the bot works, and what is left is people. TurnCount counts from the beginning, so
	// an organisation set up last month is past this step rather than back at it.
	if _, err := st.db.ExecContext(ctx,
		`insert into usage (org_id, team_id, channel, cost_usd, created_at) values (?, 'TA', 'C1', 0.01, ?)`,
		orgID, nowMinus(90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got = readOnboarding(t, mux, token)
	if got.Step != stepInvite || got.Done || got.Turns != 1 || got.Members != 1 {
		t.Fatalf("after a turn = %+v, want the invite step", got)
	}

	// An invitation sent is enough: waiting for somebody to accept is not the founder's work.
	if _, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "colleague@example.com", Role: RoleViewer}, inviteTTL2); err != nil {
		t.Fatal(err)
	}
	got = readOnboarding(t, mux, token)
	if got.Step != stepDone || !got.Done || got.Invited != 1 {
		t.Fatalf("after an invitation = %+v, want the walk finished", got)
	}
}

// The last step is people, and somebody who cannot invite anybody is not held on it. Their walk
// ends where the bot starts working.
func TestOnboardingInviteStepIsSkippedWithoutThePermission(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	orgID, userID, token := seedOrg(t, st, RoleAdmin)
	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: orgID, Name: "Acme"}, enc)
	st.UpsertChannelScope(ctx, orgID, "TA", "C1", "#eng", false)
	st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, cost_usd) values (?, 'TA', 'C1', 0.01)`, orgID)

	if got := readOnboarding(t, mux, token); got.Step != stepInvite {
		t.Fatalf("an admin stops at %q, want the invite step", got.Step)
	}
	if err := st.SetMemberRole(ctx, userID, orgID, RoleEditor); err != nil {
		t.Fatal(err)
	}
	got := readOnboarding(t, mux, token)
	if got.CanInvite {
		t.Fatal("the test needs a role that cannot invite")
	}
	if got.Step != stepDone || !got.Done {
		t.Errorf("an editor is held on a step they cannot do: %+v", got)
	}
}

// A viewer is held on the same first step — there is nothing else to show them — but is told to
// ask rather than shown a button that would refuse them.
func TestOnboardingViewerIsNotOfferedTheInstall(t *testing.T) {
	_, mux, st := installTestBot(t)
	_, _, token := seedOrg(t, st, RoleViewer)
	got := readOnboarding(t, mux, token)
	if got.Step != stepInstall {
		t.Fatalf("step = %q, want the install step", got.Step)
	}
	if got.CanInstall {
		t.Error("a viewer cannot connect a workspace, so the walk must not offer it")
	}
}

// One organisation's walk must not be finished by another's workspace.
func TestOnboardingIsPerOrganisation(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	_, _, mine := seedOrg(t, st, RoleAdmin)

	other, err := st.CreateOrg(ctx, "Someone Else", 0)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := b.sealer.Seal([]byte("xoxb-b"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "TB", OrgID: other.ID, Name: "Theirs"}, enc); err != nil {
		t.Fatal(err)
	}
	st.UpsertChannelScope(ctx, other.ID, "TB", "C9", "#theirs", false)

	if got := readOnboarding(t, mux, mine); got.Step != stepInstall || len(got.Teams) != 0 {
		t.Fatalf("another tenant's workspace finished this one's walk: %+v", got)
	}
}

// Signing in with Slack carries straight on to the install when there is nothing connected and
// the person can do something about it. Everyone else lands in the console.
func TestNeedsInstall(t *testing.T) {
	b, _, st := installTestBot(t)
	ctx := context.Background()
	orgID, userID, _ := seedOrg(t, st, RoleAdmin)

	admin := &AdminUser{ID: userID, OrgID: orgID, Via: "slack"}
	b.permissionsFor(ctx, admin)
	if !b.needsInstall(ctx, admin) {
		t.Error("an admin whose organisation has no workspace should be sent on to Slack")
	}

	// A role that could not finish it is not sent: a consent screen they will be refused at is
	// a worse landing than the console.
	if err := st.SetMemberRole(ctx, userID, orgID, RoleViewer); err != nil {
		t.Fatal(err)
	}
	viewer := &AdminUser{ID: userID, OrgID: orgID, Via: "slack"}
	b.permissionsFor(ctx, viewer)
	if b.needsInstall(ctx, viewer) {
		t.Error("a viewer must not be sent to a consent screen they cannot complete")
	}
	if err := st.SetMemberRole(ctx, userID, orgID, RoleAdmin); err != nil {
		t.Fatal(err)
	}

	// Nor is anyone once a workspace is in: signing in again is not a reason to reinstall.
	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: orgID, Name: "Acme"}, enc); err != nil {
		t.Fatal(err)
	}
	if b.needsInstall(ctx, admin) {
		t.Error("an organisation with a connected workspace must not be sent to install another")
	}

	// And not when the deployment cannot install at all.
	st.RevokeTeam(ctx, "TA", "test")
	t.Setenv("SLACK_CLIENT_ID", "")
	if b.needsInstall(ctx, admin) {
		t.Error("without SLACK_CLIENT_ID there is no install to send anyone to")
	}
}

// Where an install returns to. The first one an organisation does belongs to the walk, which
// then asks for the channel invite; every one after it belongs to the page it was started from.
func TestInstallReturnFollowsTheWalk(t *testing.T) {
	b, _, st := installTestBot(t)
	ctx := context.Background()
	orgID, _, _ := seedOrg(t, st, RoleAdmin)

	if got := b.installReturn(ctx, orgID); got != "/admin/onboarding/" {
		t.Errorf("with nothing connected = %q, want the walk", got)
	}
	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: orgID, Name: "Acme"}, enc)
	if got := b.installReturn(ctx, orgID); got != "/admin/workspaces/" {
		t.Errorf("with a workspace connected = %q, want the Workspaces page", got)
	}
	// A revoked workspace is not a connected one: an organisation that disconnected its only
	// workspace is back at the start of the walk.
	st.RevokeTeam(ctx, "TA", "test")
	if got := b.installReturn(ctx, orgID); got != "/admin/onboarding/" {
		t.Errorf("after disconnecting the only workspace = %q, want the walk", got)
	}
}

// A refused install that never reached the session — an expired state, a different browser —
// still lands somewhere that can explain itself rather than on a bare redirect: the walk, with
// the reason, for somebody signed in; the sign-in page, with the install parked behind it, for
// somebody who is not — a reason on a page the sign-in redirect throws away is no reason at all.
func TestInstallErrorLandsOnTheWalk(t *testing.T) {
	_, mux, st := installTestBot(t)
	_, _, token := seedOrg(t, st, RoleAdmin)
	w := do(t, mux, "GET", "/slack/oauth/callback?code=x&state=forged", token)
	if w.Code != http.StatusFound {
		t.Fatalf("forged state = %d, want a redirect", w.Code)
	}
	loc := w.Header().Get("Location")
	if want := "/admin/onboarding/?install_error="; len(loc) < len(want) || loc[:len(want)] != want {
		t.Errorf("location = %q, want the walk with the reason", loc)
	}
	w = do(t, mux, "GET", "/slack/oauth/callback?code=x&state=forged", "")
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || loc != "/admin/login/" || !setsCookie(w, installIntentCookie) {
		t.Errorf("signed out = %d %q, want the sign-in page with the install parked", w.Code, loc)
	}
}

// The console asks for this on every page load, so it must not be a Slack call. Only the walk
// itself, polling while somebody types the invite in another window, asks for a sync — and that
// one has to actually reach Slack, or the step never notices the invite that finished it.
func TestOnboardingSyncsOnlyWhenAsked(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	orgID, _, token := seedOrg(t, st, RoleAdmin)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/users.conversations":
			w.Write([]byte(`{"ok":true,"channels":[{"id":"C1","name":"eng"}],"response_metadata":{"next_cursor":""}}`))
		default: // team.info, and anything else the sync asks in passing
			w.Write([]byte(`{"ok":true,"team":{"id":"TA","name":"Acme"}}`))
		}
	}))
	defer srv.Close()

	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: orgID, Name: "Acme"}, enc)
	b.slacks = testRegistry(&Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))},
		TeamID: "TA", OrgID: orgID, BotUserID: "UBOT"})

	if got := readOnboarding(t, mux, token); got.Step != stepChannel {
		t.Fatalf("step = %q, want the channel step", got.Step)
	}
	if calls != 0 {
		t.Errorf("a plain read made %d Slack calls: it runs on every console page load", calls)
	}

	r := httptest.NewRequest("GET", "/api/onboarding?sync=1", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var got onboardingState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("?sync=1 is what catches the invite somebody just typed; it has to reach Slack")
	}
	if got.Step != stepMention || len(got.Channels) != 1 || got.Channels[0].Name != "#eng" {
		t.Fatalf("the sync did not pick the channel up: %+v", got)
	}
}

// The second person from one company who signs up on their own rather than being invited gets an
// organisation of their own, and the setup walk points them at the Add to Slack button. Pressing
// it reaches a workspace their colleague already connected, and what comes back has to name the
// way out — an invitation — rather than read as a deployment that broke.
func TestInstallingAWorkspaceSomebodyElseHoldsSaysWhatToDo(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()

	// Alice's organisation, with the workspace already connected.
	alice, err := st.CreateOrg(ctx, "Acme (Alice)", 0)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := b.sealer.Seal([]byte("xoxb-alice"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_ACME", OrgID: alice.ID, Name: "Acme"}, enc); err != nil {
		t.Fatal(err)
	}

	// Bob's, founded a minute later by signing up rather than accepting an invitation.
	bobOrg, _, bob := seedOrg(t, st, RoleAdmin)
	if bobOrg == alice.ID {
		t.Fatal("the test needs two organisations")
	}
	if got := readOnboarding(t, mux, bob); got.Step != stepInstall {
		t.Fatalf("Bob's own organisation starts at %q, want the install step", got.Step)
	}

	exchange, isAdmin, revoke := oauthExchange, slackUserIsAdmin, slackRevokeToken
	t.Cleanup(func() { oauthExchange, slackUserIsAdmin, slackRevokeToken = exchange, isAdmin, revoke })
	oauthExchange = func(ctx context.Context, clientID, clientSecret, code, redirectURI string) (*slack.OAuthV2Response, error) {
		resp := &slack.OAuthV2Response{AccessToken: "xoxb-bob"}
		resp.Team.ID, resp.Team.Name = "T_ACME", "Acme"
		resp.AuthedUser.ID = "U_BOB"
		return resp, nil
	}
	slackUserIsAdmin = func(ctx context.Context, token, userID string) (bool, error) { return true, nil }
	revoked := ""
	slackRevokeToken = func(ctx context.Context, token string) { revoked = token }

	state, _ := st.NewOAuthState(ctx, bobOrg, "U_BOB", installStateTTL)
	w := callback(t, mux, state, state, bob)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	msg := loc.Query().Get("install_error")
	if msg == "" {
		t.Fatalf("Bob took Acme's workspace: %d %q", w.Code, loc)
	}
	if !strings.Contains(msg, "invite") {
		t.Errorf("the refusal does not name the way out: %q", msg)
	}
	if strings.Contains(msg, "could not be saved") {
		t.Errorf("a refusal to take somebody's workspace is reported as a fault: %q", msg)
	}
	// It lands on Bob's walk, which is where the button he pressed lives.
	if loc.Path != "/admin/onboarding/" {
		t.Errorf("refusal landed on %q, want the walk Bob is being held on", loc.Path)
	}
	// Alice keeps her install, token and all.
	team, _ := st.Team(ctx, "T_ACME")
	if team == nil || team.OrgID != alice.ID {
		t.Fatalf("Acme's workspace changed hands: %+v", team)
	}
	tok, err := b.sealer.Open(team.tokenEnc)
	if err != nil || string(tok) != "xoxb-alice" {
		t.Errorf("Alice's bot token was overwritten: %q %v", tok, err)
	}
	// And Bob's token is not handed back: Slack issues it for an app already installed in that
	// workspace, so revoking it could revoke the one Alice is using.
	if revoked != "" {
		t.Errorf("revoked %q — that token may be the one the other organisation is running on", revoked)
	}
}

// The "ask them to invite me" message is the one thing this walk sends into somebody else's
// workspace, and it is composed from what a stranger typed into their own name. Slack's mrkdwn
// turns <url|text> into a link, so a name carrying one would put a link of the sender's choosing
// inside a message the recipient has every reason to trust.
func TestAskInviteDoesNotCarryMrkdwnFromAName(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	rec := &recordingSlack{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	// Alice's organisation holds the workspace; Bob signed in from the same workspace into an
	// organisation of his own, and calls himself something with a link in it.
	alice, err := st.CreateOrg(ctx, "Acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_ACME", OrgID: alice.ID, Name: "Acme", Status: "active",
		InstalledBy: "U_ALICE", DMScope: true}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	bob, err := st.CreateUser(ctx, "bob@example.com", `Bob <https://evil.example/login|Reset your password>`, "")
	if err != nil {
		t.Fatal(err)
	}
	bobOrg, err := st.CreateOrg(ctx, "Bob's", bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddIdentity(ctx, bob.ID, ProviderSlack, "T_ACME:U_BOB"); err != nil {
		t.Fatal(err)
	}

	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-acme", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T_ACME", OrgID: alice.ID}
	b := &Bot{store: st, slacks: testRegistry(sl), settings: newSettingsCache(st, Config{})}

	r := httptest.NewRequest("POST", "/api/onboarding/ask-invite", nil)
	r = r.WithContext(context.WithValue(r.Context(), userKey,
		&AdminUser{ID: bob.ID, OrgID: bobOrg.ID, Name: bob.Name, Email: bob.Email}))
	w := httptest.NewRecorder()
	b.askInvite(w, r)
	if w.Code != 200 {
		t.Fatalf("ask-invite = %d: %s", w.Code, w.Body.String())
	}

	rec.mu.Lock()
	sent := append([]string{}, rec.sent...)
	rec.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("expected one direct message, got %v", sent)
	}
	if strings.Contains(sent[0], "<https://evil.example") {
		t.Errorf("a link from somebody's name reached another organisation's admin: %s", sent[0])
	}
	if !strings.Contains(sent[0], "&lt;https://evil.example") {
		t.Errorf("the name should still be shown, escaped: %s", sent[0])
	}
}
