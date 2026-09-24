package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// Writes that need a human are held in pending_writes and announced under the bot's reply as a
// card with three buttons: Confirm runs it, Cancel drops it, Something else drops it and hands
// the thread back to the person, who says what they want in the thread like any other message —
// the session is already active there, so their next reply is just the next turn. Typing
// `confirm` or `cancel` still works: the buttons are the fast path, not the only one.

const (
	actConfirm = "attest_confirm"
	actCancel  = "attest_cancel"
	actOther   = "attest_other"
)

// silentRefusal is what a quiet turn tells the model in place of holding a write. Nothing it
// held could ever be confirmed: there is no thread for the buttons and nobody watching for them.
// Saying so plainly beats promising a card that will never appear.
//
// It is recorded as well as said, because the model being told is not the same as anybody
// knowing: a routine whose every write is refused otherwise finishes green, run after run, with
// the report reading as though the work behind it happened.
func (c *Call) silentRefusal(what string) string {
	c.silentHeld = appendOnce(c.silentHeld, what)
	return what + " needs a person's OK, and this routine runs quietly — there is no thread for the " +
		"Confirm buttons and nobody would see them. Do not try it again. Report what you found, and say " +
		"that this step still needs someone to run it. If this keeps happening, this routine is set to ask " +
		"first: whoever looks after it sets its Writes to Run without asking in the console, or an admin " +
		"sets the connection's Writes to Automatic or writes an allow rule covering it."
}

// heldWrite is one write this turn is waiting on a human for, and the card text that describes
// it. They accumulate: two proposals are two decisions, not one.
type heldWrite struct {
	id      int64
	summary string
}

// maxHeldPerTurn bounds the cards one turn can put in a thread. A turn that wants to ask five
// separate permissions has misunderstood the job.
const maxHeldPerTurn = 3

// holdForConfirm records a write this turn is waiting on, so its card goes out with the reply.
func (c *Call) holdForConfirm(id int64, summary string) {
	if id == 0 {
		return
	}
	c.pendingID, c.pendingSummary = id, summary
	for _, h := range c.held {
		if h.id == id {
			return
		}
	}
	if len(c.held) < maxHeldPerTurn {
		c.held = append(c.held, heldWrite{id: id, summary: summary})
	}
}

// attachNote is the line on a card for the files a write carries. An approver is approving the
// upload as well as the write, so it has to be on the card they read.
func attachNote(p ProxyRequest) string {
	switch n := len(p.AttachFiles); {
	case n == 0:
		return ""
	case n == 1:
		return "\nand upload 1 file from this thread to it"
	default:
		return fmt.Sprintf("\nand upload %d files from this thread to it", n)
	}
}

// httpConfirmSummary is the "what will run" line shown on the card for a proxied request.
func httpConfirmSummary(conn *Connection, p ProxyRequest) string {
	where := ""
	if conn != nil {
		where = " on " + conn.Name
	}
	// The method, URL and body are the model's — composed under whatever the thread, a web page
	// or a document told it — so they are escaped like any other text somebody else wrote. What
	// the person reads on the card has to be what will run, not a link dressed up as one.
	s := fmt.Sprintf("*Waiting for your OK*%s\n`%s %s`", escapeMrkdwn(where), escapeMrkdwn(strings.ToUpper(p.Method)), escapeMrkdwn(truncate(p.URL, 300)))
	if body := strings.TrimSpace(p.Body); body != "" {
		s += fmt.Sprintf("\n```%s```", escapeCode(truncate(redact(body), 1200)))
	}
	return s + attachNote(p)
}

// mcpConfirmSummary is the same line for an MCP tool call.
func mcpConfirmSummary(conn *Connection, tool string, args map[string]any) string {
	s := fmt.Sprintf("*Waiting for your OK*\nRun `%s` on %s", escapeMrkdwn(tool), escapeMrkdwn(conn.Name))
	if len(args) > 0 {
		raw, _ := json.Marshal(args)
		s += fmt.Sprintf("\n```%s```", escapeCode(truncate(redact(string(raw)), 1200)))
	}
	return s
}

