package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Minting installation tokens.
//
// A github_app connection stores an installation id and no credential at all. Every request
// exchanges the app's own key for an installation token that lives an hour, and — this is the
// part a pasted token can never do — asks for one scoped to the single repository the
// connection names. A connection for acme/api therefore spends a token that cannot touch the
// other nineteen repositories in the same installation, without anybody having to manage
// nineteen separate credentials.
//
// The cache is keyed on the installation rather than the connection, because twenty repository
// connections under one install would otherwise mint twenty tokens where one exchange plus a
// repository scope does. The digest slot carries the scope so two differently-scoped tokens for
// one installation cannot be handed to each other.

// installTokenKey identifies a minted token by the installation it came from and what it was
// scoped to. Not exchangeKey: that digests the whole secret, which here is just the installation
// id, so every scope under one install would collide on the same key.
func installTokenKey(installationID int64, repo string) tokenCacheKey {
	return tokenCacheKey{id: installationID, kind: "github_app", digest: sha256.Sum256([]byte(strings.ToLower(repo)))}
}

// forgetInstallToken drops every token minted from one installation. Needed because forgetToken
// sweeps by connection id, and an installation-keyed entry matches none of those: rotating or
// deleting a repository connection would leave its minted token working for up to an hour.
func (p *Proxy) forgetInstallToken(installationID int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for key := range p.tokens {
		if key.kind == "github_app" && key.id == installationID {
			delete(p.tokens, key)
		}
	}
	p.mu.Unlock()
}

// installationToken returns a token for this connection's installation, scoped to its repository.
func (p *Proxy) installationToken(ctx context.Context, orgID int64, conn *Connection, s *Secret) (string, error) {
	id := s.InstallationID
	if id == 0 {
		id = conn.GitHubInstallationID
	}
	if id == 0 {
		return "", errors.New("this connection names no GitHub App installation")
	}
	if !p.ghApp.configured() {
		return "", errors.New("no GitHub App is configured on this deployment, so its installations cannot be used")
	}
	// The installation has to belong to the organisation spending it, and that is checked here
	// rather than only where the id arrives, because this is the one place every path passes
	// through — an unsaved connection built while the console is still asking questions, a stored
	// one, a tool call. An installation id is not a credential and not a secret: GitHub puts it in
	// its own settings URLs. What stops it being one is this comparison, so it is made before the
	// cache is consulted — the cache is keyed on the installation alone, so a token another
	// organisation legitimately minted is sitting there under exactly the key an impostor asks for.
	if err := p.installBelongsTo(ctx, orgID, id); err != nil {
		return "", err
	}
	key := installTokenKey(id, conn.Repo)
	if tok, ok := p.cached(key); ok {
		return tok, nil
	}
	tok, expires, err := p.mintInstallationToken(ctx, id, conn.Repo)
	if err != nil {
		// Without a webhook this is how an uninstall or a narrowed selection reaches us: the
		// next call says so. Record it on the installation so the console can explain it once
		// rather than leaving every repository under it looking separately broken.
		if status, why := installFailure(err); why != "" {
			if markErr := p.store.MarkGitHubInstallError(ctx, orgID, id, status, why); markErr != nil {
				slog.Warn("marking github install", "installation", id, "err", markErr)
			}
			p.forgetInstallToken(id)
		}
		return "", err
	}
	// Trust GitHub's expiry rather than assuming an hour; cached() keeps its own two-minute floor.
	if ttl := time.Until(expires); ttl > 0 {
		p.remember(key, tok, ttl)
	}
	return tok, nil
}

