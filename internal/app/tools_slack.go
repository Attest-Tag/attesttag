package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// canRead decides whether the person who asked may see a channel the model named. The bot is
// in channels the asker is not, and it reads with its own token, so without this check
// "summarize C0ABC" would hand anyone the contents of any private channel the bot was invited
// to. The thread's own channel is always allowed — they are in it. A public channel is allowed:
// every member of the workspace can already read it. Anything else — a private channel, a DM,
// a group DM — needs the asker to be a member.
func (a *Agent) canRead(ctx context.Context, c *Call, channel string) error {
	if channel == "" || channel == c.Channel {
		return nil
	}
	if c.SL == nil {
		return errors.New("I can't reach that Slack workspace right now, so I've not read it")
	}
	if !c.SL.IsPrivateConversation(ctx, channel) {
		return nil
	}
	// The channel's name is only needed to explain a refusal, and costs a call, so it is
	// resolved here rather than on the path where the read is allowed.
	who := func() string { return c.SL.ChannelName(ctx, channel) }
	if c.UserID == "" {
		return fmt.Errorf("I can't tell who is asking, so I won't read #%s", who())
	}
	member, err := c.SL.IsMember(ctx, channel, c.UserID)
	if err != nil {
		return fmt.Errorf("I couldn't check whether you're in that channel, so I didn't read it: %w", err)
	}
	if !member {
		return fmt.Errorf("#%s is private and you're not in it, so I won't read it for you. Ask someone who is, or have them add you", who())
	}
	return nil
}

// emojiName normalises what a model writes for an emoji into what Slack takes: a short name, no
// colons, no skin-tone suffix of its own invention. Models write ":white_check_mark:" about as
// often as "white_check_mark", and Slack answers invalid_name to the first, which costs a round
// and usually a second wrong spelling after it.
var emojiName = regexp.MustCompile(`^[a-z0-9_+'-]{1,64}$`)

func cleanEmoji(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ":")
	if !emojiName.MatchString(s) {
		return "", fmt.Errorf("%q is not an emoji name; give a short Slack name without colons, like white_check_mark or x", s)
	}
	return s, nil
}

// slackOnlyTools are the ones that ask Slack itself something only Slack can answer.
var slackOnlyTools = []string{"react", "slack_search", "list_pins", "get_user", "list_channels"}

