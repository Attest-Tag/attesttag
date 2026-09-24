package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Repositories: a GitHub repository connected from the Workspaces page with an access token. It is
// an ordinary github-preset connection with `repo` set, filed under the "Repositories" bundle
// (created on demand, github tool pack on) and attached to the scope on its own. The channel
// gets the github_* tools for it, and the prompt tells the model which repository is meant.

const repoBundleName = "Repositories"

// GitHub owners are alphanumerics and hyphens; repository names may also contain dots and underscores.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)

// normalizeRepo accepts owner/name or any github.com url form and returns owner/name.
// "" stays "" (clear / inherit).
func normalizeRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	for _, p := range []string{"https://", "http://", "git@github.com:", "www.", "github.com/"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.Trim(s, "/")
	if parts := strings.Split(s, "/"); len(parts) >= 2 {
		s = parts[0] + "/" + parts[1]
	}
	s = strings.TrimSuffix(s, ".git")
	if !repoRe.MatchString(s) {
		return "", fmt.Errorf("repository must be owner/name or a github.com url, got %q", strings.TrimSpace(s))
	}
	return s, nil
}

// hasRepo reports whether owner/name is reachable through this access.
func (a *Access) hasRepo(repo string) bool {
	for _, c := range a.Repos() {
		if strings.EqualFold(c.Repo, repo) {
			return true
		}
	}
	return false
}

