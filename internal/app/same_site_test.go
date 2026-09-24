package app

import (
	"net/http"
	"net/http/httptest"
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
