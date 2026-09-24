package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// fakeWorkspace answers the handful of Slack methods the access checks call: who a user is,
// what a conversation is, and who is in it.
type fakeWorkspace struct {
	emails  map[string]string   // user id -> profile email
	teams   map[string]string   // user id -> team_id; absent means the installed workspace, T1
	guests  map[string]bool     // user id -> is_restricted (a single- or multi-channel guest)
	deleted map[string]bool     // user id -> deleted
	private map[string]bool     // channel id -> is_private
	members map[string][]string // channel id -> member ids
	calls   map[string]int      // method -> times called
}

func (f *fakeWorkspace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[strings.TrimPrefix(r.URL.Path, "/api/")]++
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	switch r.URL.Path {
	case "/api/users.info":
		id := r.Form.Get("user")
		email, ok := f.emails[id]
		if !ok {
			enc.Encode(map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		team, known := f.teams[id]
		if !known {
			team = "T1"
		}
		enc.Encode(map[string]any{"ok": true, "user": map[string]any{
			"id": id, "name": id, "team_id": team, "is_restricted": f.guests[id], "deleted": f.deleted[id],
			"profile": map[string]any{"email": email}}})
	case "/api/conversations.info":
		ch := r.Form.Get("channel")
		priv, known := f.private[ch]
		if !known {
			enc.Encode(map[string]any{"ok": false, "error": "channel_not_found"})
			return
		}
		enc.Encode(map[string]any{"ok": true, "channel": map[string]any{
			"id": ch, "name": strings.ToLower(ch), "is_private": priv}})
	case "/api/conversations.members":
		enc.Encode(map[string]any{"ok": true, "members": f.members[r.Form.Get("channel")],
			"response_metadata": map[string]any{"next_cursor": ""}})
	default:
		http.NotFound(w, r)
	}
}

func testAgent(t *testing.T, f *fakeWorkspace) (*Agent, func()) {
	t.Helper()
	srv := httptest.NewServer(f)
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1"}
	return &Agent{slacks: testRegistry(sl)}, srv.Close
}

// F1: the bot is in channels the asker is not, and reads with its own token.
func TestCanReadRefusesPrivateChannelsTheAskerIsNotIn(t *testing.T) {
	f := &fakeWorkspace{
		private: map[string]bool{"CPRIV": true, "CPUB": false},
		members: map[string][]string{"CPRIV": {"UINSIDER", "UBOT"}},
	}
	a, done := testAgent(t, f)
	defer done()
	ctx := context.Background()
	sl := a.slacks.Any(ctx)
	outsider := &Call{Channel: "CHOME", UserID: "UOUTSIDER", TeamID: "T1", SL: sl}
	insider := &Call{Channel: "CHOME", UserID: "UINSIDER", TeamID: "T1", SL: sl}

	if err := a.canRead(ctx, outsider, "CPRIV"); err == nil {
		t.Error("an outsider must not be able to read a private channel through the bot")
	} else if !strings.Contains(err.Error(), "private") {
		t.Errorf("the refusal should say why: %v", err)
	}
	if err := a.canRead(ctx, insider, "CPRIV"); err != nil {
		t.Errorf("a member of the channel should be able to read it: %v", err)
	}
	if err := a.canRead(ctx, outsider, "CPUB"); err != nil {
		t.Errorf("a public channel is readable by anyone in the workspace: %v", err)
	}
	if err := a.canRead(ctx, outsider, "CHOME"); err != nil {
		t.Errorf("the thread's own channel is always readable: %v", err)
	}
	if err := a.canRead(ctx, &Call{Channel: "CHOME", TeamID: "T1", SL: sl}, "CPRIV"); err == nil {
		t.Error("with no asker there is nobody to check, so it must refuse")
	}
}

// A conversation Slack will not describe counts as private: "could not tell" is not "allowed".
func TestCanReadFailsClosedOnLookupError(t *testing.T) {
	a, done := testAgent(t, &fakeWorkspace{})
	defer done()
	if err := a.canRead(context.Background(), &Call{Channel: "CHOME", UserID: "U1"}, "CGONE"); err == nil {
		t.Error("expected a refusal when membership cannot be established")
	}
}

func TestConversationLookupsAreCached(t *testing.T) {
	f := &fakeWorkspace{private: map[string]bool{"CPRIV": true}, members: map[string][]string{"CPRIV": {"U1"}}}
	a, done := testAgent(t, f)
	defer done()
	c := &Call{Channel: "CHOME", UserID: "U1", TeamID: "T1", SL: a.slacks.Any(context.Background())}
	for i := 0; i < 4; i++ {
		if err := a.canRead(context.Background(), c, "CPRIV"); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.calls["conversations.info"]; n != 1 {
		t.Errorf("conversations.info called %d times, want 1 (cached)", n)
	}
	if n := f.calls["conversations.members"]; n != 1 {
		t.Errorf("conversations.members called %d times, want 1 (cached)", n)
	}
}

// The memory scope used to read the channel id's "C" prefix, which private channels also have.
func TestIsPublicAsksSlack(t *testing.T) {
	f := &fakeWorkspace{private: map[string]bool{"CPRIVATE": true, "CPUBLIC": false}}
	a, done := testAgent(t, f)
	defer done()
	ctx := context.Background()
	if a.isPublic(ctx, &Call{Channel: "CPRIVATE", SL: a.slacks.Any(ctx)}) {
		t.Error("a private channel with a C id must not count as public")
	}
	if !a.isPublic(ctx, &Call{Channel: "CPUBLIC", SL: a.slacks.Any(ctx)}) {
		t.Error("a public channel should count as public")
	}
	if got := a.memoryScope(ctx, &Call{Channel: "CPRIVATE", TeamID: "T1", SL: a.slacks.Any(ctx)}, true); got != "channel:T1/CPRIVATE" {
		t.Errorf("a private channel's memory must stay in the channel, got %q", got)
	}
}

// ALLOWED_EMAIL_DOMAINS: who the bot answers at all.
func TestMayUseBot(t *testing.T) {
	f := &fakeWorkspace{
		emails: map[string]string{
			"UEMP":    "alex@example.com",
			"UCAPS":   "Someone@Example.com",
			"UGUEST":  "contractor@othercorp.com",
			"UNOMAIL": "",
			"UEXT":    "partner@example.com", // right domain, wrong workspace: Slack Connect
			"USCG":    "temp@example.com",    // right domain, a single-channel guest
			"UGONE":   "left@example.com",
		},
		teams:   map[string]string{"UEXT": "T2"},
		guests:  map[string]bool{"USCG": true},
		deleted: map[string]bool{"UGONE": true},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	// The allowlist is a per-organisation setting now, with ALLOWED_EMAIL_DOMAINS as the default
	// for an organisation that has not set its own, so the bot needs a settings cache to read.
	st := testStore(t)
	b := &Bot{
		slacks:   testRegistry(&Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1"}),
		store:    st,
		settings: newSettingsCache(st, Config{}),
	}
	ctx := context.Background()

	t.Setenv("ALLOWED_EMAIL_DOMAINS", "")
	if ok, _ := b.mayUseBot(ctx, b.slacks.Any(ctx), "UGUEST"); !ok {
		t.Error("with no domains configured every member of the workspace may use the bot")
	}
	// Membership comes before any domain list, and is checked even when there is no list: a
	// Slack Connect member or a guest is in the channel but not in this organisation.
	org := orgOfChat(b.slacks.Any(ctx))
	for _, tc := range []struct {
		user string
		want bool
		why  string
	}{
		{"UEXT", false, "an account from another workspace, joined through Slack Connect"},
		{"USCG", false, "a single-channel guest"},
		{"UGONE", false, "a deactivated account"},
		{"UEMP", true, "a full member"},
		{"", false, "no user at all"},
	} {
		if ok, why := b.mayUseBot(ctx, b.slacks.Any(ctx), tc.user); ok != tc.want {
			t.Errorf("no list: mayUseBot(%q) = %v (%q), want %v — %s", tc.user, ok, why, tc.want, tc.why)
		}
	}
	if _, why := b.mayUseBot(ctx, b.slacks.Any(ctx), "UEXT"); !strings.Contains(why, "Settings") {
		t.Errorf("the refusal should say where an admin can change it, got %q", why)
	}
	// The organisation may let them in, and then the domain list (below) is all that gates them.
	if err := st.PutSetting(ctx, org, "allow_external_users", "1"); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(org)
	for _, u := range []string{"UEXT", "USCG"} {
		if ok, why := b.mayUseBot(ctx, b.slacks.Any(ctx), u); !ok {
			t.Errorf("allow_external_users=1: mayUseBot(%q) refused: %q", u, why)
		}
	}
	if ok, _ := b.mayUseBot(ctx, b.slacks.Any(ctx), "UGONE"); ok {
		t.Error("a deactivated account is refused whatever the organisation allows")
	}
	if err := st.PutSetting(ctx, org, "allow_external_users", "0"); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(org)

	// The organisation's own list is what gates the bot. The env var is only the default for one
	// that has not set one, so this is the path that matters.
	if err := st.PutSetting(ctx, orgOfChat(b.slacks.Any(ctx)), "allowed_email_domains", "example.com"); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(orgOfChat(b.slacks.Any(ctx)))
	for _, tc := range []struct {
		user string
		want bool
		why  string
	}{
		{"UEMP", true, "an employee"},
		{"UCAPS", true, "case must not matter"},
		{"UGUEST", false, "a member whose email is on another company's domain"},
		{"UNOMAIL", false, "an account with no email"},
		{"UEXT", false, "the right domain does not make a Slack Connect account a member"},
		{"USCG", false, "nor a guest"},
		{"UUNKNOWN", false, "a user Slack will not describe"},
		{"", false, "no user at all"},
		{"UBOT", true, "our own SELF_TEST messages"},
	} {
		if ok, why := b.mayUseBot(ctx, b.slacks.Any(ctx), tc.user); ok != tc.want {
			t.Errorf("mayUseBot(%q) = %v (%q), want %v — %s", tc.user, ok, why, tc.want, tc.why)
		}
	}
	if _, why := b.mayUseBot(ctx, b.slacks.Any(ctx), "UGUEST"); !strings.Contains(why, "example.com") {
		t.Errorf("the refusal should name the domain, got %q", why)
	}
	// The event handler hands SELF_TEST messages in with the synthetic requester "selftest".
	if ok, _ := b.mayUseBot(ctx, b.slacks.Any(ctx), "selftest"); ok {
		t.Error("the selftest requester must be refused while SELF_TEST is off")
	}
	b.cfg.SelfTest = true
	if ok, why := b.mayUseBot(ctx, b.slacks.Any(ctx), "selftest"); !ok {
		t.Errorf("SELF_TEST messages must pass the email gate, got %q", why)
	}
}

// A new organisation's allowlist starts at its founder's domain, so an organisation that never
// opens Settings is not answering everyone a shared channel brings in. A consumer address seeds
// nothing: "gmail.com" would admit every Gmail user.
func TestSeedEmailDomain(t *testing.T) {
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "")
	st := testStore(t)
	b := &Bot{store: st, settings: newSettingsCache(st, Config{})}
	ctx := context.Background()
	found := func(email string) int64 {
		t.Helper()
		u, err := st.CreateUser(ctx, email, "Founder", "")
		if err != nil {
			t.Fatal(err)
		}
		org, err := st.CreateOrg(ctx, "Org of "+email, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		b.seedEmailDomain(ctx, org.ID, email)
		return org.ID
	}
	if got := b.settings.Get(ctx, found("ceo@Acme.Example")).AllowedEmailDomains; len(got) != 1 || got[0] != "acme.example" {
		t.Errorf("a company address seeds its domain, lowercased; got %v", got)
	}
	for _, email := range []string{"someone@gmail.com", "someone@outlook.com", "someone@proton.me"} {
		if got := b.settings.Get(ctx, found(email)).AllowedEmailDomains; len(got) != 0 {
			t.Errorf("%q must seed nothing, got %v", email, got)
		}
	}
	// Not addresses at all: the store would never hold these, but the helper must still shrug.
	u0, _ := st.CreateUser(ctx, "shrug@acme.example", "Shrug", "")
	org0, _ := st.CreateOrg(ctx, "Shrug", u0.ID)
	for _, bad := range []string{"not-an-address", "", "x@", "x@-bad-.example"} {
		b.seedEmailDomain(ctx, org0.ID, bad)
		if got := b.settings.Get(ctx, org0.ID).AllowedEmailDomains; len(got) != 0 {
			t.Errorf("%q must seed nothing, got %v", bad, got)
		}
	}
	// A list that is already there is the organisation's own and is left alone.
	u, _ := st.CreateUser(ctx, "second@acme.example", "Second", "")
	org, _ := st.CreateOrg(ctx, "Acme again", u.ID)
	if err := st.PutSetting(ctx, org.ID, "allowed_email_domains", "other.example"); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(org.ID)
	b.seedEmailDomain(ctx, org.ID, "second@acme.example")
	if got := b.settings.Get(ctx, org.ID).AllowedEmailDomains; len(got) != 1 || got[0] != "other.example" {
		t.Errorf("an existing list is kept, got %v", got)
	}
}

// The console validates the list before storing it: a stray word is not a domain.
func TestValidateEmailDomains(t *testing.T) {
	for _, ok := range []string{"", "acme.example", "acme.example, sub.acme.example", "@Acme.Example"} {
		if err := validateSecuritySetting("allowed_email_domains", ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"acme", "a b", "-acme.example", "acme.example/x", "user@acme.example"} {
		if err := validateSecuritySetting("allowed_email_domains", bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	for _, v := range []string{"0", "1"} {
		if err := validateSecuritySetting("allow_external_users", v); err != nil {
			t.Errorf("allow_external_users=%s should be accepted: %v", v, err)
		}
	}
	if err := validateSecuritySetting("allow_external_users", "yes"); err == nil {
		t.Error("allow_external_users=yes should be refused")
	}
}

// Settings → Security says what an empty list of domains means, and wherever ALLOWED_EMAIL_DOMAINS
// is set that is its list, not every member of the workspace — so the console is told the list.
func TestSettingsNameTheDomainsAnEmptyListFallsBackTo(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, token := signedUp(t, b, mux, st, "founder@example.com")
	for env, want := range map[string]string{"": "", "Acme.example, @sub.acme.example": "acme.example|sub.acme.example"} {
		t.Setenv("ALLOWED_EMAIL_DOMAINS", env)
		code, body := authReq(t, mux, "GET", "/api/settings", nil, token)
		if code != 200 {
			t.Fatalf("GET /api/settings = %d: %v", code, body)
		}
		envOut, _ := body["env"].(map[string]any)
		list, ok := envOut["default_email_domains"].([]any)
		if !ok {
			t.Fatalf("ALLOWED_EMAIL_DOMAINS=%q: default_email_domains is %v, want a list, empty or not", env, envOut["default_email_domains"])
		}
		var got []string
		for _, d := range list {
			got = append(got, d.(string))
		}
		if strings.Join(got, "|") != want {
			t.Errorf("ALLOWED_EMAIL_DOMAINS=%q reached the console as %v", env, got)
		}
	}
}

// F2: failed console logins lock out.
func TestLoginLockout(t *testing.T) {
	l := &loginFails{at: map[string]*failRun{}}
	key := "ip:203.0.113.9"
	for i := 0; i < loginMaxFails-1; i++ {
		l.fail(key)
		if _, locked := l.locked(key); locked {
			t.Fatalf("locked out after %d failures, before the limit of %d", i+1, loginMaxFails)
		}
	}
	l.fail(key)
	d, locked := l.locked(key)
	if !locked {
		t.Fatalf("expected a lockout after %d failures", loginMaxFails)
	}
	if d <= 0 || d > loginLockout {
		t.Errorf("lockout of %v is outside 0..%v", d, loginLockout)
	}
	if _, locked := l.locked("ip:198.51.100.4"); locked {
		t.Error("a different address must not be locked out")
	}
	l.reset(key)
	if _, locked := l.locked(key); locked {
		t.Error("a successful sign-in should clear the count")
	}
}

func TestLoginFailuresExpire(t *testing.T) {
	l := &loginFails{at: map[string]*failRun{}}
	key := "user:admin"
	l.fail(key)
	l.mu.Lock()
	l.at[key].first = time.Now().Add(-loginWindow - time.Minute) // an old, stale run
	l.mu.Unlock()
	for i := 0; i < loginMaxFails-1; i++ {
		l.fail(key)
	}
	if _, locked := l.locked(key); locked {
		t.Error("failures older than the window should not count toward a lockout")
	}
}

func TestClientIPPrefersTheProxysLastEntry(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/password", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	// Cloud Run's front end appends the real client address to whatever the caller sent, so a
	// spoofed leading entry must not become the throttling key.
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want the last X-Forwarded-For entry", got)
	}
	r2 := httptest.NewRequest("POST", "/api/auth/password", nil)
	r2.RemoteAddr = "198.51.100.7:5555"
	if got := clientIP(r2); got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want the remote address", got)
	}
}

// Which scopes a workspace granted is recorded when it installs, not probed at boot: two
// workspaces can grant different sets, and a reinstall can change one of them. The startup
// report must therefore name the workspaces that cannot satisfy ALLOWED_EMAIL_DOMAINS, and
// stay silent when no domain rule is configured at all.
func TestEmailScopeIsRecordedPerInstall(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: NewChatRegistry(st, nil)}

	// Two installs of the same app, granted different scopes.
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_OK", OrgID: 1, Name: "Has the scope", EmailScope: true, DMScope: true}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_NOPE", OrgID: 1, Name: "Missing the scope"}, []byte("x")); err != nil {
		t.Fatal(err)
	}

	teams, err := st.ActiveTeams(ctx)
	if err != nil || len(teams) != 2 {
		t.Fatalf("ActiveTeams = %d, %v; want 2", len(teams), err)
	}
	byID := map[string]*Team{}
	for _, tm := range teams {
		byID[tm.TeamID] = tm
	}
	if !byID["T_OK"].EmailScope || !byID["T_OK"].DMScope {
		t.Error("a workspace that granted the scopes must be recorded as having them")
	}
	if byID["T_NOPE"].EmailScope || byID["T_NOPE"].DMScope {
		t.Error("a workspace that granted neither scope must not be recorded as having them")
	}

	// Neither call may panic or reach Slack; both read what the install recorded.
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "")
	b.checkEmailScope(ctx)
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")
	b.checkEmailScope(ctx)

	// A reinstall that grants more replaces the old answer rather than adding a second row.
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_NOPE", OrgID: 1, Name: "Missing the scope", EmailScope: true}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	tm, _ := st.Team(ctx, "T_NOPE")
	if tm == nil || !tm.EmailScope {
		t.Error("reinstalling with the scope granted must update the recorded answer")
	}
	if all, _ := st.Teams(ctx, orgID); len(all) != 2 {
		t.Errorf("a reinstall must not create a second row: %d rows", len(all))
	}
}
