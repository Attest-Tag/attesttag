package app

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// The GitHub App this deployment installs as.
//
// An organisation's admin installs it on their GitHub account and picks repositories in
// GitHub's own interface; attest_tag never sees a token and never renders a repository picker
// at install time. What it holds afterwards is an installation id, which is not a credential:
// every call mints a fresh installation token from the app's own key, scoped to the repository
// it is for, and throws it away an hour later.
//
// The key is the one secret, it lives in the environment and never in the database, and it is
// parsed once at boot so a bad one is a startup error rather than a puzzle on the first call.
// MASTER_KEY rotation therefore does not touch it: a sealed github_app secret holds an
// installation id and nothing else.

// githubApp is the app's identity and signing key. Nil when the deployment has no app
// configured, which is the ordinary case for one that only uses pasted tokens.
type githubApp struct {
	id   string // the numeric App ID, which is what the JWT is issued by
	slug string // the URL name, which is how people reach the install page
	key  *rsa.PrivateKey
	// The OAuth client, used for one thing only: proving that the person who just installed the
	// app can actually see the installation they came back with. GitHub's own guidance, because
	// "bad actors can hit this URL with a spoofed installation_id" and state alone does not stop
	// an admin of one organisation naming another's id.
	clientID, clientSecret string
}

// newGitHubApp reads the app out of the environment. Three outcomes, and the middle one is the
// point: nothing configured is fine and means the install flow is simply not offered; a
// half-configured or unreadable app is a startup error, because the alternative is a console
// that offers an Install button leading to a 500; and a complete one is returned.
func newGitHubApp() (*githubApp, error) {
	id := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	slug := strings.TrimSpace(os.Getenv("GITHUB_APP_SLUG"))
	b64 := strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY_B64"))
	raw := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if id == "" && slug == "" && b64 == "" && raw == "" {
		return nil, nil
	}
	if id == "" || slug == "" {
		return nil, errors.New("GITHUB_APP_ID and GITHUB_APP_SLUG are both needed to install as a GitHub App")
	}
	key, err := parseGitHubAppKey(b64, raw)
	if err != nil {
		return nil, err
	}
	// The OAuth client is not required to start, only to install: an unreadable key is a broken
	// deployment, while a missing client is one where existing installations go on working and
	// the Install button is simply not offered. Taking Slack and the console down over the
	// second would be the wrong trade by a long way.
	return &githubApp{id: id, slug: slug, key: key,
		clientID:     strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_ID")),
		clientSecret: strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_SECRET"))}, nil
}

// parseGitHubAppKey accepts the key in either of the two shapes a deployment can carry it in.
//
// Cloud Run gets the base64 one because deploy/gcp/cloudrun.sh reads a dotenv file a line at a time
// and a PEM is twenty-eight of them; base64 has no commas either, so --set-env-vars survives it.
// A local .env may use the raw PEM with its newlines escaped, which is what pasting one into a
// single-line variable produces.
func parseGitHubAppKey(b64, raw string) (*rsa.PrivateKey, error) {
	pemBytes := []byte(strings.ReplaceAll(raw, `\n`, "\n"))
	if b64 != "" {
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_B64 is not base64: %w", err)
		}
		pemBytes = der
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("the GitHub App private key is not PEM. Base64 the .pem file whole: base64 -i app.pem | tr -d '\\n'")
	}
	// GitHub issues PKCS#1 ("RSA PRIVATE KEY"), so that is the live branch; PKCS#8 is accepted
	// because a key round-tripped through openssl comes back as one. Same ladder as gcpToken.
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("the GitHub App private key is not an RSA key")
	}
	return nil, fmt.Errorf("the GitHub App private key could not be read (PEM block %q)", block.Type)
}

// configured reports whether this deployment has an app at all — enough to mint installation
// tokens for installations it already holds. A nil receiver is the unconfigured case and answers
// false, so callers need no nil check of their own.
func (g *githubApp) configured() bool { return g != nil && g.key != nil }

