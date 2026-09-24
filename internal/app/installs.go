package app

import (
	"context"
	"crypto/hmac"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// Connecting a Slack workspace. One account ("Workspace" in the console) holds several
// connected Slack workspaces ("Teams"), each installed through Slack's OAuth v2 flow:
//
//	/slack/install          an admin presses "Add to Slack"; we mint a state row and send them to Slack
//	/slack/oauth/callback   Slack sends them back with a code; we exchange it for that workspace's bot token
//
// Both are served by the console's own HTTP server, which already exists and already speaks
// HTTPS behind Cloud Run. Slack events and interactions use separate signed HTTP
// endpoints on that same server; OAuth installs still store one bot token per workspace.
//
// The bot token is sealed with the same AES-GCM sealer as connection credentials and lives in
// the teams table. SLACK_BOT_TOKEN plays no part: tokens come from installs, not the env.

// botScopes is what the "Add to Slack" consent screen asks for. It has to stay in step with
// the manifest in the README; keeping it in code means the authorize URL cannot silently
// drift away from what the app is actually configured to grant.
var botScopes = []string{
	"app_mentions:read", "assistant:write", "chat:write", "chat:write.customize",
	"channels:history", "groups:history", "im:history", "mpim:history",
	"channels:read", "groups:read", "im:read", "im:write",
	// channels:leave and groups:leave are deliberately NOT asked for. Leaving is how a channel
	// is removed from the console, and the scopes exist — but Slack does not offer them in the
	// OAuth scope picker for this app, so requesting them fails the whole install with
	// "Invalid permissions requested" and nothing connects at all.
	//
	// Nothing else needs to change for that: removing a channel already degrades on its own
	// when the scope is absent. The console explains that the workspace has to be reconnected
	// rather than offering a button that fails, and scope_leave_test.go pins that behaviour.
	// A workspace that granted the scope under an older install keeps it and keeps working.
	"users:read", "users:read.email",
	"pins:read", "files:read", "files:write", "reactions:write",
	"search:read.public",
}

const installStateTTL = 15 * time.Minute

// installStateCookie rides alongside the oauth_states row. The row says which organisation an
// install is for; the cookie says which browser started it. Without the cookie the state token
// was the whole authority, and a state minted in one organisation could be handed to a
// workspace admin elsewhere — whose consent then sealed their workspace's token into the
// wrong tenant.
const installStateCookie = "attest_install_state"

// installIntentCookie remembers that a signed-out browser was on its way to connect a workspace —
// from the site's Add to Slack button, or from Slack's own Marketplace listing — so that the
// sign-in or sign-up it is sent to finishes with the install, not with an Overview of zeroes they
// then have to find their way out of. A flag, never a destination: where it leads is fixed in
// code, so no address bar can turn it into an open redirect.
const (
	installIntentCookie = "attest_install_intent"
	installIntentTTL    = time.Hour
)

// oauthExchange trades Slack's code for a bot token. A variable so tests can stand in for Slack.
var oauthExchange = func(ctx context.Context, clientID, clientSecret, code, redirectURI string) (*slack.OAuthV2Response, error) {
	return slack.GetOAuthV2ResponseContext(ctx, &http.Client{Timeout: 20 * time.Second}, clientID, clientSecret, code, redirectURI)
}

// slackUserIsAdmin asks the workspace, on its own fresh token, whether the person who pressed
// Allow is one of its admins or owners. A variable so tests can stand in for Slack.
var slackUserIsAdmin = func(ctx context.Context, token, userID string) (bool, error) {
	u, err := slack.New(token).GetUserInfoContext(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.IsAdmin || u.IsOwner || u.IsPrimaryOwner, nil
}

// slackRevokeToken gives a bot token back to Slack, best effort. A variable so tests can stand in.
var slackRevokeToken = func(ctx context.Context, token string) {
	if _, err := slack.New(token).SendAuthRevokeContext(ctx, ""); err != nil {
		slog.Warn("could not revoke a refused install's token", "err", err)
	}
}

func installConfigured() bool {
	return os.Getenv("SLACK_CLIENT_ID") != "" && os.Getenv("SLACK_CLIENT_SECRET") != ""
}

// installCallbackURL is the redirect URL that must be registered on the Slack app. Slack
// requires https, so a plain 127.0.0.1 origin will not work — use a tunnel for local tests.
func (b *Bot) installCallbackURL(r *http.Request) string {
	return b.baseURL(r) + "/slack/oauth/callback"
}

func (b *Bot) installRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /slack/install", b.handleInstall)
	mux.HandleFunc("GET /slack/oauth/callback", b.handleInstallCallback)
	b.teamsRoutes(mux)
}

// installFailed sends the browser back to the console page the install came from — the
// onboarding walk or Workspaces (installReturn picks) — which shows the reason inline.
func installFailed(w http.ResponseWriter, r *http.Request, back, msg string) {
	http.Redirect(w, r, back+"?install_error="+url.QueryEscape(msg), http.StatusFound)
}

// rememberInstall parks the intent and sends the browser to sign in. Signing up is offered there
// too, which is the ordinary path for somebody arriving from the site or the Marketplace.
func (b *Bot) rememberInstall(w http.ResponseWriter, r *http.Request, dest string) {
	if dest != "github" {
		dest = "slack"
	}
	http.SetCookie(w, &http.Cookie{Name: installIntentCookie, Value: dest, Path: "/", HttpOnly: true,
		MaxAge: int(installIntentTTL.Seconds()), SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	http.Redirect(w, r, "/admin/login/", http.StatusFound)
}

// installPending says whether this browser parked an install on its way to signing in.
func installPending(r *http.Request) bool {
	c, err := r.Cookie(installIntentCookie)
	return err == nil && c.Value != ""
}

// installNext spends the parked intent: "/slack/install" when there was one, "" otherwise. Called
// once a session exists, by whichever of sign-in, sign-up and the second factor finished it.
func (b *Bot) installNext(w http.ResponseWriter, r *http.Request) string {
	if !installPending(r) {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: installIntentCookie, Value: "", Path: "/", MaxAge: -1})
	// Still a flag rather than a destination: the cookie names which of two fixed paths, and
	// anything else — including the bare "1" older browsers are carrying — means Slack. No value
	// anybody can put in that cookie turns this into an open redirect.
	if c, err := r.Cookie(installIntentCookie); err == nil && c.Value == "github" {
		return "/github/install"
	}
	return "/slack/install"
}

// signInNext is where a finished sign-in sends the browser instead of the console: the join
// screen when an invitation is in hand, the install when one was interrupted, "" otherwise. An
// invitation wins — somebody joining an existing organisation is not the one who connects its
// workspace — and the install flag is spent either way, so it cannot resurface a week later.
func (b *Bot) signInNext(w http.ResponseWriter, r *http.Request) string {
	install := b.installNext(w, r)
	if next := inviteNext(r); next != "" {
		return next
	}
	return install
}

// handleInstall starts the flow. It is a browser navigation rather than a fetch, so it
// authenticates from the session cookie and redirects instead of returning JSON.
func (b *Bot) handleInstall(w http.ResponseWriter, r *http.Request) {
	u := b.authenticate(r)
	if u == nil {
		b.rememberInstall(w, r, "slack")
		return
	}
	back := b.installReturn(r.Context(), u.OrgID)
	// Connecting a workspace mints and stores a credential — the workspace's bot token — so
	// it is a connections permission, not a channel-settings one.
	if !u.Permissions[PermConnManage] {
		installFailed(w, r, back, "You do not have permission to connect a Slack workspace.")
		return
	}
	// requireAdmin cannot wrap this handler — it is a browser navigation that has to redirect
	// rather than return JSON, and it carries no CSRF token — so the two checks that outrank
	// every permission are repeated here. Without them, connecting a workspace was the one
	// authenticated action that skipped the organisation's sign-in policy and its two-factor
	// requirement.
	ctx := r.Context()
	if !b.settings.Get(ctx, u.OrgID).allowsVia(u.Via) {
		installFailed(w, r, back, "This organisation has changed which sign-in methods it accepts. Sign out and sign in again.")
		return
	}
	if b.twoFactorOwed(ctx, u) {
		installFailed(w, r, back, "This organisation requires two-factor authentication. Set it up before connecting a workspace.")
		return
	}
	if !installConfigured() {
		installFailed(w, r, back, "Connecting a workspace needs SLACK_CLIENT_ID and SLACK_CLIENT_SECRET, and "+
			b.installCallbackURL(r)+" registered as a redirect URL on the Slack app.")
		return
	}
	state, err := b.store.NewOAuthState(r.Context(), u.OrgID, u.UserID, installStateTTL)
	if err != nil {
		installFailed(w, r, back, "could not start the install: "+err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: installStateCookie, Value: state, Path: "/slack/oauth/", HttpOnly: true,
		MaxAge: int(installStateTTL.Seconds()), SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	q := url.Values{
		"client_id":    {os.Getenv("SLACK_CLIENT_ID")},
		"scope":        {strings.Join(botScopes, ",")},
		"redirect_uri": {b.installCallbackURL(r)},
		"state":        {state},
	}
	http.Redirect(w, r, "https://slack.com/oauth/v2/authorize?"+q.Encode(), http.StatusFound)
}

func (b *Bot) handleInstallCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	// The browser that started the install is the one that may finish it: the cookie set by
	// handleInstall has to name the same state Slack sent back. It is cleared either way.
	c, err := r.Cookie(installStateCookie)
	http.SetCookie(w, &http.Cookie{Name: installStateCookie, Value: "", Path: "/slack/oauth/", MaxAge: -1})
	bound := state != "" && err == nil && c.Value != "" && hmac.Equal([]byte(c.Value), []byte(state))
	// Slack sends the browser back with a top-level navigation, which carries the session cookie.
	u := b.authenticate(r)
	if !bound {
		if state == "" && q.Get("error") != "" {
			// Cancelled on a consent screen we did not open. Nothing to restart.
			if u == nil {
				http.Redirect(w, r, "/admin/login/", http.StatusFound)
				return
			}
			installFailed(w, r, b.installReturn(r.Context(), u.OrgID), "The install was cancelled ("+q.Get("error")+").")
			return
		}
		if u == nil {
			// Whatever brought them here, the way to finish is to sign in and start from
			// /slack/install; the intent survives the sign-in so they do not have to find it.
			b.rememberInstall(w, r, "slack")
			return
		}
		if state == "" {
			// No state at all is Slack starting the install on its own: the Add to Slack button on
			// the Marketplace listing goes straight to the consent screen and comes back here with
			// a code and nothing binding it to an organisation. That code is never exchanged — a
			// bare code in a link is exactly how a stranger's workspace would be planted in the
			// tenant of whoever opens it. What is safe is to start again from /slack/install,
			// which mints the state, and shows them the consent screen a second time.
			http.Redirect(w, r, "/slack/install", http.StatusFound)
			return
		}
		installFailed(w, r, b.installReturn(r.Context(), u.OrgID), "This install was started in a different browser, or the link has expired. Press Add to Slack again.")
		return
	}
	// And the person finishing it is signed in to the organisation it was started for.
	if u == nil {
		// The session ran out between Add to Slack and Allow. The state row is left to expire.
		b.rememberInstall(w, r, "slack")
		return
	}
	installedBy, orgID, err := b.store.TakeOAuthState(r.Context(), q.Get("state"))
	if err != nil {
		installFailed(w, r, b.installReturn(r.Context(), u.OrgID), err.Error())
		return
	}
	// Where this ends up, decided before the install lands: the first workspace an organisation
	// connects returns to the onboarding walk, which then asks for the channel invite, and every
	// one after it returns to the Workspaces page it was started from.
	back := b.installReturn(r.Context(), orgID)
	if u.OrgID != orgID || !u.Permissions[PermConnManage] {
		// This session's organisation, not the state's: `back` was chosen for the organisation the
		// install was started for, which by this branch is not the one the browser is signed in to.
		installFailed(w, r, b.installReturn(r.Context(), u.OrgID), "This install was started for a different organisation, or you no longer have permission to connect a workspace.")
		return
	}
	if e := q.Get("error"); e != "" { // Cancel on the consent screen
		installFailed(w, r, back, "The install was cancelled ("+e+").")
		return
	}
	code := q.Get("code")
	if code == "" {
		installFailed(w, r, back, "Slack did not return an install code. Try again.")
		return
	}
	resp, err := oauthExchange(r.Context(), os.Getenv("SLACK_CLIENT_ID"), os.Getenv("SLACK_CLIENT_SECRET"), code, b.installCallbackURL(r))
	if err != nil {
		slog.Warn("slack install failed", "err", err)
		installFailed(w, r, back, "Slack refused the install: "+err.Error())
		return
	}
	if resp.Team.ID == "" {
		// An org-wide install has no single team. Supporting it would change the unit of
		// installation from a workspace to an enterprise, which this version does not model.
		installFailed(w, r, back, "Org-wide installs are not supported yet — install into a single workspace.")
		return
	}
	// Only a workspace admin or owner may bind their workspace to an organisation here. A
	// plain member could otherwise connect their employer's workspace to a tenant of their
	// own and read its channels through the bot. Fail closed: if Slack cannot say, refuse,
	// and give the token back so nothing is left granted.
	if admin, err := slackUserIsAdmin(r.Context(), resp.AccessToken, resp.AuthedUser.ID); err != nil || !admin {
		slackRevokeToken(r.Context(), resp.AccessToken)
		if err != nil {
			slog.Warn("could not confirm the installer is a workspace admin", "team", resp.Team.ID, "err", err)
			installFailed(w, r, back, "Slack could not confirm that you administer that workspace. Try again in a moment.")
			return
		}
		installFailed(w, r, back, "Only a Slack workspace admin or owner can connect a workspace.")
		return
	}
	if err := b.saveInstall(r.Context(), resp, installedBy, orgID); err != nil {
		// A workspace another organisation already holds is refused, not lost — and it is the
		// one refusal here that is somebody's ordinary mistake rather than a fault. It happens
		// when a second person from the same company signs up on their own instead of being
		// invited: they get an organisation of their own, and the setup walk points them at the
		// workspace their colleague already connected. Saying "could not be saved" would send
		// them looking for a broken deployment; what they need is the name of the way out.
		//
		// The token from this install is deliberately not revoked. Slack hands back a bot token
		// for an app already installed in that workspace, and giving it back could be giving
		// back the one the other organisation is using.
		if errors.Is(err, ErrTeamOwnedElsewhere) {
			slog.Warn("refused an install of a workspace another organisation holds", "team", resp.Team.ID, "org", orgID)
			installFailed(w, r, back, "That Slack workspace is already connected to another attest_tag organisation. "+
				"Ask whoever set it up to invite you to theirs — a workspace belongs to one organisation.")
			return
		}
		slog.Error("save install", "team", resp.Team.ID, "err", err)
		installFailed(w, r, back, "The install succeeded but could not be saved: "+err.Error())
		return
	}
	slog.Info("slack workspace connected", "team", resp.Team.ID, "name", resp.Team.Name, "by", installedBy)
	// Not under requireAdmin — Slack sends the browser here — so the actor is named by hand:
	// the session that finished the install, and the Slack account that pressed Allow.
	b.audit(r, "workspace.connected", AuditEvent{OrgID: orgID, ActorID: u.ID, ActorPublic: u.PublicID, ActorEmail: u.Email,
		ActorName: u.Name, ActorSlack: installedBy, TeamID: resp.Team.ID, TargetKind: "workspace", TargetID: resp.Team.ID,
		TargetName: resp.Team.Name, Details: auditDetails(map[string]any{"scopes": resp.Scope})})
	http.Redirect(w, r, back+"?team="+url.QueryEscape(resp.Team.ID), http.StatusFound)
}

// saveInstall seals the token, records the workspace, and gives it its scope row so the
// console has somewhere to hang instructions and bundles before anyone opens the page.
func (b *Bot) saveInstall(ctx context.Context, resp *slack.OAuthV2Response, installedBy string, orgID int64) error {
	enc, err := b.sealer.Seal([]byte(resp.AccessToken))
	if err != nil {
		return err
	}
	t := &Team{
		TeamID: resp.Team.ID, OrgID: orgID, Name: resp.Team.Name, EnterpriseID: resp.Enterprise.ID,
		BotUserID: resp.BotUserID, Scopes: resp.Scope, InstalledBy: installedBy,
	}
	// auth.test fills in the bot id and confirms the token works; team.info gives the name,
	// domain and icon the console rail shows. Neither is fatal: a workspace that installed
	// successfully should appear even if a follow-up read is rate limited.
	api := slack.New(resp.AccessToken)
	if auth, err := api.AuthTestContext(ctx); err == nil {
		t.BotID, t.BotUserID = auth.BotID, auth.UserID
		if t.Name == "" {
			t.Name = auth.Team
		}
	} else {
		slog.Warn("auth.test after install", "team", t.TeamID, "err", err)
	}
	if info, err := api.GetTeamInfoContext(ctx); err == nil {
		if info.Name != "" {
			t.Name = info.Name
		}
		t.Domain = info.Domain
		t.Icon, _ = info.Icon["image_132"].(string)
	}
	// What the granted scopes actually allow, per install: a workspace can grant a different
	// set from the one before it, and a reinstall can change it again.
	t.EmailScope = strings.Contains(resp.Scope, "users:read.email")
	t.DMScope = strings.Contains(resp.Scope, "im:write")
	if t.Name == "" {
		t.Name = t.TeamID
	}
	if err := b.store.SaveTeam(ctx, t, enc); err != nil {
		return err
	}
	if _, err := b.store.UpsertScope(ctx, t.OrgID, "team", t.TeamID, t.TeamID, t.Name); err != nil {
		return err
	}
	b.slacks.Evict(t.TeamID) // a reinstall means a new token
	b.changed(ctx, t.OrgID)
	go b.syncScopesFor(context.WithoutCancel(ctx), t.OrgID, t.TeamID)
	return nil
}

// disconnect stops a workspace: Slack revokes the token, the row stays so the console can
// still explain why that workspace went quiet, and the cached client is dropped.
func (b *Bot) disconnectTeam(ctx context.Context, teamID, reason string) error {
	orgID, _ := b.store.OrgOfTeam(ctx, teamID)
	if sl, err := b.slacks.For(ctx, teamID); err == nil {
		if api, err := sl.slackAPI(); err == nil {
			if _, err := api.SendAuthRevokeContext(ctx, ""); err != nil {
				slog.Warn("auth.revoke", "team", teamID, "err", err) // best effort: the row still goes
			}
		}
	}
	b.slacks.Evict(teamID)
	if err := b.store.RevokeTeam(ctx, teamID, reason); err != nil {
		return err
	}
	b.changed(ctx, orgID)
	return nil
}

// ensureAccountScope creates the top link of the hierarchy — the Workspace itself — so the
// console has somewhere to hang account-wide instructions and bundles before any Slack
// workspace is connected.
func (b *Bot) ensureAccountScope(ctx context.Context, orgID int64) {
	name := "Organisation"
	if o, _ := b.store.Org(ctx, orgID); o != nil {
		name = o.Name
	}
	if _, err := b.store.UpsertScope(ctx, orgID, "workspace", "", "", name); err != nil {
		slog.Error("account scope", "err", err)
	}
}

// teamName, channelName and userName are the console's naming helpers. Each takes the team a
// row came from; when that workspace is gone the raw id is returned, because showing another
// workspace's name for it would be worse than showing none.
func (b *Bot) teamName(ctx context.Context, teamID string) string {
	if teamID == "" {
		return ""
	}
	if t, _ := b.store.Team(ctx, teamID); t != nil && t.Name != "" {
		return t.Name
	}
	return teamID
}

// nameTeams fills in the TeamName that Scope's JSON advertises. The store cannot: names live in
// the teams table, not on the scope row. Both scope listings used to ship the field empty, so an
// account with several connected workspaces could only tell its scopes apart by raw team id.
func (b *Bot) nameTeams(ctx context.Context, sc []*Scope) []*Scope {
	for _, s := range sc {
		s.TeamName = b.teamName(ctx, s.TeamID)
	}
	return sc
}

func (b *Bot) channelName(ctx context.Context, teamID, channel string) string {
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		return channel
	}
	return sl.ChannelName(ctx, channel)
}

// conversationNamer names conversations the way the console shows them, for one response's rows:
// the overview's month and the Activity lists. A channel reads as its scope, from the database,
// because a page of rows would otherwise ask Slack about each channel on every load, which is what
// the scopes table is there to save. The rest is named for what it is, because the table would
// otherwise have only the id to show, and a Teams chat's runs past a hundred characters.
type conversationNamer struct {
	b      *Bot
	orgID  int64
	scopes map[string]string // "team|channel" → the channel's scope name
	people map[string]string // "team|channel" → somebody who spoke there; read at the first DM
}

func (b *Bot) conversationNamer(ctx context.Context, orgID int64) *conversationNamer {
	return &conversationNamer{b: b, orgID: orgID, scopes: b.store.ChannelNames(ctx, orgID)}
}

// name is what a conversation is called, or "" for none.
func (n *conversationNamer) name(ctx context.Context, teamID, channel string) string {
	key := teamID + "|" + channel
	switch {
	case channel == "":
		return ""
	case channel == assistantChannel:
		return "Console assistant"
	case isDirectConversation(channel):
		// Ahead of the scopes, because a Slack DM can have a scope row of its own, named "#DM".
		if n.people == nil {
			n.people = n.b.store.ConversationSpeakers(ctx, n.orgID)
		}
		if user := n.people[key]; user != "" {
			if who := n.b.userName(ctx, teamID, user); who != user {
				return "DM with " + who
			}
		}
		return "Direct message"
	case isTeamsGroupChat(channel):
		return "Group chat"
	}
	if name := n.scopes[key]; name != "" {
		return name
	}
	if name := n.b.channelName(ctx, teamID, channel); name != channel {
		return channelLabel(name)
	}
	return channel
}

// usageName is name for what was spent. Only the read-every-message check is filed without a
// channel: it runs before there is a turn, and so before there is anywhere to put one.
func (n *conversationNamer) usageName(ctx context.Context, teamID, channel string) string {
	if channel == "" {
		return "Message screening"
	}
	return n.name(ctx, teamID, channel)
}

// channelPrivate reports whether a channel is private, for the one path that has to write a
// scope without a list from Slack to read it from. Cached alongside the rest of the
// conversation info, so it costs nothing on the paths that already asked.
func (b *Bot) channelPrivate(ctx context.Context, teamID, channel string) bool {
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		return false
	}
	return sl.IsPrivateConversation(ctx, channel)
}

// userName resolves a Slack user id to a display name, in the workspace the id belongs to.
//
// It used to fall back to any connected workspace when that one could not be reached. A Slack
// user id only means something inside its own workspace, so the fallback asked another tenant
// about a stranger's id — answering with the wrong person's name whenever the id happened to
// exist there too. An unresolved id is shown raw instead; that is a cosmetic loss, and the
// alternative was a cross-tenant one.
func (b *Bot) userName(ctx context.Context, teamID, user string) string {
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil || sl == nil {
		return user
	}
	return sl.UserName(ctx, user)
}

// botName is the name a workspace shows for the bot user itself. userName cannot answer it:
// it deliberately says "assistant" for the bot's own id, which reads correctly in a transcript
// and not at all in a banner naming the installed app.
func (b *Bot) botName(ctx context.Context, teamID, botUser string) string {
	if botUser == "" {
		return ""
	}
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil || sl == nil {
		return botUser
	}
	return sl.DisplayName(ctx, botUser)
}

// connectedTeams is the set a console sign-in is checked against.
func (b *Bot) connectedTeams(ctx context.Context) []string {
	teams, _ := b.store.ActiveTeams(ctx)
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		out = append(out, t.TeamID)
	}
	return out
}

