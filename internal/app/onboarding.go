package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The walk a new organisation takes before the console has anything in it: connect a Slack
// workspace, invite the bot to a channel, mention it there. Three steps, in that order, because
// each one is what makes the next possible — and because every page in the console is a listing
// of things that do not exist until they are done. Somebody dropped straight onto Overview after
// signing up sees eight zeroes and no way to read what to do about them.
//
// Nothing here is stored. Each step is derived from what actually happened — a live install, a
// channel scope, a recorded turn — so it cannot drift from the truth, and somebody who did the
// work from Slack, in another browser, or last month arrives finished rather than being asked
// to do it again. There is no "dismissed" flag to go stale and no counter to reset.

// The steps, in order. The name of the first unfinished one is what the console gates on.
const (
	stepInstall = "install" // no Slack workspace connected yet
	stepChannel = "channel" // connected, but the bot is in no channel
	stepMention = "mention" // in a channel, but nobody has spoken to it
	stepInvite  = "invite"  // it works, and nobody else has been asked in
	stepDone    = "done"
)

// askedInviteFor is how long one person's "ask them to invite me" holds. It is a message to
// somebody who did not ask for it, sent on the strength of a stranger pressing a button, so it
// is sent once and then not again for a day however many times the page is reloaded.
const askedInviteFor = 24 * time.Hour

// takenWorkspace is the dead end the walk exists to name: the Slack workspace this person signed
// in from is already connected to a different organisation, so Add to Slack would be refused.
// It is only ever set for the workspace they are themselves a member of, and it says nothing
// about the organisation that holds it — only that one does, and who in their own workspace put
// it there. Both are things they can already read in Slack's own app settings.
type takenWorkspace struct {
	Name string `json:"name"`
	// Who installed it, as their workspace names them. Empty when Slack cannot say.
	Installer string `json:"installer"`
	// Whether we can reach that person: the workspace granted im:write and we know who they are.
	CanAsk bool `json:"can_ask"`
	// Whether they have already been messaged for this person, so the button says so.
	Asked bool `json:"asked"`
}

// onboardingChannel is a channel the bot has been let into, for the "you're in" line.
type onboardingChannel struct {
	TeamID  string `json:"team_id"`
	SlackID string `json:"slack_id"`
	Name    string `json:"name"`
}

type onboardingTeam struct {
	TeamID string `json:"team_id"`
	Name   string `json:"name"`
}

// onboardingState is what the console needs to draw the walk and to decide whether to hold
// somebody on it.
type onboardingState struct {
	Step string `json:"step"`
	Done bool   `json:"done"`
	// Whether this person can do the step in front of them, and if not, what to say. A viewer
	// cannot connect a workspace; being held on a screen with a button they cannot press is
	// worse than being told to ask somebody.
	CanInstall        bool `json:"can_install"`
	InstallConfigured bool `json:"install_configured"`
	// Where "Add to Slack" points. A real navigation, not a fetch: it leaves for Slack.
	InstallURL string `json:"install_url"`
	// Whether a Microsoft Teams organisation can be connected here instead, and which platform
	// the first connected workspace is on — the steps after the first are said in its words.
	MSTeams  bool                `json:"msteams"`
	Platform string              `json:"platform,omitempty"`
	Teams    []onboardingTeam    `json:"teams"`
	Channels []onboardingChannel `json:"channels"`
	// What to type in Slack. The handle is resolved live, because it is whatever the workspace
	// that installed the app chose to call it, not what this deployment is named.
	BotHandle string `json:"bot_handle"`
	Turns     int    `json:"turns"`
	// Set when the first step cannot succeed however many times it is pressed, because the
	// workspace behind this person's Slack sign-in belongs to somebody else already.
	Taken *takenWorkspace `json:"taken,omitempty"`
	// The last step: people. Members counts this organisation, invited counts invitations sent
	// and not yet accepted, and CanInvite is false for a role that cannot send one — for whom
	// the step is not a step at all and is treated as done.
	Members   int  `json:"members"`
	Invited   int  `json:"invited"`
	CanInvite bool `json:"can_invite"`
	// Set when the address behind this account has not been confirmed yet. Not a step, and not
	// a gate on the install any more — but it is what stands between this person and a password
	// reset, and between them and inviting anyone, so the walk says so and carries the button
	// that fixes it.
	EmailUnverified bool `json:"email_unverified"`
}