// findRepoBundle returns the bundle repository connections live in, or nil before the first one.
func (b *Bot) findRepoBundle(ctx context.Context, orgID int64) (*Bundle, error) {
	bs, err := b.store.Bundles(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, bd := range bs {
		if strings.EqualFold(bd.Name, repoBundleName) {
			return bd, nil
		}
	}
	return nil, nil
}

// createRepoBundle makes the Repositories bundle with the GitHub tool pack switched on.
func (b *Bot) createRepoBundle(ctx context.Context, orgID int64, by string) (*Bundle, error) {
	bd, err := b.store.CreateBundle(ctx, orgID, repoBundleName, by)
	if err != nil {
		return nil, err
	}
	if err := b.store.UpdateBundle(ctx, orgID, bd.ID, bd.Name,
		"Repositories connected in the console. Each connection is one GitHub repository.", []string{"github"}); err != nil {
		return nil, err
	}
	return b.store.Bundle(ctx, orgID, bd.ID)
}

// githubRepo is one repository a token can reach, as the console's picker lists it.
type githubRepo struct {
	Repo    string `json:"repo"`
	Private bool   `json:"private"`
	Pushed  string `json:"pushed_at,omitempty"`
}

// tokenAccess wraps a bare token in an unsaved github connection, so a call made while the
// console is still asking questions goes through the same host and header rules as a stored one.
func (b *Bot) tokenAccess(token string) (*Access, error) {
	c, sec, err := b.buildConnection(&connectionInput{Name: "github", Preset: "github", CredType: "bearer",
		Secret: &Secret{Token: token}}, nil)
	if err != nil {
		return nil, err
	}
	enc, err := b.sealSecret(sec)
	if err != nil {
		return nil, err
	}
	c.secretEnc = enc
	return &Access{Rules: []Rule{{Conn: c, Rank: 9}}}, nil
}

// repoAuth is how a repository is reached: a pasted or stored token, or a GitHub App
// installation. Exactly one is set. Both kinds may be used in the same account and even in the
// same bundle — an organisation that installs the app does not have to re-key the repositories
// it already connected with a token, and one it cannot install on (a personal account, an
// Enterprise Server) still has the token path.
type repoAuth struct {
	token          string
	installationID int64
	// verified says GitHub has already confirmed this authorisation reaches these repositories,
	// which is true of everything /installation/repositories just listed. It skips the per-repo
	// check in connectRepo — asking GitHub again, once per repository, for an answer it gave in
	// the same breath is what would turn "install the app" into a thirty-second redirect.
	verified bool
}

func (a repoAuth) ready() bool { return a.token != "" || a.installationID > 0 }

// installAccess wraps an installation in an unsaved github connection, so a call made while the
// console is still asking questions goes through the same host and header rules as a stored one.
// The twin of tokenAccess, and the reason both exist: the pre-save calls must not be a second,
// looser path to GitHub.
func (b *Bot) installAccess(installationID int64) (*Access, error) {
	c, sec, err := b.buildConnection(&connectionInput{Name: "github", Preset: "github", CredType: "github_app",
		Secret: &Secret{InstallationID: installationID}}, nil)
	if err != nil {
		return nil, err
	}
	enc, err := b.sealSecret(sec)
	if err != nil {
		return nil, err
	}
	c.secretEnc = enc
	c.GitHubInstallationID = installationID
	return &Access{Rules: []Rule{{Conn: c, Rank: 9}}}, nil
}

// accessFor builds the unsaved access for whichever kind of authorisation this is.
func (b *Bot) accessFor(auth repoAuth) (*Access, error) {
	if auth.installationID > 0 {
		return b.installAccess(auth.installationID)
	}
	return b.tokenAccess(auth.token)
}

// reposForInstall lists what an installation can reach: exactly the repositories the account's
// admin ticked in GitHub's own picker. Unlike a token listing, there is nothing here the person
// did not choose, so there is no affiliation filter to get wrong.
func (b *Bot) reposForInstall(ctx context.Context, orgID, installationID int64) ([]githubRepo, bool, error) {
	if installationID == 0 {
		return nil, false, errors.New("a GitHub App installation is required")
	}
	// Deliberately not scoped to a repository: listing the installation's repositories is the
	// one call that needs the installation-wide token.
	acc, err := b.installAccess(installationID)
	if err != nil {
		return nil, false, err
	}
	out := []githubRepo{}
	for page := 1; page <= repoPages; page++ {
		url := fmt.Sprintf("https://api.github.com/installation/repositories?per_page=100&page=%d", page)
		resp, err := b.proxy.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: url, MaxBytes: proxyMaxRead},
			ProxyAudit{Requester: "console-repos"}, true)
		if err != nil {
			return nil, false, fmt.Errorf("could not reach GitHub: %w", err)
		}
		if resp.Status >= 400 {
			return nil, false, fmt.Errorf("GitHub returned %d listing the installation's repositories", resp.Status)
		}
		if resp.Truncated {
			return nil, false, errors.New("GitHub's repository list came back too large to read; enter the repository by name")
		}
		// An object with the list inside it, not a bare array as /user/repos returns.
		var body struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
				Private  bool   `json:"private"`
				PushedAt string `json:"pushed_at"`
			} `json:"repositories"`
		}
		if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
			return nil, false, fmt.Errorf("could not read GitHub's repository list: %w", err)
		}
		for _, r := range body.Repositories {
			if repo, err := normalizeRepo(r.FullName); err == nil && repo != "" {
				out = append(out, githubRepo{Repo: repo, Private: r.Private, Pushed: r.PushedAt})
			}
		}
		if len(body.Repositories) < 100 {
			return out, false, nil
		}
	}
	return out, true, nil
}

// savedToken returns the access token sealed on a github connection, so a second repository can
// be connected under a token that is already stored instead of asking for it again. The token
// never travels to the browser: the console names a connection, the server opens it.
func (b *Bot) savedAuth(ctx context.Context, orgID, connID int64) (repoAuth, error) {
	c, err := b.store.Connection(ctx, orgID, connID)
	if err != nil {
		return repoAuth{}, err
	}
	if c == nil || c.Preset != "github" {
		return repoAuth{}, errors.New("no such GitHub connection")
	}
	sec, err := b.proxy.secret(c)
	if err != nil {
		return repoAuth{}, fmt.Errorf("could not open what is stored for %s: %w", c.Name, err)
	}
	if c.CredType == "github_app" || c.GitHubInstallationID > 0 {
		id := sec.InstallationID
		if id == 0 {
			id = c.GitHubInstallationID
		}
		return repoAuth{installationID: id}, nil
	}
	if sec.Token == "" {
		return repoAuth{}, fmt.Errorf("%s has no stored access token", c.Name)
	}
	return repoAuth{token: sec.Token}, nil
}