// escapeCode is escapeMrkdwn for text shown inside a code block: a body holding ``` would close
// the block early, and whatever followed would render as card structure.
func escapeCode(s string) string {
	return strings.ReplaceAll(escapeMrkdwn(s), "```", "` ` `")
}

// postPendingConfirm posts the card once the turn's answer is in the thread, so people read what
// the bot proposes before the buttons.
func (a *Agent) postPendingConfirm(ctx context.Context, c *Call) {
	if c.SL == nil {
		return
	}
	// One card per held write. A turn that proposed a ticket and a fix job is asking two
	// questions, and until this loop existed only the second one was ever put to anybody.
	for _, h := range c.held {
		if _, err := c.SL.PostConfirm(ctx, c.Channel, c.ThreadTS, h.summary, h.id); err != nil {
			slog.Warn("confirm card", "channel", c.Channel, "err", err)
		}
	}
}

// ---- the card ----

// confirmValue carries the pending write id and its thread on every button.
func confirmValue(id int64, threadTS string) string {
	return fmt.Sprintf("%d|%s", id, threadTS)
}

func parseConfirmValue(v string) (int64, string, bool) {
	head, thread, ok := strings.Cut(v, "|")
	if !ok {
		return 0, "", false
	}
	id, err := strconv.ParseInt(head, 10, 64)
	if err != nil || id == 0 {
		return 0, "", false
	}
	return id, thread, true
}

// confirmCard is the card itself: what will run, then Confirm / Cancel / Something else.
func confirmCard(summary, threadTS string, id int64) Card {
	if summary == "" {
		summary = "*Waiting for your OK* before I run the action above."
	}
	v := confirmValue(id, threadTS)
	return Card{
		Parts: []CardPart{{Markdown: summary}},
		Buttons: []Button{
			{ActionID: actConfirm, Value: v, Label: "Confirm", Primary: true},
			{ActionID: actCancel, Value: v, Label: "Cancel"},
			{ActionID: actOther, Value: v, Label: "Something else…"},
		},
		Footer: "Expires in 5 minutes. Replying `confirm` or `cancel` in the thread works too.",
	}
}

// PostConfirm posts the approval card for a held write and returns its timestamp.
func (s *Chat) PostConfirm(ctx context.Context, channel, threadTS, summary string, id int64) (string, error) {
	_, ts, err := s.PostCard(ctx, channel, threadTS, confirmCard(summary, threadTS, id),
		"Waiting for your OK before this runs.")
	return ts, err
}

// PostCard posts a branded card and returns the channel it landed in and its timestamp. threadTS
// may be empty (a DM), and channel may be a user id, which resolves to that person's IM.
func (s *Chat) PostCard(ctx context.Context, channel, threadTS string, card Card, fallback string) (string, string, error) {
	return s.t.postCard(ctx, channel, threadTS, card, fallback)
}

// ResolveCard rewrites an answered card from a card the caller still holds: what it said stays,
// its buttons go, and the outcome takes the footer's place. ResolveConfirm scrapes the old text
// back out of the message, which only works for a card with one section; anything richer has to
// be rebuilt from the record so the card and the database cannot disagree.
func (s *Chat) ResolveCard(ctx context.Context, channel, ts string, keep Card, outcome string) {
	if ts == "" {
		return
	}
	if err := s.t.updateCard(ctx, channel, ts, Card{Parts: keep.Parts, Footer: outcome}, outcome); err != nil {
		slog.Debug("resolve card", "err", err)
	}
}

// ResolveConfirm rewrites a card once it has been answered: the summary stays, the buttons go.
func (s *Chat) ResolveConfirm(ctx context.Context, channel, ts, summary, outcome string) {
	if ts == "" {
		return
	}
	var parts []CardPart
	if summary != "" {
		parts = []CardPart{{Markdown: summary}}
	}
	if err := s.t.updateCard(ctx, channel, ts, Card{Parts: parts, Footer: outcome}, outcome); err != nil {
		slog.Debug("resolve confirm card", "err", err)
	}
}