// canInstall is the stricter question: may somebody install it from here? That needs the OAuth
// client on top, because binding an installation means first proving the person who came back
// can actually see it, and that takes a user token. Without it the flow is not offered rather
// than offered and unsafe.
func (g *githubApp) canInstall() bool {
	return g.configured() && g.clientID != "" && g.clientSecret != ""
}

// installMissing names the environment variables standing between this deployment and the
// install flow, so the console can say which ones rather than merely not offering the button. A
// self-hoster reading about the App route and finding nothing in the console has no way to tell
// a missing feature from an unset variable; this is that way. Empty when canInstall is true.
func (g *githubApp) installMissing() []string {
	if g.canInstall() {
		return nil
	}
	if !g.configured() {
		// Nothing set at all. The private key is named by its preferred spelling; GITHUB_APP_
		// PRIVATE_KEY is the alternative and listing both would only make this longer to read.
		return []string{"GITHUB_APP_ID", "GITHUB_APP_SLUG", "GITHUB_APP_PRIVATE_KEY_B64",
			"GITHUB_APP_CLIENT_ID", "GITHUB_APP_CLIENT_SECRET"}
	}
	// A key but no OAuth client: the one half-configured state that is not fatal at boot, because
	// installations it already holds keep working and only installing from here is withheld.
	var out []string
	if g.clientID == "" {
		out = append(out, "GITHUB_APP_CLIENT_ID")
	}
	if g.clientSecret == "" {
		out = append(out, "GITHUB_APP_CLIENT_SECRET")
	}
	return out
}

// installURL is where an admin is sent to choose an account and its repositories. GitHub does
// the choosing; state rides along so the installation that comes back can be bound to the
// organisation that asked for it.
func (g *githubApp) installURL(state string) string {
	return fmt.Sprintf("https://github.com/apps/%s/installations/new?state=%s", g.slug, state)
}

// appJWTTTL is how long an app JWT is asked to live. GitHub refuses anything over ten minutes,
// and refuses it by naming exp, which is the same error a drifting clock produces — hence the
// minute of back-dating below rather than a tighter margin here.
const appJWTTTL = 9 * time.Minute

// jwt signs the assertion that proves we are this app. It authenticates the app itself, not any
// installation: it is what mints installation tokens and what reads /app/installations/{id}, and
// it can do nothing to a repository.
//
// Not cached. One RSA signature costs less than the map lookup that would avoid it.
func (g *githubApp) jwt(now time.Time) (string, error) {
	if !g.configured() {
		return "", errors.New("no GitHub App is configured on this deployment")
	}
	b64 := func(v any) string { j, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(j) }
	// iat is back-dated a minute because GitHub checks it against its own clock and rejects a
	// token issued in its future. Cloud Run's clock is not ours to trust to the second.
	header := b64(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims := b64(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(appJWTTTL).Unix(), "iss": g.id})
	sum := sha256.Sum256([]byte(header + "." + claims))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return header + "." + claims + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ---- installing ----

const (
	// githubStateCookie is named apart from the Slack one on purpose: somebody adding a Slack
	// workspace in one tab and a GitHub account in another would otherwise have each install
	// quietly clear the other's proof that the browser which finished is the one that started.
	githubStateCookie = "attest_github_install_state"
	githubStateTTL    = 15 * time.Minute
)

// setupURL is what GitHub's "Setup URL" field has to contain, for the host this request came in
// on. Built from the request rather than from ADMIN_BASE_URL so the same binary works behind
// Tailscale, on localhost and in production without a setting: whichever host you sign in on is
// the one the console tells you to paste into GitHub.
//
// GitHub keeps exactly one Setup URL per app and redirects there itself — it takes no
// redirect_uri — so this is the one value that has to be changed by hand when moving between
// environments on a single app. Everything on our side of the flow is relative and does not care.
func githubSetupURL(r *http.Request) string { return requestOrigin(r) + "/github/setup" }

// ghInstallation is the part of GitHub's installation record this flow needs.
type ghInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	SuspendedAt         string            `json:"suspended_at"`
	AppSlug             string            `json:"app_slug"`
}