// repoPages is how many pages of 100 the listing walks before it gives up and says so.
const repoPages = 3

// reposForToken lists the repositories a token can reach, most recently pushed first, so the
// console can offer them instead of asking anyone to type a name. A fine-grained token returns
// exactly the repositories it was scoped to — usually one — while a classic token returns
// everything the account can see, which is why the list is capped and truncation is reported:
// the console keeps its manual entry box for whatever falls off the end.
func (b *Bot) reposForToken(ctx context.Context, orgID int64, token string) ([]githubRepo, bool, error) {
	if token == "" {
		return nil, false, errors.New("a GitHub access token is required")
	}
	acc, err := b.tokenAccess(token)
	if err != nil {
		return nil, false, err
	}
	out := []githubRepo{}
	for page := 1; page <= repoPages; page++ {
		url := fmt.Sprintf("https://api.github.com/user/repos?per_page=100&page=%d&sort=pushed"+
			"&affiliation=owner,collaborator,organization_member", page)
		resp, err := b.proxy.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: url, MaxBytes: proxyMaxRead},
			ProxyAudit{Requester: "console-repos"}, true)
		if err != nil {
			return nil, false, fmt.Errorf("could not reach GitHub: %w", err)
		}
		switch {
		case resp.Status == 401:
			return nil, false, errors.New("GitHub rejected the token (401): check it was pasted whole and has not expired")
		case resp.Status == 403:
			return nil, false, errors.New("GitHub will not list repositories for this token (403): enter the repository by name instead")
		case resp.Status >= 400:
			return nil, false, fmt.Errorf("GitHub returned %d for the repository list", resp.Status)
		}
		if resp.Truncated {
			return nil, false, errors.New("GitHub's repository list came back too large to read; enter the repository by name")
		}
		var body []struct {
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
			PushedAt string `json:"pushed_at"`
		}
		if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
			return nil, false, fmt.Errorf("could not read GitHub's repository list: %w", err)
		}
		for _, r := range body {
			if repo, err := normalizeRepo(r.FullName); err == nil && repo != "" {
				out = append(out, githubRepo{Repo: repo, Private: r.Private, Pushed: r.PushedAt})
			}
		}
		if len(body) < 100 {
			return out, false, nil
		}
	}
	return out, true, nil
}

