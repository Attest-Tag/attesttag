package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// What a stolen cookie, a copied database, or a page on another origin can and cannot do with a
// console session. Each test here asserts the safe behaviour of one of the review's findings.

// rawPost is post() without the JSON decoding, for tests that need the response headers.
func rawPost(t *testing.T, mux *http.ServeMux, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSessionTokensAreHashedAtRest(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, _, token := signedUp(t, b, mux, st, "founder@example.com")

	var raw, unhashed int
	st.db.QueryRowContext(ctx, `select count(*) from admin_sessions where token=?`, token).Scan(&raw)
	st.db.QueryRowContext(ctx, `select count(*) from admin_sessions where length(token)<>64`).Scan(&unhashed)
	if raw != 0 || unhashed != 0 {
		t.Fatalf("the session table holds %d raw token(s) and %d row(s) that are not hashes", raw, unhashed)
	}
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, token); code != 200 {
		t.Fatalf("the raw token no longer signs in: %d", code)
	}

	// A database from before tokens were hashed still holds credentials; opening it rewrites
	// them, and the cookie people already hold keeps working.
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := randomToken()
	if _, err := old.db.Exec(`insert into admin_sessions (token, id, org_id, expires_at) values (?, 1, 1, ?)`,
		legacy, time.Now().Add(time.Hour).UTC().Format(time.DateTime)); err != nil {
		t.Fatal(err)
	}
	old.db.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	var stored string
	reopened.db.QueryRow(`select token from admin_sessions`).Scan(&stored)
	if stored != hashSessionToken(legacy) {
		t.Fatalf("legacy row was not rehashed: %q", stored)
	}
	if u, _ := reopened.AdminSession(ctx, legacy); u == nil || u.ID != 1 {
		t.Fatal("the legacy session stopped working after the rehash")
	}
}

func TestCookiesAreSecureUnlessLoopback(t *testing.T) {
	_, mux, _ := identityBot(t) // ADMIN_BASE_URL is https://console.example.com
	check := func(email, want string, secure bool) {
		t.Helper()
		w := rawPost(t, mux, "/api/auth/signup", `{"email":"`+email+`","password":"correct horse battery","org":"Acme"}`, nil)
		if w.Code != 200 {
			t.Fatalf("signup = %d: %s", w.Code, w.Body.String())
		}
		for _, name := range []string{sessionCookie, csrfCookie} {
			c := cookieNamed(w, name)
			if c == nil {
				t.Fatalf("%s: no %s cookie", want, name)
			}
			if c.Secure != secure {
				t.Errorf("%s: %s Secure=%v, want %v", want, name, c.Secure, secure)
			}
		}
	}
	check("a@example.com", "https origin", true)
	// A plain-http origin that is not the machine itself still gets a Secure cookie: the flag is
	// what stops the browser sending it in the clear, and one http request must not unset it.
	t.Setenv("ADMIN_BASE_URL", "http://console.example.com")
	check("b@example.com", "http origin", true)
	t.Setenv("ADMIN_BASE_URL", "http://localhost:8080")
	check("c@example.com", "loopback", false)
}