// summaryFromMessage pulls the card's own summary back out of the message Slack sent us, so
// resolving a card keeps what it said. The card lives in an attachment (that is what carries
// the brand colour), so its blocks are not the message's own.
func summaryFromMessage(m slack.Message) string {
	if s := summaryFromBlocks(m.Blocks); s != "" {
		return s
	}
	for _, a := range m.Attachments {
		if s := summaryFromBlocks(a.Blocks); s != "" {
			return s
		}
	}
	return ""
}

func summaryFromBlocks(bs slack.Blocks) string {
	for _, b := range bs.BlockSet {
		if sb, ok := b.(*slack.SectionBlock); ok && sb.Text != nil {
			return sb.Text.Text
		}
	}
	return ""
}

// ---- interaction handling ----

// Actions outlive the HTTP delivery handoff, but never run without a deadline.
func runSlackAction(fn func(context.Context)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		fn(ctx)
	}()
}

// interaction routes a button press on a confirmation card.
func (b *Bot) interaction(ctx context.Context, teamID string, cb slack.InteractionCallback) {
	if cb.Type != slack.InteractionTypeBlockActions {
		return
	}
	// cb.Team is a value struct, so an org-wide install's "team": null decodes to an empty id
	// with no error. Dropping the press is the only safe answer: a button that runs a write
	// must never run it against a workspace we guessed at.
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		slog.Warn("button press dropped", "team", teamID, "err", err)
		return
	}
	// Pressing Confirm runs a write, so the button is gated the same way a message is: someone
	// the bot won't answer must not be able to approve an action for it either.
	if ok, _ := b.mayUseBot(ctx, sl, cb.User.ID); !ok {
		slog.Info("button press ignored: user may not use the bot", "user", cb.User.ID)
		return
	}
	// Slack retries deliveries when an acknowledgement is lost. A redelivered Approve
	// would be a second attempt at a write
	// somebody already ran, so dedupe on the trigger id — seen_events and its GC already exist.
	if cb.TriggerID != "" && !b.store.SeenEvent(ctx, "ix:"+teamID+":"+cb.TriggerID, deliveryOwner(ctx)) {
		slog.Debug("interaction ignored: redelivery", "user", cb.User.ID)
		return
	}
	for _, act := range cb.ActionCallback.BlockActions {
		if act == nil {
			continue
		}
		channel := cb.Channel.ID
		if channel == "" {
			channel = cb.Container.ChannelID
		}
		card := cb.Container.MessageTs
		if card == "" {
			card = cb.Message.Timestamp
		}
		b.cardPressed(sl, cardPress{User: cb.User.ID, Channel: channel, Thread: cb.Container.ThreadTs, Card: card,
			ActionID: act.ActionID, Value: act.Value, Summary: summaryFromMessage(cb.Message)})
	}
}

// cardPress is one press of a button on one of attest_tag's cards, as either platform reports it.
// Summary is what the card said, so answering it can keep the words while the buttons go.
type cardPress struct {
	User, Channel, Thread, Card string
	ActionID, Value, Summary    string
}

