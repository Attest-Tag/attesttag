package app

import (
	"context"
	"os"
	"testing"
)

// TestReposForTokenLive walks the real listing with a real token (GITHUB_LIVE_TOKEN=… ), to catch
// the case the stub cannot: GitHub answering a fine-grained token differently than expected.
func TestReposForTokenLive(t *testing.T) {
	token := os.Getenv("GITHUB_LIVE_TOKEN")
	if token == "" {
		t.Skip("set GITHUB_LIVE_TOKEN=… to run against the real api.github.com")
	}
	b := repoTestBot(t, nil)
	b.proxy = NewProxy(b.sealer, b.store) // real transport
	repos, truncated, err := b.reposForToken(context.Background(), orgID, token)
	if err != nil {
		t.Fatalf("reposForToken: %v", err)
	}
	t.Logf("%d repositories, truncated=%v", len(repos), truncated)
	for _, r := range repos[:min(5, len(repos))] {
		t.Logf("  %s private=%v pushed=%s", r.Repo, r.Private, r.Pushed)
	}
	if len(repos) == 0 {
		t.Error("no repositories came back for a token that should reach at least one")
	}
}

// TestSavedTokenLive proves the reuse path end to end: seal a real token onto a connection the
// way the console does, then list repositories with nothing but that connection's id.
func TestSavedTokenLive(t *testing.T) {
	token := os.Getenv("GITHUB_LIVE_TOKEN")
	if token == "" {
		t.Skip("set GITHUB_LIVE_TOKEN=… to run against the real api.github.com")
	}
	b := repoTestBot(t, nil)
	b.proxy = NewProxy(b.sealer, b.store) // real transport
	id := storeGitHubConn(t, b, "live", "acme/blog", token)
	ctx := context.Background()
	reused, err := b.savedAuth(ctx, orgID, id)
	if err != nil {
		t.Fatalf("savedAuth: %v", err)
	}
	repos, _, err := b.reposForToken(ctx, orgID, reused.token)
	if err != nil {
		t.Fatalf("reposForToken with the sealed token: %v", err)
	}
	t.Logf("%d repositories reached with the stored token", len(repos))
	if len(repos) == 0 {
		t.Error("the stored token should reach at least one repository")
	}
}