// onboarding works out where an organisation stands. sync asks Slack for the channels the bot
// is in before answering: the console polls this while somebody is doing the invite in another
// window, and a sixty-second-old answer would leave them staring at a step they have finished.
func (b *Bot) onboarding(ctx context.Context, u *AdminUser, sync bool) onboardingState {
	st := onboardingState{
		Step:              stepInstall,
		CanInstall:        u.Permissions[PermConnManage],
		CanInvite:         u.Permissions[PermUsersManage],
		InstallConfigured: installConfigured(),
		InstallURL:        "/slack/install",
		MSTeams:           b.slacks != nil && b.slacks.msteams != nil,
		Teams:             []onboardingTeam{},
		Channels:          []onboardingChannel{},
	}
	// Somebody on this page has not been to Settings and may not be allowed to go there, so the
	// resend button belongs where they are — even though the install no longer waits for it.
	st.EmailUnverified = b.emailUnverified(ctx, u)
	teams, _ := b.store.Teams(ctx, u.OrgID)
	var bot *Team
	for _, t := range teams {
		if t.Status == "active" {
			st.Teams = append(st.Teams, onboardingTeam{TeamID: t.TeamID, Name: t.Name})
			if bot == nil {
				bot = t
			}
		}
	}
	if bot == nil {
		// Pressing Add to Slack is about to fail for a reason no error message can fix, so say
		// so before they press it. Only asked at this step: once a workspace is connected the
		// question is moot, and it costs a lookup.
		st.Taken = b.workspaceTaken(ctx, u)
		return st
	}
	st.Step = stepChannel
	st.Platform = nonEmpty(bot.Platform, platformSlack)
	if sync {
		// Its own window rather than the listing's minute: this is somebody watching the screen
		// for the invite they just typed. GetConversationsForUser is Tier-2, and the console
		// polls every few seconds, so the window still holds the request rate well under it.
		for _, t := range st.Teams {
			b.syncScopesWithin(ctx, u.OrgID, t.TeamID, 8*time.Second)
		}
	}
	// Capped: the walk names the channels it found, and past a handful that is a list nobody
	// reads. The step only turns on whether there is one at all.
	for _, sc := range b.store.ChannelScopes(ctx, u.OrgID, 10) {
		st.Channels = append(st.Channels, onboardingChannel{TeamID: sc.TeamID, SlackID: sc.SlackID, Name: sc.Name})
	}
	if len(st.Channels) > 0 {
		st.Step = stepMention
		st.Turns = b.store.TurnCount(ctx, u.OrgID)
		if st.Turns > 0 {
			st.Step = stepInvite
			st.Done = b.peopleIn(ctx, u, &st)
			if st.Done {
				st.Step = stepDone
			}
		}
	}
	// Last, and only for the two steps that ask you to type "@handle": resolving the name is a
	// users.info call the first time a process sees that bot, and a finished organisation asks
	// this endpoint on every console page load without ever printing the answer.
	if st.Step == stepChannel || st.Step == stepMention {
		if st.Platform == platformMSTeams {
			// A Teams app is mentioned by the name in its package, which is this deployment's.
			st.BotHandle = msteamsAppName
		} else {
			st.BotHandle = b.botName(ctx, bot.TeamID, bot.BotUserID)
		}
	}
	return st
}

// needsInstall is the one question the sign-in path asks: is this person about to land in a
// console with no workspace behind it, and are they the one who can fix that? If so the
// browser goes to Slack's consent screen instead of to an empty Overview.
func (b *Bot) needsInstall(ctx context.Context, u *AdminUser) bool {
	if u == nil || !u.Permissions[PermConnManage] || !installConfigured() {
		return false
	}
	if b.twoFactorOwed(ctx, u) || !b.settings.Get(ctx, u.OrgID).allowsVia(u.Via) {
		return false
	}
	return !b.hasActiveTeam(ctx, u.OrgID)
}

// hasActiveTeam is the whole of "is this organisation set up at all". A revoked row does not
// count: an organisation that disconnected its only workspace is back at the start of the walk,
// which is the only reading that leaves the console with something to show.
func (b *Bot) hasActiveTeam(ctx context.Context, orgID int64) bool {
	teams, err := b.store.Teams(ctx, orgID)
	if err != nil {
		return false
	}
	for _, t := range teams {
		if t.Status == "active" {
			return true
		}
	}
	return false
}

// installReturn is the console page an install comes back to, finished or refused: the walk
// while this organisation has no workspace, the Workspaces page once it has one.
func (b *Bot) installReturn(ctx context.Context, orgID int64) string {
	if b.hasActiveTeam(ctx, orgID) {
		return "/admin/workspaces/"
	}
	return "/admin/onboarding/"
}

func (b *Bot) onboardingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/onboarding", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		u := adminFromCtx(r.Context())
		writeJSON(w, 200, b.onboarding(r.Context(), u, r.URL.Query().Get("sync") == "1"))
	}))
	// Asking a stranger's colleague for an invitation. requireAdmin, not requirePerm: this is
	// the one thing somebody in an organisation of nobody, holding nothing, needs to be able
	// to do — and what it spends is one Slack message a day, not the organisation's access.
	mux.HandleFunc("POST /api/onboarding/ask-invite", b.requireAdmin(b.askInvite))
}

// peopleIn fills in the last step and says whether it is finished. Finished means somebody else
// is in this organisation, or has been asked — waiting for them to accept is not the founder's
// work any more. A role that cannot invite is not held on a step it cannot do.
func (b *Bot) peopleIn(ctx context.Context, u *AdminUser, st *onboardingState) bool {
	if !st.CanInvite {
		return true
	}
	members, err := b.store.MembersOf(ctx, u.OrgID)
	if err != nil {
		return true // not a reason to hold somebody on a step
	}
	st.Members = len(members)
	if st.Members > 1 {
		return true
	}
	invites, err := b.store.PendingInvitesFor(ctx, u.OrgID)
	if err != nil {
		return true
	}
	st.Invited = len(invites)
	return st.Invited > 0
}