// ---- console API for the Workspaces page's rail ----

func (b *Bot) teamsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/teams", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		teams, err := b.store.Teams(r.Context(), orgOf(r))
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		ctx := r.Context()
		out := make([]map[string]any, 0, len(teams))
		for _, t := range teams {
			m := map[string]any{
				"team_id": t.TeamID, "name": t.Name, "domain": t.Domain, "icon": t.Icon,
				"bot_user_id": t.BotUserID, "status": t.Status, "installed_by": t.InstalledBy,
				"installed_at": t.InstalledAt, "revoked_at": t.RevokedAt, "last_error": t.LastError,
				"email_scope": t.EmailScope, "dm_scope": t.DMScope, "scopes": t.Scopes,
				// A workspace that granted fewer scopes than the app asks for works partially;
				// saying so here is what turns "the bot went quiet" into one clear action.
				"needs_reinstall": t.Status == "active" && (!t.EmailScope || !t.DMScope),
			}
			// The banner names the person who connected the workspace and the bot user rather
			// than showing raw U… ids. Resolved here, not stored at install time: a name kept
			// on the teams row would freeze whatever it was that day, and would stay empty for
			// every workspace connected before the column existed. Both go through the
			// per-workspace name cache, and both fall back to the id when Slack cannot answer.
			if t.InstalledBy != "" {
				m["installed_by_name"] = b.userName(ctx, t.TeamID, t.InstalledBy)
			}
			if t.BotUserID != "" {
				m["bot_user_name"] = b.botName(ctx, t.TeamID, t.BotUserID)
			}
			out = append(out, m)
		}
		writeJSON(w, 200, map[string]any{"teams": out, "install_configured": installConfigured(), "install_url": "/slack/install"})
	}))
	// Asking Slack again what channels the bot is in. The rail syncs itself on a page load, but
	// only once a minute per workspace — long enough that somebody who has just invited the bot
	// to a channel is left reloading and wondering. This is that wondering, made into a button.
	mux.HandleFunc("POST /api/teams/{team}/sync", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		team := r.PathValue("team")
		t, _ := b.store.Team(r.Context(), team)
		// Another organisation's workspace answers the same as one that does not exist.
		if t == nil || t.OrgID != orgOf(r) {
			writeJSON(w, 404, map[string]any{"error": "That workspace is not connected."})
			return
		}
		if t.Status != "active" {
			writeJSON(w, 409, map[string]any{"error": "That workspace is disconnected, so Slack has nothing to tell us about it."})
			return
		}
		before, _ := b.store.CountChannelScopes(r.Context(), orgOf(r), team)
		// Five seconds, not none: the button is next to the workspace it refreshes, so it will be
		// pressed twice, and GetConversationsForUser is rate-limited per workspace. It also keeps
		// a refresh pressed straight after a channel was removed from undoing that removal, which
		// is what the leave stamps the same throttle for.
		b.syncScopesWithin(r.Context(), orgOf(r), team, 5*time.Second)
		after, err := b.store.CountChannelScopes(r.Context(), orgOf(r), team)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "channels": after, "added": max(after-before, 0)})
	}))
	// Disconnecting revokes the workspace's token: a credential operation, like connecting.
	mux.HandleFunc("POST /api/teams/{team}/disconnect", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		team := r.PathValue("team")
		// The workspace has to be this organisation's. Without the org check the team id in the
		// path is the whole authorisation, and any tenant could revoke another's install and its
		// token. A workspace someone else holds answers the same as one that does not exist, so
		// this cannot be used to discover who has which workspace.
		t, _ := b.store.Team(r.Context(), team)
		if t == nil || t.OrgID != orgOf(r) {
			writeJSON(w, 404, map[string]any{"error": "That workspace is not connected."})
			return
		}
		if err := b.disconnectTeam(r.Context(), team, "disconnected from the console"); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		b.audit(r, "workspace.disconnected", AuditEvent{TeamID: team, TargetKind: "workspace", TargetID: team, TargetName: t.Name})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// Removing a workspace for good — the token handed back and every row recorded for it swept
	// up. The same permission as Disconnect, which it does on the way past; team_delete.go has
	// the rest of the reasoning.
	mux.HandleFunc("POST /api/teams/{team}/delete", b.requirePerm(PermConnManage, b.handleDeleteTeam))
}

// orgIDs lists the organisations the background loops sweep: every one with a connected Slack
// workspace, since an organisation with none has nothing for them to do.
func (b *Bot) orgIDs(ctx context.Context) []int64 {
	teams, err := b.store.ActiveTeams(ctx)
	if err != nil {
		slog.Error("list organisations", "err", err)
		return nil
	}
	seen := map[int64]bool{}
	out := []int64{}
	for _, t := range teams {
		if !seen[t.OrgID] {
			seen[t.OrgID] = true
			out = append(out, t.OrgID)
		}
	}
	return out
}

// reindex re-runs ingest for one organisation and drops its cached chunks, so a document that
// was just uploaded or deleted is reflected in the next search rather than the next restart.
func (b *Bot) reindex(ctx context.Context, orgID int64) {
	if _, err := b.ix.IngestAndRecord(ctx, orgID, b.docs.For(orgID)); err != nil {
		slog.Warn("reindex", "org", orgID, "err", err)
	}
	b.ix.Forget(orgID)
}
