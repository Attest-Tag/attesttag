package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Microsoft Teams deliveries: the Bot Framework POSTs every activity to /msteams/messages with a
// JWT that says it came from Microsoft. A verified message is translated into exactly the call a
// Slack event becomes — b.incoming — and everything from there on, the session, the budget, the
// agent loop, is the same code for both platforms.

// msActivity is the part of a Bot Framework activity this bot reads.
type msActivity struct {
	Type           string            `json:"type"`
	ID             string            `json:"id"`
	Timestamp      string            `json:"timestamp"`
	ServiceURL     string            `json:"serviceUrl"`
	ChannelID      string            `json:"channelId"`
	From           msAccount         `json:"from"`
	Recipient      msAccount         `json:"recipient"`
	Conversation   msConversation    `json:"conversation"`
	Text           string            `json:"text"`
	Entities       []json.RawMessage `json:"entities"`
	ChannelData    msChannelData     `json:"channelData"`
	Value          json.RawMessage   `json:"value"`
	Attachments    []msInAttachment  `json:"attachments"`
	ReplyToID      string            `json:"replyToId"`
	Action         string            `json:"action"`
	MembersAdded   []msAccount       `json:"membersAdded"`
	MembersRemoved []msAccount       `json:"membersRemoved"`
}

type msAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AADObjectID string `json:"aadObjectId,omitempty"`
	Role        string `json:"role,omitempty"`
}

type msConversation struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	ConversationType string `json:"conversationType,omitempty"` // personal | groupChat | channel
	TenantID         string `json:"tenantId,omitempty"`
}

type msChannelData struct {
	Tenant struct {
		ID string `json:"id"`
	} `json:"tenant"`
	Team struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		AADGroupID string `json:"aadGroupId"`
	} `json:"team"`
	Channel struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	EventType string `json:"eventType"`
}

func (a msActivity) tenant() string {
	return nonEmpty(a.Conversation.TenantID, a.ChannelData.Tenant.ID)
}

// base and root split a conversation id: a message in a channel's reply chain arrives addressed to
// "19:…@thread.tacv2;messageid=<root>", and the channel is the part before the semicolon.
func (a msActivity) base() string { b, _, _ := strings.Cut(a.Conversation.ID, ";messageid="); return b }
func (a msActivity) root() string { _, r, _ := strings.Cut(a.Conversation.ID, ";messageid="); return r }

// thread is the thread this activity belongs to: the reply chain it is in, a new top-level post's
// own id, or the one thread a chat has.
func (a msActivity) thread() string {
	if !isTeamsChannel(a.base()) {
		return msteamsChatThread
	}
	return nonEmpty(a.root(), a.ID)
}

func (a msActivity) personal() bool {
	return a.Conversation.ConversationType == "personal" || isTeamsPersonal(a.base())
}

// at is when the activity happened, in the log's fixed-width form.
func (a msActivity) at() string {
	if t, err := time.Parse(time.RFC3339Nano, a.Timestamp); err == nil {
		return msTime(t)
	}
	return msTime(time.Now())
}

// channelName is what the activity says its conversation is called. Teams sends a channel's name,
// except the General channel's, which it leaves out: that one is named for what it is.
func (a msActivity) channelName() string {
	if !isTeamsChannel(a.base()) {
		return ""
	}
	name := a.ChannelData.Channel.Name
	if name == "" && a.ChannelData.Channel.ID != "" && a.ChannelData.Channel.ID == a.ChannelData.Team.ID {
		name = "General"
	}
	if name == "" {
		return ""
	}
	if a.ChannelData.Team.Name != "" {
		return a.ChannelData.Team.Name + " › " + name
	}
	return name
}

func (b *Bot) msteamsHTTPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /msteams/messages", b.handleMSTeams)
}

const msteamsBodyLimit = 1 << 20

// refuseMSTeams answers a delivery we will not act on, and says why in the log — for the reason
// refuseSlack does: a bot that never answers looks the same whether Microsoft never called or
// called and was turned away, and only the log can tell those apart.
func refuseMSTeams(w http.ResponseWriter, r *http.Request, status int, says, why string) {
	slog.Warn("Teams delivery refused", "why", why, "status", status)
	http.Error(w, says, status)
}

