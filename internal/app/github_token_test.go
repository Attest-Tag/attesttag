package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"
)

// What each purpose's token may do is the whole of what code review is trusted with on GitHub, so
// the list is pinned here: widening a purpose is a change to this test as well, made on purpose.
// The default is the fix jobs' and the Slack tools' set, and it never grows — a permission the App
// gains later is asked for by a new purpose. Issues is in none of them: whether the summary comment
// needs it is a thing to learn live, and if it does it goes into review_post alone.
func TestGitHubPurposePermissions(t *testing.T) {
	want := map[string]map[string]string{
		"":            {"contents": "write", "pull_requests": "write"},
		"review_read": {"contents": "read", "pull_requests": "read"},
		"review_post": {"pull_requests": "write"},
	}
	if len(githubPurposePermissions) != len(want) {
		t.Fatalf("purposes = %v, want exactly %v", githubPurposePermissions, want)
	}
	for purpose, perms := range want {
		got, err := githubTokenPermissions(purpose)
		if err != nil || !maps.Equal(got, perms) {
			t.Errorf("purpose %q mints %v (%v), want %v", purpose, got, err, perms)
		}
		// A caller that changes the set it was handed must not change the next caller's.
		got["administration"] = "write"
		if again, _ := githubTokenPermissions(purpose); again["administration"] != "" {
			t.Fatalf("purpose %q's permissions are shared with its callers", purpose)
		}
	}
	for _, unknown := range []string{"review", "REVIEW_READ", "admin", " "} {
		if _, err := githubTokenPermissions(unknown); err == nil {
			t.Errorf("unknown purpose %q was given a permission set", unknown)
		}
	}
}

// Two purposes never share a cached token. The cache is keyed on the installation, and before
// purposes a key named only the repository, so a review asking for read-only would have been
// handed the fix job's write token, minted a minute earlier for the same repository.
func TestInstallationTokensAreCachedPerPurpose(t *testing.T) {
	p, f := reviewProxyFixture(t)
	conn, err := p.reviewConnection(fakeInstallation, "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := p.secret(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mint := func(purpose string) string {
		t.Helper()
		tok, err := p.installationToken(ctx, orgID, conn, sec, purpose)
		if err != nil {
			t.Fatalf("minting for %q: %v", purpose, err)
		}
		return tok
	}
	read := mint(githubPurposeReviewRead)
	post := mint(githubPurposeReviewPost)
	def := mint(githubPurposeDefault)
	if read == post || read == def || post == def {
		t.Fatalf("purposes shared a token: read %q, post %q, default %q", read, post, def)
	}
	if again := mint(githubPurposeReviewRead); again != read || f.mintCount() != 3 {
		t.Errorf("the read token was not reused from the cache: %q after %d mints", again, f.mintCount())
	}
	for i, want := range []string{readPerms, postPerms, "contents:write,pull_requests:write"} {
		if got := permissionList(f.mints[i].Permissions); got != want {
			t.Errorf("mint %d asked GitHub for %q, want %q", i+1, got, want)
		}
	}

	keys := map[tokenCacheKey]string{}
	for _, purpose := range []string{githubPurposeDefault, githubPurposeReviewRead, githubPurposeReviewPost} {
		perms, _ := githubTokenPermissions(purpose)
		k := installTokenKey(fakeInstallation, "acme/web", perms)
		if other, dup := keys[k]; dup {
			t.Errorf("purposes %q and %q share a cache key", other, purpose)
		}
		keys[k] = purpose
	}
	perms, _ := githubTokenPermissions(githubPurposeReviewRead)
	if installTokenKey(fakeInstallation, "acme/web", perms) != installTokenKey(fakeInstallation, "ACME/Web", perms) {
		t.Error("the same repository in another case is another cache entry")
	}

	if _, err := p.installationToken(ctx, orgID, conn, sec, "review_write"); err == nil {
		t.Error("an unknown purpose minted a token")
	}
	if f.mintCount() != 3 {
		t.Error("an unknown purpose reached GitHub's token endpoint")
	}
	if _, _, err := p.mintInstallationToken(ctx, fakeInstallation, "acme/web", nil); err == nil {
		t.Error("a token was minted with no permissions named, which GitHub reads as all of them")
	}
}