func (a *Agent) registerSlackTools() {
	// Putting an emoji on a message, which until now nothing but the "read every message"
	// classifier could do (bot.go, watch) — and that one never runs on a channel taking
	// forwarded mail, because those turns are explicit and skip it. So a channel whose
	// instructions said "tick the message once the ticket is filed" was asking for something no
	// tool existed to do, and the model had no way to say so.
	//
	// Not gated on a Confirm: a reaction writes nothing anywhere, says nothing to anybody who
	// was not already reading the channel, and is undone by clicking it. Holding one would put a
	// five-minute approval card under a tick.
	a.register(Tool{
		Name: "react",
		Desc: "Put an emoji reaction on a message in this channel — a tick when something was filed, an x when it was skipped. Use it when the channel's instructions ask for one. Defaults to the message that started this thread, which on a forwarded email is the email itself.",
		Params: schema(map[string]any{
			"emoji": str("Slack emoji name without colons, like white_check_mark, x or eyes"),
			"ts":    str("Message timestamp (optional, defaults to the message that started this thread)"),
		}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct {
				Emoji string `json:"emoji"`
				TS    string `json:"ts"`
			}
			json.Unmarshal(args, &p)
			emoji, err := cleanEmoji(p.Emoji)
			if err != nil {
				return "", err
			}
			if c.SL == nil {
				return "", errors.New("I can't reach that Slack workspace right now, so I've not reacted")
			}
			ts := strings.TrimSpace(p.TS)
			if ts == "" {
				ts = c.ThreadTS
			}
			if ts == "" {
				return "", errors.New("there is no message here to react to")
			}
			// c.Channel, never a channel the model names. canRead is about whether the asker may
			// see somewhere; a reaction is visible to everyone in the channel it lands in, so the
			// question is different and the answer is simply that this tool does not travel.
			if err := c.SL.AddReaction(ctx, c.Channel, ts, emoji); err != nil {
				return "", fmt.Errorf("Slack refused the reaction :%s:: %w", emoji, err)
			}
			return fmt.Sprintf("Added :%s: to the message. Do not add it again, and do not describe it as an approval.", emoji), nil
		},
	})
	a.register(Tool{
		Name: "read_channel_history",
		Desc: "Read recent top-level messages from a Slack channel the bot is in. Defaults to the current channel and the last 7 days. Max 100 messages. Private channels can only be read for someone who is in them.",
		Params: schema(map[string]any{
			"channel": str("Channel id like C0123 (optional, defaults to current)"),
			"days":    num("How many days back to look (default 7, max 90)"),
		}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct {
				Channel string `json:"channel"`
				Days    int    `json:"days"`
			}
			json.Unmarshal(args, &p)
			if p.Channel == "" {
				p.Channel = c.Channel
			}
			if p.Days <= 0 {
				p.Days = 7
			}
			if p.Days > 90 {
				p.Days = 90
			}
			if err := a.canRead(ctx, c, p.Channel); err != nil {
				return "", err
			}
			msgs, err := c.SL.History(ctx, p.Channel, time.Now().Add(-time.Duration(p.Days)*24*time.Hour), 100)
			if err != nil {
				return "", err
			}
			if len(msgs) == 0 {
				return "no messages in that window", nil
			}
			return formatMsgs(ctx, c.SL, msgs), nil
		},
	})
	a.register(Tool{
		Name: "read_thread",
		Desc: "Read all replies of a Slack thread given its channel and root message ts (e.g. from a permalink /p1788320047708289 → 1788320047.708289).",
		Params: schema(map[string]any{
			"channel": str("Channel id"),
			"ts":      str("Root message ts like 1788320047.708289"),
		}, "channel", "ts"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Channel, TS string }
			json.Unmarshal(args, &p)
			p.TS = normalizeTS(p.TS)
			if err := a.canRead(ctx, c, p.Channel); err != nil {
				return "", err
			}
			msgs, err := c.SL.Thread(ctx, p.Channel, p.TS, 200)
			if err != nil {
				return "", err
			}
			return formatMsgs(ctx, c.SL, msgs), nil
		},
	})
	a.register(Tool{
		Name: "slack_search",
		Desc: "Search messages across public channels in the workspace (only what the asking user may see). Use for 'what did people say/decide about X'.",
		Params: schema(map[string]any{
			"query": str("Search words. Supports Slack modifiers like in:#channel from:@user after:2026-08-01"),
		}, "query"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Query string }
			json.Unmarshal(args, &p)
			// Slack will not let a bot token search on nobody's behalf: the call carries the
			// action token from the message that started the turn, and there is no way to ask
			// for one after the fact. A routine or an investigation therefore cannot search,
			// and saying so plainly beats handing the model Slack's invalid_action_token —
			// which it read as "the search tool is broken" and reported to the person asking.
			if c.ActionToken == "" {
				return "workspace search is not available on this turn: Slack only issues a search token with a message somebody just sent, and this turn did not start from one. Read a specific channel instead, or ask in a thread.", nil
			}
			api, err := c.SL.slackAPI()
			if err != nil {
				return "", err
			}
			resp, err := api.SearchAssistantContextContext(ctx, slack.AssistantSearchContextParameters{
				Query: p.Query, ActionToken: c.ActionToken, ChannelTypes: []string{"public_channel"}, Limit: 15, ContextChannelID: c.Channel,
			})
			if err != nil {
				if strings.Contains(err.Error(), "invalid_action_token") {
					return "", fmt.Errorf("Slack refused the search token for this turn (%w). It expires quickly: ask the person to send the question again", err)
				}
				return "", err
			}
			if len(resp.Results.Messages) == 0 {
				return "no matches", nil
			}
			var b strings.Builder
			for _, m := range resp.Results.Messages {
				name := m.AuthorName
				if name == "" {
					name = c.SL.UserName(ctx, m.AuthorUserID)
				}
				ch := m.ChannelName
				if ch == "" {
					ch = c.SL.ChannelName(ctx, m.ChannelID)
				}
				fmt.Fprintf(&b, "- #%s %s <%s|link>: %s\n", ch, name, m.Permalink, truncate(oneLine(m.Content), 400))
			}
			return b.String(), nil
		},
	})
	a.register(Tool{
		Name:   "list_pins",
		Desc:   "List pinned messages in a channel (defaults to the current one).",
		Params: schema(map[string]any{"channel": str("Channel id (optional)")}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Channel string }
			json.Unmarshal(args, &p)
			if p.Channel == "" {
				p.Channel = c.Channel
			}
			if err := a.canRead(ctx, c, p.Channel); err != nil {
				return "", err
			}
			api, err := c.SL.slackAPI()
			if err != nil {
				return "", err
			}
			items, _, err := api.ListPinsContext(ctx, p.Channel)
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				return "no pins", nil
			}
			var b strings.Builder
			for _, it := range items {
				if it.Message != nil {
					fmt.Fprintf(&b, "- %s: %s\n", c.SL.UserName(ctx, it.Message.User), truncate(oneLine(it.Message.Text), 300))
				} else if it.File != nil {
					fmt.Fprintf(&b, "- file: %s %s\n", it.File.Name, it.File.Permalink)
				}
			}
			return b.String(), nil
		},
	})
	a.register(Tool{
		Name: "get_user",
		Desc: "Look up Slack users by id: name, title, timezone, email if visible. Pass every id you need at once — " +
			"scheduling with a thread full of people is one call, not one call each.",
		Params: schema(map[string]any{
			"user_id": str("User id like U0123"),
			"user_ids": map[string]any{"type": "array", "description": "Several user ids, looked up in one call",
				"items": map[string]any{"type": "string"}},
		}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct {
				UserID  string   `json:"user_id"`
				UserIDs []string `json:"user_ids"`
			}
			json.Unmarshal(args, &p)
			ids := userIDList(append(p.UserIDs, p.UserID))
			if len(ids) == 0 {
				return "", fmt.Errorf("give user_id or user_ids")
			}
			api, err := c.SL.slackAPI()
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, id := range ids {
				u, err := api.GetUserInfoContext(ctx, id)
				if err != nil {
					// One bad id must not lose the people either side of it: a thread carries
					// app mentions and deactivated accounts, and a whole lookup that fails on
					// the first of them costs a round and answers nothing.
					fmt.Fprintf(&b, "%s: not found (%s)\n", id, truncate(oneLine(err.Error()), 120))
					continue
				}
				// The bot token sees every address in the workspace; the asker may not. An address
				// is shown only for somebody in the channel the question was asked in.
				email := "(not shown: not in this channel)"
				if ok, _ := c.SL.IsMember(ctx, c.Channel, id); ok {
					email = u.Profile.Email
				}
				fmt.Fprintf(&b, "%s\nname: %s\ndisplay: %s\ntitle: %s\ntz: %s\nemail: %s\nbot: %v\n",
					id, u.RealName, u.Profile.DisplayName, u.Profile.Title, u.TZ, email, u.IsBot)
			}
			return b.String(), nil
		},
	})
	a.register(Tool{
		Name:   "list_channels",
		Desc:   "List public channels in the workspace with ids and purposes.",
		Params: schema(map[string]any{}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			api, err := c.SL.slackAPI()
			if err != nil {
				return "", err
			}
			chans, _, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
				Types: []string{"public_channel"}, Limit: 200, ExcludeArchived: true,
			})
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, ch := range chans {
				fmt.Fprintf(&b, "- #%s (%s) members=%d %s\n", ch.Name, ch.ID, ch.NumMembers, truncate(ch.Purpose.Value, 80))
			}
			return b.String(), nil
		},
	})
}

