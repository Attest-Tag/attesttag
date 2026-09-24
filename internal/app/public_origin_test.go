package app

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// The bot learns its public origin from authenticated console traffic, keeps it across a
// reopen, ignores loopback hosts, and never trades https for http. ADMIN_BASE_URL wins.
func TestLearnOrigin(t *testing.T) {
	t.Setenv("ADMIN_BASE_URL", "")
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := Config{HealthAddr: ":8090"}
	if got := publicBaseURL(ctx, st, cfg); got != "http://localhost:8090" {
		t.Fatalf("before learning: %q", got)
	}

	r := httptest.NewRequest("GET", "http://127.0.0.1:8090/api/settings", nil)
	st.LearnOrigin(ctx, requestOrigin(r))
	if got := st.PublicOrigin(ctx); got != "" {
		t.Errorf("loopback must not be learned, got %q", got)
	}

	r = httptest.NewRequest("GET", "http://app.attesttag.com/api/settings", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	st.LearnOrigin(ctx, requestOrigin(r))
	if got := publicBaseURL(ctx, st, cfg); got != "https://app.attesttag.com" {
		t.Fatalf("learned: %q", got)
	}

	st.LearnOrigin(ctx, "http://attesttag-xyz.a.run.app")
	if got := st.PublicOrigin(ctx); got != "https://app.attesttag.com" {
		t.Errorf("http must not replace https, got %q", got)
	}

	// A different host may not take the origin over, even on https. This used to be allowed —
	// "the newest https origin wins" — and that is an account takeover once anyone can sign up:
	// one request with a forged Host header repoints the password-reset links the deployment
	// emails to every tenant. The host is pinned to what was learned first instead.
	st.LearnOrigin(ctx, "https://evil.example/")
	if got := st.PublicOrigin(ctx); got != "https://app.attesttag.com" {
		t.Errorf("a forged Host must not repoint the origin, got %q", got)
	}

	// The supported way to move the deployment to a new hostname.
	t.Setenv("PUBLIC_ORIGIN_HOSTS", "attesttag-xyz.a.run.app")
	st.LearnOrigin(ctx, "https://attesttag-xyz.a.run.app/")
	if got := st.PublicOrigin(ctx); got != "https://attesttag-xyz.a.run.app" {
		t.Errorf("an allowlisted host should be learned, got %q", got)
	}
	st.LearnOrigin(ctx, "https://evil.example/")
	if got := st.PublicOrigin(ctx); got != "https://attesttag-xyz.a.run.app" {
		t.Errorf("the allowlist excludes everything else, got %q", got)
	}
	t.Setenv("PUBLIC_ORIGIN_HOSTS", "")

	st.db.Close()
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.PublicOrigin(ctx); got != "https://attesttag-xyz.a.run.app" {
		t.Errorf("not persisted: %q", got)
	}

	var nilStore *Store
	if got := publicBaseURL(ctx, nilStore, Config{}); got != "" {
		t.Errorf("nil store, no address: %q", got)
	}
	t.Setenv("ADMIN_BASE_URL", "https://override.example/")
	if got := publicBaseURL(ctx, st2, cfg); got != "https://override.example" {
		t.Errorf("env override: %q", got)
	}
}