// cardPressed routes a press that has already been gated — its workspace found, its presser
// allowed to use the bot, a redelivery dropped. Whose press it is to make is decided further in,
// by each action, against the presser: no platform restricts a button to a person.
func (b *Bot) cardPressed(sl *Chat, p cardPress) {
	// Switch on the action id first and let each family decode its own value. Parsing before
	// the switch, as this did, means the next differently-encoded button loses every press
	// with nothing logged to say why.
	switch p.ActionID {
	case actConfirm, actCancel, actOther:
		id, thread, ok := parseConfirmValue(p.Value)
		if !ok {
			slog.Debug("confirm press with an unreadable value", "action", p.ActionID)
			return
		}
		if thread == "" {
			thread = p.Thread
		}
		switch p.ActionID {
		case actConfirm:
			runSlackAction(func(ctx context.Context) { b.confirmPressed(ctx, sl, p.Channel, thread, p.Card, p.User, p.Summary, id) })
		case actCancel:
			runSlackAction(func(ctx context.Context) {
				b.cancelPressed(ctx, sl, p.Channel, thread, p.Card, p.User, p.Summary, id, false)
			})
		case actOther:
			runSlackAction(func(ctx context.Context) {
				b.cancelPressed(ctx, sl, p.Channel, thread, p.Card, p.User, p.Summary, id, true)
			})
		}
	case actConnectOpen:
		// A link button. Slack still reports the press; there is nothing to do about it,
		// and it is named here so it does not read as an interaction we forgot to handle.
	case actAccessApprove, actAccessDeny:
		id, _, ok := parseConfirmValue(p.Value)
		if !ok {
			slog.Debug("access press with an unreadable value", "action", p.ActionID)
			return
		}
		runSlackAction(func(ctx context.Context) {
			b.accessPressed(ctx, sl, p.User, p.Channel, p.Card, id, p.ActionID == actAccessApprove)
		})
	default:
		slog.Debug("unhandled interaction", "action", p.ActionID)
	}
}

// mayConfirm says who may answer a held write: the person who asked for it, or somebody who
// holds an approval role in this workspace. Anybody who could see the card used to be able to
// run it — which made every member and guest of the workspace an approver of everyone else's
// writes, with the write then resolved against whatever channel the press came from.
func (b *Bot) mayConfirm(ctx context.Context, sl *Chat, user, requester string) bool {
	if user == "" || requester == "" {
		return false
	}
	if user == requester {
		return true
	}
	return b.agent != nil && b.agent.rankOf(ctx, sl.OrgID, sl, user) >= 0
}

// confirmPressed runs a held write after someone pressed Confirm.
func (b *Bot) confirmPressed(ctx context.Context, sl *Chat, channel, threadTS, card, user, summary string, id int64) {
	requester, err := b.store.PendingWriteRequester(ctx, sl.OrgID, sl.TeamID, channel, id)
	if err != nil || requester == "" {
		sl.ResolveConfirm(ctx, channel, card, summary, "This request expired or was already answered. Ask again and I'll re-run it.")
		return
	}
	if !b.mayConfirm(ctx, sl, user, requester) {
		sl.PostText(ctx, channel, threadTS, fmt.Sprintf("<@%s>, only <@%s> or an approver can confirm this one.", user, requester))
		return
	}
	raw, err := b.store.TakePendingWriteByID(ctx, sl.OrgID, sl.TeamID, channel, id)
	if err != nil || raw == "" {
		sl.ResolveConfirm(ctx, channel, card, summary, "This request expired or was already answered. Ask again and I'll re-run it.")
		return
	}
	sl.ResolveConfirm(ctx, channel, card, summary, fmt.Sprintf("Confirmed by <@%s> — running it now.", user))
	// A write that ran because somebody pressed Confirm is an approval, and the log says
	// whose: the requester and the approver are different people when a tier is involved.
	b.auditSlack(ctx, sl, user, "write.confirmed", AuditEvent{TargetKind: "pending_write", TargetID: strconv.FormatInt(id, 10),
		TargetName: oneLine(summary), Details: auditDetails(map[string]any{"requester": requester, "channel": channel, "thread_ts": threadTS})})
	sess, kind := b.threadSession(ctx, sl, channel, threadTS)
	// The write runs as the person who asked for it, not the person who pressed the button. An
	// approver may confirm somebody else's action; that says the action may happen, never that
	// it happens out of the approver's own account. With a per-person credential the difference
	// is whose calendar the meeting lands on.
	b.runPending(ctx, sl, kind, channel, threadTS, requester, user, raw, sess)
}

