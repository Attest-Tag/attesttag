package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// msteamsTransport is one Teams tenant as the rest of attest_tag sees it. Posting goes to the Bot
// Framework's REST API with the deployment's token; reading goes to the conversation log the bot
// keeps (store_msteams.go), because Microsoft gives an application no way to read a thread back on
// its own identity; and what a person is — their name, their address, whether they are a guest —
// comes from the Bot Framework's roster, which needs no Graph permission at all.
type msteamsTransport struct {
	c          *msteamsClient
	teamID     string // msteams:<tenant id>
	tenantID   string
	serviceURL string
	streams    sync.Map // the streamer's key → *msStream, for the answers being streamed now
}

const (
	platformMSTeams   = "msteams"
	msteamsTeamPrefix = "msteams:"
)

// errStreamUnsupported is a stream this conversation cannot have. The streamer answers it by
// posting the whole answer at the end, which is what a Teams channel gets.
var errStreamUnsupported = errors.New("this conversation cannot stream an answer")

func (c *msteamsClient) transport(teamID, serviceURL string) *msteamsTransport {
	return &msteamsTransport{c: c, teamID: teamID, tenantID: strings.TrimPrefix(teamID, msteamsTeamPrefix), serviceURL: serviceURL}
}

// A few answers are kept for the life of the process, per tenant, because the registry rebuilds
// transports every few minutes and nothing about them changes that often: what a channel is
// called, and which one-to-one conversation reaches a person.
var (
	msChannelNames sync.Map // teamID|conversation → name
	msDirect       sync.Map // teamID|user → conversation id
)

// rememberChannelName records what an activity said a conversation is called.
func rememberChannelName(teamID, conversation, name string) {
	if name != "" {
		msChannelNames.Store(teamID+"|"+conversation, name)
	}
}

// ---- addressing ----

// isTeamsChannel says whether a conversation id is a channel in a team. Only a channel has reply
// chains; a group chat is "19:…@thread.v2" and a one-to-one chat "a:…", and neither has threads.
func isTeamsChannel(id string) bool {
	return strings.HasPrefix(id, "19:") && (strings.HasSuffix(id, "@thread.tacv2") || strings.HasSuffix(id, "@thread.skype"))
}

// isTeamsPersonal says whether a conversation is a one-to-one chat with the bot.
func isTeamsPersonal(id string) bool { return strings.HasPrefix(id, "a:") }

// isTeamsGroupChat says whether a conversation is a chat among several people, outside any team.
func isTeamsGroupChat(id string) bool { return strings.HasPrefix(id, "19:") && !isTeamsChannel(id) }

var entraObjectIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isEntraObjectID says whether an id is a person rather than a conversation. The Slack half of the
// app DMs somebody by passing their user id where a channel goes, and so does this one.
func isEntraObjectID(id string) bool { return entraObjectIDRe.MatchString(id) }

// conversationFor is the conversation a post goes to: a reply chain in a channel, or the channel,
// chat or person itself.
func (t *msteamsTransport) conversationFor(ctx context.Context, channel, thread string) (string, error) {
	if isEntraObjectID(channel) {
		return t.direct(ctx, channel)
	}
	if thread != "" && thread != msteamsChatThread && isTeamsChannel(channel) {
		return channel + ";messageid=" + thread, nil
	}
	return channel, nil
}

