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
	"maps"
	"net/http"
	"slices"
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
// repository scope does. The digest slot carries the scope — the repository and the permission
// set — so two differently-scoped tokens for one installation cannot be handed to each other.

// Token purposes. What a minted token may do is chosen by the Go code asking for it, never by a
// model: ProxyRequest.Purpose has no JSON name, so no tool argument and no held write can carry
// one. The default is the set every caller had before purposes existed — the fix-job push and the
// Slack tools — and it never grows: a permission the App gains later is asked for by a new
// purpose, so a token leaked from a worker reaches no more than it did the day it was written.
//
// Code review asks for two narrower ones. Reading a pull request needs nothing that writes, and
// review_read is read-only, so a review that is talked into anything at all by the code it is
// reading holds a token that cannot act on it. Posting needs only pull_requests:write — not
// contents, which is the fix-job push, and not issues: GitHub documents the issue-comment
// endpoints a summary is posted and edited through as open to either Issues or Pull requests on a
// pull request, and whether that holds for an installation token is a thing to learn live. If it
// does not, the fix is issues:write here and in nothing else, and every installation owner is then
// asked to accept the new permission.
//
// review_checks is the review's check run in the pull request's checks list, and nothing else: an
// App made before code review had one has no Checks permission, and a token asked for with a
// permission the installation never granted is refused outright. Kept apart from review_post so that
// refusal costs the check run alone, never the review, and asked for only by an installation that
// granted it (reviewChecksGranted).
const (
	githubPurposeDefault      = ""
	githubPurposeReviewRead   = "review_read"
	githubPurposeReviewPost   = "review_post"
	githubPurposeReviewChecks = "review_checks"
)

// githubPurposePermissions is the one list of what each purpose's token may do, pinned by
// TestGitHubPurposePermissions so that widening one is a change somebody makes on purpose.
var githubPurposePermissions = map[string]map[string]string{
	githubPurposeDefault:      {"contents": "write", "pull_requests": "write"},
	githubPurposeReviewRead:   {"contents": "read", "pull_requests": "read"},
	githubPurposeReviewPost:   {"pull_requests": "write"},
	githubPurposeReviewChecks: {"checks": "write"},
}

// githubTokenPermissions is the permission set a purpose mints with, as a copy the caller may
// keep. An unknown purpose is refused rather than read as the default: a caller that spelled
// "review_read" wrong asked for less than the default, and must not be handed more.
func githubTokenPermissions(purpose string) (map[string]string, error) {
	perms, ok := githubPurposePermissions[purpose]
	if !ok {
		return nil, fmt.Errorf("unknown GitHub token purpose %q", truncate(purpose, 40))
	}
	return maps.Clone(perms), nil
}

// permissionList is a permission set as one stable line, "contents:read,pull_requests:read",
// sorted so the same set always reads the same — for the cache key and for an error message.
func permissionList(perms map[string]string) string {
	parts := make([]string, 0, len(perms))
	for k, v := range perms {
		parts = append(parts, k+":"+v)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// installTokenKey identifies a minted token by the installation it came from and what it was
// scoped to: the repository and the permission set. Not exchangeKey: that digests the whole
// secret, which here is just the installation id, so every scope under one install would collide
// on the same key. The permissions are in the digest so a review's read-only token is never
// handed to a fix job that needs to push, and — the direction that matters — a fix job's
// write token is never handed to a review that asked for a read-only one.
func installTokenKey(installationID int64, repo string, perms map[string]string) tokenCacheKey {
	return tokenCacheKey{id: installationID, kind: "github_app",
		digest: sha256.Sum256([]byte(strings.ToLower(repo) + "\x00" + permissionList(perms)))}
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

// installationToken returns a token for this connection's installation, scoped to its repository
// and to what purpose says it is for (githubPurposePermissions).
func (p *Proxy) installationToken(ctx context.Context, orgID int64, conn *Connection, s *Secret, purpose string) (string, error) {
	perms, err := githubTokenPermissions(purpose)
	if err != nil {
		return "", err
	}
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
	key := installTokenKey(id, conn.Repo, perms)
	if tok, ok := p.cached(key); ok {
		return tok, nil
	}
	tok, expires, err := p.mintInstallationToken(ctx, id, conn.Repo, perms)
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

// mintInstallationToken does the exchange itself: app JWT in, installation token out, holding
// perms and nothing more.
func (p *Proxy) mintInstallationToken(ctx context.Context, id int64, repo string, perms map[string]string) (string, time.Time, error) {
	if len(perms) == 0 {
		// GitHub reads no permissions field as "everything the installation was granted", which
		// is exactly the token this is here to never mint.
		return "", time.Time{}, errors.New("an installation token must name the permissions it is for")
	}
	jwt, err := p.ghApp.jwt(time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	// Narrow the token to what the caller does with it rather than everything the App was granted:
	// for the default purpose, read and write file contents (clone, push a branch) and open pull
	// requests. GitHub only ever narrows here, and metadata:read (which code search needs) is
	// always included implicitly. Without it, a token leaked from the worker could reach issues,
	// actions, deployments or anything else in the App's grant.
	fields := map[string]any{"permissions": perms}
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
		return "", time.Time{}, &installTokenError{status: resp.StatusCode, msg: out.Message, repo: repo, id: id, perms: permissionList(perms)}
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
	perms  string // what was asked for, permissionList's form
}

func (e *installTokenError) Error() string {
	switch {
	case e.status == 404:
		return fmt.Sprintf("the GitHub App installation %d no longer exists: it was uninstalled at GitHub. Install it again from the console", e.id)
	case e.status == 422 && strings.Contains(strings.ToLower(e.msg), "permission"):
		// The App asks for more than this installation has accepted: a new version of the App
		// added a permission, and GitHub holds it back until an owner of the account accepts it.
		// Nothing in the repository selection is wrong, and saying so sent people to the wrong page.
		return fmt.Sprintf("the GitHub App installation %d has not accepted the permissions this needs (%s). An owner of the account accepts the App's new permissions at GitHub, under the installation's Configure page", e.id, e.perms)
	case e.status == 422 && e.repo != "":
		// GitHub's 422 says one of two things and the message is the only way to tell them apart,
		// so when it says neither plainly both are named.
		return fmt.Sprintf("GitHub would not mint a token for %s (%s): either it is not in this installation's repository selection, or the installation has not accepted the App's new permissions. Both are fixed at GitHub on the installation's Configure page", e.repo, truncate(e.msg, 160))
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