// A purpose is refused where it cannot be kept, before anything is sent: an unknown one, and any
// one on a pasted token, which is whatever its owner made it and cannot be narrowed.
func TestProxyRefusesAPurposeItCannotKeep(t *testing.T) {
	p, f := reviewProxyFixture(t)
	ctx := context.Background()
	app, err := p.reviewConnection(fakeInstallation, "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal(&Secret{Token: "ghp_pasted"})
	sealed, _ := p.sealer.Seal(enc)
	pasted := &Connection{Name: "pasted", Preset: "github", CredType: "bearer", Repo: "acme/web",
		AllowedHosts: []string{"api.github.com"}, Status: "active", secretEnc: sealed}
	u := "https://api.github.com/repos/acme/web/pulls/7"
	for _, tc := range []struct {
		conn    *Connection
		purpose string
	}{
		{app, "review_admin"},
		{pasted, githubPurposeReviewRead},
		{pasted, githubPurposeReviewPost},
	} {
		acc := &Access{Rules: []Rule{{Conn: tc.conn}}}
		if _, err := p.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: u, Purpose: tc.purpose}, ProxyAudit{}, true); err == nil {
			t.Errorf("%s with purpose %q was sent", tc.conn.Name, tc.purpose)
		}
	}
	if len(f.sent()) != 0 || f.mintCount() != 0 {
		t.Errorf("refused purposes reached GitHub: %v, %d mints", f.sent(), f.mintCount())
	}
	// The default purpose through a pasted token is the old behaviour exactly.
	f.mux.HandleFunc("GET /repos/acme/web/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_pasted" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{}`))
	})
	if _, err := p.Do(ctx, orgID, &Access{Rules: []Rule{{Conn: pasted}}}, ProxyRequest{Method: "GET", URL: u}, ProxyAudit{}, true); err != nil {
		t.Errorf("the default purpose through a pasted token: %v", err)
	}
}

// The Go-only fields have no JSON name, so nothing a model writes, and no held write replayed from
// its stored JSON, can carry a purpose, a raw read or the review poster's pass for markers.
func TestProxyRequestGoOnlyFieldsNeverTravelAsJSON(t *testing.T) {
	b, err := json.Marshal(ProxyRequest{Method: "POST", URL: "https://api.github.com/x", Purpose: githubPurposeReviewPost,
		Raw: true, ReviewPoster: true, MaxBytes: 1, HoldWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"review_post", "urpose", "aw\"", "oster"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the stored request carries %q: %s", leak, b)
		}
	}
	var in ProxyRequest
	json.Unmarshal([]byte(`{"method":"POST","purpose":"review_post","Purpose":"review_post","raw":true,"Raw":true,
		"review_poster":true,"ReviewPoster":true,"reviewPoster":true}`), &in)
	if in.Purpose != "" || in.Raw || in.ReviewPoster {
		t.Errorf("JSON set a Go-only field: %+v", in)
	}
}

// GitHub's 422 on a token request means one of two things, and only its message tells which. The
// one that needs an owner to accept the App's new permissions must say so, not send people to the
// repository selection.
func TestInstallTokenErrorSaysWhenPermissionsNeedAccepting(t *testing.T) {
	perms := &installTokenError{status: 422, msg: "The permissions requested are not granted to this installation.",
		repo: "acme/web", id: fakeInstallation, perms: postPerms}
	if got := perms.Error(); !strings.Contains(got, "accept") || !strings.Contains(got, postPerms) || strings.Contains(got, "selection") {
		t.Errorf("a permissions 422 reads %q", got)
	}
	other := &installTokenError{status: 422, msg: "There is at least one repository that does not exist or is not accessible to the parent installation.",
		repo: "acme/web", id: fakeInstallation}
	if got := other.Error(); !strings.Contains(got, "selection") || !strings.Contains(got, "accept") {
		t.Errorf("an ambiguous 422 should name both causes: %q", got)
	}
	var e *installTokenError
	if !errors.As(error(perms), &e) {
		t.Fatal("installTokenError is not itself")
	}
}
