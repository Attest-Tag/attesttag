package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// A connection people sign into for themselves is only worth having if one person's grant is
// unreachable from another person's question. That is what most of this file is about.

func userConnFixture(t *testing.T) (*Store, *Proxy, *Connection) {
	t.Helper()
	fixedMasterKey(t)
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bd, err := st.CreateBundle(ctx, orgID, "tools", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(&Secret{OAuth: &OAuthState{ClientID: "cid", ClientSecret: "csec",
		AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		Scopes: "openid email https://www.googleapis.com/auth/calendar.events"}})
	enc, err := sealer.Seal(sec)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.InsertConnection(ctx, orgID, &Connection{BundleID: bd.ID, Name: "Google", Preset: "google",
		CredType: "oauth_user", AllowedHosts: []string{"www.googleapis.com"}, PathPrefixes: []string{"/calendar/v3"},
		Status: "active"}, enc)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := st.Connection(ctx, orgID, id)
	if err != nil || conn == nil {
		t.Fatalf("read back the connection: %v", err)
	}
	return st, NewProxy(sealer, st), conn
}

// grant seals a token for one person, the way the callback does.
func grant(t *testing.T, st *Store, p *Proxy, conn *Connection, teamID, user, token string, expires time.Time) {
	t.Helper()
	enc, err := p.sealUserSecret(&OAuthState{AccessToken: token, RefreshToken: "r-" + token,
		ExpiresAt: expires.Unix(), ClientID: "cid", TokenURL: "https://oauth2.googleapis.com/token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveUserConnection(context.Background(), orgID, conn.ID, teamID, user, user+"@example.com", enc); err != nil {
		t.Fatal(err)
	}
}

// The whole point of the feature: the token that goes on the wire is the asker's.
func TestUserTokenIsPerPerson(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	grant(t, st, p, conn, "T1", "USAM", "sam-token", future)
	grant(t, st, p, conn, "T1", "UPRIYA", "priya-token", future)

	for _, tc := range []struct{ user, want string }{{"USAM", "sam-token"}, {"UPRIYA", "priya-token"}} {
		got, err := p.userToken(ctx, orgID, conn, "T1", tc.user)
		if err != nil {
			t.Fatalf("%s: %v", tc.user, err)
		}
		if got != tc.want {
			t.Fatalf("%s got %q, want %q — one person's question reached another's account", tc.user, got, tc.want)
		}
	}
}

// Nobody connected, nobody impersonated. Each of these must stop short of the wire rather than
// falling back to any token that happens to exist.
func TestUserTokenRefusesWithoutAGrant(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	grant(t, st, p, conn, "T1", "USAM", "sam-token", time.Now().Add(time.Hour))

	for _, tc := range []struct{ name, team, user string }{
		{"a person who has not connected", "T1", "UNEW"},
		{"no requester at all", "T1", ""},
		{"the same id in another workspace", "T2", "USAM"},
	} {
		if _, err := p.userToken(ctx, orgID, conn, tc.team, tc.user); !errors.Is(err, ErrNeedsUserAuth) {
			t.Fatalf("%s: got %v, want ErrNeedsUserAuth", tc.name, err)
		}
	}
	// And another organisation's row is not reachable either, even naming the same connection id.
	if _, err := p.userToken(ctx, orgID+1, conn, "T1", "USAM"); !errors.Is(err, ErrNeedsUserAuth) {
		t.Fatalf("another org reached the grant: %v", err)
	}
}

// An expired token with a refresh token is renewed and written back sealed; an expired one
// without asks the person again rather than reporting a fault they cannot act on.
func TestUserTokenRefreshes(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	var gotGrant, gotRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotGrant, gotRefresh = r.FormValue("grant_type"), r.FormValue("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"fresh","expires_in":3600}`))
	}))
	defer srv.Close()
	// The token endpoint is reached over the public-only transport, which refuses a loopback
	// address and an ephemeral port. Lend it the test server's client for the duration.
	saved := oauthHTTPClient
	oauthHTTPClient = srv.Client()
	defer func() { oauthHTTPClient = saved }()

	enc, err := p.sealUserSecret(&OAuthState{AccessToken: "stale", RefreshToken: "r1",
		ExpiresAt: time.Now().Add(-time.Minute).Unix(), ClientID: "cid", TokenURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveUserConnection(ctx, orgID, conn.ID, "T1", "USAM", "sam@example.com", enc); err != nil {
		t.Fatal(err)
	}
	got, err := p.userToken(ctx, orgID, conn, "T1", "USAM")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got != "fresh" {
		t.Fatalf("got %q, want the refreshed token", got)
	}
	if gotGrant != "refresh_token" || gotRefresh != "r1" {
		t.Fatalf("sent grant_type=%q refresh_token=%q", gotGrant, gotRefresh)
	}
	// The new token is on the row, so the next call does not refresh again.
	uc, _ := st.UserConnection(ctx, orgID, conn.ID, "T1", "USAM")
	st2, err := p.openUserSecret(uc)
	if err != nil {
		t.Fatal(err)
	}
	if st2.AccessToken != "fresh" {
		t.Fatalf("stored %q, want the refreshed token", st2.AccessToken)
	}

	// Expired with nothing to renew: ask again.
	enc, _ = p.sealUserSecret(&OAuthState{AccessToken: "stale", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	st.SaveUserConnection(ctx, orgID, conn.ID, "T1", "UDRY", "dry@example.com", enc)
	if _, err := p.userToken(ctx, orgID, conn, "T1", "UDRY"); !errors.Is(err, ErrNeedsUserAuth) {
		t.Fatalf("expired without a refresh token: got %v, want ErrNeedsUserAuth", err)
	}
}

// The connect link is a bearer capability naming exactly one person, so it must not survive
// having any part of that name changed.
func TestConnectTokenBinding(t *testing.T) {
	fixedMasterKey(t)
	now := time.Now()
	tok := mintConnectToken(7, 42, "T1", "USAM", now)
	if tok == "" {
		t.Fatal("no token minted")
	}
	cl, ok := verifyConnectToken(tok, now)
	if !ok || cl.OrgID != 7 || cl.ConnID != 42 || cl.TeamID != "T1" || cl.SlackUserID != "USAM" {
		t.Fatalf("round trip lost the claim: %+v ok=%v", cl, ok)
	}
	if _, ok := verifyConnectToken(tok, now.Add(connectLinkTTL+time.Minute)); ok {
		t.Fatal("an expired link still opened")
	}
	// Rewrite each field in the body and keep the MAC: every one must be rejected.
	parts := strings.Split(tok, ".")
	for i, sub := range map[int]string{1: "8", 2: "43", 3: "T2", 4: "UPRIYA", 5: "9999999999"} {
		bad := append([]string{}, parts...)
		bad[i] = sub
		if _, ok := verifyConnectToken(strings.Join(bad, "."), now); ok {
			t.Fatalf("a link with field %d rewritten to %q was accepted", i, sub)
		}
	}
	// The last character has to actually change: about one token in fourteen already ends in
	// "0", and appending it again left the token untouched — so this check passed or failed
	// depending on the MAC it happened to get.
	last := tok[len(tok)-1]
	swap := byte('0')
	if last == swap {
		swap = '1'
	}
	if _, ok := verifyConnectToken(tok[:len(tok)-1]+string(swap), now); ok {
		t.Fatal("a link with a rewritten MAC was accepted")
	}
}

// Two connections on www.googleapis.com — Drive and Calendar — and the path is what tells them
// apart. Matching on the host alone sent calendar calls out under Drive's credential.
func TestMatchPicksByPathAcrossASharedHost(t *testing.T) {
	drive := &Connection{ID: 1, Name: "Drive", CredType: "gcp_sa", AllowedHosts: []string{"www.googleapis.com"},
		Methods: []string{"GET"}}
	google := &Connection{ID: 2, Name: "Google", CredType: "oauth_user", AllowedHosts: []string{"www.googleapis.com"},
		PathPrefixes: []string{"/calendar/v3"}}
	p := &Proxy{}
	acc := &Access{Rules: []Rule{{Conn: drive}, {Conn: google}}}

	u, _ := url.Parse("https://www.googleapis.com/calendar/v3/calendars/primary/events")
	// Drive is first and allows the host, but only GET; the booking is a POST that Google allows.
	conn, why := p.Match(acc, "POST", u)
	if conn == nil || conn.ID != google.ID {
		t.Fatalf("POST to calendar matched %v (%s), want the Google connection", conn, why)
	}
	// Drive still gets its own paths, and still only for the methods it allows.
	du, _ := url.Parse("https://www.googleapis.com/drive/v3/files")
	if conn, why := p.Match(acc, "GET", du); conn == nil || conn.ID != drive.ID {
		t.Fatalf("drive read matched %v (%s), want Drive", conn, why)
	}
	if conn, why := p.Match(acc, "DELETE", du); conn != nil || why == "" {
		t.Fatalf("a delete Drive does not allow was let through as %v", conn)
	}
	// A host nobody covers still reports the host, not a near miss on another connection.
	ou, _ := url.Parse("https://example.com/x")
	if _, why := p.Match(acc, "GET", ou); !strings.Contains(why, "example.com") {
		t.Fatalf("blocked reason lost the host: %q", why)
	}
}

// A path prefix is checked against the path that goes on the wire, so a ".." in it would let a
// Calendar-only connection spend its credential on Gmail: Google collapses the segment, our
// prefix check does not. Refused before any of that, in both spellings.
func TestMatchRefusesDotSegments(t *testing.T) {
	google := &Connection{ID: 2, Name: "Google", CredType: "oauth_user", AllowedHosts: []string{"www.googleapis.com"},
		PathPrefixes: []string{"/calendar/v3"}}
	p := &Proxy{}
	acc := &Access{Rules: []Rule{{Conn: google}}, Domains: []Domain{{Host: "www.googleapis.com"}}}
	for _, raw := range []string{
		"https://www.googleapis.com/calendar/v3/../gmail/v1/users/me/messages",
		"https://www.googleapis.com/calendar/v3/%2e%2e/gmail/v1/users/me/messages",
		"https://www.googleapis.com/calendar/v3/%2E%2E/drive/v3/files",
		"https://www.googleapis.com/calendar/v3/./../drive/v3/files",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		conn, why := p.Match(acc, "GET", u)
		if conn != nil || why == "" {
			t.Fatalf("%s matched %v (%q); a dot segment must be refused", raw, conn, why)
		}
	}
	// The ordinary path is untouched.
	u, _ := url.Parse("https://www.googleapis.com/calendar/v3/calendars/primary/events")
	if conn, why := p.Match(acc, "GET", u); conn == nil || conn.ID != google.ID {
		t.Fatalf("a plain calendar path matched %v (%s)", conn, why)
	}
}

// scopeLines is the consent screen: a list of URLs tells somebody nothing about what they are
// agreeing to, and the sign-in ones are noise.
func TestScopeLinesReadAsPermissions(t *testing.T) {
	got := scopeLines("openid email https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/gmail.readonly")
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(got), got)
	}
	// Every scope the presets ask for has to have words. One that does not falls through to its
	// raw URL, and the consent screen then asks somebody to agree to
	// "https://www.googleapis.com/auth/contacts.other.readonly", which nobody can weigh.
	for _, pr := range Presets {
		if pr.CredType != "oauth_user" || pr.Scopes == "" {
			continue
		}
		for _, line := range scopeLines(pr.Scopes) {
			if strings.HasPrefix(line, "http") {
				t.Errorf("%s: no words for scope %s", pr.ID, line)
			}
		}
	}
	for _, want := range []string{"Create and change events", "Read your mail"} {
		found := false
		for _, g := range got {
			if strings.Contains(g, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no line said %q: %q", want, got)
		}
	}
}

func TestAccountFromIDToken(t *testing.T) {
	// header.payload.signature, payload base64url without padding
	if got := accountFromIDToken("aaa.eyJlbWFpbCI6InNhbUBleGFtcGxlLmNvbSJ9.bbb"); got != "sam@example.com" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"", "notatoken", "a.b", "a.!!!.c"} {
		if got := accountFromIDToken(bad); got != "" {
			t.Fatalf("%q yielded %q", bad, got)
		}
	}
}

// The whole round trip through the HTTP surface, against a stubbed provider: the page renders
// what is being granted, the button sends the person to the provider with a code challenge and
// a request for a refresh token, and the callback seals what comes back against the one person
// the link named.
func TestConnectRoundTrip(t *testing.T) {
	fixedMasterKey(t)
	t.Setenv("ADMIN_BASE_URL", "https://console.example.com")
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var tokenForm url.Values
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		tokenForm = r.Form
		// id_token payload: {"email":"sam@example.com"}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,` +
			`"id_token":"aaa.eyJlbWFpbCI6InNhbUBleGFtcGxlLmNvbSJ9.bbb"}`))
	}))
	defer provider.Close()
	// The token endpoint has to look public to pass the SSRF guard, so the connection names a
	// public URL and the test's client is what actually reaches the stub.
	const tokenURL = "https://oauth.example.com/token"
	saved := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: rewriteTo(provider.URL)}
	defer func() { oauthHTTPClient = saved }()

	bd, err := st.CreateBundle(ctx, orgID, "tools", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(&Secret{OAuth: &OAuthState{ClientID: "cid", ClientSecret: "csec",
		AuthURL: "https://accounts.example.com/authorize", TokenURL: tokenURL,
		Scopes: "openid email https://www.googleapis.com/auth/calendar.events"}})
	enc, _ := sealer.Seal(sec)
	connID, err := st.InsertConnection(ctx, orgID, &Connection{BundleID: bd.ID, Name: "Google", Preset: "google",
		CredType: "oauth_user", AllowedHosts: []string{"www.googleapis.com"}, Status: "active"}, enc)
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, Config{}),
		resolver: NewResolver(st), proxy: NewProxy(sealer, st)}
	b.slacks = NewChatRegistry(st, sealer)
	mux := http.NewServeMux()
	b.routes(mux, nil)

	link := b.connectLinkURL(ctx, orgID, connID, "T1", "USAM")
	if link == "" {
		t.Fatal("no connect link")
	}
	path := strings.TrimPrefix(link, "https://console.example.com")

	// 1. The page says what is being granted, in words rather than scope URLs.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("connect page: %d %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "Create and change events") {
		t.Fatalf("the page did not say what it grants: %s", body)
	}
	// The policy that governs where the Continue button may land is the one on the page holding
	// the form, not the one on the POST it makes — form-action is enforced against the whole
	// navigation, redirects included. Served under the blanket `form-action 'self'`, the button
	// did nothing at all: the POST ran, the 302 to the provider was swallowed by the browser,
	// and neither end logged a thing.
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://accounts.example.com") {
		t.Fatalf("the connect page's policy does not let its form reach the provider: %q", csp)
	}
	var csrf *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == connectCSRFCookie {
			csrf = c
		}
	}
	if csrf == nil {
		t.Fatal("no CSRF cookie was set, so the form could be submitted from anywhere")
	}

	// 2. Without that cookie the POST is refused.
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", path, strings.NewReader("csrf=guessed"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a forged POST got %d, want 403", w.Code)
	}

	// 3. Ticking nothing is a question put back to them, not a silent grant of everything.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", path, strings.NewReader(url.Values{"csrf": {csrf.Value}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(csrf)
	mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Pick at least one") {
		t.Fatalf("an empty choice got %d, want the page back with a prompt: %s", w.Code, truncate(w.Body.String(), 200))
	}

	// 4. With a part ticked, off to the provider.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", path, strings.NewReader(url.Values{"csrf": {csrf.Value}, "grant": {"calendar:write"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(csrf)
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	auth, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := auth.Query()
	for k, want := range map[string]string{"client_id": "cid", "code_challenge_method": "S256",
		"access_type": "offline", "prompt": "consent", "response_type": "code",
		"redirect_uri": "https://console.example.com/connect/callback"} {
		if q.Get(k) != want {
			t.Fatalf("authorize %s=%q, want %q", k, q.Get(k), want)
		}
	}
	// Incremental authorisation would hand Google every scope this person ever granted, and the
	// consent screen would then ask for all of them however few boxes were ticked.
	if q.Has("include_granted_scopes") {
		t.Error("include_granted_scopes is back: the consent screen will ignore what was ticked")
	}
	// And what is asked for is exactly the ticked part, nothing else.
	if got := q.Get("scope"); !strings.Contains(got, "calendar.events") {
		t.Errorf("scope %q lost the part that was ticked", got)
	} else if strings.Contains(got, "gmail") || strings.Contains(got, "contacts") {
		t.Errorf("scope %q asks for parts nobody ticked", got)
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatal("no PKCE challenge or state on the authorize URL")
	}

	// 5. The provider sends them back; the tokens land against USAM and nobody else.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/connect/callback?code=thecode&state="+q.Get("state"), nil))
	if w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sam@example.com") {
		t.Fatalf("the page did not name the account: %s", w.Body.String())
	}
	if tokenForm.Get("code") != "thecode" || tokenForm.Get("code_verifier") == "" {
		t.Fatalf("token request sent %v", tokenForm)
	}
	tok, err := b.proxy.userToken(ctx, orgID, mustConn(t, st, connID), "T1", "USAM")
	if err != nil || tok != "at" {
		t.Fatalf("USAM got %q, %v", tok, err)
	}
	if _, err := b.proxy.userToken(ctx, orgID, mustConn(t, st, connID), "T1", "UPRIYA"); !errors.Is(err, ErrNeedsUserAuth) {
		t.Fatal("somebody else reached the token that was just granted")
	}

	// 5. The state is spent, so a replayed callback grants nothing.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/connect/callback?code=thecode&state="+q.Get("state"), nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("a replayed callback got %d, want 403", w.Code)
	}
}