// slackTeamOf is the Slack workspace this account signs in from, or "" if it does not sign in
// with Slack. The identity subject is the pair, so the workspace is the half before the colon.
func (b *Bot) slackTeamOf(ctx context.Context, userID int64) string {
	ids, err := b.store.Identities(ctx, userID)
	if err != nil {
		return ""
	}
	for _, i := range ids {
		if i.Provider == ProviderSlack {
			team, _, _ := strings.Cut(i.Subject, ":")
			return team
		}
	}
	return ""
}

// workspaceTaken reports the workspace this person signs in from being connected to a different
// organisation. It is the common ending of a second person from one company signing up on their
// own instead of being invited: they found an organisation of nobody, and the only workspace
// they could connect to it is the one their colleague connected last week.
func (b *Bot) workspaceTaken(ctx context.Context, u *AdminUser) *takenWorkspace {
	teamID := b.slackTeamOf(ctx, u.ID)
	if teamID == "" {
		return nil
	}
	t, err := b.store.Team(ctx, teamID)
	if err != nil || t == nil || t.Status != "active" || t.OrgID == u.OrgID {
		return nil
	}
	tw := &takenWorkspace{Name: nonEmpty(t.Name, teamID), Asked: b.askedInvite(u.ID, teamID, false)}
	if t.InstalledBy != "" {
		tw.Installer = b.userName(ctx, teamID, t.InstalledBy)
		// Reaching them needs a DM, which needs the scope that workspace granted at install.
		tw.CanAsk = t.DMScope
	}
	return tw
}

// askedInvite reads, and with set writes, the once-a-day record of having messaged a workspace's
// installer on somebody's behalf. In memory rather than in a table: the worst a restart can do is
// allow one more message a day later, and a table would outlive the question it answers.
func (b *Bot) askedInvite(userID int64, teamID string, set bool) bool {
	key := strconv.FormatInt(userID, 10) + ":" + teamID
	b.askedMu.Lock()
	defer b.askedMu.Unlock()
	if b.asked == nil {
		b.asked = map[string]time.Time{}
	}
	fresh := time.Since(b.asked[key]) < askedInviteFor
	if set && !fresh {
		b.asked[key] = time.Now()
	}
	return fresh
}

// askInvite messages the person who connected the workspace, asking them to invite the person
// pressing the button. It is sent as the bot in *their* organisation's install — the only token
// that can reach that workspace — and it names who is asking, because a message that does not is
// one they cannot act on.
func (b *Bot) askInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := adminFromCtx(ctx)
	taken := b.workspaceTaken(ctx, u)
	if taken == nil {
		writeJSON(w, 400, map[string]any{"error": "That workspace is not connected to another organisation."})
		return
	}
	if !taken.CanAsk {
		writeJSON(w, 400, map[string]any{"error": "The bot cannot send a direct message in that workspace. Ask them yourself."})
		return
	}
	if taken.Asked {
		writeJSON(w, 200, map[string]any{"ok": true, "asked": true})
		return
	}
	teamID := b.slackTeamOf(ctx, u.ID)
	t, _ := b.store.Team(ctx, teamID)
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil || t == nil {
		writeJSON(w, 502, map[string]any{"error": "That workspace could not be reached just now. Try again in a moment."})
		return
	}
	// Escaped, all three of them. This message is composed from what a stranger typed into their
	// own account and delivered by the bot to an admin of a *different* organisation: a name
	// carrying `<https://…|press here>` would read as a link the bot is vouching for, in the one
	// message from us that person has any reason to trust.
	who := escapeMrkdwn(nonEmpty(u.Name, u.Email))
	msg := "*" + who + "* (" + escapeMrkdwn(u.Email) + ") signed up for attest_tag and tried to connect *" +
		escapeMrkdwn(taken.Name) + "*, which is already connected to your organisation.\n\n" +
		"A workspace belongs to one organisation, so they cannot connect it again — but you can invite them to yours, " +
		"and they will land in the same console you are using.\n\n" +
		"<" + b.baseURL(r) + "/admin/settings/?tab=users|Invite them> · If you do not know them, ignore this."
	// Posting to a user id opens the direct message: that is what im:write buys.
	if _, err := sl.PostMarkdown(ctx, t.InstalledBy, "", msg, ""); err != nil {
		slog.Warn("could not ask a workspace installer for an invitation", "team", teamID, "err", err)
		writeJSON(w, 502, map[string]any{"error": "Slack would not deliver that message. Ask them yourself."})
		return
	}
	b.askedInvite(u.ID, teamID, true)
	slog.Info("asked a workspace installer for an invitation", "user", u.ID, "team", teamID)
	writeJSON(w, 200, map[string]any{"ok": true, "asked": true})
}