// maxUserLookups caps one get_user call. Each id is a Slack round trip, and nothing a turn
// legitimately does — inviting a thread, naming the people in a channel — needs more.
const maxUserLookups = 30

// userIDList cleans the ids a model passed: `<@U123>` and `U123` are the same person, a thread
// names the same people over and over, and empty strings arrive whenever the singular argument
// went unused. Order is kept so the answer reads in the order asked.
func userIDList(raw []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		id := strings.ToUpper(strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "<@>")))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if out = append(out, id); len(out) == maxUserLookups {
			break
		}
	}
	return out
}

// The user id rides along with the name because it is what every other tool takes: get_user wants
// an id, and a mention in a reply is `<@U0123>`. Reading a thread and then having no way to name
// the people in it — to look up their addresses, or to invite them — made read_thread a dead end
// for anything but summarising. The people named *inside* a message get the same treatment, on
// one lookup budget for the whole listing.
func formatMsgs(ctx context.Context, sl *Chat, msgs []ThreadMsg) string {
	budget := maxNameLookups
	var b strings.Builder
	for _, m := range msgs {
		t := slackTime(m.TS)
		who := m.Name
		if m.UserID != "" {
			who = fmt.Sprintf("%s (<@%s>)", m.Name, m.UserID)
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", t, who, oneLine(sl.nameMentions(ctx, m.Text, &budget)))
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func slackTime(ts string) string {
	var n int64
	fmt.Sscanf(ts, "%d", &n)
	if n == 0 {
		return ts
	}
	// A Slack ts is seconds with the microseconds after a dot; a Teams message id is the whole
	// time in milliseconds, which read as seconds lands some fifty thousand years from now.
	if !strings.Contains(ts, ".") && n > 1e11 {
		return time.UnixMilli(n).Format("2006-01-02 15:04")
	}
	return time.Unix(n, 0).Format("2006-01-02 15:04")
}

// normalizeTS accepts "1788320047.708289" or a permalink fragment "p1788320047708289". Only
// Slack's sixteen digits — seconds, then microseconds — get their dot back: a Teams message id is
// thirteen, the time in milliseconds, and is already the id the thread is read by.
func normalizeTS(s string) string {
	s = strings.TrimSpace(s)
	if q := strings.Index(s, "?"); q >= 0 {
		s = s[:q]
	}
	if i := strings.LastIndex(s, "/p"); i >= 0 {
		s = s[i+2:]
	}
	s = strings.TrimPrefix(s, "p")
	if !strings.Contains(s, ".") && len(s) == 16 {
		s = s[:len(s)-6] + "." + s[len(s)-6:]
	}
	return s
}
