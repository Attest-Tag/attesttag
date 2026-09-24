package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console's Content-Security-Policy names one cross-origin and only one: the site this
// deployment serves its recorded walkthroughs from. A deployment that has no site must name
// nobody — a self-hosted console reaching out to the origin of whoever published the binary is
// both a broken page on an air-gapped network and a report, to that origin, of who is using it.
func TestConsoleCSPNamesOnlyItsOwnSite(t *testing.T) {
	csp := func(site string) string {
		h := secureHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}), nil, site)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/", nil))
		return w.Header().Get("Content-Security-Policy")
	}

	// No site configured: the console talks to its own origin and nothing else.
	bare := csp("")
	if !strings.Contains(bare, "connect-src 'self';") {
		t.Errorf("with no site, connect-src should be 'self' alone: %q", bare)
	}
	if strings.Contains(bare, "attesttag.com") {
		t.Errorf("a deployment with no site named one anyway: %q", bare)
	}

	// Configured: exactly that origin, and the path is dropped — a CSP source carrying a path
	// does not match what it looks like it matches.
	for _, in := range []string{"https://site.example.com", "https://site.example.com/", "https://site.example.com/walkthroughs"} {
		if got := csp(in); !strings.Contains(got, "connect-src 'self' https://site.example.com;") {
			t.Errorf("csp(%q) connect-src = %q", in, got)
		}
	}

	// Garbage names nobody rather than corrupting the directive.
	for _, in := range []string{"not a url", "site.example.com", "://x", " "} {
		if got := siteOrigin(in); got != "" {
			t.Errorf("siteOrigin(%q) = %q, want empty", in, got)
		}
	}

	// And the policy for /api is unchanged by any of this: data for a program, no permissions.
	h := secureHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), nil, "https://site.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/me", nil))
	if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'none'; frame-ancestors 'none'" {
		t.Errorf("the API policy changed: %q", got)
	}
}

// The Get started page asks one origin for its walkthroughs and the browser permits one origin:
// this pins that they are the same string. They used to be two settings — a runtime SITE_URL for
// the policy and a build-time NEXT_PUBLIC_SITE_URL baked into the console — and nothing built the
// second one, so every console shipped asking for nothing while its CSP allowed a fetch that
// never happened. handleMe hands the page the origin instead; the test is that it still does.
func TestConsoleIsToldTheOriginItsPolicyAllows(t *testing.T) {
	t.Setenv("ADMIN_BASE_URL", "")
	st := testStore(t)
	for _, site := range []string{"", "https://site.example.com", "https://site.example.com/walkthroughs"} {
		b := &Bot{store: st, cfg: Config{SiteURL: site}, settings: newSettingsCache(st, Config{})}
		rec := httptest.NewRecorder()
		b.handleMe(rec, httptest.NewRequest("GET", "/api/me", nil))
		var me struct {
			SiteURL string `json:"site_url"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatalf("SITE_URL=%q: /api/me is not JSON: %v", site, err)
		}
		// The page builds `${site_url}/api/walkthroughs`; connect-src is what the browser checks
		// it against. A CSP source carries no path, so the two agree only if the page was given
		// the origin and nothing more.
		if want := siteOrigin(site); me.SiteURL != want {
			t.Errorf("SITE_URL=%q: /api/me said %q, CSP allows %q", site, me.SiteURL, want)
		}
	}
}