func mustConn(t *testing.T, st *Store, id int64) *Connection {
	t.Helper()
	c, err := st.Connection(context.Background(), orgID, id)
	if err != nil || c == nil {
		t.Fatalf("connection %d: %v", id, err)
	}
	return c
}

// rewriteTo sends every request to one address, whatever host it names. The outbound transport
// refuses a loopback address and an ephemeral port, which is right in production and leaves a
// test with no way to stand in for a provider.
func rewriteTo(base string) http.RoundTripper {
	u, _ := url.Parse(base)
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
		return http.DefaultTransport.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// One person's notes about their own account are theirs: they survive a re-sign-in, they do not
// make an unconnected row look connected, and they are bounded so nobody can push the question
// out of their own prompt.
func TestUserInstructionsAreTheirOwn(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()

	// Written before signing in at all: the row exists, and it grants nothing.
	if err := st.SaveUserInstructions(ctx, orgID, conn.ID, "T1", "USAM", "Sign off as Sam."); err != nil {
		t.Fatal(err)
	}
	uc, err := st.UserConnection(ctx, orgID, conn.ID, "T1", "USAM")
	if err != nil || uc == nil {
		t.Fatalf("no row after writing instructions: %v", err)
	}
	if uc.Instructions != "Sign off as Sam." {
		t.Fatalf("instructions = %q", uc.Instructions)
	}
	if uc.Connected() {
		t.Error("a row holding only instructions reads as connected")
	}
	if _, err := p.userToken(ctx, orgID, conn, "T1", "USAM"); !errors.Is(err, ErrNeedsUserAuth) {
		t.Errorf("instructions alone let a call through: %v", err)
	}

	// Signing in afterwards keeps them: the grant and the preference are separate things.
	grant(t, st, p, conn, "T1", "USAM", "sam-token", time.Now().Add(time.Hour))
	uc, _ = st.UserConnection(ctx, orgID, conn.ID, "T1", "USAM")
	if uc.Instructions != "Sign off as Sam." {
		t.Errorf("signing in wiped the instructions: %q", uc.Instructions)
	}
	if !uc.Connected() {
		t.Error("a signed-in row does not read as connected")
	}

	// Nobody else's turn sees them.
	if other, _ := st.UserConnection(ctx, orgID, conn.ID, "T1", "UPRIYA"); other != nil {
		t.Error("another person picked up these instructions")
	}

	// And they cannot grow without bound.
	long := strings.Repeat("x", maxUserInstructions+500)
	if err := st.SaveUserInstructions(ctx, orgID, conn.ID, "T1", "USAM", long); err != nil {
		t.Fatal(err)
	}
	uc, _ = st.UserConnection(ctx, orgID, conn.ID, "T1", "USAM")
	if len([]rune(uc.Instructions)) != maxUserInstructions {
		t.Errorf("stored %d runes, want them capped at %d", len([]rune(uc.Instructions)), maxUserInstructions)
	}
}

// The sign-in page offers reading and writing as separate boxes, inside whatever the admin
// allowed. "Calendar read only, mail read and write, no contacts at all" has to be sayable.
func TestConnectPartsSplitReadFromWrite(t *testing.T) {
	// An admin who connected all three, calendar and mail writable.
	pr := presetByID("google")
	hosts, prefixes, asks := resolveOptions(pr, []ConnectionOption{
		{ID: "calendar", Write: true}, {ID: "gmail", Write: true}, {ID: "contacts"}})
	conn := &Connection{Preset: "google", AllowedHosts: hosts, PathPrefixes: prefixes}

	parts := connectParts(conn, asks, nil)
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3: %+v", len(parts), parts)
	}
	boxes := map[string]bool{}
	for _, p := range parts {
		for _, g := range p.Grants {
			boxes[g.Key] = true
			if !g.Checked {
				t.Errorf("%s starts unticked; the first render offers what the admin allowed", g.Key)
			}
		}
	}
	for _, want := range []string{"calendar:read", "calendar:write", "gmail:read", "gmail:write", "contacts:read"} {
		if !boxes[want] {
			t.Errorf("no box for %s", want)
		}
	}
	if boxes["contacts:write"] {
		t.Error("contacts has no write scopes, so it must not offer a write box")
	}

	// The asked-for combination is exactly the ticked boxes, plus the scopes that name the account.
	got := scopesForParts(conn, asks, map[string]bool{
		"calendar:read": true, "gmail:read": true, "gmail:write": true})
	for _, want := range []string{"openid", "email", "calendar.readonly", "gmail.readonly", "gmail.compose"} {
		if !strings.Contains(got, want) {
			t.Errorf("scopes %q missing %s", got, want)
		}
	}
	for _, absent := range []string{"calendar.events", "contacts", "directory"} {
		if strings.Contains(got, absent) {
			t.Errorf("scopes %q include %s, which was not ticked", got, absent)
		}
	}

	// A person cannot tick their way past the admin: on a read-only calendar there is no write
	// box, and asking for one anyway changes nothing.
	hosts, prefixes, readOnly := resolveOptions(pr, []ConnectionOption{{ID: "calendar"}})
	ro := &Connection{Preset: "google", AllowedHosts: hosts, PathPrefixes: prefixes}
	for _, p := range connectParts(ro, readOnly, nil) {
		for _, g := range p.Grants {
			if strings.HasSuffix(g.Key, ":write") {
				t.Errorf("a read-only connection offered %s", g.Key)
			}
		}
	}
	if got := scopesForParts(ro, readOnly, map[string]bool{"calendar:write": true}); got != "" {
		t.Errorf("forging a write box gave %q, want nothing granted", got)
	}

	// Ticking nothing grants nothing.
	if got := scopesForParts(conn, asks, map[string]bool{}); got != "" {
		t.Errorf("an empty choice gave %q", got)
	}
}