// connectRepo stores owner/name with its token (re-keying an existing connection for the same
// repo instead of adding a twin) after proving the token opens the repository, then attaches
// the connection to the scope when there is one: from the Bundles page a repository is only
// saved, and a channel adds it later. Nothing is written until GitHub has accepted the token.
func (b *Bot) connectRepo(ctx context.Context, orgID int64, sc *Scope, repo string, auth repoAuth, writes, by string) (*Connection, error) {
	if !auth.ready() {
		return nil, errors.New("a GitHub App installation or an access token is required")
	}
	if writes != "auto" {
		writes = "confirm"
	}
	bundle, err := b.findRepoBundle(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var existing []*Connection
	if bundle != nil {
		if existing, err = b.store.ConnectionsForBundle(ctx, orgID, bundle.ID); err != nil {
			return nil, err
		}
	}
	var cur *Connection
	name := repo[strings.Index(repo, "/")+1:]
	for _, c := range existing {
		if strings.EqualFold(c.Repo, repo) {
			cur = c
		} else if strings.EqualFold(c.Name, name) {
			name = strings.ReplaceAll(repo, "/", "-") // two repos with the same name: keep the owner
		}
	}
	if cur != nil {
		name = cur.Name
	}
	// The preset stays "github" for both kinds. Tool packs are keyed on it (tools_http.go) and
	// the Repositories bundle enables "github", so a separate preset for app-backed repositories
	// would silently strip every github_* tool from them.
	in := &connectionInput{Name: name, Preset: "github", CredType: "bearer", Writes: writes,
		Notes: "GitHub repository " + repo + ": pass repo=" + repo + " to the github_* tools.", Secret: &Secret{Token: auth.token}}
	if auth.installationID > 0 {
		in.CredType, in.Secret = "github_app", &Secret{InstallationID: auth.installationID}
	}
	c, sec, err := b.buildConnection(in, cur)
	if err != nil {
		return nil, err
	}
	c.Repo = repo
	c.GitHubInstallationID = auth.installationID
	// Repositories connected with one token are one credential, and only here is that token in
	// the clear. An app-backed row keeps the empty digest: its installation id already says
	// which credential it is, and the token it spends is minted per call and never stored.
	c.SecretFP = secretFingerprint(auth.token)
	c.Status = "active"
	enc, err := b.sealSecret(sec)
	if err != nil {
		return nil, err
	}
	c.secretEnc = enc
	if !auth.verified {
		if err := b.checkRepoAccess(ctx, orgID, c, repo); err != nil {
			return nil, err
		}
	}
	if bundle == nil {
		if bundle, err = b.createRepoBundle(ctx, orgID, by); err != nil {
			return nil, err
		}
	}
	c.BundleID = bundle.ID
	if cur == nil {
		c.CreatedBy = by
		id, err := b.store.InsertConnection(ctx, orgID, c, enc)
		if err != nil {
			return nil, err
		}
		c.ID = id
	} else if err := b.store.UpdateConnection(ctx, orgID, c, enc); err != nil {
		return nil, err
	}
	if sc != nil {
		if err := b.store.AttachConnection(ctx, orgID, sc.ID, c.ID); err != nil {
			return nil, err
		}
	}
	return b.store.Connection(ctx, orgID, c.ID)
}

// secretFingerprint identifies a token without carrying it: the first eight bytes of its
// SHA-256, hex. Two repositories connected with the same token get the same value, and nothing
// about the token can be recovered from it — a GitHub token has far more entropy than anything
// a digest of it could be searched against. Empty in, empty out, so the app-backed path stores
// nothing rather than the digest of an empty string.
func secretFingerprint(token string) string {
	if strings.TrimSpace(token) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// checkRepoAccess asks GitHub for the repository with the connection's token, through the
// proxy so the same host and header rules apply, and turns the usual failures into advice.
func (b *Bot) checkRepoAccess(ctx context.Context, orgID int64, c *Connection, repo string) error {
	acc := &Access{Rules: []Rule{{Conn: c, Rank: 9}}}
	resp, err := b.proxy.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://api.github.com/repos/" + repo},
		ProxyAudit{Requester: "console-test"}, true)
	if err != nil {
		return fmt.Errorf("could not reach GitHub: %w", err)
	}
	switch {
	case resp.Status == 401:
		return errors.New("GitHub rejected the token (401): check it was pasted whole and has not expired")
	case resp.Status == 403:
		return fmt.Errorf("GitHub refused %s with this token (403): it lacks permission for that repository", repo)
	case resp.Status == 404:
		return fmt.Errorf("GitHub cannot find %s with this token (404): check the name, and that the token can see it if it is private", repo)
	case resp.Status >= 400:
		return fmt.Errorf("GitHub returned %d for %s", resp.Status, repo)
	}
	return nil
}

// repoSummary lists the repository connections reachable from a scope, narrowest grant first.
func (b *Bot) repoSummary(ctx context.Context, orgID int64, sc *Scope, acc *Access) []map[string]any {
	out := []map[string]any{}
	if acc == nil {
		return out
	}
	bundleName := map[int64]string{}
	bs, _ := b.store.Bundles(ctx, orgID)
	for _, bd := range bs {
		bundleName[bd.ID] = bd.Name
	}
	seen := map[string]bool{}
	for _, r := range acc.Rules {
		if r.Conn.Repo == "" || seen[r.Conn.Repo] {
			continue
		}
		seen[r.Conn.Repo] = true
		origin := "inherited from workspace"
		if sc.Kind == "workspace" || r.Rank == 2 {
			origin = "attached here"
		}
		via := "bundle"
		if r.Direct {
			via = "connection"
		}
		out = append(out, map[string]any{"connection_id": r.Conn.ID, "repo": r.Conn.Repo, "name": r.Conn.Name,
			"bundle": bundleName[r.Conn.BundleID], "via": via, "origin": origin})
	}
	return out
}

// connectRepos connects each repository with the same token, one connection apiece, and keeps
// going past a repository the token cannot open so the rest still land. The caller reports the
// failures; only an empty result is an error.
func (b *Bot) connectRepos(ctx context.Context, orgID int64, sc *Scope, repos []string, auth repoAuth, writes, by string) ([]string, []map[string]string, error) {
	done, failed := []string{}, []map[string]string{}
	for _, repo := range repos {
		if _, err := b.connectRepo(ctx, orgID, sc, repo, auth, writes, by); err != nil {
			failed = append(failed, map[string]string{"repo": repo, "error": err.Error()})
			continue
		}
		done = append(done, repo)
	}
	if len(done) == 0 {
		if len(failed) == 1 {
			return done, failed, errors.New(failed[0]["error"])
		}
		return done, failed, fmt.Errorf("none of the %d repositories could be connected", len(failed))
	}
	return done, failed, nil
}

// attachRepoConnections adds repositories already saved under Repositories to a scope by
// connection id, no token needed. Anything that is not a GitHub repository connection of this
// organisation is reported and skipped; nothing landing is an error.
func (b *Bot) attachRepoConnections(ctx context.Context, orgID int64, sc *Scope, ids []int64) ([]string, []map[string]string, error) {
	done, failed := []string{}, []map[string]string{}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		c, err := b.store.Connection(ctx, orgID, id)
		if err != nil || c == nil || c.Preset != "github" || c.Repo == "" {
			failed = append(failed, map[string]string{"repo": fmt.Sprintf("connection %d", id), "error": "not a saved GitHub repository"})
			continue
		}
		if err := b.store.AttachConnection(ctx, orgID, sc.ID, c.ID); err != nil {
			failed = append(failed, map[string]string{"repo": c.Repo, "error": err.Error()})
			continue
		}
		done = append(done, c.Repo)
	}
	if len(done) == 0 {
		if len(failed) == 1 {
			return done, failed, errors.New(failed[0]["error"])
		}
		if len(failed) == 0 {
			return done, failed, errors.New("pick at least one saved repository")
		}
		return done, failed, fmt.Errorf("none of the %d repositories could be added", len(failed))
	}
	return done, failed, nil
}