// authorizeURL is the full web application flow, the one place GitHub lets us say where to come
// back to. The installation flow does not: it always uses the first callback URL in the app's
// settings and ignores redirect_uri, which is why binding cannot depend on it — a single fixed
// URL means localhost, Tailscale and production cannot all work from one app registration.
//
// So the install is left to land wherever GitHub sends it, and the binding happens here instead,
// on whatever host the person is actually using.
func (g *githubApp) authorizeURL(state, redirectURI string) string {
	q := url.Values{"client_id": {g.clientID}, "redirect_uri": {redirectURI}, "state": {state}}
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

func (b *Bot) githubAppRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /github/install", b.handleGitHubInstall)
	mux.HandleFunc("GET /github/connect", b.handleGitHubConnect)
	mux.HandleFunc("GET /github/setup", b.handleGitHubSetup)
	mux.Handle("GET /api/github/installations", b.requirePerm(PermConnView, b.handleGitHubInstallations))
}

// githubBack sends the browser home with a message. Relative on purpose: the answer belongs on
// the host the request arrived on, which is the whole of what makes Tailscale and localhost work.
func githubBack(w http.ResponseWriter, r *http.Request, param, msg string) {
	to := "/admin/bundles/"
	if param != "" {
		to += "?" + param + "=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// handleGitHubInstall sends an admin to GitHub to choose an account and its repositories.
//
// This is a browser navigation, so it cannot be wrapped in requireAdmin — there is no CSRF token
// on a link, and the answer has to be a redirect rather than JSON. It therefore repeats by hand
// the checks requireAdmin would have made, exactly as handleInstall does for Slack.
func (b *Bot) handleGitHubInstall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !b.ghApp.canInstall() {
		githubBack(w, r, "github_error", "This deployment cannot install the GitHub App: its OAuth client is not set.")
		return
	}
	u := b.authenticate(r)
	if u == nil {
		b.rememberInstall(w, r, "github") // parks the intent and sends them to sign in
		return
	}
	if !u.Permissions[PermConnManage] {
		githubBack(w, r, "github_error", "Connecting a GitHub account needs the connections permission.")
		return
	}
	if st := b.settings.Get(ctx, u.OrgID); !st.allowsVia(u.Via) {
		githubBack(w, r, "github_error", "Your organisation's sign-in policy does not allow this session to connect accounts.")
		return
	}
	if b.twoFactorOwed(ctx, u) {
		githubBack(w, r, "github_error", "Turn on two-factor authentication before connecting an account.")
		return
	}
	state, err := b.store.NewOAuthState(ctx, u.OrgID, u.UserID, githubStateTTL)
	if err != nil {
		githubBack(w, r, "github_error", "Could not start the install: "+err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: githubStateCookie, Value: state, Path: "/github/", HttpOnly: true,
		MaxAge: int(githubStateTTL.Seconds()), SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	http.Redirect(w, r, b.ghApp.installURL(state), http.StatusFound)
}

// handleGitHubSetup is where GitHub sends the browser back. It binds the installation it names
// to the organisation that asked for it, and refuses every other reading of the same request.
func (b *Bot) handleGitHubSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !b.ghApp.canInstall() {
		githubBack(w, r, "github_error", "This deployment cannot install the GitHub App: its OAuth client is not set.")
		return
	}
	q := r.URL.Query()
	state, action := q.Get("state"), q.Get("setup_action")

	c, cookieErr := r.Cookie(githubStateCookie)
	http.SetCookie(w, &http.Cookie{Name: githubStateCookie, Value: "", Path: "/github/", MaxAge: -1})
	bound := state != "" && cookieErr == nil && c.Value != "" && hmac.Equal([]byte(c.Value), []byte(state))

	// A non-admin asked their account's owners to approve the install. Nothing exists yet and
	// nothing is wrong; saying "install failed" here would be a lie.
	if action == "request" {
		githubBack(w, r, "github_notice", "Your request was sent to the GitHub account's owners. Come back once they approve it.")
		return
	}
	// No state means the install began somewhere we did not send them — GitHub's own Apps page,
	// or a marketplace listing. Do not bind it: an installation id is not proof of anything, and
	// attaching it to whichever browser happens to arrive is the multi-tenant hijack this whole
	// dance exists to prevent. Start the flow properly instead; GitHub recognises the existing
	// install and comes straight back, this time with state. Same reasoning as handleInstallCallback.
	if state == "" {
		http.Redirect(w, r, "/github/install", http.StatusFound)
		return
	}
	if !bound {
		githubBack(w, r, "github_error", "That install link was opened in a different browser. Start again from this page.")
		return
	}
	u := b.authenticate(r)
	if u == nil {
		githubBack(w, r, "github_error", "Sign in and start the install again.")
		return
	}
	installedBy, orgID, err := b.store.TakeOAuthState(ctx, state)
	if err != nil {
		githubBack(w, r, "github_error", "That install link has expired. Start again from this page.")
		return
	}
	if u.OrgID != orgID || !u.Permissions[PermConnManage] {
		githubBack(w, r, "github_error", "That install was started by a different account.")
		return
	}
	// installation_id is present when GitHub sent the browser here itself, straight after an
	// install; it is absent on the authorize hop, which knows who the person is but not what they
	// just clicked. Zero means "bind everything they can see" rather than "nothing named".
	id, _ := strconv.ParseInt(strings.TrimSpace(q.Get("installation_id")), 10, 64)
	if id < 0 {
		id = 0
	}
	// The installation id in this URL is a claim, not evidence. Exchange GitHub's code for the
	// installer's own token and take the installations from what that token can see — so an id
	// typed in by somebody who cannot see it is not in the list, and is refused.
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		githubBack(w, r, "github_error", "GitHub did not ask you to authorise the app, so we cannot confirm the "+
			"installation is yours. Turn on \"Request user authorization (OAuth) during installation\" in the app's settings.")
		return
	}
	// GitHub matches the exchange against whatever the authorize step sent, and the two paths here
	// sent different things: the install redirect takes no redirect_uri at all, while /github/connect
	// sent this host's setup URL. Repeating the wrong one is an error rather than a mismatch we
	// could ignore.
	redirectURI := ""
	if id == 0 {
		redirectURI = githubSetupURL(r)
	}
	mine, err := b.ghApp.userInstallations(ctx, code, redirectURI)
	if err != nil {
		slog.Warn("github installer check", "installation", id, "err", err)
		githubBack(w, r, "github_error", "Could not confirm the installation with GitHub. Start the install again.")
		return
	}
	saved, n, elsewhere := b.bindInstallations(ctx, orgID, mine, id, installedBy)
	if saved == 0 {
		if len(elsewhere) > 0 {
			githubBack(w, r, "github_error", strings.Join(elsewhere, ", ")+" is already connected to another attest_tag "+
				"organisation. Ask whoever set it up to invite you, or uninstall it at GitHub first.")
			return
		}
		// Either a spoofed id, or a genuine race where the install has not landed on GitHub's
		// side yet. Both get the same answer; only the log tells them apart.
		slog.Warn("github install not visible to the installer", "installation", id, "org", orgID, "visible", len(mine))
		githubBack(w, r, "github_error", "That installation is not one your GitHub account can see.")
		return
	}
	b.changed(ctx, orgID)
	slog.Info("github app installed", "installation", id, "org", orgID, "bound", saved, "connected", n)
	// One account bound names itself in the URL so the console can point at it; several cannot,
	// and 0 is the console's cue to just show the list.
	named := id
	if saved > 1 {
		named = 0
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/bundles/?github_install=%d&connected_repos=%d", named, n), http.StatusFound)
}

// handleGitHubInstallations is what the console reads to decide between an Install button and a
// list of accounts. setup_url is derived from the request, so the page tells you the exact value
// to paste into the app's Setup URL field for the host you are on — localhost, a Tailscale name
// or the production origin — which is the one part of the flow GitHub will not take per-request.
func (b *Bot) handleGitHubInstallations(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"installations": []*GitHubInstall{}, "install_configured": b.ghApp.canInstall()}
	if b.ghApp.canInstall() {
		out["install_url"] = "/github/install"
		out["setup_url"] = githubSetupURL(r)
		out["app_slug"] = b.ghApp.slug
	} else {
		out["install_missing"] = b.ghApp.installMissing()
	}
	list, err := b.store.GitHubInstalls(r.Context(), orgOf(r))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	for _, g := range list {
		g.AppSlug = b.ghApp.slugOrEmpty()
	}
	out["installations"] = list
	writeJSON(w, 200, out)
}

func (g *githubApp) slugOrEmpty() string {
	if !g.configured() {
		return ""
	}
	return g.slug
}

// autoConnectMax caps how many repositories an install connects by itself when the app was given
// the whole account. "Selected" is a list somebody ticked on purpose and is taken at its word
// whatever its length; "all" on a large organisation is not a per-repository decision at all, so
// past this many the console asks rather than filing hundreds of connections nobody chose.
const autoConnectMax = 50

// connectInstalledRepos files the installation's repositories as saved connections, so the
// person who just picked them at GitHub is not asked to pick them again here. That second picker
// was the whole complaint: the choosing already happened, on GitHub's own page, and repeating it
// is asking the same question twice.
//
// Nothing is attached to a channel — same rule as the Bundles page, they land under Repositories
// and a channel adds what it wants. Re-running on a reinstall re-keys what exists and adds what
// is new, because connectRepo matches on the repository rather than inserting a twin.
func (b *Bot) connectInstalledRepos(ctx context.Context, orgID int64, ins ghInstallation, by string) (int, error) {
	repos, _, err := b.reposForInstall(ctx, orgID, ins.ID)
	if err != nil {
		return 0, err
	}
	if len(repos) == 0 {
		return 0, nil
	}
	if ins.RepositorySelection != "selected" && len(repos) > autoConnectMax {
		return 0, nil // the picker will offer them instead
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Repo)
	}
	// verified: GitHub listed these a moment ago, so re-checking each one buys nothing.
	done, failed, err := b.connectRepos(ctx, orgID, nil, names, repoAuth{installationID: ins.ID, verified: true}, "", by)
	if err != nil {
		return 0, err
	}
	for _, f := range failed {
		slog.Warn("connecting an installed repository", "repo", f["repo"], "err", f["error"])
	}
	return len(done), nil
}