func TestRecoveryCodesNeedProof(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	u, _, token := signedUp(t, b, mux, st, "founder@example.com")
	secret, first := enrol(t, mux, token)

	if code, _ := authReq(t, mux, "POST", "/api/account/totp/recovery", map[string]string{}, token); code != 403 {
		t.Fatalf("recovery codes were reissued with no proof: %d", code)
	}
	if code, _ := authReq(t, mux, "POST", "/api/account/totp/recovery", map[string]string{"password": "wrong"}, token); code != 403 {
		t.Fatalf("a wrong password reissued recovery codes: %d", code)
	}
	if left := st.RecoveryCodesLeft(ctx, u.ID); left != recoveryCodeCount {
		t.Fatalf("a refused reissue changed the codes: %d left", left)
	}
	// A current code is proof, once.
	next, _ := totpCodeAt(secret, totpStep(time.Now())+1)
	code, body := authReq(t, mux, "POST", "/api/account/totp/recovery", map[string]string{"code": next}, token)
	if code != 200 {
		t.Fatalf("a current code was refused: %d %v", code, body)
	}
	fresh := body["recovery_codes"].([]any)
	for _, f := range fresh {
		for _, o := range first {
			if f == o {
				t.Fatal("a reissue handed back one of the old codes")
			}
		}
	}
	if code, _ := authReq(t, mux, "POST", "/api/account/totp/recovery", map[string]string{"code": next}, token); code != 403 {
		t.Fatalf("the same code was proof twice: %d", code)
	}
	// So is the password; and the same code is not proof for turning the factor off either.
	if code, _ := authReq(t, mux, "POST", "/api/account/totp/disable", map[string]string{"code": next}, token); code != 403 {
		t.Fatalf("a spent code turned two-factor off: %d", code)
	}
	if code, _ := authReq(t, mux, "POST", "/api/account/totp/disable", map[string]string{"password": "correct horse battery"}, token); code != 200 {
		t.Fatalf("the password did not turn two-factor off: %d", code)
	}
}

func TestFirstPasswordNeedsProof(t *testing.T) {
	_, mux, st := identityBot(t)
	ctx := context.Background()
	// A Slack sign-in: an account with no password and no authenticator.
	u, err := st.CreateUser(ctx, "slacker@example.com", "Slacker", "")
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(ctx, "Org", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, OrgID: org.ID, Via: "slack"}, time.Hour)
	st.db.ExecContext(ctx, `update admin_sessions set created_at=? where token=?`,
		time.Now().Add(-11*time.Minute).UTC().Format(time.DateTime), hashSessionToken(stale))

	// A console found signed in an hour ago cannot give the account a password.
	if code, body := authReq(t, mux, "POST", "/api/auth/password", map[string]string{"password": "a whole new passphrase"}, stale); code != 403 {
		t.Fatalf("a stale session set a first password: %d %v", code, body)
	}
	if acct, _ := st.User(ctx, u.ID); acct.HasPassword() {
		t.Fatal("the password was stored anyway")
	}
	// A session minted minutes ago — the person has just signed in with Slack — can.
	fresh, _ := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, OrgID: org.ID, Via: "slack"}, time.Hour)
	if code, body := authReq(t, mux, "POST", "/api/auth/password", map[string]string{"password": "a whole new passphrase"}, fresh); code != 200 {
		t.Fatalf("a fresh session could not set a first password: %d %v", code, body)
	}
	if acct, _ := st.User(ctx, u.ID); !acct.HasPassword() {
		t.Fatal("the password was not stored")
	}
	// And setting it closed every other console, the stale one included.
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, stale); code != 401 {
		t.Errorf("the stale session survived a password being set: %d", code)
	}
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, fresh); code != 200 {
		t.Errorf("the session that set the password was closed too: %d", code)
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	u, orgID, token := signedUp(t, b, mux, st, "founder@example.com")
	other, _ := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, OrgID: orgID, Via: "password"}, time.Hour)
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, other); code != 200 {
		t.Fatalf("second session does not work before the change: %d", code)
	}
	if code, body := authReq(t, mux, "POST", "/api/auth/password",
		map[string]string{"current": "correct horse battery", "password": "a whole new passphrase"}, token); code != 200 {
		t.Fatalf("change = %d %v", code, body)
	}
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, other); code != 401 {
		t.Errorf("the other session survived the password change: %d", code)
	}
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, token); code != 200 {
		t.Errorf("the session that changed the password was closed: %d", code)
	}
}

