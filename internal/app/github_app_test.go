package app

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// testAppKey is one RSA key in the two shapes a deployment can carry it: the base64 blob Cloud
// Run gets, and the escaped PEM a local .env gets from pasting one into a single-line variable.
func testAppKey(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, base64.StdEncoding.EncodeToString(pemBytes), strings.ReplaceAll(string(pemBytes), "\n", `\n`)
}

// deploy/gcp/cloudrun.sh reads a dotenv file one line at a time, so the key ships base64; a local
// .env may hold the PEM with its newlines escaped. Both have to arrive as the same key, or the
// app works in one place and not the other for reasons nobody can see.
func TestParseGitHubAppKeyAcceptsBothShapes(t *testing.T) {
	key, b64, escaped := testAppKey(t)

	fromB64, err := parseGitHubAppKey(b64, "")
	if err != nil {
		t.Fatalf("base64 PEM: %v", err)
	}
	fromEscaped, err := parseGitHubAppKey("", escaped)
	if err != nil {
		t.Fatalf("escaped PEM: %v", err)
	}
	if !fromB64.Equal(key) || !fromEscaped.Equal(key) {
		t.Error("the two shapes did not produce the same key")
	}

	// PKCS#8 is accepted too: a key round-tripped through openssl comes back as one.
	p8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p8pem := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})
	if got, err := parseGitHubAppKey(base64.StdEncoding.EncodeToString(p8pem), ""); err != nil || !got.Equal(key) {
		t.Errorf("PKCS#8: %v", err)
	}
}

// The failures worth telling apart, because each has a different fix.
func TestParseGitHubAppKeyRefusalsSayWhatToDo(t *testing.T) {
	for _, tc := range []struct{ name, b64, raw, want string }{
		{"not base64 at all", "this is not base64!!", "", "not base64"},
		{"base64 of something that is not a PEM", base64.StdEncoding.EncodeToString([]byte("hello")), "", "base64 -i app.pem"},
		{"nothing at all", "", "", "not PEM"},
	} {
		_, err := parseGitHubAppKey(tc.b64, tc.raw)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

// Nothing configured is a deployment that connects repositories with pasted tokens, and must
// start. Half-configured is a console offering an Install button that leads nowhere, and must
// not: the whole reason this is read at boot is to fail then rather than on the first click.
func TestNewGitHubAppRefusesHalfAConfiguration(t *testing.T) {
	_, b64, _ := testAppKey(t)
	clear := func(t *testing.T) {
		for _, k := range []string{"GITHUB_APP_ID", "GITHUB_APP_SLUG", "GITHUB_APP_PRIVATE_KEY_B64", "GITHUB_APP_PRIVATE_KEY"} {
			t.Setenv(k, "")
		}
	}

	t.Run("nothing configured", func(t *testing.T) {
		clear(t)
		app, err := newGitHubApp()
		if err != nil {
			t.Fatalf("an unconfigured deployment must still start: %v", err)
		}
		if app.configured() {
			t.Error("an unconfigured deployment reported an app")
		}
		// A nil app answers configured() without a nil check at every call site.
		var nilApp *githubApp
		if nilApp.configured() {
			t.Error("a nil app reported itself configured")
		}
	})

	t.Run("a key but no id", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_APP_PRIVATE_KEY_B64", b64)
		if _, err := newGitHubApp(); err == nil || !strings.Contains(err.Error(), "GITHUB_APP_ID") {
			t.Errorf("err = %v, want one naming the missing variable", err)
		}
	})

	t.Run("an id but a broken key", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_APP_ID", "4941055")
		t.Setenv("GITHUB_APP_SLUG", "attesttag")
		t.Setenv("GITHUB_APP_PRIVATE_KEY_B64", "nonsense")
		if _, err := newGitHubApp(); err == nil {
			t.Error("a broken key started the process")
		}
	})

	// Without the OAuth client the install flow cannot prove the installer can see what they
	// came back with, and binding on an unverified id is the hole GitHub warns about. Refusing
	// at boot beats discovering it on somebody's first install.
	t.Run("a key and an id but no OAuth client", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_APP_ID", "4941055")
		t.Setenv("GITHUB_APP_SLUG", "attesttag")
		t.Setenv("GITHUB_APP_PRIVATE_KEY_B64", b64)
		app, err := newGitHubApp()
		if err != nil {
			t.Fatalf("a missing OAuth client must not stop the process starting: %v", err)
		}
		// Installations already connected go on working — minting their tokens needs only the
		// key — but nobody may install a new one, because the proof that the installer can see
		// what they came back with is exactly what the OAuth client buys.
		if !app.configured() {
			t.Error("the app should still mint tokens for installations already held")
		}
		if app.canInstall() {
			t.Error("the install flow was offered with no way to verify the installer")
		}
	})

	t.Run("complete", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_APP_ID", "4941055")
		t.Setenv("GITHUB_APP_SLUG", "attesttag")
		t.Setenv("GITHUB_APP_PRIVATE_KEY_B64", b64)
		t.Setenv("GITHUB_APP_CLIENT_ID", "Iv23liEXAMPLE0000000")
		t.Setenv("GITHUB_APP_CLIENT_SECRET", "shh")
		app, err := newGitHubApp()
		if err != nil || !app.configured() {
			t.Fatalf("app = %v, err = %v", app, err)
		}
		if !app.canInstall() {
			t.Error("a complete configuration should offer the install flow")
		}
		if got := app.installURL("s3cr3t"); got != "https://github.com/apps/attesttag/installations/new?state=s3cr3t" {
			t.Errorf("installURL = %q", got)
		}
	})
}