// ---- proving the installer owns what they came back with ----

// userInstallations is the check GitHub's own documentation asks for. The setup redirect is a
// plain GET anybody can craft, so installation_id in it is a claim, not evidence: state proves
// the flow started here and the session proves who is asking, but neither stops a signed-in
// admin of one organisation posting back somebody else's installation id and binding it.
//
// So the code GitHub sends is exchanged for a token belonging to the person who just clicked
// Install, and that token is asked which installations it can see. An id that is not in the
// answer is refused. The token is used for this one question and thrown away: it is proof of
// identity, never a credential we keep.
func (g *githubApp) userInstallations(ctx context.Context, code, redirectURI string) ([]ghInstallation, error) {
	form := url.Values{"client_id": {g.clientID}, "client_secret": {g.clientSecret}, "code": {code}}
	// Repeat whatever the authorize step sent, and only that. GitHub matches the two, so passing
	// one here when the install-time flow sent none is itself an error.
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://github.com/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	json.Unmarshal(raw, &tok)
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("GitHub would not confirm who installed the app: %s", truncate(tok.Error, 160))
	}

	// One page is enough: nobody reaches this having installed the app on more than a handful of
	// accounts in the same click, and the id we are checking for is from the click that just
	// happened.
	req, err = http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user/installations?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp2, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp2.Body, 1<<20))
	if resp2.StatusCode >= 400 {
		return nil, fmt.Errorf("GitHub returned %d listing the installer's installations", resp2.StatusCode)
	}
	var out struct {
		Installations []ghInstallation `json:"installations"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Installations, nil
}

// handleGitHubConnect binds whatever this person has installed, from wherever they are.
//
// It exists because the installation redirect is not ours to aim: GitHub sends it to the first
// callback URL in the app's settings whatever host the install began on. Rather than make that
// one URL the only environment that works, the install is allowed to end anywhere and this
// finishes the job — the authorize flow does honour redirect_uri, so the round trip comes back
// to the host in front of the person.
func (b *Bot) handleGitHubConnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !b.ghApp.canInstall() {
		githubBack(w, r, "github_error", "This deployment cannot connect GitHub accounts: its OAuth client is not set.")
		return
	}
	u := b.authenticate(r)
	if u == nil {
		b.rememberInstall(w, r, "github")
		return
	}
	if !u.Permissions[PermConnManage] {
		githubBack(w, r, "github_error", "Connecting a GitHub account needs the connections permission.")
		return
	}
	if st := b.settings.Get(ctx, u.OrgID); !st.allowsVia(u.Via) {
		githubBack(w, r, "github_error", "Your organisation's sign-in policy does not allow this session to connect accounts.")
		return
	}
	if b.twoFactorOwed(ctx, u) {
		githubBack(w, r, "github_error", "Turn on two-factor authentication before connecting an account.")
		return
	}
	state, err := b.store.NewOAuthState(ctx, u.OrgID, u.UserID, githubStateTTL)
	if err != nil {
		githubBack(w, r, "github_error", "Could not start: "+err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: githubStateCookie, Value: state, Path: "/github/", HttpOnly: true,
		MaxAge: int(githubStateTTL.Seconds()), SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	http.Redirect(w, r, b.ghApp.authorizeURL(state, githubSetupURL(r)), http.StatusFound)
}

// bindInstallations records the installations this person can see, files their repositories, and
// says what happened. One named id binds only that one — the install-time redirect knows which
// was just created. None named binds everything they can see that is not already spoken for,
// which is what the authorize hop has to do: it is told who the person is, not what they just
// clicked.
func (b *Bot) bindInstallations(ctx context.Context, orgID int64, mine []ghInstallation, want int64, by string) (bound, repos int, elsewhere []string) {
	for _, ins := range mine {
		if want > 0 && ins.ID != want {
			continue
		}
		perms, _ := json.Marshal(ins.Permissions)
		g := &GitHubInstall{ID: ins.ID, OrgID: orgID, AccountLogin: ins.Account.Login, AccountID: ins.Account.ID,
			AccountType: ins.Account.Type, RepoSelection: ins.RepositorySelection, Permissions: string(perms),
			AppSlug: ins.AppSlug, InstalledBy: by, SuspendedAt: ins.SuspendedAt}
		if err := b.store.SaveGitHubInstall(ctx, g); err != nil {
			// Somebody else's, or a write that failed. Neither is this person's to override, and
			// the first is the tenancy guard doing its job rather than a fault.
			if errors.Is(err, ErrInstallOwnedElsewhere) {
				elsewhere = append(elsewhere, ins.Account.Login)
			} else {
				slog.Warn("saving github install", "installation", ins.ID, "err", err)
			}
			continue
		}
		bound++
		n, err := b.connectInstalledRepos(ctx, orgID, ins, by)
		if err != nil {
			// The installation is saved and usable; only the shortcut failed. The picker can
			// still add its repositories, so this is not the install failing.
			slog.Warn("connecting installed repositories", "installation", ins.ID, "err", err)
		}
		repos += n
	}
	return bound, repos, elsewhere
}
