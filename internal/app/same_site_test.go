package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The sign-in, sign-up and sign-out posts establish or clear a session, so they sit outside the
// CSRF double-submit requireAdmin applies. sameSiteOnly is what stops a page on another origin
// auto-submitting a form to them: a browser that says the request came from another site is
// refused, and a caller that is not a browser — no Fetch metadata, no Origin — is left alone.
func TestSameSiteOnlyRefusesCrossSite(t *testing.T) {
	h := sameSiteOnly(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	cases := []struct {
		name   string
		fetch  string
		origin string
		host   string
		wantOK bool
	}{
		{"cross-site form post", "cross-site", "", "app.example.com", false},
		{"same-site subdomain", "same-site", "", "app.example.com", false},
		{"same-origin fetch", "same-origin", "", "app.example.com", true},
		{"user typed url", "none", "", "app.example.com", true},
		{"no metadata, no origin (curl/test)", "", "", "app.example.com", true},
		{"no metadata, matching origin", "", "https://app.example.com", "app.example.com", true},
		{"no metadata, foreign origin", "", "https://evil.example", "app.example.com", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.Host = c.host
		if c.fetch != "" {
			r.Header.Set("Sec-Fetch-Site", c.fetch)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h(w, r)
		if ok := w.Code == 200; ok != c.wantOK {
			t.Errorf("%s: status %d, want ok=%v", c.name, w.Code, c.wantOK)
		}
	}
}

// The wrapper only helps on the routes that carry it, and the one that was missing it was the
// last step of a sign-in: the post that trades a login challenge for a session. Every signed-out
// post that can set or clear a session is listed here and driven through the real mux, so a new
// one registered bare fails this rather than shipping.
func TestSessionSettingPostsRefuseCrossSite(t *testing.T) {
	_, mux, _ := installTestBot(t)
	for _, path := range []string{
		"/api/auth/signup", "/api/auth/login", "/api/auth/two-factor", "/api/auth/verify",
		"/api/auth/forgot", "/api/auth/reset", "/api/auth/sso/start", "/api/auth/logout",
	} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("POST %s from another site: status %d, want 403", path, w.Code)
		}
	}
}