// ownsInstall says whether this organisation holds the installation it just named. Not "does the
// installation exist": the id is public, and the whole of what keeps one tenant's repositories
// out of another's console is this comparison against the row the install flow wrote.
func (b *Bot) ownsInstall(ctx context.Context, orgID, installID int64) error {
	g, err := b.store.GitHubInstall(ctx, installID)
	if err != nil {
		return fmt.Errorf("could not check the GitHub App installation: %w", err)
	}
	if g == nil || g.OrgID != orgID || g.Status == "revoked" {
		// One answer for all three, so the endpoint says nothing about which ids exist here.
		return errors.New("that GitHub App installation is not one this organisation has connected")
	}
	return nil
}

// repoToken resolves what the console sent: a token someone pasted, or the id of a connection
// whose sealed token it wants to reuse.
func (b *Bot) repoAuthFor(ctx context.Context, orgID int64, pasted string, connID, installID int64) (repoAuth, error) {
	if installID > 0 {
		// An id out of a request body is a claim, not a possession — it rides in GitHub's own
		// settings URLs and in this console's own post-install redirect, so anybody can name one.
		// Checked here as well as in the proxy so the console answers "not yours" before it
		// reaches GitHub, and so a listing cannot be used to read another organisation's
		// repositories back. The twin of savedAuth's org-scoped Connection lookup below.
		if err := b.ownsInstall(ctx, orgID, installID); err != nil {
			return repoAuth{}, err
		}
		return repoAuth{installationID: installID}, nil
	}
	if pasted = strings.TrimSpace(pasted); pasted != "" {
		return repoAuth{token: pasted}, nil
	}
	if connID > 0 {
		return b.savedAuth(ctx, orgID, connID)
	}
	return repoAuth{}, errors.New("choose a GitHub App installation, or paste an access token")
}