// GitHub rejects a JWT whose exp is more than ten minutes out, and rejects one issued in its own
// future — and reports both by naming exp, so the two are indistinguishable from the error. The
// back-dated iat is what buys room for a clock that drifts.
func TestAppJWTIsSignedAndWithinGitHubsWindow(t *testing.T) {
	key, b64, _ := testAppKey(t)
	app := &githubApp{id: "4941055", slug: "attesttag"}
	var err error
	if app.key, err = parseGitHubAppKey(b64, ""); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	tok, err := app.jwt(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT has three parts, got %d", len(parts))
	}

	var hdr struct{ Alg, Typ string }
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	decode := func(s string, v any) {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}
	decode(parts[0], &hdr)
	decode(parts[1], &claims)

	if hdr.Alg != "RS256" {
		t.Errorf("alg = %q, want RS256", hdr.Alg)
	}
	if claims.Iss != "4941055" {
		t.Errorf("iss = %q, want the app id", claims.Iss)
	}
	if claims.Iat != now.Add(-time.Minute).Unix() {
		t.Errorf("iat is not back-dated a minute: %d", claims.Iat)
	}
	if d := time.Unix(claims.Exp, 0).Sub(now); d > 10*time.Minute {
		t.Errorf("exp is %s out; GitHub refuses anything over 10 minutes", d)
	}

	// And it really is signed by the key GitHub has the public half of.
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}

	// An unconfigured deployment refuses rather than signing with nothing.
	if _, err := (&githubApp{}).jwt(now); err == nil {
		t.Error("an app with no key signed a JWT")
	}
}

// GitHub keeps one Setup URL per app and takes no redirect_uri, so that single field is the only
// thing that has to change between environments. Everything on our side follows the host the
// browser actually used — which is what makes `tailscale serve` and localhost work off one
// binary, with no ADMIN_BASE_URL to set and nothing to redeploy.
func TestGitHubSetupURLFollowsTheHostTheBrowserUsed(t *testing.T) {
	for _, tc := range []struct{ name, host, proto, want string }{
		{"localhost", "127.0.0.1:8090", "", "http://127.0.0.1:8090/github/setup"},
		{"tailscale behind its https front", "mini.example.ts.net", "https", "https://mini.example.ts.net/github/setup"},
		{"production", "app.attesttag.com", "https", "https://app.attesttag.com/github/setup"},
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/api/github/installations", nil)
		r.Host = tc.host
		if tc.proto != "" {
			r.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		if got := githubSetupURL(r); got != tc.want {
			t.Errorf("%s: setup URL = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The parked intent names one of two fixed destinations and never a URL, so no value anybody can
// put in that cookie turns the post-sign-in redirect into an open redirect. The bare "1" older
// browsers are still carrying has to keep meaning Slack.
func TestInstallIntentIsAFlagNotADestination(t *testing.T) {
	for _, tc := range []struct{ cookie, want string }{
		{"github", "/github/install"},
		{"slack", "/slack/install"},
		{"1", "/slack/install"},                        // what browsers from before this change hold
		{"https://evil.example.com", "/slack/install"}, // not a destination, whatever it says
		{"", ""}, // no intent parked: no redirect at all
	} {
		r := httptest.NewRequest("GET", "/api/auth/login", nil)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: installIntentCookie, Value: tc.cookie})
		}
		b := &Bot{}
		if got := b.installNext(httptest.NewRecorder(), r); got != tc.want {
			t.Errorf("intent %q -> %q, want %q", tc.cookie, got, tc.want)
		}
	}
}

// The console shows the Install entry disabled, naming what to set, rather than hiding it — so
// the names it prints have to be the ones somebody can actually act on. A missing entry here is
// a console telling an admin to set a variable that does not exist.
func TestInstallMissingNamesTheVariablesToSet(t *testing.T) {
	raw, err := os.ReadFile("github_app.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	key, _, _ := testAppKey(t)
	full := &githubApp{id: "1", slug: "s", key: key, clientID: "cid", clientSecret: "sec"}
	if got := full.installMissing(); got != nil {
		t.Errorf("a complete app reports %v missing; nothing should be", got)
	}

	for _, tc := range []struct {
		name string
		app  *githubApp
		want []string
	}{
		{"nothing configured", nil, []string{
			"GITHUB_APP_ID", "GITHUB_APP_SLUG", "GITHUB_APP_PRIVATE_KEY_B64",
			"GITHUB_APP_CLIENT_ID", "GITHUB_APP_CLIENT_SECRET"}},
		{"key but no oauth client", &githubApp{id: "1", slug: "s", key: key},
			[]string{"GITHUB_APP_CLIENT_ID", "GITHUB_APP_CLIENT_SECRET"}},
		{"client id only", &githubApp{id: "1", slug: "s", key: key, clientID: "cid"},
			[]string{"GITHUB_APP_CLIENT_SECRET"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.app.installMissing()
			if len(got) != len(tc.want) {
				t.Fatalf("missing = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("missing = %v, want %v", got, tc.want)
				}
			}
			// Every name printed must be one this file actually reads from the environment,
			// or the console is giving an instruction that cannot work.
			for _, v := range got {
				if !strings.Contains(src, `os.Getenv("`+v+`")`) {
					t.Errorf("%s is offered as the fix but nothing reads it", v)
				}
			}
		})
	}
}
