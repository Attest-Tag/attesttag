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
)

// Somebody who presses Add to Slack signed out — on the site, or on Slack's own Marketplace
// listing — should end up installing, not on an Overview they then have to leave. The intent is
// a cookie the sign-in spends, and where it leads is fixed in code, never read from a URL.

// setsCookie says whether the response sets a non-empty, non-expiring cookie of that name.
func setsCookie(w *httptest.ResponseRecorder, name string) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.Value != "" && c.MaxAge >= 0 {
			return true
		}
	}
	return false
}

// clearsCookie says whether the response expires a cookie of that name.
func clearsCookie(w *httptest.ResponseRecorder, name string) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

func postWith(t *testing.T, mux *http.ServeMux, path string, body any, cookies ...*http.Cookie) (*httptest.ResponseRecorder, map[string]any) {
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
	return w, out
}

func TestInstallIntentSurvivesSignUpAndSignIn(t *testing.T) {
	_, mux, _ := identityBot(t)
	intent := &http.Cookie{Name: installIntentCookie, Value: "1"}
	creds := map[string]string{"email": "founder@example.com", "password": "correct horse battery", "org": "Acme Ltd"}

	// Signing up with the install parked: the answer says where to go, and the flag is spent.
	w, body := postWith(t, mux, "/api/auth/signup", creds, intent)
	if w.Code != 200 || body["next"] != "/slack/install" || !clearsCookie(w, installIntentCookie) {
		t.Fatalf("signup with the install parked = %d %v", w.Code, body)
	}
	// Signing in likewise.
	w, body = postWith(t, mux, "/api/auth/login", creds, intent)
	if w.Code != 200 || body["next"] != "/slack/install" || !clearsCookie(w, installIntentCookie) {
		t.Fatalf("login with the install parked = %d %v", w.Code, body)
	}
	// With nothing parked, nothing is said.
	w, body = postWith(t, mux, "/api/auth/login", creds)
	if w.Code != 200 || body["next"] != "" {
		t.Fatalf("login with nothing parked = %d %v", w.Code, body)
	}
	// An invitation in hand outranks it: joining comes first, and the install flag is spent
	// rather than left to surface on some later sign-in.
	w, body = postWith(t, mux, "/api/auth/login", creds, intent, &http.Cookie{Name: inviteCookie, Value: "tok"})
	if w.Code != 200 || body["next"] != "/admin/join/" || !clearsCookie(w, installIntentCookie) {
		t.Fatalf("login with an invitation and the install parked = %d %v", w.Code, body)
	}

	// The sign-in page learns about the parked install from /api/me, since the cookie is HttpOnly.
	me := func(cookies ...*http.Cookie) map[string]any {
		r := httptest.NewRequest("GET", "/api/me", nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := me(intent); out["install_pending"] != true {
		t.Errorf("/api/me with the install parked = %v, want install_pending", out)
	}
	if out := me(); out["install_pending"] != false {
		t.Errorf("/api/me with nothing parked = %v, want no install_pending", out)
	}
}

// Slack's Marketplace button starts the OAuth flow itself and comes back with a code and no
// state. That code is never exchanged — nothing binds it to an organisation, and a bare code in
// a link is how a stranger's workspace would be planted in somebody else's tenant — and the
// person is sent to /slack/install to start properly: directly when signed in, through the
// sign-in page with the install parked when not. A cancel on that screen is reported, not
// restarted. And a session that ran out between Add to Slack and Allow parks the install too.
func TestSlackStartedInstallRestartsFromInstall(t *testing.T) {
	_, mux, st := installTestBot(t)
	token := seedAdmin(t, st)
	old := oauthExchange
	oauthExchange = func(context.Context, string, string, string, string) (*slack.OAuthV2Response, error) {
		t.Fatal("a code that arrived with no state was exchanged")
		return nil, nil
	}
	defer func() { oauthExchange = old }()

	w := do(t, mux, "GET", "/slack/oauth/callback?code=x", token)
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || loc != "/slack/install" {
		t.Errorf("signed in, no state = %d %q, want a restart from /slack/install", w.Code, loc)
	}
	w = do(t, mux, "GET", "/slack/oauth/callback?code=x", "")
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || loc != "/admin/login/" || !setsCookie(w, installIntentCookie) {
		t.Errorf("signed out, no state = %d %q, want the sign-in page with the install parked", w.Code, loc)
	}
	w = do(t, mux, "GET", "/slack/oauth/callback?error=access_denied", token)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/admin/onboarding/?install_error=") || !strings.Contains(loc, "cancelled") {
		t.Errorf("signed in, cancelled = %q, want the walk saying so", loc)
	}
	w = do(t, mux, "GET", "/slack/oauth/callback?error=access_denied", "")
	if loc := w.Header().Get("Location"); loc != "/admin/login/" || setsCookie(w, installIntentCookie) {
		t.Errorf("signed out, cancelled = %q, want the sign-in page with nothing parked", loc)
	}

	// Started properly, then the session ran out before Allow: the state is bound to this
	// browser, but there is nobody to bind the workspace to. Park it and sign them in again.
	start := do(t, mux, "GET", "/slack/install", token)
	loc, err := url.Parse(start.Header().Get("Location"))
	if err != nil || loc.Query().Get("state") == "" {
		t.Fatalf("install did not start: %d %q", start.Code, start.Header().Get("Location"))
	}
	var stateCookie string
	for _, c := range start.Result().Cookies() {
		if c.Name == installStateCookie {
			stateCookie = c.Value
		}
	}
	w = callback(t, mux, loc.Query().Get("state"), stateCookie, "")
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || loc != "/admin/login/" || !setsCookie(w, installIntentCookie) {
		t.Errorf("bound state, signed out = %d %q, want the sign-in page with the install parked", w.Code, loc)
	}
}