func (b *Bot) handleMSTeams(w http.ResponseWriter, r *http.Request) {
	c := b.slacks.msteams
	if c == nil {
		refuseMSTeams(w, r, http.StatusServiceUnavailable, "Microsoft Teams is not configured",
			"MSTEAMS_APP_ID is unset, so nothing from the Bot Framework can be verified")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, msteamsBodyLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if !tooBig(w, err) {
			refuseMSTeams(w, r, http.StatusBadRequest, "could not read the activity", "body read failed: "+err.Error())
		}
		return
	}
	var act msActivity
	if err := json.Unmarshal(raw, &act); err != nil {
		refuseMSTeams(w, r, http.StatusBadRequest, "not an activity", "body is not JSON: "+err.Error())
		return
	}
	// The token is checked before anything in the activity is believed. What the activity says
	// about its channel and service URL is only used to check the token against.
	if _, err := c.verifier.verify(r.Context(), r.Header.Get("Authorization"), act.ChannelID, act.ServiceURL); err != nil {
		refuseMSTeams(w, r, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	// The cutover freeze, answered as a Slack event is (slack_http.go): acknowledged and dropped.
	// Everything an activity leads to writes — who sent it, what the conversation is called, the
	// message log, a link code, an install — and the database it would write to is about to be
	// replaced by a snapshot taken before it. After the token, as Slack's is after the signature:
	// a delivery that is not Microsoft's is refused whatever state the deployment is in.
	if b.cfg.Maintenance {
		slog.Info("maintenance: Teams activity acknowledged and dropped", "type", act.Type, "activity", act.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if act.ChannelID != msteamsChannelID {
		w.WriteHeader(http.StatusOK) // another Bot Framework channel: nothing here speaks it
		return
	}
	if !msServiceURLOK(act.ServiceURL) {
		refuseMSTeams(w, r, http.StatusBadRequest, "unexpected service url", "service url "+act.ServiceURL+" is not a Bot Framework host")
		return
	}
	// The Bot Framework wants an answer within fifteen seconds and retries without one, so the
	// work happens after the acknowledgement. A turn that starts runs on its own deadline.
	ctx := context.WithoutCancel(r.Context())
	switch act.Type {
	case "message":
		go b.msteamsMessage(ctx, act)
	case "installationUpdate":
		go b.msteamsInstalled(ctx, act)
	case "conversationUpdate":
		go b.msteamsConversationUpdate(ctx, act)
	}
	w.WriteHeader(http.StatusOK)
}

// msteamsSeen records who and where an activity came from: the person, the conversation's name,
// and the service URL a tenant's posts go to.
func (b *Bot) msteamsSeen(ctx context.Context, act msActivity, teamID string, linked *Team) {
	if act.From.AADObjectID != "" && act.From.Role != "bot" {
		if err := b.store.SaveTeamsUser(ctx, teamID, msUser{UserID: act.From.AADObjectID, TeamsID: act.From.ID,
			Name: act.From.Name, Conversation: act.base()}); err != nil {
			slog.Warn("could not record a Teams user", "team", teamID, "err", err)
		}
	}
	rememberChannelName(teamID, act.base(), act.channelName())
	if linked != nil && linked.ServiceURL != act.ServiceURL {
		if err := b.store.SetTeamServiceURL(ctx, teamID, act.ServiceURL); err == nil {
			b.slacks.Evict(teamID) // the cached transport still points at the old one
		}
	}
}

// linkedTeam is the tenant's row when it is joined to an organisation and live, or nil.
func (b *Bot) linkedTeam(ctx context.Context, teamID string) *Team {
	t, err := b.store.Team(ctx, teamID)
	if err != nil || t == nil || t.Status != "active" || t.Platform != platformMSTeams {
		return nil
	}
	return t
}

func (b *Bot) msteamsMessage(ctx context.Context, act msActivity) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := b.slacks.msteams
	tenant := act.tenant()
	if tenant == "" || act.base() == "" {
		slog.Warn("Teams message dropped: no tenant or conversation on it")
		return
	}
	teamID := msteamsTeamPrefix + tenant
	linked := b.linkedTeam(ctx, teamID)
	b.msteamsSeen(ctx, act, teamID, linked)
	if isCardPress(act.Value) {
		b.msteamsCardPress(ctx, act, teamID, linked)
		return
	}
	botNames, mentioned, addressed := b.msteamsMentions(ctx, act, teamID)
	text := fromTeams(act.Text, botNames, mentioned)
	explicit := addressed || act.personal()
	if code, ok := linkCommand(text); ok && explicit {
		b.msteamsLink(ctx, act, teamID, code)
		return
	}
	if linked == nil {
		if explicit {
			b.msteamsNotLinked(ctx, act, teamID)
		}
		return
	}
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		slog.Warn("Teams message dropped", "team", teamID, "err", err)
		return
	}
	if isTeamsChannel(act.base()) {
		b.msteamsChannelScope(ctx, linked, act)
		b.msteamsSyncTeam(ctx, linked, act, false)
	}
	if err := c.store.LogTeamsMessage(ctx, teamID, msMessage{Channel: act.base(), Thread: act.thread(), ID: act.ID,
		UserID: act.From.AADObjectID, UserName: act.From.Name, IsBot: act.From.Role == "bot", Text: text, At: act.at(),
		Files: teamsFiles(act.Attachments)}); err != nil {
		slog.Warn("could not log a Teams message", "team", teamID, "err", err)
	}
	kind := "channel"
	if act.personal() {
		kind = "dm"
	}
	botID := ""
	if act.From.Role == "bot" {
		botID = act.From.ID
	}
	b.incoming(ctx, sl, kind, act.base(), act.From.AADObjectID, botID, act.thread(), act.ID, text, "", explicit)
}

// msteamsMentions reads the mention entities: which <at> names are the bot, and which are people,
// by their object id. A person the bot has not met yet is looked up in the roster once, in the
// conversation the mention was made in.
func (b *Bot) msteamsMentions(ctx context.Context, act msActivity, teamID string) (map[string]bool, map[string]string, bool) {
	botNames, people, addressed := map[string]bool{}, map[string]string{}, false
	for _, raw := range act.Entities {
		var e msMention
		if json.Unmarshal(raw, &e) != nil || e.Type != "mention" {
			continue
		}
		sub := teamsAtRe.FindStringSubmatch(e.Text)
		if sub == nil {
			continue
		}
		name := strings.TrimSpace(sub[1])
		if e.Mentioned.ID == act.Recipient.ID {
			botNames[name], addressed = true, true
			continue
		}
		if !strings.HasPrefix(e.Mentioned.ID, "29:") {
			continue // a channel or a team: named, not a person
		}
		if u, _ := b.store.TeamsUserByTeamsID(ctx, teamID, e.Mentioned.ID); u != nil {
			people[name] = u.UserID
			continue
		}
		var m msMember
		tr := b.slacks.msteams.transport(teamID, act.ServiceURL)
		if err := tr.call(ctx, "GET", "/v3/conversations/"+url.PathEscape(act.base())+"/members/"+url.PathEscape(e.Mentioned.ID), nil, &m); err != nil || m.AADObjectID == "" {
			continue
		}
		_ = b.store.SaveTeamsUser(ctx, teamID, msUser{UserID: m.AADObjectID, TeamsID: m.ID, Name: m.Name,
			Email: nonEmpty(m.Email, m.UPN), Conversation: act.base()})
		people[name] = m.AADObjectID
	}
	return botNames, people, addressed
}

// msteamsChannelScope makes sure the console knows about a channel the bot has been talked to in,
// so access and instructions can be attached to it. Once per process per channel is enough: the
// row outlives the process, and a name that changes is picked up by the next restart.
var msScoped sync.Map

func (b *Bot) msteamsChannelScope(ctx context.Context, t *Team, act msActivity) {
	key := t.TeamID + "|" + act.base()
	if _, done := msScoped.LoadOrStore(key, true); done {
		return
	}
	name := nonEmpty(act.channelName(), "channel")
	b.store.UpsertChannelScope(ctx, t.OrgID, t.TeamID, act.base(), "#"+name, true)
}

// msteamsSyncTeam gives every channel of a Teams team the bot is in a scope row, so a team's
// channels are in the console as soon as the app is added to it, rather than one at a time as
// people talk in them. The Bot Framework lists a team's channels only to a bot installed in that
// team, and only by the team's id, which nothing but an activity from inside the team carries —
// so this runs when one arrives: the install, and the first message from the team in each
// process. force is for the install, when the list is known to have changed.
//
// Every row reads as private, as msteamsChannelScope's do: a standard channel is open to its
// team, not to the tenant.
var msTeamsSynced sync.Map

// msTeamChannels is what the last sync of each team found, so that the app being taken out of a
// team can take its channels out of the console with it.
var msTeamChannels sync.Map // tenant team id + "|" + Teams team id → []string

func (b *Bot) msteamsSyncTeam(ctx context.Context, t *Team, act msActivity, force bool) {
	teamsID := act.ChannelData.Team.ID
	if t == nil || teamsID == "" {
		return
	}
	key := t.TeamID + "|" + teamsID
	if _, done := msTeamsSynced.LoadOrStore(key, true); done && !force {
		return
	}
	tr := b.slacks.msteams.transport(t.TeamID, act.ServiceURL)
	teamName := act.ChannelData.Team.Name
	if teamName == "" {
		var info struct {
			Name string `json:"name"`
		}
		if err := tr.call(ctx, "GET", "/v3/teams/"+url.PathEscape(teamsID), nil, &info); err == nil {
			teamName = info.Name
		}
	}
	var list struct {
		Conversations []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"conversations"`
	}
	if err := tr.call(ctx, "GET", "/v3/teams/"+url.PathEscape(teamsID)+"/conversations", nil, &list); err != nil {
		msTeamsSynced.Delete(key) // the next activity from the team tries again
		slog.Warn("could not list a Teams team's channels", "team", t.TeamID, "err", err)
		return
	}
	ids := make([]string, 0, len(list.Conversations))
	for _, ch := range list.Conversations {
		if !isTeamsChannel(ch.ID) {
			continue
		}
		// Microsoft leaves the General channel's name out so that each client can say it in its
		// own language. The General channel's id is the team's.
		name := ch.Name
		if name == "" && ch.ID == teamsID {
			name = "General"
		}
		if name == "" {
			continue
		}
		if teamName != "" {
			name = teamName + " › " + name
		}
		rememberChannelName(t.TeamID, ch.ID, name)
		if _, err := b.store.UpsertChannelScope(ctx, t.OrgID, t.TeamID, ch.ID, "#"+name, true); err != nil {
			slog.Warn("scope for a Teams channel", "team", t.TeamID, "err", err)
			continue
		}
		msScoped.Store(t.TeamID+"|"+ch.ID, true)
		ids = append(ids, ch.ID)
	}
	msTeamChannels.Store(key, ids)
}

// msteamsLeftTeam takes a team's channels out of the console when the app is removed from it,
// the way a Slack channel the bot leaves is marked rather than dropped: its instructions and
// connections are kept for when it comes back.
func (b *Bot) msteamsLeftTeam(ctx context.Context, t *Team, teamsID string) {
	key := t.TeamID + "|" + teamsID
	v, ok := msTeamChannels.LoadAndDelete(key)
	msTeamsSynced.Delete(key)
	channels, _ := v.([]string)
	if !ok {
		channels = []string{teamsID} // at least the General channel, whose id is the team's
	}
	for _, ch := range channels {
		msScoped.Delete(t.TeamID + "|" + ch)
		if sc, err := b.store.ScopeFor(ctx, t.OrgID, "channel", t.TeamID, ch); err == nil && sc != nil {
			if err := b.store.MarkScopeLeft(ctx, t.OrgID, sc.ID); err != nil {
				slog.Warn("could not mark a Teams channel left", "team", t.TeamID, "err", err)
			}
		}
	}
}

// ---- joining a tenant to an organisation ----

var linkCommandRe = regexp.MustCompile(`(?i)^\s*link\s+([a-z0-9]{4}-?[a-z0-9]{4})\s*$`)

// linkCommand is "link ABCD-EFGH", the one message that joins a tenant to an organisation.
func linkCommand(text string) (string, bool) {
	m := linkCommandRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// replyTo answers an activity in the conversation, and thread, it came from — with or without a
// linked workspace, because the answers here are the ones given before there is one. An install is
// not a message, and its id is no message a reply chain can hang off (Teams answers "Invalid parent
// message ID"), so in a channel its greeting starts a post of its own.
func (b *Bot) replyTo(ctx context.Context, act msActivity, teamID, text string) {
	thread := act.thread()
	if act.Type != "message" && isTeamsChannel(act.base()) {
		thread = ""
	}
	tr := b.slacks.msteams.transport(teamID, act.ServiceURL)
	if _, err := tr.postText(ctx, act.base(), thread, text); err != nil {
		slog.Warn("could not answer in Teams", "team", teamID, "err", err)
	}
}

// teamsLinks bounds how often a code may be tried against one tenant, so a stranger cannot spray
// codes into a tenant they do not control hoping to bind it before its own admin does.
var teamsLinks = newRateLimiter()

// undoTeamsLink takes back what one link attempt wrote, and only that. A tenant already connected
// to this same account before the attempt is put back as it was, disconnected if it was: deleting
// it, which the refusal used to do, erased every channel, setting and thread the tenant had here
// because somebody not allowed to link it had tried a code.
func (b *Bot) undoTeamsLink(ctx context.Context, orgID int64, teamID string, before *Team) {
	if before != nil && before.OrgID == orgID {
		err := b.store.SaveTeam(ctx, before, nil)
		if err == nil && before.Status != "active" {
			err = b.store.RevokeTeam(ctx, teamID, before.LastError)
		}
		if err != nil {
			slog.Warn("put a Teams tenant back after a refused link", "team", teamID, "err", err)
		}
	} else {
		b.store.DeleteTeam(ctx, orgID, teamID)
	}
	b.slacks.Evict(teamID)
}

func (b *Bot) msteamsLink(ctx context.Context, act msActivity, teamID, code string) {
	c := b.slacks.msteams
	if ok, _ := teamsLinks.allow("mslink:"+teamID, 10, time.Hour); !ok {
		b.replyTo(ctx, act, teamID, "Too many attempts to connect this Microsoft Teams organisation just now. Wait a few minutes and try again.")
		return
	}
	existing, _ := b.store.Team(ctx, teamID)
	// Checked here, spent at the end. Spending first used the code up for a redeemer the membership
	// check below then refused — and told them to try the same code again, which could not work.
	orgID, err := b.store.PeekLinkCode(ctx, code, platformMSTeams)
	if err != nil {
		if !errors.Is(err, ErrLinkCode) {
			slog.Error("spend a link code", "team", teamID, "err", err)
		}
		b.replyTo(ctx, act, teamID, "That code didn't work: it may have expired or already been used. "+
			"Get a new one from the attest_tag console, under *Workspaces → Add workspace → Microsoft Teams*.")
		return
	}
	if existing != nil && existing.OrgID != orgID {
		slog.Warn("Teams tenant already linked elsewhere", "team", teamID, "org", existing.OrgID, "wanted", orgID)
		b.replyTo(ctx, act, teamID, "This Microsoft Teams organisation is already connected to a different attest_tag "+
			"account, so I've left it there. An admin of that account can disconnect it first.")
		return
	}
	name := nonEmpty(act.ChannelData.Team.Name, "Microsoft Teams")
	if existing != nil && existing.Name != "" {
		name = existing.Name
	}
	err = b.store.SaveTeam(ctx, &Team{TeamID: teamID, OrgID: orgID, Name: name, BotUserID: c.botID(), BotID: c.appID,
		InstalledBy: act.From.AADObjectID, EmailScope: true, DMScope: true, Platform: platformMSTeams,
		ServiceURL: act.ServiceURL}, nil)
	if errors.Is(err, ErrTeamOwnedElsewhere) {
		b.replyTo(ctx, act, teamID, "This Microsoft Teams organisation is already connected to a different attest_tag account.")
		return
	}
	if err != nil {
		slog.Error("link a Teams tenant", "team", teamID, "err", err)
		b.replyTo(ctx, act, teamID, "I couldn't finish connecting just now. Try the same code again in a minute.")
		return
	}
	// Only a genuine member of this Microsoft 365 organisation may connect it — not a guest, and
	// not someone from another tenant in a shared channel who could otherwise bind a tenant they
	// do not belong to. The Bot Framework roster is the only signal available (it has no "team
	// owner" role), so the bar is membership, and it is fail-closed: the link stands only on a
	// positive confirmation that the redeemer is a member of this tenant. A lookup that cannot
	// answer — a client that would not build, a roster call that errored — undoes the link rather
	// than letting an unverified binding stand, because binding a tenant to the wrong account is
	// the harm here. A transient failure just means the redeemer runs the code again.
	// (The client is built from the saved row, so the check runs after SaveTeam and undoes it; the
	// tenant is never usable in between, since this all happens in the one request.)
	confirmed := false
	if sl, err := b.slacks.For(ctx, teamID); err == nil {
		if uf, err := sl.user(ctx, act.From.AADObjectID, false); err == nil && !uf.Restricted && (uf.TeamID == "" || uf.TeamID == teamID) {
			confirmed = true
		}
	}
	if !confirmed {
		slog.Warn("Teams link refused: redeemer not confirmed a member of this tenant", "team", teamID, "by", act.From.AADObjectID)
		b.undoTeamsLink(ctx, orgID, teamID, existing)
		b.replyTo(ctx, act, teamID, "Only a member of this Microsoft Teams organisation can connect it, and I couldn't confirm this account is one. If you are a member of this Teams org, try the code again; otherwise ask an admin of it to run the code.")
		return
	}
	// Spent only now that the link stands. Two redeemers of one code in the same moment can both get
	// this far; the spend is the step only one of them wins, and the other undoes its link.
	if _, err := b.store.SpendLinkCode(ctx, code, platformMSTeams, teamID); err != nil {
		if !errors.Is(err, ErrLinkCode) {
			slog.Error("spend a link code", "team", teamID, "err", err)
		}
		b.undoTeamsLink(ctx, orgID, teamID, existing)
		b.replyTo(ctx, act, teamID, "That code didn't work: it may have expired or already been used. "+
			"Get a new one from the attest_tag console, under *Workspaces → Add workspace → Microsoft Teams*.")
		return
	}
	if _, err := b.store.UpsertScope(ctx, orgID, "team", teamID, teamID, name); err != nil {
		slog.Warn("scope for a linked Teams tenant", "team", teamID, "err", err)
	}
	b.slacks.Evict(teamID)
	b.msteamsSyncTeam(ctx, &Team{TeamID: teamID, OrgID: orgID}, act, true)
	b.changed(ctx, orgID)
	slog.Info("Teams tenant linked", "team", teamID, "org", orgID, "by", act.From.AADObjectID)
	org, _ := b.store.Org(ctx, orgID)
	who := "your attest_tag account"
	if org != nil && org.Name != "" {
		who = "*" + org.Name + "* on attest_tag"
	}
	msg := "Connected. This Microsoft Teams organisation now belongs to " + who + ". Mention me in a channel, or message me here."
	// Whoever links the organisation is usually the first to try the bot, so a gate that will refuse
	// them is said now rather than on their next message — most often an email-domain list seeded
	// from a Slack sign-up, which a tenant's own addresses are not on. Only a definite answer is
	// said: a roster lookup that failed just now is no reason to tell anyone they are refused.
	if sl, err := b.slacks.For(ctx, teamID); err == nil {
		if _, err := sl.user(ctx, act.From.AADObjectID, false); err == nil {
			if ok, why := b.mayUseBot(ctx, sl, act.From.AADObjectID); !ok {
				msg = "Connected. This Microsoft Teams organisation now belongs to " + who + ", but I can't answer you yet. " + why
			}
		}
	}
	b.replyTo(ctx, act, teamID, msg)
}

// notLinkedSaid keeps the explanation to once every few minutes per conversation: somebody trying
// the bot out before it is connected should be told how to connect it, not told it on every line.
var notLinkedSaid sync.Map

func (b *Bot) msteamsNotLinked(ctx context.Context, act msActivity, teamID string) {
	key := teamID + "|" + act.base()
	if v, ok := notLinkedSaid.Load(key); ok && time.Since(v.(time.Time)) < 5*time.Minute {
		return
	}
	notLinkedSaid.Store(key, time.Now())
	b.replyTo(ctx, act, teamID, msteamsNotLinkedText)
}

const msteamsNotLinkedText = "I'm not connected to an attest_tag account in this organisation yet. Someone who manages " +
	"attest_tag can open the console, go to *Workspaces → Add workspace → Microsoft Teams*, and send me the code it shows, " +
	"like this: `link ABCD-EFGH`."

// ---- installs and conversation changes ----

// msteamsInstalled greets the conversation the app was added to: how to connect it when it is not
// yet, and how to use it when it is.
func (b *Bot) msteamsInstalled(ctx context.Context, act msActivity) {
	if act.tenant() == "" {
		return
	}
	teamID := msteamsTeamPrefix + act.tenant()
	linked := b.linkedTeam(ctx, teamID)
	if act.Action == "remove" {
		if linked != nil && act.ChannelData.Team.ID != "" {
			b.msteamsLeftTeam(ctx, linked, act.ChannelData.Team.ID)
		}
		return
	}
	if !strings.HasPrefix(act.Action, "add") {
		return
	}
	b.msteamsSeen(ctx, act, teamID, linked)
	b.msteamsSyncTeam(ctx, linked, act, true)
	if act.Action != "add" { // add-upgrade: the app was updated, not newly met
		return
	}
	if linked == nil {
		b.replyTo(ctx, act, teamID, "Thanks for adding me. "+msteamsNotLinkedText)
		return
	}
	b.replyTo(ctx, act, teamID, "Hi, I'm "+msteamsAppName+". Mention me with a question, or message me directly.")
}

// msteamsConversationUpdate keeps a linked team's channels current in the console as they are
// created, renamed and deleted. Membership changes need no handling: who is in a conversation is
// asked of the roster when it matters.
func (b *Bot) msteamsConversationUpdate(ctx context.Context, act msActivity) {
	if act.tenant() == "" {
		return
	}
	teamID := msteamsTeamPrefix + act.tenant()
	ch := act.ChannelData.Channel
	switch act.ChannelData.EventType {
	case "channelCreated", "channelRenamed", "channelRestored":
		if ch.ID == "" || ch.Name == "" {
			return
		}
		name := ch.Name
		if act.ChannelData.Team.Name != "" {
			name = act.ChannelData.Team.Name + " › " + name
		}
		rememberChannelName(teamID, ch.ID, name)
		if t := b.linkedTeam(ctx, teamID); t != nil {
			if _, err := b.store.UpsertChannelScope(ctx, t.OrgID, teamID, ch.ID, "#"+name, true); err != nil {
				slog.Warn("scope for a Teams channel", "team", teamID, "err", err)
			}
		}
	case "channelDeleted":
		t := b.linkedTeam(ctx, teamID)
		if t == nil || ch.ID == "" {
			return
		}
		msScoped.Delete(teamID + "|" + ch.ID)
		if sc, err := b.store.ScopeFor(ctx, t.OrgID, "channel", teamID, ch.ID); err == nil && sc != nil {
			if err := b.store.MarkScopeLeft(ctx, t.OrgID, sc.ID); err != nil {
				slog.Warn("could not mark a Teams channel left", "team", teamID, "err", err)
			}
		}
	case "teamDeleted", "teamArchived":
		if t := b.linkedTeam(ctx, teamID); t != nil && act.ChannelData.Team.ID != "" {
			b.msteamsLeftTeam(ctx, t, act.ChannelData.Team.ID)
		}
	}
}

// ---- card presses ----

// isCardPress says whether a message carries a press of one of attest_tag's own buttons.
func isCardPress(value json.RawMessage) bool {
	if len(value) == 0 {
		return false
	}
	var v map[string]any
	if json.Unmarshal(value, &v) != nil {
		return false
	}
	_, ok := v[msCardAction].(string)
	return ok
}

// msteamsCardPress turns a Teams button press into the press cardPressed routes for both
// platforms, gated the way a Slack press is: a workspace that is linked, a presser who may use the
// bot, and a redelivery dropped.
func (b *Bot) msteamsCardPress(ctx context.Context, act msActivity, teamID string, linked *Team) {
	if linked == nil {
		return
	}
	var v map[string]any
	if json.Unmarshal(act.Value, &v) != nil {
		return
	}
	action, _ := v[msCardAction].(string)
	value, _ := v[msCardValue].(string)
	summary, _ := v[msCardSummary].(string)
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		slog.Warn("button press dropped", "team", teamID, "err", err)
		return
	}
	user := act.From.AADObjectID
	if ok, _ := b.mayUseBot(ctx, sl, user); !ok {
		slog.Info("button press ignored: user may not use the bot", "user", user)
		return
	}
	if !b.store.SeenEvent(ctx, "ix:"+teamID+":"+act.ID, deliveryOwner(ctx)) {
		return
	}
	b.cardPressed(sl, cardPress{User: user, Channel: act.base(), Thread: act.thread(), Card: act.ReplyToID,
		ActionID: action, Value: value, Summary: summary})
}