// installBelongsTo is the tenancy check for an installation id. "Not yours" and "no such
// installation" answer the same, so the refusal cannot be used to learn which ids are installed
// on this deployment; only the log tells them apart.
func (p *Proxy) installBelongsTo(ctx context.Context, orgID, id int64) error {
	g, err := p.store.GitHubInstall(ctx, id)
	if err != nil {
		return fmt.Errorf("could not check the GitHub App installation: %w", err)
	}
	if g == nil || g.OrgID != orgID {
		if g != nil {
			slog.Warn("github installation claimed by another organisation", "installation", id, "claimed_by", orgID)
		}
		return errors.New("that GitHub App installation is not one this organisation has connected")
	}
	if g.Status == "revoked" {
		return errors.New("that GitHub App installation was disconnected here; connect it again to use it")
	}
	return nil
}

// mintInstallationToken does the exchange itself: app JWT in, installation token out.
func (p *Proxy) mintInstallationToken(ctx context.Context, id int64, repo string) (string, time.Time, error) {
	jwt, err := p.ghApp.jwt(time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	// Narrow the token to what attest_tag actually does with it — read and write file contents
	// (clone, push a branch) and open pull requests — rather than everything the App was granted.
	// GitHub only ever narrows here, and metadata:read (which code search needs) is always
	// included implicitly, so this holds for both callers: the fix-job push and the proxy's
	// cross-repo search. Without it, a token leaked from the worker could reach issues, actions,
	// deployments or anything else in the App's grant.
	fields := map[string]any{"permissions": map[string]string{"contents": "write", "pull_requests": "write"}}
	if repo != "" {
		// The bare name, not owner/name: GitHub scopes by repository within the installation's
		// own account. This is the whole security win over a pasted token, and it costs nothing.
		if _, name, ok := strings.Cut(repo, "/"); ok {
			fields["repositories"] = []string{name}
		}
	}
	body, _ := json.Marshal(fields)
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", id)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	// oauthHTTPClient, like every other exchange here: this is the proxy's own plumbing rather
	// than a tenant's call, so the channel's host rules do not apply to it.
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		Message   string `json:"message"`
	}
	json.Unmarshal(raw, &out)
	if resp.StatusCode >= 400 || out.Token == "" {
		return "", time.Time{}, &installTokenError{status: resp.StatusCode, msg: out.Message, repo: repo, id: id}
	}
	expires, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		expires = time.Now().Add(time.Hour)
	}
	return out.Token, expires, nil
}

// installTokenError carries the status so the caller can tell a vanished installation from a
// narrowed repository selection from a clock that has drifted — three different fixes that GitHub
// reports with three similar-looking refusals.
type installTokenError struct {
	status int
	msg    string
	repo   string
	id     int64
}

func (e *installTokenError) Error() string {
	switch {
	case e.status == 404:
		return fmt.Sprintf("the GitHub App installation %d no longer exists: it was uninstalled at GitHub. Install it again from the console", e.id)
	case e.status == 422 && e.repo != "":
		return fmt.Sprintf("%s is not in this installation's repository selection. Add it at GitHub under the app's Configure page", e.repo)
	case e.status == 403 && strings.Contains(strings.ToLower(e.msg), "suspend"):
		return fmt.Sprintf("the GitHub App installation %d is suspended. An account owner unsuspends it at GitHub", e.id)
	case e.status == 401 && strings.Contains(e.msg, "exp"):
		return "GitHub refused the app token over its expiry: this server's clock has drifted"
	case e.status == 401:
		return "GitHub did not accept the app's private key: check GITHUB_APP_ID matches the key"
	}
	return fmt.Sprintf("GitHub would not issue an installation token (%d): %s", e.status, truncate(e.msg, 160))
}

// installFailure says what to record against the installation, and "" for the failures that are
// about this one call rather than the installation itself.
func installFailure(err error) (status, why string) {
	var e *installTokenError
	if !errors.As(err, &e) {
		return "", ""
	}
	switch {
	case e.status == 404:
		return "revoked", "uninstalled at GitHub"
	case e.status == 403 && strings.Contains(strings.ToLower(e.msg), "suspend"):
		return "suspended", "suspended at GitHub"
	}
	return "", ""
}