// The whole point of the feature: what somebody wrote about their own account has to reach the
// model on their turn, and nobody else's.
func TestInstructionsReachTheirOwnPrompt(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	// One box per connection, and a connection is the whole of Google Workspace — so the three
	// kinds people actually write land in it together: how to go about a search, what to do about
	// the calendar, and how to sound. All three have to survive into the prompt intact.
	const says = "Only ever look at my inbox, never sent mail or archives. " +
		"Never book me before 9am. Sign my drafts off with my first name only."
	if err := st.SaveUserInstructions(ctx, orgID, conn.ID, "T1", "USAM", says); err != nil {
		t.Fatal(err)
	}
	a := &Agent{store: st, proxy: p, settings: newSettingsCache(st, Config{}), loc: time.UTC}
	call := func(user string) string {
		return a.systemPrompt(ctx, &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", UserID: user,
			Access: &Access{Rules: []Rule{{Conn: conn, Rank: 2}}}})
	}
	mine := call("USAM")
	if !strings.Contains(mine, says) {
		t.Errorf("the asker's own instructions never reached their prompt:\n%s", truncate(mine, 600))
	}
	for _, kind := range []string{"never sent mail", "before 9am", "first name only"} {
		if !strings.Contains(mine, kind) {
			t.Errorf("the prompt dropped %q — one box has to carry every kind of rule", kind)
		}
	}
	// It has to read as an instruction about method, not as a note on tone — and it must not
	// read as permission.
	if !strings.Contains(mine, "how you go about the work") {
		t.Error("the prompt does not say the instruction covers method")
	}
	if !strings.Contains(mine, "never grants access") {
		t.Error("the prompt does not say the instruction cannot widen anything")
	}
	if theirs := call("UPRIYA"); strings.Contains(theirs, says) {
		t.Error("one person's instructions reached another person's prompt")
	}
	if nobody := call(""); strings.Contains(nobody, says) {
		t.Error("instructions reached a turn with no requester")
	}
}

