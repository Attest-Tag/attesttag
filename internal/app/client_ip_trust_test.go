package app

import (
	"net/http/httptest"
	"testing"
)

// X-Forwarded-For is a claim, and whether it is worth anything depends entirely on who is making
// it. Behind a proxy the header is the only way to tell callers apart, and the last entry is the
// one our own proxy wrote. Reached directly, every byte of it is the caller's to invent — and the
// throttles keyed on it (the sign-in lockout, signup, the operator's attempts) then counted every
// guess against a fresh address and never fired.
//
// The peer is what separates the two cases, and no configuration is needed for the deployments
// that exist: a proxy on the same network reaches us from a private address, the open internet
// does not.
func TestClientIPTrustsTheHeaderOnlyFromAProxy(t *testing.T) {
	const spoofed = "9.9.9.9"
	cases := []struct {
		name, remote, xff, trustProxy, kService, want string
	}{
		{"behind a proxy on a private network", "10.0.0.1:1234", spoofed + ", 203.0.113.9", "", "", "203.0.113.9"},
		{"behind a proxy on loopback", "127.0.0.1:8080", spoofed + ", 203.0.113.9", "", "", "203.0.113.9"},
		{"reached directly from the internet", "203.0.113.7:5555", spoofed, "", "", "203.0.113.7"},
		{"reached directly, no header at all", "203.0.113.7:5555", "", "", "", "203.0.113.7"},
		{"a private peer nobody wants trusted", "10.0.0.1:1234", spoofed, "0", "", "10.0.0.1"},
		{"a reverse proxy that has a public address", "203.0.113.7:5555", spoofed, "1", "", spoofed},
		{"Cloud Run, where something always terminates in front", "203.0.113.7:5555", spoofed, "", "svc", spoofed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TRUST_PROXY", c.trustProxy)
			t.Setenv("K_SERVICE", c.kService)
			r := httptest.NewRequest("POST", "/api/auth/login", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(r); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// The failure this is really about: a container published straight to the internet, where every
// request can name its own address. Without the peer check each of these is a separate throttle
// bucket, so a lockout counting to five never reaches two.
func TestASpoofedHeaderCannotSplitTheThrottle(t *testing.T) {
	t.Setenv("TRUST_PROXY", "")
	t.Setenv("K_SERVICE", "")
	seen := map[string]bool{}
	for _, claim := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("X-Forwarded-For", claim)
		seen[clientIP(r)] = true
	}
	if len(seen) != 1 {
		t.Errorf("three forged headers produced %d throttle keys, want 1: %v", len(seen), seen)
	}
}