func TestRemovingAMemberRevokesKeysAndSessions(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, admin := signedUp(t, b, mux, st, "admin@example.com")
	m, err := st.CreateUser(ctx, "member@example.com", "Member", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMembership(ctx, m.ID, orgID, RoleAdmin, 0); err != nil {
		t.Fatal(err)
	}
	theirs, _ := st.CreateAdminSession(ctx, AdminUser{ID: m.ID, OrgID: orgID, Via: "password"}, time.Hour)
	_, keyID := mintKey(t, mux, theirs, "their integration")

	if code, body := authReq(t, mux, "DELETE", "/api/console/users/"+m.PublicID, nil, admin); code != 200 {
		t.Fatalf("remove = %d %v", code, body)
	}
	var revoked int
	st.db.QueryRowContext(ctx, `select count(*) from api_keys where id=? and revoked_at is not null`, keyID).Scan(&revoked)
	if revoked != 1 {
		t.Error("the removed member's key still reads as live")
	}
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, theirs); code != 401 {
		t.Errorf("the removed member's session still opens the console: %d", code)
	}
}

func TestSessionWithoutOrgIsUnauthenticated(t *testing.T) {
	b, mux, st := identityBot(t)
	u, _, _ := signedUp(t, b, mux, st, "founder@example.com")
	tok, _ := st.CreateAdminSession(context.Background(), AdminUser{ID: u.ID, OrgID: 0}, time.Hour)
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, tok); code != 401 {
		t.Fatalf("a session with no organisation reached the deployment namespace: %d", code)
	}
}

func TestLogoutIsPostOnly(t *testing.T) {
	_, mux, _ := identityBot(t)
	r := httptest.NewRequest("GET", "/api/auth/logout", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/auth/logout = %d, want 405: a link on any page could sign the visitor out", w.Code)
	}
}

func TestResendVerificationNeedsCSRF(t *testing.T) {
	_, mux, _ := identityBot(t)
	w := rawPost(t, mux, "/api/auth/signup", `{"email":"new@example.com","password":"correct horse battery","org":"Acme"}`, nil)
	session, csrf := cookieNamed(w, sessionCookie), cookieNamed(w, csrfCookie)
	if session == nil || csrf == nil {
		t.Fatal("signup set no cookies")
	}
	// The cookie alone — what a cross-site page can send — is refused.
	r := httptest.NewRequest("POST", "/api/auth/resend-verification", nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(strings.ToLower(w.Body.String()), "csrf") {
		t.Fatalf("a cookie-only request sent mail: %d %s", w.Code, w.Body.String())
	}
	// With the token the page echoes back, it goes through.
	r = httptest.NewRequest("POST", "/api/auth/resend-verification", nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	r.Header.Set(csrfHeader, csrf.Value)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("with the CSRF token: %d %s", w.Code, w.Body.String())
	}
}

func TestSecurityHeaders(t *testing.T) {
	inline := "console.log('theme')"
	fsys := fstest.MapFS{
		"index.html":       {Data: []byte(`<html><script>` + inline + `</script><script src="/admin/x.js" async=""></script></html>`)},
		"login/index.html": {Data: []byte(`<script>` + inline + `</script>`)},
	}
	hashes := inlineScriptHashes(fsys)
	sum := sha256.Sum256([]byte(inline))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if len(hashes) != 1 || hashes[0] != want {
		t.Fatalf("hashes = %v, want [%s]", hashes, want)
	}
	h := secureHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), hashes, "")
	get := func(path, proto string) http.Header {
		r := httptest.NewRequest("GET", path, nil)
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Header()
	}
	api := get("/api/me", "")
	if api.Get("X-Content-Type-Options") != "nosniff" || api.Get("X-Frame-Options") != "DENY" || api.Get("Cache-Control") != "no-store" {
		t.Errorf("api headers: %v", api)
	}
	if !strings.HasPrefix(api.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("api CSP allows rendering: %s", api.Get("Content-Security-Policy"))
	}
	if api.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent on a plain-http request")
	}
	if get("/api/me", "https").Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS on an https request")
	}
	console := get("/admin/", "").Get("Content-Security-Policy")
	if !strings.Contains(console, "script-src 'self' "+want) || !strings.Contains(console, "frame-ancestors 'none'") {
		t.Errorf("console CSP: %s", console)
	}
	page := get("/configure/T1/C1", "").Get("Content-Security-Policy")
	if strings.Contains(page, "script-src") || !strings.Contains(page, "default-src 'self'") {
		t.Errorf("page CSP: %s", page)
	}
}