// "Connect my Gmail" is a request to connect, not a question about connecting. The answer a
// person wants back is the link itself, and the prompt has to leave no room for the other
// answer — that an admin will connect their mailbox for them, which no admin can do.
func TestAskingToConnectSendsTheLink(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	rec := &recordingSlack{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1"}
	// A public address, because a Connect link that cannot be built is not sent at all.
	a := &Agent{store: st, proxy: p, settings: newSettingsCache(st, Config{}), loc: time.UTC,
		slacks: testRegistry(sl), cfg: Config{HealthAddr: "127.0.0.1:8080"}}
	c := &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", UserID: "USAM", Kind: "channel", SL: sl,
		Text: "connect my gmail", Access: &Access{Rules: []Rule{{Conn: conn, Rank: 2}}}}

	prompt := a.systemPrompt(ctx, c)
	for _, want := range []string{"has not connected theirs yet", "connect_account", "never send anyone to an admin for it"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt never says %q:\n%s", want, truncate(prompt, 1200))
		}
	}
	tool, ok := a.toolsFor(ctx, c)["connect_account"]
	if !ok {
		t.Fatal("a channel whose service runs on people's own accounts cannot offer to connect one")
	}

	out, err := tool.Run(ctx, c, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var dm string
	for _, post := range rec.posts() {
		if strings.HasPrefix(post, "chat.postMessage USAM:") {
			dm = post
		}
	}
	if dm == "" {
		t.Fatalf("nothing reached the person privately; posts = %v", rec.posts())
	}
	if !strings.Contains(dm, "/connect/") {
		t.Errorf("the card carries no link to press: %s", dm)
	}
	// The link is a bearer capability, so it goes to them in Slack and never into prompt text:
	// a tool result travels to whoever serves the model.
	if strings.Contains(out, "/connect/") {
		t.Errorf("the tool result carries the link itself:\n%s", out)
	}
	if !strings.Contains(out, "do not have one") {
		t.Errorf("the tool result leaves the model free to offer a link of its own:\n%s", out)
	}
}

