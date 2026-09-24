package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRepo(t *testing.T) {
	ok := map[string]string{
		"":                                     "",
		"acme/app":                             "acme/app",
		"  acme/app  ":                         "acme/app",
		"https://github.com/acme/app":          "acme/app",
		"https://github.com/acme/app.git":      "acme/app",
		"https://www.github.com/acme/app/":     "acme/app",
		"github.com/acme/app/tree/main/cmd":    "acme/app",
		"git@github.com:acme/app.git":          "acme/app",
		"https://github.com/acme/app/pull/12":  "acme/app",
		"https://github.com/Acme/my.repo-name": "Acme/my.repo-name",
	}
	for in, want := range ok {
		got, err := normalizeRepo(in)
		if err != nil || got != want {
			t.Errorf("normalizeRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"acme", "https://gitlab.com/x/y", "owner/", "/name", "owner/na me", "own.er/name"} {
		if got, err := normalizeRepo(in); err == nil {
			t.Errorf("normalizeRepo(%q) = %q, expected an error", in, got)
		}
	}
}

func TestAccessReposAndDefault(t *testing.T) {
	app := &Connection{ID: 1, Repo: "acme/app"}
	docs := &Connection{ID: 2, Repo: "acme/docs"}
	plain := &Connection{ID: 3}
	acc := &Access{Rules: []Rule{{Conn: app, Rank: 2}, {Conn: plain, Rank: 2}, {Conn: docs, Rank: 1}, {Conn: app, Rank: 1}}}
	repos := acc.Repos()
	if len(repos) != 2 || repos[0].Repo != "acme/app" || repos[1].Repo != "acme/docs" {
		t.Errorf("Repos() = %+v", repos)
	}
	if !acc.hasRepo("Acme/App") || acc.hasRepo("other/x") {
		t.Error("hasRepo should match case-insensitively and reject unknown repos")
	}
}

// fakeAPI is a round tripper standing in for a remote API, so tools can be tested without a
// network or a real token.
type fakeAPI func(*http.Request) (int, string)

func (f fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := f(r)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

// repoTestBot returns a bot whose proxy answers GitHub from fn.
func repoTestBot(t *testing.T, fn fakeAPI) *Bot {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{store: st, sealer: sealer, settings: newSettingsCache(st, Config{}), proxy: NewProxy(sealer, st)}
	b.proxy.client.Transport = fn
	return b
}

func TestReposForToken(t *testing.T) {
	var seen []string
	b := repoTestBot(t, func(r *http.Request) (int, string) {
		seen = append(seen, r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want the token", got)
		}
		return 200, `[{"full_name":"acme/app","private":true,"pushed_at":"2026-09-01T00:00:00Z"},
			{"full_name":"acme/docs","private":false}]`
	})
	repos, truncated, err := b.reposForToken(context.Background(), orgID, "tok")
	if err != nil || truncated {
		t.Fatalf("reposForToken: %v, truncated=%v", err, truncated)
	}
	if len(repos) != 2 || repos[0].Repo != "acme/app" || !repos[0].Private || repos[1].Private {
		t.Fatalf("repos = %+v", repos)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "page=1") {
		t.Fatalf("expected one page fetched, got %v", seen)
	}
}

func TestReposForTokenPagesAndTruncates(t *testing.T) {
	full := make([]string, 100)
	for i := range full {
		full[i] = fmt.Sprintf(`{"full_name":"acme/r%d"}`, i)
	}
	page := "[" + strings.Join(full, ",") + "]"
	calls := 0
	b := repoTestBot(t, func(*http.Request) (int, string) {
		calls++
		return 200, page
	})
	repos, truncated, err := b.reposForToken(context.Background(), orgID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if calls != repoPages || !truncated || len(repos) != repoPages*100 {
		t.Fatalf("calls=%d truncated=%v repos=%d", calls, truncated, len(repos))
	}
}

func TestReposForTokenErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{401, "rejected the token"},
		{403, "will not list repositories"},
		{500, "returned 500"},
	} {
		b := repoTestBot(t, func(*http.Request) (int, string) { return tc.status, `{}` })
		if _, _, err := b.reposForToken(context.Background(), orgID, "tok"); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: err = %v, want it to mention %q", tc.status, err, tc.want)
		}
	}
	b := repoTestBot(t, func(*http.Request) (int, string) { return 200, `[]` })
	if _, _, err := b.reposForToken(context.Background(), orgID, ""); err == nil {
		t.Error("an empty token should be refused before any call")
	}
}

func TestReposForTokenRefusesATruncatedList(t *testing.T) {
	// A body past the proxy's ceiling arrives cut mid-object; the listing must say so rather
	// than hand half a JSON document to the parser.
	big := `[{"full_name":"acme/app","description":"` + strings.Repeat("x", proxyMaxRead) + `"}]`
	b := repoTestBot(t, func(*http.Request) (int, string) { return 200, big })
	if _, _, err := b.reposForToken(context.Background(), orgID, "tok"); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v, want it to report the list was too large", err)
	}
}

