package app

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// The response headers the whole service sends, in one wrapper around the mux so that no route
// can forget them. The console is a same-origin JavaScript application holding a session cookie,
// and the API answers it with JSON that must never be cached by a shared proxy or framed by
// another site.
//
// The console is a static export with inline bootstrap scripts — the theme switch and Next's
// flight data — so a script policy of 'self' alone would blank it. Their hashes are computed once
// from the embedded files at startup, and a rebuilt console ships new inline bodies with new
// hashes. Styles stay 'unsafe-inline': the chunks create <style> elements at runtime, and a style
// policy buys little against script execution anyway.

var (
	inlineScriptRe = regexp.MustCompile(`(?is)<script((?:\s[^>]*)?)>(.*?)</script>`)
	scriptSrcRe    = regexp.MustCompile(`(?i)\ssrc\s*=`)
)

// inlineScriptHashes walks the exported console and returns a CSP source expression for every
// inline script body it holds, sorted and deduplicated. An unreadable export yields none, and
// secureHeaders then falls back to allowing inline scripts rather than serving a blank console.
func inlineScriptHashes(fsys fs.FS) []string {
	seen := map[string]bool{}
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		for _, m := range inlineScriptRe.FindAllSubmatch(raw, -1) {
			if scriptSrcRe.Match(m[1]) || len(m[2]) == 0 {
				continue
			}
			sum := sha256.Sum256(m[2])
			seen["'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'"] = true
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// siteOrigin is where this deployment's public pages live — the only cross-origin place the
// console is allowed to fetch from, and only when there is one. The Get started page fetches
// its walkthroughs from exactly this, because handleMe sends the page the same string through
// this same function: one origin, normalised once, so the request and the policy cannot
// disagree.
//
// Empty is the default and means the console talks to nothing but its own origin, which is
// what a self-host wants: a stricter policy, and no named domain belonging to somebody else.
func siteOrigin(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	// Scheme and host only: a CSP source with a path in it does not match what it looks like
	// it matches, and a stray one would quietly widen or narrow the policy.
	return u.Scheme + "://" + u.Host
}

// secureHeaders wraps the mux. Three policies: the console (scripts by hash), the server-rendered
// pages (Configure, setup links: no scripts at all), and everything under /api, /v1 and /slack,
// which is data for a program and gets no rendering permissions whatsoever. HSTS goes out only on
// a request that actually arrived over TLS, so a laptop run on plain http is not pinned.
func secureHeaders(next http.Handler, scriptHashes []string, site string) http.Handler {
	scripts := "'self' " + strings.Join(scriptHashes, " ")
	if len(scriptHashes) == 0 {
		scripts = "'self' 'unsafe-inline'"
	}
	// The console's Get started page reads the walkthrough library from this deployment's
	// own site, if it has one, and plays the videos from wherever that site says they are
	// hosted. connect-src names the one extra origin it may ask — nothing when no site is
	// configured; media-src is as open as img-src already is, since a video runs no more
	// code than a picture does.
	connect := "'self'"
	if o := siteOrigin(site); o != "" {
		connect += " " + o
	}
	consoleCSP := "default-src 'self'; script-src " + scripts + "; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: https:; media-src 'self' https:; font-src 'self' data:; " +
		"connect-src " + connect + "; frame-ancestors 'none'; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'"
	pageCSP := "default-src 'self'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'"
	apiCSP := "default-src 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/slack/"):
			h.Set("Content-Security-Policy", apiCSP)
			h.Set("Cache-Control", "no-store")
		case p == "/admin" || strings.HasPrefix(p, "/admin/"):
			h.Set("Content-Security-Policy", consoleCSP)
		default:
			h.Set("Content-Security-Policy", pageCSP)
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