// The other half of the same question, and the one that produced the wrong answer: a channel
// wired to something else entirely. Refusing is not the answer there. Connecting an account is
// the person's own to do and holds wherever the connection is later added, so the link still
// goes to them — with the half they cannot do themselves said plainly next to it.
func TestAConnectionSetUpElsewhereStillGetsItsLink(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	rec := &recordingSlack{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1"}
	a := &Agent{store: st, proxy: p, settings: newSettingsCache(st, Config{}), loc: time.UTC,
		slacks: testRegistry(sl), cfg: Config{HealthAddr: "127.0.0.1:8080"}}
	hubspot := &Connection{Name: "HubSpot", CredType: "bearer", AllowedHosts: []string{"api.hubapi.com"}}
	call := func() *Call {
		return &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", UserID: "USAM", Kind: "channel", SL: sl,
			Text: "get me the latest email from gmail", Access: &Access{Rules: []Rule{{Conn: hubspot, Rank: 2}}}}
	}

	c := call()
	prompt := a.systemPrompt(ctx, c)
	for _, want := range []string{"the organisation has set this up: Google", "call connect_account", "add that connection to this channel"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt never says %q:\n%s", want, truncate(prompt, 1200))
		}
	}
	tool, ok := a.toolsFor(ctx, c)["connect_account"]
	if !ok {
		t.Fatal("a channel that has not been given the connection cannot offer to connect it")
	}

	out, err := tool.Run(ctx, c, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var dm string
	for _, post := range rec.posts() {
		if strings.HasPrefix(post, "chat.postMessage USAM:") {
			dm = post
		}
	}
	if dm == "" {
		t.Fatalf("nothing reached the person privately; posts = %v", rec.posts())
	}
	if !strings.Contains(dm, "/connect/") {
		t.Errorf("the card carries no link to press: %s", dm)
	}
	// Both halves: the link went, and it does not on its own make the service reachable here.
	if !strings.Contains(out, "an admin adds it to this channel as well") {
		t.Errorf("the tool result promises more than connecting buys:\n%s", out)
	}
	if strings.Contains(out, "/connect/") {
		t.Errorf("the tool result carries the link itself:\n%s", out)
	}

	// And an organisation with nothing of the kind pays for none of it: no paragraph, no tool.
	if err := st.DeleteConnection(ctx, orgID, conn.ID); err != nil {
		t.Fatal(err)
	}
	got := a.systemPrompt(ctx, call())
	if strings.Contains(got, "the organisation has set this up") {
		t.Error("the paragraph is written for an organisation that has no personal connection at all")
	}
	// What replaces it is the errand, not silence: with nothing to connect anywhere, the turn
	// still owes the person what setting it up would take. TestAnOrganisationWithNoneIsToldWhatToAskFor
	// is where that lane is checked.
	if !strings.Contains(got, "Nothing in this organisation runs on people's own accounts") {
		t.Error("with the connection gone the turn is left with neither a link nor an errand")
	}
}