// dropHeldWrites discards the writes held in a thread that user may answer — every one of them
// for an approver here, otherwise the ones they asked for themselves — and returns the ids it
// dropped and the requester of one it left standing because it was somebody else's. mayConfirm
// is the rule, as it is for a card: no to your own proposal never cancels a colleague's, and a
// word typed in a thread is not a way past the question a button asks.
func (b *Bot) dropHeldWrites(ctx context.Context, sl *Chat, channel, threadTS, user string) (dropped []int64, left string) {
	held, err := b.store.PendingWritesInThread(ctx, sl.OrgID, sl.TeamID, channel, threadTS)
	if err != nil {
		slog.Warn("could not read the writes held in a thread", "channel", channel, "thread", threadTS, "err", err)
		return nil, ""
	}
	for _, h := range held {
		if !b.mayConfirm(ctx, sl, user, h.Requester) {
			if left == "" {
				left = h.Requester
			}
			continue
		}
		b.store.DiscardPendingWrite(ctx, sl.OrgID, sl.TeamID, channel, h.ID)
		dropped = append(dropped, h.ID)
	}
	return dropped, left
}

// cancelPressed drops the held write, and everything else held in the thread that the presser may
// answer: the person said no to what was proposed, not to one request of several — but only to
// their own proposals, unless they are an approver here. With ask, they went further and said
// they want something else — the thread stays open and their next message is the next turn,
// which is all "Something else" has to do.
func (b *Bot) cancelPressed(ctx context.Context, sl *Chat, channel, threadTS, card, user, summary string, id int64, ask bool) {
	if requester, _ := b.store.PendingWriteRequester(ctx, sl.OrgID, sl.TeamID, channel, id); requester != "" && !b.mayConfirm(ctx, sl, user, requester) {
		sl.PostText(ctx, channel, threadTS, fmt.Sprintf("<@%s>, only <@%s> or an approver can answer this one.", user, requester))
		return
	}
	b.store.DiscardPendingWrite(ctx, sl.OrgID, sl.TeamID, channel, id)
	if threadTS != "" {
		b.dropHeldWrites(ctx, sl, channel, threadTS, user)
	}
	b.auditSlack(ctx, sl, user, "write.cancelled", AuditEvent{TargetKind: "pending_write", TargetID: strconv.FormatInt(id, 10),
		TargetName: oneLine(summary), Details: auditDetails(map[string]any{"channel": channel, "thread_ts": threadTS, "asked_for_something_else": ask})})
	outcome := fmt.Sprintf("Cancelled by <@%s>. Nothing was sent.", user)
	if ask {
		// Their next message in the thread is the next turn, so make sure this is still a thread
		// we answer in without being tagged again.
		if threadTS != "" {
			if sess, _ := b.threadSession(ctx, sl, channel, threadTS); sess != nil && sess.Status != "active" {
				b.store.SetSessionField(ctx, sl.TeamID, channel, threadTS, "status", "active")
			}
		}
		outcome = fmt.Sprintf("Dropped. <@%s>, tell me here what you'd like instead.", user)
	}
	sl.ResolveConfirm(ctx, channel, card, summary, outcome)
}

// threadSession finds the session a button press belongs to, opening one if the thread has none.
func (b *Bot) threadSession(ctx context.Context, sl *Chat, channel, threadTS string) (*Session, string) {
	kind := "channel"
	if isDirectConversation(channel) {
		kind = "dm"
	}
	sess, _ := b.store.GetSession(ctx, sl.TeamID, channel, threadTS)
	if sess != nil {
		if sess.Kind != "" {
			kind = sess.Kind
		}
		return sess, kind
	}
	sess, err := b.store.EnsureSession(ctx, sl.TeamID, channel, threadTS, kind, "")
	if err != nil || sess == nil {
		// The turn still has to run: the person pressed a button and is waiting on an answer.
		slog.Warn("session for confirmation", "channel", channel, "err", err)
		sess = &Session{Channel: channel, ThreadTS: threadTS, TeamID: sl.TeamID, Kind: kind, Status: "active"}
	}
	return sess, kind
}