// direct finds, or opens, the one-to-one conversation with a person. Teams opens one only for
// somebody the bot can reach: who has the app installed personally, or shares a team with it.
func (t *msteamsTransport) direct(ctx context.Context, user string) (string, error) {
	key := t.teamID + "|" + user
	if v, ok := msDirect.Load(key); ok {
		return v.(string), nil
	}
	member := user
	if u, _ := t.c.store.TeamsUser(ctx, t.teamID, user); u != nil && u.TeamsID != "" {
		member = u.TeamsID
	}
	body := map[string]any{
		"isGroup":     false,
		"bot":         map[string]string{"id": t.c.botID()},
		"members":     []map[string]string{{"id": member}},
		"tenantId":    t.tenantID,
		"channelData": map[string]any{"tenant": map[string]string{"id": t.tenantID}},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := t.call(ctx, "POST", "/v3/conversations", body, &out); err != nil {
		return "", fmt.Errorf("could not open a chat with that person: %w", err)
	}
	if out.ID == "" {
		return "", errors.New("Teams opened no chat with that person")
	}
	msDirect.Store(key, out.ID)
	return out.ID, nil
}

// threadOf is the thread a posted message belongs to, from the log: an update or a delete names
// only the message, and in a channel the Bot Framework wants the reply chain it is in.
func (t *msteamsTransport) threadOf(ctx context.Context, channel, id string) string {
	return t.c.store.TeamsMessageThread(ctx, t.teamID, channel, id)
}

// ---- the REST calls ----

func (t *msteamsTransport) call(ctx context.Context, method, path string, in, out any) error {
	if !msServiceURLOK(t.serviceURL) {
		return fmt.Errorf("refusing to call %q: not a Bot Framework service URL", t.serviceURL)
	}
	tok, err := t.c.botToken(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.serviceURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Teams answered %s: %s", resp.Status, truncate(oneLine(string(raw)), 300))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func activitiesPath(conversation string) string {
	return "/v3/conversations/" + url.PathEscape(conversation) + "/activities"
}

// msOutgoing is the part of an activity the bot sends.
type msOutgoing struct {
	Type        string         `json:"type"`
	Text        string         `json:"text,omitempty"`
	TextFormat  string         `json:"textFormat,omitempty"`
	Entities    []any          `json:"entities,omitempty"` // msMention, and msStreamInfo on a stream
	Attachments []msAttachment `json:"attachments,omitempty"`
	Summary     string         `json:"summary,omitempty"`
}

// entities is a message's mentions as the entities it carries, and anything else it needs.
func entities(mentions []msMention, more ...any) []any {
	if len(mentions) == 0 && len(more) == 0 {
		return nil
	}
	out := make([]any, 0, len(mentions)+len(more))
	for _, m := range mentions {
		out = append(out, m)
	}
	return append(out, more...)
}

type msAttachment struct {
	ContentType string `json:"contentType"`
	Content     any    `json:"content"`
}

const adaptiveCardType = "application/vnd.microsoft.card.adaptive"

// renderer is the renderer for this tenant: people are named from who the bot has met, channels
// from what their activities called them.
func (t *msteamsTransport) renderer(ctx context.Context) msRenderer {
	return msRenderer{
		who: func(id string) (string, string) {
			u, _ := t.c.store.TeamsUser(ctx, t.teamID, id)
			if u == nil {
				return "", ""
			}
			return u.Name, u.TeamsID
		},
		channel: func(id string) string {
			if v, ok := msChannelNames.Load(t.teamID + "|" + id); ok {
				return v.(string)
			}
			return ""
		},
	}
}

// send posts an activity and logs it, so the thread it lands in can be read back.
func (t *msteamsTransport) send(ctx context.Context, channel, thread string, act msOutgoing, logText string) (string, string, error) {
	conv, err := t.conversationFor(ctx, channel, thread)
	if err != nil {
		return "", "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := t.call(ctx, "POST", activitiesPath(conv), act, &out); err != nil {
		return "", "", err
	}
	landed, _, _ := strings.Cut(conv, ";messageid=")
	logThread := thread
	switch {
	case !isTeamsChannel(landed):
		logThread = msteamsChatThread
	case logThread == "":
		logThread = out.ID // a new top-level post starts its own thread
	}
	if out.ID != "" {
		if err := t.c.store.LogTeamsMessage(ctx, t.teamID, msMessage{Channel: landed, Thread: logThread, ID: out.ID,
			UserID: t.c.botID(), UserName: "assistant", IsBot: true, Text: logText}); err != nil {
			slog.Warn("could not log a Teams post", "team", t.teamID, "err", err)
		}
	}
	return landed, out.ID, nil
}

func (t *msteamsTransport) update(ctx context.Context, channel, id string, act msOutgoing, logText string) error {
	conv, err := t.conversationFor(ctx, channel, t.threadOf(ctx, channel, id))
	if err != nil {
		return err
	}
	type withID struct {
		msOutgoing
		ID string `json:"id"`
	}
	if err := t.call(ctx, "PUT", activitiesPath(conv)+"/"+url.PathEscape(id), withID{act, id}, nil); err != nil {
		return err
	}
	return t.c.store.SetTeamsMessageText(ctx, t.teamID, channel, id, logText)
}

// messageText is the text of a post: the body, then the footer as a line of its own.
func (t *msteamsTransport) messageText(ctx context.Context, body string, mrkdwn bool, footer string) (string, []msMention) {
	r := t.renderer(ctx)
	text, mentions := r.markdown(body, mrkdwn)
	if footer != "" {
		f, more := r.markdown(footer, true)
		text += "\n\n" + f
		mentions = append(mentions, more...)
	}
	return text, dedupeMentions(mentions)
}

// ---- transport: writing ----

func (t *msteamsTransport) postText(ctx context.Context, channel, threadTS, text string) (string, error) {
	md, mentions := t.messageText(ctx, text, true, "")
	_, id, err := t.send(ctx, channel, threadTS, msOutgoing{Type: "message", Text: md, TextFormat: "markdown", Entities: entities(mentions)}, text)
	return id, err
}

func (t *msteamsTransport) postMarkdown(ctx context.Context, channel, threadTS, md, footer string) (string, error) {
	text, mentions := t.messageText(ctx, md, false, footer)
	_, id, err := t.send(ctx, channel, threadTS, msOutgoing{Type: "message", Text: text, TextFormat: "markdown", Entities: entities(mentions)}, md)
	return id, err
}

func (t *msteamsTransport) updateMarkdown(ctx context.Context, channel, ts, md, footer, _ string) error {
	text, mentions := t.messageText(ctx, md, false, footer)
	return t.update(ctx, channel, ts, msOutgoing{Type: "message", Text: text, TextFormat: "markdown", Entities: entities(mentions)}, md)
}

func (t *msteamsTransport) cardActivity(ctx context.Context, card Card, fallback string) msOutgoing {
	return msOutgoing{Type: "message", Summary: fallback,
		Attachments: []msAttachment{{ContentType: adaptiveCardType, Content: t.renderer(ctx).adaptiveCard(card)}}}
}

func cardLogText(card Card) string {
	parts := make([]string, 0, len(card.Parts)+1)
	for _, p := range card.Parts {
		parts = append(parts, p.Markdown)
	}
	if card.Footer != "" {
		parts = append(parts, card.Footer)
	}
	return strings.Join(parts, "\n")
}

func (t *msteamsTransport) postCard(ctx context.Context, channel, threadTS string, card Card, fallback string) (string, string, error) {
	return t.send(ctx, channel, threadTS, t.cardActivity(ctx, card, fallback), cardLogText(card))
}

func (t *msteamsTransport) updateCard(ctx context.Context, channel, ts string, card Card, fallback string) error {
	return t.update(ctx, channel, ts, t.cardActivity(ctx, card, fallback), cardLogText(card))
}

func (t *msteamsTransport) deleteMessage(ctx context.Context, channel, ts string) error {
	conv, err := t.conversationFor(ctx, channel, t.threadOf(ctx, channel, ts))
	if err != nil {
		return err
	}
	if err := t.call(ctx, "DELETE", activitiesPath(conv)+"/"+url.PathEscape(ts), nil, nil); err != nil {
		return err
	}
	return t.c.store.DeleteTeamsMessage(ctx, t.teamID, channel, ts)
}

// addReaction is not offered: the react tool is withheld on a Teams turn (toolsFor), and the read-
// every-message classifier's reactions fail here with a reason rather than silently.
func (t *msteamsTransport) addReaction(context.Context, string, string, string) error {
	return errors.New("reactions are not available on Microsoft Teams")
}

// permalink is a Teams deep link to one message.
func (t *msteamsTransport) permalink(_ context.Context, channel, ts string) string {
	if channel == "" || ts == "" {
		return ""
	}
	q := url.Values{"tenantId": {t.tenantID}}
	if !isTeamsChannel(channel) {
		q.Set("context", `{"contextType":"chat"}`)
	}
	return "https://teams.microsoft.com/l/message/" + url.PathEscape(channel) + "/" + url.PathEscape(ts) + "?" + q.Encode()
}

// uploadContent is refused: a bot's file in a Teams channel means SharePoint and a consent this
// app does not ask for. Every caller already has somewhere to fall back to — a long answer stays in
// the thread, a job's diff stays on the job's page in the console.
func (t *msteamsTransport) uploadContent(context.Context, string, string, string, string, string, string) (string, string, error) {
	return "", "", errors.New("the bot cannot attach files in Microsoft Teams")
}

// setStatus shows the typing indicator. Teams has nothing to show "is reading the docs" with, so
// any status at all is shown as typing, and clearing it is simply not sending one.
func (t *msteamsTransport) setStatus(ctx context.Context, channel, threadTS, status string) {
	if status == "" {
		return
	}
	conv, err := t.conversationFor(ctx, channel, threadTS)
	if err != nil {
		return
	}
	if err := t.call(ctx, "POST", activitiesPath(conv), msOutgoing{Type: "typing"}, nil); err != nil {
		slog.Debug("teams typing", "err", err)
	}
}

func (t *msteamsTransport) suggestPrompts(context.Context, string, string, [][2]string) {}
func (t *msteamsTransport) setTitle(context.Context, string, string, string)            {}

// ---- transport: people ----

// msMember is a person as the Bot Framework's roster describes them.
type msMember struct {
	ID          string `json:"id"` // 29:…
	Name        string `json:"name"`
	AADObjectID string `json:"aadObjectId"`
	Email       string `json:"email"`
	UPN         string `json:"userPrincipalName"`
	TenantID    string `json:"tenantId"`
	Role        string `json:"userRole"` // user | guest | anonymous
}

// member asks the roster about one person, in a conversation they were last seen in: the Bot
// Framework describes a member only in the context of a conversation that has them in it.
func (t *msteamsTransport) member(ctx context.Context, user string) (msMember, error) {
	u, err := t.c.store.TeamsUser(ctx, t.teamID, user)
	if err != nil {
		return msMember{}, err
	}
	if u == nil || u.Conversation == "" {
		return msMember{}, fmt.Errorf("the bot has not met %s in Teams yet", user)
	}
	id := user
	if u.TeamsID != "" {
		id = u.TeamsID
	}
	var m msMember
	if err := t.call(ctx, "GET", "/v3/conversations/"+url.PathEscape(u.Conversation)+"/members/"+url.PathEscape(id), nil, &m); err != nil {
		return msMember{}, err
	}
	if m.AADObjectID != "" && m.AADObjectID != user {
		return msMember{}, fmt.Errorf("the roster answered for a different person")
	}
	_ = t.c.store.SaveTeamsUser(ctx, t.teamID, msUser{UserID: user, TeamsID: m.ID, Name: m.Name, Email: nonEmpty(m.Email, m.UPN)})
	return m, nil
}

func (t *msteamsTransport) userName(ctx context.Context, id string) (string, error) {
	if u, _ := t.c.store.TeamsUser(ctx, t.teamID, id); u != nil && u.Name != "" {
		return u.Name, nil
	}
	m, err := t.member(ctx, id)
	if err != nil {
		return "", err
	}
	return m.Name, nil
}

// userFacts is what the gates read about a person, from the roster. The workspace a person belongs
// to is their tenant, in the same "msteams:" form as this one's, so a guest from another company
// reads as another workspace exactly as a Slack Connect visitor does.
func (t *msteamsTransport) userFacts(ctx context.Context, id string) (userFacts, error) {
	m, err := t.member(ctx, id)
	if err != nil {
		return userFacts{}, err
	}
	tenant := m.TenantID
	if tenant == "" {
		tenant = t.tenantID
	}
	return userFacts{
		Email:      strings.ToLower(strings.TrimSpace(nonEmpty(m.Email, m.UPN))),
		TeamID:     msteamsTeamPrefix + tenant,
		Restricted: m.Role == "guest" || m.Role == "anonymous",
	}, nil
}

// userByEmail finds a person among those the bot has met. The Bot Framework has no lookup by
// address, and Graph's needs a consent this app does not ask a tenant for.
func (t *msteamsTransport) userByEmail(ctx context.Context, email string) (string, error) {
	u, err := t.c.store.TeamsUserByEmail(ctx, t.teamID, email)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", fmt.Errorf("nobody with %s has talked to the bot in this Teams organisation yet", email)
	}
	return u.UserID, nil
}

// ---- transport: conversations ----

// conversation describes a conversation. Every channel reads as private: a standard Teams channel
// is open to its team, not to the whole tenant, and what "public" means to the read checks is
// "anyone in the workspace may read it" — which for a Teams channel is not true. Membership is
// what decides, through members below.
func (t *msteamsTransport) conversation(_ context.Context, id string) (string, convInfo, error) {
	name := ""
	if v, ok := msChannelNames.Load(t.teamID + "|" + id); ok {
		name = v.(string)
	}
	switch {
	case isTeamsPersonal(id):
		return nonEmpty(name, "DM"), convInfo{IsIM: true}, nil
	case isTeamsChannel(id):
		return nonEmpty(name, "channel"), convInfo{IsPrivate: true}, nil
	case strings.HasPrefix(id, "19:"):
		return nonEmpty(name, "group chat"), convInfo{IsMPIM: true}, nil
	}
	return "", convInfo{}, fmt.Errorf("%q is not a Teams conversation", id)
}

// members is everyone in a conversation, by object id, a page at a time.
func (t *msteamsTransport) members(ctx context.Context, channel string) (map[string]bool, error) {
	ids := map[string]bool{}
	token := ""
	for {
		q := url.Values{"pageSize": {"500"}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		var page struct {
			Members           []msMember `json:"members"`
			ContinuationToken string     `json:"continuationToken"`
		}
		if err := t.call(ctx, "GET", "/v3/conversations/"+url.PathEscape(channel)+"/pagedmembers?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Members {
			if m.AADObjectID != "" {
				ids[m.AADObjectID] = true
			}
		}
		if page.ContinuationToken == "" || len(ids) >= 20000 {
			return ids, nil
		}
		token = page.ContinuationToken
	}
}

func (t *msteamsTransport) toRaw(m msMessage) rawMessage {
	r := rawMessage{UserID: m.UserID, Text: m.Text, TS: m.ID, Replies: m.Replies}
	if m.IsBot && m.UserID != t.c.botID() {
		r.BotID, r.Username = m.UserID, m.UserName
	}
	for _, f := range m.Files {
		r.Files = append(r.Files, f.asSlackFile())
	}
	return r
}

func (t *msteamsTransport) replies(ctx context.Context, channel, threadTS string) ([]rawMessage, error) {
	msgs, err := t.c.store.TeamsThread(ctx, t.teamID, channel, threadTS)
	if err != nil {
		return nil, err
	}
	out := make([]rawMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, t.toRaw(m))
	}
	return out, nil
}

func (t *msteamsTransport) history(ctx context.Context, channel string, since time.Time, limit int) ([]rawMessage, error) {
	msgs, err := t.c.store.TeamsHistory(ctx, t.teamID, channel, since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]rawMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, t.toRaw(m))
	}
	return out, nil
}