// The case underneath both: nobody has set any of this up. A refusal is true and useless there,
// so what the turn owes the person is the errand — who does what, once — and it owes it in the
// thread, where they can point an admin at it.
func TestAnOrganisationWithNoneIsToldWhatToAskFor(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	if err := st.DeleteConnection(ctx, orgID, conn.ID); err != nil { // an organisation with none at all
		t.Fatal(err)
	}
	rec := &recordingSlack{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1"}
	a := &Agent{store: st, proxy: p, settings: newSettingsCache(st, Config{}), loc: time.UTC,
		slacks: testRegistry(sl), cfg: Config{HealthAddr: "127.0.0.1:8080"}}
	hubspot := &Connection{Name: "HubSpot", CredType: "bearer", AllowedHosts: []string{"api.hubapi.com"}}
	call := func(text string) *Call {
		return &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", ThreadTS: "1700000000.000100", UserID: "USAM",
			Kind: "channel", SL: sl, Text: text, Access: &Access{Rules: []Rule{{Conn: hubspot, Rank: 2}}}}
	}

	c := call("get me my latest email from gmail")
	if prompt := a.systemPrompt(ctx, c); !strings.Contains(prompt, "Call connect_account") {
		t.Errorf("the prompt does not send the turn to the tool:\n%s", truncate(prompt, 1200))
	}
	tool, ok := a.toolsFor(ctx, c)["connect_account"]
	if !ok {
		t.Fatal("an organisation with nothing set up cannot say what setting it up takes")
	}
	out, err := tool.Run(ctx, c, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var card string
	for _, post := range rec.posts() {
		if strings.HasPrefix(post, "chat.postMessage C1:") {
			card = post
		}
	}
	if card == "" {
		t.Fatalf("the steps never reached the thread; posts = %v", rec.posts())
	}
	// Every step whose first attempt is the one people get wrong: the redirect URI, the page
	// the connection is made on, and the page that gives it to a channel.
	for _, want := range []string{"/connect/callback", "/bundles", "/workspaces", "Internal"} {
		if !strings.Contains(card, want) {
			t.Errorf("the steps never mention %q: %s", want, card)
		}
	}
	if !strings.Contains(out, "Do not repeat the steps") {
		t.Errorf("the model is free to paraphrase the steps it was given:\n%s", out)
	}

	// And a turn that asked about none of it pays for none of it.
	quiet := call("what did the team decide about pricing?")
	if strings.Contains(a.systemPrompt(ctx, quiet), "connect_account") {
		t.Error("the setup paragraph is sent to a turn that asked nothing of the kind")
	}
	if _, ok := a.toolsFor(ctx, quiet)["connect_account"]; ok {
		t.Error("the tool is sent to a turn that asked nothing of the kind")
	}
}

// A connection is deleted on its own or with the bundle it is filed in, and what hangs off it by
// id has to end either way: user_connections and drive_syncs name it with no foreign key to
// cascade through, and the proxy and the MCP hub cache by its id. The bundle's route did none of
// it, so every person's grant under the bundle stayed live at the provider, in a row the console
// no longer showed and nothing could remove, and each Drive sync went on failing against a
// connection that was not there.
func TestDeletingAConnectionEitherWayEndsWhatHangsOffIt(t *testing.T) {
	for _, route := range []string{"connection", "bundle"} {
		t.Run(route, func(t *testing.T) {
			b, mux, st := installTestBot(t)
			org, _, tok := seedOrg(t, st, RoleAdmin)
			ctx := context.Background()
			b.proxy = NewProxy(b.sealer, st)
			b.agent = &Agent{mcp: newMCPHub(b.proxy)}

			revoked := make(chan string, 8)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.ParseForm()
				revoked <- r.URL.Path + " " + r.Form.Get("token")
			}))
			defer provider.Close()
			saved := oauthHTTPClient
			oauthHTTPClient = &http.Client{Transport: rewriteTo(provider.URL)}
			defer func() { oauthHTTPClient = saved }()

			// Each connection has one person's grant, sealed the way the callback seals it, a Drive
			// sync spending it, and a session in the MCP hub's cache.
			conn := func(bundleID int64, name string) int64 {
				t.Helper()
				enc, err := b.sealSecret(&Secret{OAuth: &OAuthState{ClientID: "cid", ClientSecret: "csec",
					AuthURL: "https://accounts.example.com/authorize", TokenURL: "https://oauth.example.com/token"}})
				if err != nil {
					t.Fatal(err)
				}
				id, err := st.InsertConnection(ctx, org, &Connection{BundleID: bundleID, Name: name, Preset: "google",
					CredType: "oauth_user", AllowedHosts: []string{"www.googleapis.com"}, Status: "active"}, enc)
				if err != nil {
					t.Fatal(err)
				}
				us, err := b.proxy.sealUserSecret(&OAuthState{AccessToken: "at-" + name, RefreshToken: "rt-" + name,
					ClientID: "cid", TokenURL: "https://oauth.example.com/token"})
				if err != nil {
					t.Fatal(err)
				}
				if err := st.SaveUserConnection(ctx, org, id, "T1", "USAM", "sam@example.com", us); err != nil {
					t.Fatal(err)
				}
				if _, err := st.AddDriveSync(ctx, org, DriveSync{ConnectionID: id, FolderID: "root", FolderName: name,
					Dest: name, CreatedBy: "U1"}); err != nil {
					t.Fatal(err)
				}
				b.agent.mcp.conns[id] = &mcpConn{}
				return id
			}
			going, err := st.CreateBundle(ctx, org, "going", "admin@example.com")
			if err != nil {
				t.Fatal(err)
			}
			staying, err := st.CreateBundle(ctx, org, "staying", "admin@example.com")
			if err != nil {
				t.Fatal(err)
			}
			calendar, gmail, drive := conn(going.ID, "calendar"), conn(going.ID, "gmail"), conn(staying.ID, "drive")

			path, gone, kept := "/api/bundles/"+strconv.FormatInt(going.ID, 10), []int64{calendar, gmail}, []int64{drive}
			if route == "connection" {
				path, gone, kept = "/api/connections/"+strconv.FormatInt(calendar, 10), []int64{calendar}, []int64{gmail, drive}
			}
			if code, body := authReq(t, mux, "DELETE", path, nil, tok); code != 200 {
				t.Fatalf("DELETE %s = %d %v", path, code, body)
			}
			close(revoked)

			names := map[int64]string{calendar: "calendar", gmail: "gmail", drive: "drive"}
			want := map[string]bool{}
			for _, id := range gone {
				want["/revoke rt-"+names[id]] = true
			}
			got := map[string]bool{}
			for r := range revoked {
				got[r] = true
			}
			if len(got) != len(want) {
				t.Errorf("revoked at the provider: %v, want %v", got, want)
			}
			for r := range want {
				if !got[r] {
					t.Errorf("%s was never revoked at the provider: %v", r, got)
				}
			}

			syncs, err := st.DriveSyncs(ctx, org)
			if err != nil {
				t.Fatal(err)
			}
			synced := map[int64]bool{}
			for _, s := range syncs {
				synced[s.ConnectionID] = true
			}
			for _, id := range gone {
				if ucs, err := st.UserConnectionsFor(ctx, org, id); err != nil || len(ucs) != 0 {
					t.Errorf("%s: %d personal grants outlived their connection (%v)", names[id], len(ucs), err)
				}
				if synced[id] {
					t.Errorf("%s: its Drive sync outlived the connection it spends", names[id])
				}
				if _, cached := b.agent.mcp.conns[id]; cached {
					t.Errorf("%s: the MCP hub still holds a session for it", names[id])
				}
			}
			for _, id := range kept {
				if ucs, err := st.UserConnectionsFor(ctx, org, id); err != nil || len(ucs) != 1 {
					t.Errorf("%s: %d personal grants, want the 1 it had (%v)", names[id], len(ucs), err)
				}
				if !synced[id] {
					t.Errorf("%s: its Drive sync went with a connection it does not spend", names[id])
				}
				if _, cached := b.agent.mcp.conns[id]; !cached {
					t.Errorf("%s: its MCP session was dropped", names[id])
				}
			}
		})
	}
}
