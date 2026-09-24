package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestParseEmailDomains(t *testing.T) {
	got := parseEmailDomains(" Example.com, @example.org;example.com  foo.io ")
	if want := "example.com,example.org,foo.io"; strings.Join(got, ",") != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if parseEmailDomains("") != nil {
		t.Fatal("empty should be nil")
	}
}

// fakeSlack stands in for slack.com: the two OpenID Connect endpoints plus users.info, which
// the bot calls with its own token to learn the admin/owner flags.
type fakeSlack struct {
	info        map[string]any // openid.connect.userInfo reply
	user        map[string]any // users.info "user" object
	gotRedirect string
	gotSecret   string
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/openid.connect.token":
		r.ParseForm()
		f.gotRedirect, f.gotSecret = r.Form.Get("redirect_uri"), r.Form.Get("client_secret")
		if r.Form.Get("code") != "good-code" || r.Form.Get("grant_type") != "authorization_code" {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "access_token": "xoxp-test", "token_type": "Bearer"})
	case "/api/openid.connect.userInfo":
		if r.Header.Get("Authorization") != "Bearer xoxp-test" {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		json.NewEncoder(w).Encode(f.info)
	case "/api/users.info":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": f.user})
	default:
		http.NotFound(w, r)
	}
}

func TestSlackSignInFlow(t *testing.T) {
	fs := &fakeSlack{}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	old := slackOIDC
	slackOIDC = srv.URL
	defer func() { slackOIDC = old }()
	t.Setenv("SLACK_CLIENT_ID", "cid")
	t.Setenv("SLACK_CLIENT_SECRET", "sec")
	t.Setenv("ADMIN_EMAIL_DOMAINS", "example.com")
	t.Setenv("ADMIN_BASE_URL", "") // derive from the request host
	t.Setenv("ADMIN_TOKEN", "")

	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveTeam(context.Background(), &Team{TeamID: "T1", OrgID: 1, Name: "Acme"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	// Signing in with Slack founds an organisation for a person nobody invited, so this flow is
	// only reachable under open registration; a deployment defaults to first-run.
	cfg := Config{SignupMode: SignupOpen}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), slacks: testRegistry(&Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, TeamID: "T1"})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/login", b.handleLogin)
	mux.HandleFunc("GET /api/auth/callback", b.handleCallback)
	mux.HandleFunc("GET /api/me", b.handleMe)
	app := httptest.NewServer(mux)
	defer app.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) *http.Response {
		t.Helper()
		resp, err := client.Get(app.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	location := func(resp *http.Response) *url.URL {
		t.Helper()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("want 302, got %d", resp.StatusCode)
		}
		u, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// start returns the state Slack would echo back.
	start := func() string {
		t.Helper()
		loc := location(get("/api/auth/login"))
		q := loc.Query()
		// No team= pin: an admin of any connected workspace may sign in.
		if loc.Path != "/openid/connect/authorize" || q.Get("client_id") != "cid" || q.Get("team") != "" ||
			q.Get("scope") != "openid profile email" || q.Get("response_type") != "code" ||
			q.Get("redirect_uri") != app.URL+"/api/auth/callback" || q.Get("state") == "" {
			t.Fatalf("bad authorize redirect: %s", loc)
		}
		return q.Get("state")
	}
	callback := func(code, state string) *http.Response {
		t.Helper()
		return get("/api/auth/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state))
	}
	refusal := func(resp *http.Response) string {
		t.Helper()
		loc := location(resp)
		if loc.Path != "/admin/" || loc.Query().Get("auth_error") == "" {
			t.Fatalf("want redirect to the login card with auth_error, got %s", loc)
		}
		return loc.Query().Get("auth_error")
	}
	me := func() map[string]any {
		t.Helper()
		resp, err := client.Get(app.URL + "/api/me")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	userInfo := func(email string) map[string]any {
		return map[string]any{"ok": true, "sub": "U1", "name": "Alice", "email": email,
			"email_verified":            true,
			"https://slack.com/team_id": "T1", "https://slack.com/user_id": "U1"}
	}
	verified := func(email string) bool {
		t.Helper()
		u, err := st.UserByEmail(context.Background(), email)
		if err != nil || u == nil {
			t.Fatalf("no account for %s: %v", email, err)
		}
		return u.EmailVerified
	}
	roles := func(admin, owner bool) map[string]any {
		return map[string]any{"id": "U1", "name": "alice", "real_name": "Alice", "is_admin": admin, "is_owner": owner,
			"profile": map[string]any{"email": "alice@example.com"}}
	}

	if m := me(); m["signed_in"] != false || m["slack_login"] != true {
		t.Fatalf("before sign-in: %v", m)
	}

	// A bad code fails the token exchange.
	fs.info, fs.user = userInfo("alice@example.com"), roles(false, false)
	if msg := refusal(callback("bad-code", start())); !strings.Contains(msg, "invalid_code") {
		t.Fatalf("bad code: %q", msg)
	}
	// Cancel on Slack's consent screen.
	state := start()
	if msg := refusal(get("/api/auth/callback?error=access_denied&state=" + state)); !strings.Contains(msg, "cancelled") {
		t.Fatalf("cancel: %q", msg)
	}
	// A stale or forged state never reaches Slack.
	start()
	if msg := refusal(callback("good-code", "forged")); !strings.Contains(msg, "expired") {
		t.Fatalf("state: %q", msg)
	}
	if m := me(); m["signed_in"] != false {
		t.Fatalf("refusals must not leave a session: %v", m)
	}

	// Signing in with a Slack account nobody has seen founds an organisation and lands on the
	// console. Being a plain member rather than an owner is not a barrier: what they founded is
	// their own organisation, which they are the only member of.
	fs.info, fs.user = userInfo("alice@example.com"), roles(false, false)
	if loc := location(callback("good-code", start())); loc.String() != "/admin/" {
		t.Fatalf("want /admin/, got %s", loc)
	}
	if fs.gotRedirect != app.URL+"/api/auth/callback" || fs.gotSecret != "sec" {
		t.Fatalf("token exchange sent redirect=%q secret=%q", fs.gotRedirect, fs.gotSecret)
	}
	m := me()
	u, _ := m["user"].(map[string]any)
	if m["signed_in"] != true || u["user_id"] != "U1" || u["email"] != "alice@example.com" || u["name"] != "Alice" {
		t.Fatalf("after sign-in: %v", m)
	}
	// Slack said the workspace has confirmed that address, so the account arrives proved and is
	// not sent to read mail it never needed: the setup walk is open to it from the first screen.
	if !verified("alice@example.com") {
		t.Fatal("a Slack sign-up whose address Slack confirmed was still asked to confirm it")
	}
	// The state cookie is single-use: replaying the callback fails.
	if msg := refusal(callback("good-code", "anything")); !strings.Contains(msg, "expired") {
		t.Fatalf("replay: %q", msg)
	}

	// The claim that replaces the old workspace check: a second person in the SAME connected
	// Slack workspace does not thereby land in the first person's organisation. Sharing a
	// workspace is not membership — an invitation is.
	firstOrg, _ := u["org_id"].(string)
	if len(firstOrg) != 32 {
		t.Fatalf("org_id = %v, want a 32-character public id", u["org_id"])
	}
	fs.info = map[string]any{"ok": true, "sub": "U2", "name": "Bob", "email": "bob@example.com",
		"https://slack.com/team_id": "T1", "https://slack.com/user_id": "U2"}
	fs.user = map[string]any{"id": "U2", "name": "bob", "real_name": "Bob", "is_admin": false, "is_owner": false,
		"profile": map[string]any{"email": "bob@example.com"}}
	client.Jar, _ = cookiejar.New(nil) // a different person, so a different browser
	// And where he lands says the same thing twice: the organisation he founded has no workspace
	// connected, so the sign-in carries straight on to the install rather than to a console with
	// nothing in it. Alice went to /admin/ because the organisation she founded is the one this
	// test seeded T1 into.
	if loc := location(callback("good-code", start())); loc.String() != "/slack/install" {
		t.Fatalf("second sign-in: want /slack/install, got %s", loc)
	}
	m2 := me()
	u2, _ := m2["user"].(map[string]any)
	if u2 == nil || u2["user_id"] != "U2" {
		t.Fatalf("second sign-in: %v", m2)
	}
	if got, _ := u2["org_id"].(string); got == firstOrg {
		t.Fatal("a colleague signing in with Slack joined the first person's organisation")
	}
	// And it is the claim that is trusted, not the sign-in: Bob's response carries no
	// email_verified, so his address is still his to prove.
	if verified("bob@example.com") {
		t.Fatal("an address Slack did not say it had confirmed was taken as confirmed")
	}
}

func TestSlackLoginUnconfigured(t *testing.T) {
	t.Setenv("SLACK_CLIENT_ID", "cid")
	t.Setenv("SLACK_CLIENT_SECRET", "")
	t.Setenv("ADMIN_BASE_URL", "https://bot.example.com/")
	b := &Bot{slacks: testRegistry(&Chat{TeamID: "T1"})}
	rec := httptest.NewRecorder()
	b.handleLogin(rec, httptest.NewRequest("GET", "/api/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "https://bot.example.com/api/auth/callback") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// Slack sends a sign-in back to the public origin, and the state cookie belongs to the host that
// set it, so a sign-in started on any other address — the loopback address of a machine that also
// has a public one — always came back without its cookie, as "That sign-in link expired or was
// already used". It is sent to the public origin first now, once, and starts there.
func TestASignInStartedOnAnotherAddressStartsOnThePublicOrigin(t *testing.T) {
	t.Setenv("SLACK_CLIENT_ID", "cid")
	t.Setenv("SLACK_CLIENT_SECRET", "sec")
	t.Setenv("ADMIN_BASE_URL", "https://bot.example.com")
	b := &Bot{slacks: testRegistry(&Chat{TeamID: "T1"})}

	start := func(target, host string, https bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r.Host = host
		if https {
			r.Header.Set("X-Forwarded-Proto", "https")
		}
		w := httptest.NewRecorder()
		b.handleLogin(w, r)
		return w
	}
	stateSet := func(w *httptest.ResponseRecorder) bool {
		for _, c := range w.Result().Cookies() {
			if c.Name == stateCookie && c.Value != "" {
				return true
			}
		}
		return false
	}
	toSlack := func(w *httptest.ResponseRecorder) bool {
		return w.Code == http.StatusFound && strings.HasPrefix(w.Header().Get("Location"), slackOIDC+"/") && stateSet(w)
	}

	// On the loopback address: off to the public origin with the timezone kept, and no cookie
	// left behind on a host the sign-in will never come back to.
	w := start("/api/auth/login?tz=Europe%2FLondon", "localhost:8090", false)
	if w.Code != http.StatusFound || stateSet(w) {
		t.Fatalf("a start on localhost answered %d, state cookie set: %v", w.Code, stateSet(w))
	}
	next, err := url.Parse(w.Header().Get("Location"))
	if err != nil || next.Scheme+"://"+next.Host+next.Path != "https://bot.example.com/api/auth/login" || next.Query().Get("tz") != "Europe/London" {
		t.Fatalf("a start on localhost was sent to %q", w.Header().Get("Location"))
	}

	// On the public origin, however the host is written: straight to Slack, with the cookie.
	for _, host := range []string{"bot.example.com", "BOT.example.com:443"} {
		if w := start(next.RequestURI(), host, true); !toSlack(w) {
			t.Fatalf("a start on %s answered %d to %q", host, w.Code, w.Header().Get("Location"))
		}
	}

	// Once only: behind a proxy that rewrites Host every request looks like another address, and
	// a second bounce would be a loop.
	if w := start("/api/auth/login?bounced=1", "10.0.0.7:8080", false); !toSlack(w) {
		t.Fatalf("a bounced start was not let through: %d to %q", w.Code, w.Header().Get("Location"))
	}

	// Nothing pinned or learned: the base is this machine's own listen address, which is no place
	// to send anybody else's browser, so the sign-in starts where it is.
	t.Setenv("ADMIN_BASE_URL", "")
	if w := start("/api/auth/login", "mini.example.ts.net", true); !toSlack(w) {
		t.Fatalf("with no public origin known a start was sent elsewhere: %d to %q", w.Code, w.Header().Get("Location"))
	}
}
