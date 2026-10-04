package app

import (
	"bytes"
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
	"slices"
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
	// Not behind requireAdmin, which would demand a session and a CSRF token and so refuse every
	// delivery: the authority is the signature over the body (github_webhook.go), and auditedWrites
	// does not wrap it, so each effect the dispatcher has writes its own audit row. Registered
	// whether or not a secret is set, so a deployment without one answers GitHub with a 503 that
	// names the reason in its logs rather than a 404 that looks like a wrong URL.
	if b.ghHook != nil {
		b.ghHook.badSigs.shareAcross(b.store)
	}
	mux.HandleFunc("POST /github/webhook", b.handleGitHubWebhook)
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
	// just clicked. Zero means "bind everything they can see and may claim" rather than "nothing
	// named".
	id, _ := strconv.ParseInt(strings.TrimSpace(q.Get("installation_id")), 10, 64)
	if id < 0 {
		id = 0
	}
	// The installation id in this URL is a claim, not evidence. Exchange GitHub's code for the
	// installer's own token and take the installations from what that token can see — so an id
	// typed in by somebody who cannot see it is not in the list, and is refused. Seeing it is not
	// enough either: mayClaim asks whether they control it.
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
	mine, installer, err := b.ghApp.userInstallations(ctx, code, redirectURI)
	if err != nil {
		slog.Warn("github installer check", "installation", id, "err", err)
		githubBack(w, r, "github_error", "Could not confirm the installation with GitHub. Start the install again.")
		return
	}
	claim := func(ctx context.Context, ins ghInstallation) error { return b.ghApp.mayClaim(ctx, installer, ins) }
	res := b.bindInstallations(ctx, orgID, mine, id, installedBy, claim)
	saved, n := res.bound, res.repos
	if saved == 0 {
		switch {
		case len(res.elsewhere) > 0:
			githubBack(w, r, "github_error", strings.Join(res.elsewhere, ", ")+" is already connected to another attest_tag "+
				"organisation. Ask whoever set it up to invite you, or uninstall it at GitHub first.")
			return
		case len(res.tooLarge) > 0:
			githubBack(w, r, "github_error", strings.Join(res.tooLarge, ", ")+" covers more repositories than can be checked "+
				"here. Choose the repositories it needs at GitHub, under the app's Configure page, and connect it again.")
			return
		case len(res.refused) > 0:
			githubBack(w, r, "github_error", "Only an owner of "+strings.Join(res.refused, ", ")+", or someone who administers "+
				"every repository the app was given there, can connect it to attest_tag. Ask one of them to connect it, "+
				"or to invite you once they have.")
			return
		case len(res.unchecked) > 0:
			githubBack(w, r, "github_error", "Could not confirm with GitHub that you may connect "+
				strings.Join(res.unchecked, ", ")+". Try again in a minute.")
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
	// Code review needs more than installing does — the webhook secret above all, without which
	// GitHub's deliveries cannot be verified and no pull request is ever heard about — so it says
	// what it is missing on its own line, and install_missing keeps meaning "cannot install". Where
	// code review is off (CODE_REVIEW=off) it needs nothing, and nobody is asked to set up, or to grant
	// write access at GitHub for, a feature this deployment does not have; code_review says so, for
	// the console to word the webhook's facts without it.
	reviewOff := b.cfg.codeReviewOff()
	out["code_review"] = !reviewOff
	out["review_missing"] = []string{}
	if !reviewOff {
		out["review_missing"] = b.reviewMissing()
	}
	// Where GitHub should send deliveries, and whether the secret is set, are this deployment's
	// own facts: on a single-tenant self-host the person reading this is the one who has to paste
	// the URL into the App's settings. A hosted tenant never set up the App and never will, and
	// is not told where the operator's webhook lives.
	if b.cfg.SignupMode != SignupOpen {
		out["webhook_url"] = b.baseURL(r) + "/github/webhook"
		out["webhook_secret_set"] = b.ghHook.configured()
	}
	list, err := b.store.GitHubInstalls(r.Context(), orgOf(r))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	views := make([]githubInstallView, 0, len(list))
	for _, g := range list {
		g.AppSlug = b.ghApp.slugOrEmpty()
		missing, known := []string{}, false
		if !reviewOff {
			missing, known = reviewMissingPermissions(g.Permissions)
		}
		views = append(views, githubInstallView{GitHubInstall: g, MissingPermissions: missing, PermissionsKnown: known})
	}
	out["installations"] = views
	writeJSON(w, 200, out)
}

// githubInstallView is an installation as the console reads it: the stored row, and what code
// review would still need the installation's owner to accept at GitHub.
type githubInstallView struct {
	*GitHubInstall
	// MissingPermissions are the permissions code review's tokens ask for that this installation
	// has not granted, as "pull_requests:write"; empty when it has them all, or when what it
	// granted was never recorded (PermissionsKnown false), which is not the same thing.
	MissingPermissions []string `json:"missing_permissions"`
	PermissionsKnown   bool     `json:"permissions_known"`
}

// reviewMissingPermissions compares what an installation granted — its permissions as GitHub
// sent them at install, JSON — with what code review's two token purposes ask for
// (githubPurposePermissions), so the console can say "accept the new permissions" before the
// first review fails on a 403 instead of after. A write grant covers a read. Nothing recorded is
// unknown, not "missing everything": installations bound before permissions were stored have none.
func reviewMissingPermissions(granted string) (missing []string, known bool) {
	missing = []string{}
	var have map[string]string
	if strings.TrimSpace(granted) == "" || json.Unmarshal([]byte(granted), &have) != nil || len(have) == 0 {
		return missing, false
	}
	rank := map[string]int{"read": 1, "write": 2, "admin": 3}
	need := map[string]string{}
	for _, purpose := range []string{githubPurposeReviewRead, githubPurposeReviewPost} {
		for k, v := range githubPurposePermissions[purpose] {
			if rank[v] > rank[need[k]] {
				need[k] = v
			}
		}
	}
	for k, v := range need {
		if rank[have[k]] < rank[v] {
			missing = append(missing, k+":"+v)
		}
	}
	slices.Sort(missing)
	return missing, true
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
// answer is refused. Seeing one is not yet the right to claim it (mayClaim), so the person comes
// back with the list: their token is kept for the rest of this one request, to ask GitHub what
// they may do, and thrown away with it — proof of identity, never a credential we keep.
func (g *githubApp) userInstallations(ctx context.Context, code, redirectURI string) ([]ghInstallation, *ghInstaller, error) {
	form := url.Values{"client_id": {g.clientID}, "client_secret": {g.clientSecret}, "code": {code}}
	// Repeat whatever the authorize step sent, and only that. GitHub matches the two, so passing
	// one here when the install-time flow sent none is itself an error.
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://github.com/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	json.Unmarshal(raw, &tok)
	if tok.AccessToken == "" {
		return nil, nil, fmt.Errorf("GitHub would not confirm who installed the app: %s", truncate(tok.Error, 160))
	}
	u := &ghInstaller{token: tok.AccessToken}

	// One page is enough: nobody reaches this having installed the app on more than a handful of
	// accounts in the same click, and the id we are checking for is from the click that just
	// happened.
	var out struct {
		Installations []ghInstallation `json:"installations"`
	}
	if err := ghGet(ctx, u.token, "/user/installations?per_page=100", &out); err != nil {
		return nil, nil, fmt.Errorf("listing the installer's installations: %w", err)
	}
	return out.Installations, u, nil
}

// ghInstaller is the person who came back from GitHub, as their own user token shows them. It
// lives for one request.
type ghInstaller struct {
	token string
}

// ghGet is one read from GitHub's API with the token given — the installer's, or an installation
// token minted for the question — decoded into into. oauthHTTPClient, like every exchange in the
// install flow: this is the platform's own plumbing, not a tenant's call.
func ghGet(ctx context.Context, token, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GitHub returned %d", resp.StatusCode)
	}
	return json.Unmarshal(body, into)
}

// errMayNotClaim is mayClaim's "no": GitHub answered, and what it said does not make this person
// someone who may attach the installation here. Any other error is GitHub not answering.
var errMayNotClaim = errors.New("may not claim this installation")

// claimProofPages bounds how many pages of an organisation installation's repositories mayClaim
// reads: 1,000 repositories. Past that the installation is refused rather than half-checked, and
// the person is told to narrow its repository selection at GitHub.
const claimProofPages = 10

// errClaimTooLarge is an organisation installation with more repositories than mayClaim reads.
var errClaimTooLarge = errors.New("installation has too many repositories to check")

// mayClaim decides whether this person may attach installation ins to an attest_tag organisation.
//
// Seeing an installation is not owning it. For a user token GitHub lists every installation on a
// repository the person can read, so an organisation's members and its outside collaborators all
// see it. On visibility alone any of them could attach their employer's installation, while
// nobody has claimed it yet, to an organisation of their own: first claim wins, the installation
// tokens follow it, and so does every pull request code review is sent.
//
// Who may claim it is whoever controls what it covers. For a personal account, that is the account
// itself. For an organisation, it is someone with admin on every repository the installation
// covers: an owner, or the repository admin who chose them. The organisation case uses the person's
// own view of the installation's repositories, which carries their permission on each, and checks
// it against the installation's own count, so a repository hidden from them counts against them. An
// installation with no repositories proves nothing and is refused until it has some.
func (g *githubApp) mayClaim(ctx context.Context, u *ghInstaller, ins ghInstallation) error {
	if strings.EqualFold(ins.Account.Type, "User") {
		var me struct {
			ID int64 `json:"id"`
		}
		if err := ghGet(ctx, u.token, "/user", &me); err != nil {
			return fmt.Errorf("asking GitHub who the installer is: %w", err)
		}
		if me.ID == 0 || me.ID != ins.Account.ID {
			return errMayNotClaim
		}
		return nil
	}
	total, err := g.installationRepoCount(ctx, ins.ID)
	if err != nil {
		return err
	}
	if total == 0 {
		return errMayNotClaim
	}
	seen := 0
	for page := 1; seen < total; page++ {
		if page > claimProofPages {
			return errClaimTooLarge
		}
		var body struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				Permissions struct {
					Admin bool `json:"admin"`
				} `json:"permissions"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/user/installations/%d/repositories?per_page=100&page=%d", ins.ID, page)
		if err := ghGet(ctx, u.token, path, &body); err != nil {
			return fmt.Errorf("reading the installer's repositories: %w", err)
		}
		if body.TotalCount != total {
			return errMayNotClaim
		}
		if len(body.Repositories) == 0 {
			break
		}
		for _, r := range body.Repositories {
			if !r.Permissions.Admin {
				return errMayNotClaim
			}
		}
		seen += len(body.Repositories)
	}
	if seen < total {
		return errMayNotClaim
	}
	return nil
}

// installationRepoCount is how many repositories installation id covers, as the installation
// itself sees them. The token minted for it asks for metadata only and is not kept.
func (g *githubApp) installationRepoCount(ctx context.Context, id int64) (int, error) {
	jwt, err := g.jwt(time.Now())
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{"permissions": map[string]string{"metadata": "read"}})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", id), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tok struct {
		Token string `json:"token"`
	}
	json.Unmarshal(raw, &tok)
	if resp.StatusCode >= 400 || tok.Token == "" {
		return 0, fmt.Errorf("GitHub would not issue a token for installation %d (%d)", id, resp.StatusCode)
	}
	var list struct {
		TotalCount int `json:"total_count"`
	}
	if err := ghGet(ctx, tok.Token, "/installation/repositories?per_page=1", &list); err != nil {
		return 0, fmt.Errorf("counting installation %d's repositories: %w", id, err)
	}
	return list.TotalCount, nil
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

// bindResult is what bindInstallations did with the installations it was shown: how many it
// bound and how many repositories that filed, and, by account login, the ones it did not bind
// and why.
type bindResult struct {
	bound, repos int
	elsewhere    []string // claimed by another attest_tag organisation
	refused      []string // GitHub answered, and this person may not claim it (mayClaim)
	tooLarge     []string // too many repositories to check
	unchecked    []string // GitHub could not be asked
}

// bindInstallations records the installations this person can see and may claim, files their
// repositories, and says what happened. One named id binds only that one — the install-time
// redirect knows which was just created. None named binds everything they can see that is not
// already spoken for and that claim allows, which is what the authorize hop has to do: it is told
// who the person is, not what they just clicked.
//
// claim is asked before an installation is attached to this organisation for the first time, or
// again after it was disconnected here. One this organisation already holds is refreshed without
// it: re-reading its account and permissions grants nothing new.
func (b *Bot) bindInstallations(ctx context.Context, orgID int64, mine []ghInstallation, want int64, by string,
	claim func(context.Context, ghInstallation) error) bindResult {
	var res bindResult
	for _, ins := range mine {
		if want > 0 && ins.ID != want {
			continue
		}
		have, err := b.store.GitHubInstall(ctx, ins.ID)
		if err != nil {
			slog.Warn("reading github install", "installation", ins.ID, "err", err)
			res.unchecked = append(res.unchecked, ins.Account.Login)
			continue
		}
		if have != nil && have.OrgID != orgID {
			// Somebody else's: the tenancy guard in SaveGitHubInstall would say the same.
			res.elsewhere = append(res.elsewhere, ins.Account.Login)
			continue
		}
		if have == nil || have.Status == "revoked" {
			if err := claim(ctx, ins); err != nil {
				switch {
				case errors.Is(err, errMayNotClaim):
					slog.Warn("github install refused: the installer does not control it", "installation", ins.ID,
						"account", ins.Account.Login, "org", orgID)
					res.refused = append(res.refused, ins.Account.Login)
				case errors.Is(err, errClaimTooLarge):
					res.tooLarge = append(res.tooLarge, ins.Account.Login)
				default:
					slog.Warn("github install claim check", "installation", ins.ID, "err", err)
					res.unchecked = append(res.unchecked, ins.Account.Login)
				}
				continue
			}
		}
		perms, _ := json.Marshal(ins.Permissions)
		g := &GitHubInstall{ID: ins.ID, OrgID: orgID, AccountLogin: ins.Account.Login, AccountID: ins.Account.ID,
			AccountType: ins.Account.Type, RepoSelection: ins.RepositorySelection, Permissions: string(perms),
			AppSlug: ins.AppSlug, InstalledBy: by, SuspendedAt: ins.SuspendedAt}
		if err := b.store.SaveGitHubInstall(ctx, g); err != nil {
			// Claimed by another organisation between the read above and this write, or a write
			// that failed. Neither is this person's to override, and the first is the tenancy guard
			// doing its job rather than a fault.
			if errors.Is(err, ErrInstallOwnedElsewhere) {
				res.elsewhere = append(res.elsewhere, ins.Account.Login)
			} else {
				slog.Warn("saving github install", "installation", ins.ID, "err", err)
			}
			continue
		}
		res.bound++
		n, err := b.connectInstalledRepos(ctx, orgID, ins, by)
		if err != nil {
			// The installation is saved and usable; only the shortcut failed. The picker can
			// still add its repositories, so this is not the install failing.
			slog.Warn("connecting installed repositories", "installation", ins.ID, "err", err)
		}
		res.repos += n
	}
	return res
}