func TestProxyTruncationIsVisible(t *testing.T) {
	b := repoTestBot(t, func(*http.Request) (int, string) { return 200, strings.Repeat("y", 300<<10) })
	acc, err := b.tokenAccess("tok")
	if err != nil {
		t.Fatal(err)
	}
	// The default limit is what a tool result carries into the prompt.
	resp, err := b.proxy.Do(context.Background(), orgID, acc,
		ProxyRequest{Method: "GET", URL: "https://api.github.com/user/repos"}, ProxyAudit{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || !strings.Contains(resp.Body, "[truncated:") {
		t.Errorf("a body past the limit should come back marked, got %d bytes truncated=%v", len(resp.Body), resp.Truncated)
	}
	// A caller that parses the body itself can ask for all of it.
	resp, err = b.proxy.Do(context.Background(), orgID, acc,
		ProxyRequest{Method: "GET", URL: "https://api.github.com/user/repos", MaxBytes: proxyMaxRead}, ProxyAudit{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Truncated || len(resp.Body) != 300<<10 {
		t.Errorf("full read = %d bytes truncated=%v, want the whole body", len(resp.Body), resp.Truncated)
	}
}

// storeGitHubConn seals a token onto a github connection the way connectRepo does, so the
// reuse path can be exercised without going through the API.
func storeGitHubConn(t *testing.T, b *Bot, name, repo, token string) int64 {
	t.Helper()
	ctx := context.Background()
	bd, err := b.store.CreateBundle(ctx, orgID, repoBundleName, "test")
	if err != nil {
		t.Fatal(err)
	}
	c, sec, err := b.buildConnection(&connectionInput{BundleID: bd.ID, Name: name, Preset: "github",
		CredType: "bearer", Secret: &Secret{Token: token}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Repo, c.Status = repo, "active"
	enc, err := b.sealSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.store.InsertConnection(ctx, orgID, c, enc)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSavedTokenReuse(t *testing.T) {
	var sent string
	b := repoTestBot(t, func(r *http.Request) (int, string) {
		sent = r.Header.Get("Authorization")
		return 200, `[{"full_name":"acme/app"}]`
	})
	id := storeGitHubConn(t, b, "bkrm_blog", "acme/blog", "sealed-tok")
	ctx := context.Background()

	// A connection id stands in for the token, and the token itself never has to be repeated.
	auth, err := b.repoAuthFor(ctx, orgID, "", id, 0)
	token := auth.token
	if err != nil || token != "sealed-tok" {
		t.Fatalf("repoAuthFor(id) = %q, %v; want the sealed token", token, err)
	}
	if _, _, err := b.reposForToken(ctx, orgID, token); err != nil {
		t.Fatal(err)
	}
	if sent != "Bearer sealed-tok" {
		t.Errorf("Authorization = %q, want the stored token", sent)
	}

	// A pasted token still wins, and neither empty nor unknown resolves to anything.
	if got, _ := b.repoAuthFor(ctx, orgID, " pasted ", id, 0); got.token != "pasted" {
		t.Errorf("repoAuthFor(pasted) = %q", got.token)
	}
	if _, err := b.repoAuthFor(ctx, orgID, "", 0, 0); err == nil {
		t.Error("no token and no connection should be refused")
	}
	if _, err := b.repoAuthFor(ctx, orgID, "", id+999, 0); err == nil {
		t.Error("an unknown connection should be refused")
	}
}

// A repository connected without a scope is saved under Repositories and attached nowhere; a
// scope then adds it by connection id, which needs no token. Ids that are not saved GitHub
// repositories are reported, and nothing landing is an error.
func TestConnectRepoWithoutScopeThenAttach(t *testing.T) {
	b := repoTestBot(t, func(*http.Request) (int, string) { return 200, `{"default_branch":"main"}` })
	ctx := context.Background()
	c, err := b.connectRepo(ctx, orgID, nil, "acme/app", repoAuth{token: "tok"}, "", "me")
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := b.store.connectionScopeIDs(ctx, orgID, c.ID); len(ids) != 0 {
		t.Fatalf("saved without a scope, yet attached to %v", ids)
	}
	ch, err := b.store.UpsertScope(ctx, orgID, "channel", "T1", "C1", "#eng")
	if err != nil {
		t.Fatal(err)
	}
	done, failed, err := b.attachRepoConnections(ctx, orgID, ch, []int64{c.ID, c.ID, c.ID + 999})
	if err != nil || len(done) != 1 || done[0] != "acme/app" || len(failed) != 1 {
		t.Fatalf("attach: done=%v failed=%v err=%v", done, failed, err)
	}
	if ids, _ := b.store.connectionScopeIDs(ctx, orgID, c.ID); len(ids) != 1 || ids[0] != ch.ID {
		t.Errorf("attached to %v, want the channel %d", ids, ch.ID)
	}
	if _, _, err := b.attachRepoConnections(ctx, orgID, ch, []int64{c.ID + 999}); err == nil {
		t.Error("nothing landing should be an error")
	}
	if _, _, err := b.attachRepoConnections(ctx, orgID, ch, nil); err == nil {
		t.Error("an empty list should be an error")
	}
	// Connecting the same repository again, now with a scope, re-keys that one connection.
	c2, err := b.connectRepo(ctx, orgID, ch, "acme/app", repoAuth{token: "tok-2"}, "", "me")
	if err != nil || c2.ID != c.ID {
		t.Fatalf("reconnect: %+v, %v; want the same connection %d", c2, err, c.ID)
	}
}
