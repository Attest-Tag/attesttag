package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// slackTransport is how a workspace on Slack asks Slack things: every Web API call made on a
// conversation's behalf, and none of the judgement about the answers. The caches, their expiry
// and the rules that fail closed live one level up on *Chat, because they have to hold the same
// way whichever chat platform a workspace is on. What differs between platforms is only how the
// question is put, and that is all this file does.
type slackTransport struct {
	api *slack.Client
}

// userName is the name Slack shows for an account: the display name, then the real name, then
// the handle.
func (t *slackTransport) userName(ctx context.Context, id string) (string, error) {
	u, err := t.api.GetUserInfoContext(ctx, id)
	if err != nil {
		return "", err
	}
	name := u.Profile.DisplayName
	if name == "" {
		name = u.RealName
	}
	if name == "" {
		name = u.Name
	}
	return name, nil
}

func (t *slackTransport) userFacts(ctx context.Context, id string) (userFacts, error) {
	u, err := t.api.GetUserInfoContext(ctx, id)
	if err != nil {
		return userFacts{}, err
	}
	return userFacts{
		Email:      strings.ToLower(strings.TrimSpace(u.Profile.Email)),
		TeamID:     u.TeamID,
		Deleted:    u.Deleted,
		Bot:        u.IsBot,
		Restricted: u.IsRestricted || u.IsUltraRestricted,
	}, nil
}

func (t *slackTransport) userByEmail(ctx context.Context, email string) (string, error) {
	u, err := t.api.GetUserByEmailContext(ctx, email)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

func (t *slackTransport) conversation(ctx context.Context, id string) (string, convInfo, error) {
	c, err := t.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: id})
	if err != nil {
		return "", convInfo{}, err
	}
	return c.Name, convInfo{IsPrivate: c.IsPrivate, IsIM: c.IsIM, IsMPIM: c.IsMpIM}, nil
}

// members reads a conversation's whole roster, a page at a time.
func (t *slackTransport) members(ctx context.Context, channel string) (map[string]bool, error) {
	ids := map[string]bool{}
	cursor := ""
	for {
		page, next, err := t.api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
			ChannelID: channel, Limit: 1000, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, id := range page {
			ids[id] = true
		}
		if next == "" || len(ids) >= 20000 {
			break
		}
		cursor = next
	}
	return ids, nil
}

// replies reads a whole thread oldest-first, a page at a time.
func (t *slackTransport) replies(ctx context.Context, channel, threadTS string) ([]rawMessage, error) {
	var all []slack.Message
	cursor := ""
	for {
		msgs, more, next, err := t.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channel, Timestamp: threadTS, Limit: 200, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, msgs...)
		if !more || next == "" || len(all) > 1000 {
			break
		}
		cursor = next
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Timestamp < all[j].Timestamp })
	out := make([]rawMessage, 0, len(all))
	for _, m := range all {
		out = append(out, rawSlackMessage(m))
	}
	return out, nil
}

// history reads recent top-level messages oldest-first, less people joining and leaving.
func (t *slackTransport) history(ctx context.Context, channel string, since time.Time, limit int) ([]rawMessage, error) {
	resp, err := t.api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channel, Limit: limit, Oldest: fmt.Sprintf("%d.000000", since.Unix()),
	})
	if err != nil {
		return nil, err
	}
	msgs := resp.Messages
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Timestamp < msgs[j].Timestamp })
	out := make([]rawMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.SubType == "channel_join" || m.SubType == "channel_leave" {
			continue
		}
		out = append(out, rawSlackMessage(m))
	}
	return out, nil
}

// rawSlackMessage is a Slack message reduced to what a transcript needs of it.
func rawSlackMessage(m slack.Message) rawMessage {
	return rawMessage{UserID: m.User, BotID: m.BotID, Username: m.Username, Text: messageText(m),
		TS: m.Timestamp, Replies: m.ReplyCount, Files: m.Files}
}

// messageText is everything readable in a message: its own text, and what an app put in
// attachments or blocks instead of it. A Slack app that posts a card -- an alert, a build
// result, a document that failed to process -- leaves `text` empty and carries every field
// inside the attachment, so a message read for its text alone came back blank and was dropped
// from the thread replay entirely. That is how a turn came to ask for a doc id that was sitting
// in the root message of the very thread it was answering in.
//
// The parts are joined as lines, a field as "Title: value", and a part already present in what
// has been collected is not repeated: an app that also sets `text` to the card's fallback would
// otherwise say everything twice.
func messageText(m slack.Message) string {
	base := strings.TrimSpace(m.Text)
	var b strings.Builder // what the card adds to it
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || strings.Contains(base, s) || strings.Contains(b.String(), s) {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	addBlockText(add, m.Blocks)
	for _, a := range m.Attachments {
		before := b.Len()
		add(a.Pretext)
		add(a.AuthorName)
		add(a.Title)
		add(a.Text)
		for _, f := range a.Fields {
			add(strings.TrimSpace(f.Title + ": " + f.Value))
		}
		addBlockText(add, a.Blocks)
		add(a.Footer)
		if b.Len() == before {
			// Nothing readable in the attachment itself: the fallback is what Slack shows a
			// client that cannot render the card, which is the position we are in.
			add(a.Fallback)
		}
	}
	if b.Len() == 0 {
		return base
	}
	// A card can be long -- a rendered document, a log tail -- and the thread window is counted
	// in messages, so one of those would otherwise crowd out everything said around it. Only
	// what the card adds is capped: what somebody typed goes to the model as they typed it.
	extra := truncate(b.String(), cardTextCap)
	if base == "" {
		return extra
	}
	return base + "\n" + extra
}

const cardTextCap = 4_000

// replyFooterBlockID marks the grey line under every reply ("basic · 15k in · 101 out · $0.0019 ·
// Configure") so that reading a thread back can leave it out. It is the bot's own bookkeeping, not
// something anybody said: replayed with each earlier answer, it taught the model that answers end
// that way, and it began writing one of its own — with invented figures — above the real one.
const replyFooterBlockID = "attest_footer"

// isReplyFooter recognises that line: by its block id, or, on a reply posted before it had one, by
// the Configure link no other context line carries.
func isReplyFooter(b *slack.ContextBlock) bool {
	if b.BlockID == replyFooterBlockID {
		return true
	}
	for _, e := range b.ContextElements.Elements {
		if t, ok := e.(*slack.TextBlockObject); ok && strings.Contains(t.Text, "|Configure>") {
			return true
		}
	}
	return false
}

// addBlockText pulls the text out of the block kinds apps actually post. Anything else -- an
// image, a divider, the task cards our own streamed replies carry -- has no text worth replaying,
// and neither has the footer under a reply (replyFooterBlockID).
func addBlockText(add func(string), bs slack.Blocks) {
	for _, b := range bs.BlockSet {
		switch v := b.(type) {
		case *slack.SectionBlock:
			if v.Text != nil {
				add(v.Text.Text)
			}
			for _, f := range v.Fields {
				add(f.Text)
			}
		case *slack.HeaderBlock:
			if v.Text != nil {
				add(v.Text.Text)
			}
		case *slack.MarkdownBlock:
			add(v.Text)
		case *slack.ContextBlock:
			if isReplyFooter(v) {
				continue
			}
			for _, e := range v.ContextElements.Elements {
				if t, ok := e.(*slack.TextBlockObject); ok {
					add(t.Text)
				}
			}
		}
	}
}

func (t *slackTransport) postText(ctx context.Context, channel, threadTS, text string) (string, error) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := t.api.PostMessageContext(ctx, channel, opts...)
	return ts, err
}

// postMarkdown posts a Markdown block over an optional footer, and retries as markdown_text for
// the older workspaces that do not take markdown blocks.
func (t *slackTransport) postMarkdown(ctx context.Context, channel, threadTS, md, footer string) (string, error) {
	blocks := []slack.Block{slack.NewMarkdownBlock("", md)}
	if footer != "" {
		blocks = append(blocks, slack.NewContextBlock(replyFooterBlockID, slack.NewTextBlockObject(slack.MarkdownType, footer, false, false)))
	}
	opts := []slack.MsgOption{slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(truncate(oneLine(md), 200), false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := t.api.PostMessageContext(ctx, channel, opts...)
	if err != nil { // older workspaces without markdown blocks: fall back to markdown_text
		// Rebuilt rather than indexed: a post with no thread has no third option, and reaching
		// for one panicked the process on every failure that was not a markdown-block workspace
		// — a rate limit, a 5xx, a routine's header post on a context that had just been
		// cancelled. Nothing here recovers, so that failure took the container with it.
		text := md
		if footer != "" {
			text += "\n\n_" + footer + "_"
		}
		retry := []slack.MsgOption{slack.MsgOptionMarkdownText(text)}
		if threadTS != "" {
			retry = append(retry, slack.MsgOptionTS(threadTS))
		}
		_, ts, err = t.api.PostMessageContext(ctx, channel, retry...)
	}
	return ts, err
}

// updateMarkdown rewrites a posted message as a Markdown block over an optional footer.
// fallback is the plain text that notifications and screen readers get instead.
func (t *slackTransport) updateMarkdown(ctx context.Context, channel, ts, md, footer, fallback string) error {
	blocks := []slack.Block{slack.NewMarkdownBlock("", md)}
	if footer != "" {
		blocks = append(blocks, slack.NewContextBlock(replyFooterBlockID, slack.NewTextBlockObject(slack.MarkdownType, footer, false, false)))
	}
	_, _, _, err := t.api.UpdateMessageContext(ctx, channel, ts, slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(fallback, false))
	return err
}

// postCard posts a branded card. It reports the channel the card landed in as well as its
// timestamp, because that differs from the one asked for when it was a user id: updating the card
// later needs the D… id, and chat.postMessage is the only thing that says what it is.
func (t *slackTransport) postCard(ctx context.Context, channel, threadTS string, card Card, fallback string) (string, string, error) {
	opts := []slack.MsgOption{slack.MsgOptionAttachments(brandCard(slackBlocks(card), fallback))}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	return t.api.PostMessageContext(ctx, channel, opts...)
}

func (t *slackTransport) updateCard(ctx context.Context, channel, ts string, card Card, fallback string) error {
	_, _, _, err := t.api.UpdateMessageContext(ctx, channel, ts, slack.MsgOptionAttachments(brandCard(slackBlocks(card), fallback)))
	return err
}

// slackBlocks draws a card in Block Kit: a section per paragraph, a context line per piece of
// small print, the buttons in one actions row, and the footer as a context line under them.
func slackBlocks(c Card) []slack.Block {
	out := []slack.Block{}
	for _, p := range c.Parts {
		if p.Small {
			out = append(out, slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, p.Markdown, false, false)))
			continue
		}
		out = append(out, slack.NewSectionBlock(markdownText(p.Markdown), nil, nil))
	}
	if len(c.Buttons) > 0 {
		row := make([]slack.BlockElement, 0, len(c.Buttons))
		for _, b := range c.Buttons {
			btn := slack.NewButtonBlockElement(b.ActionID, b.Value, plainText(b.Label))
			if b.Primary {
				btn = btn.WithStyle(slack.StylePrimary)
			}
			if b.URL != "" {
				btn = btn.WithURL(b.URL)
			}
			row = append(row, btn)
		}
		out = append(out, slack.NewActionBlock("", row...))
	}
	if c.Footer != "" {
		out = append(out, slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, c.Footer, false, false)))
	}
	return out
}

func plainText(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.PlainTextType, s, true, false)
}

func markdownText(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, truncate(s, 2900), false, false)
}

// brandColor is the console's iris violet (--primary). A Block Kit button only takes Slack's
// own primary/danger/neutral, so the brand colour rides in on the attachment bar down the side
// of the card, and Cancel stays neutral: dropping a request is not a dangerous act.
const brandColor = "#5a50c8"

// brandCard wraps blocks in the branded attachment the card is posted and updated as.
func brandCard(blocks []slack.Block, fallback string) slack.Attachment {
	return slack.Attachment{Color: brandColor, Fallback: fallback, Blocks: slack.Blocks{BlockSet: blocks}}
}

func (t *slackTransport) deleteMessage(ctx context.Context, channel, ts string) error {
	_, _, err := t.api.DeleteMessageContext(ctx, channel, ts)
	return err
}

// addReaction treats "already_reacted" as done: a redelivered event asking for the same
// reaction has already had its effect.
func (t *slackTransport) addReaction(ctx context.Context, channel, ts, emoji string) error {
	err := t.api.AddReactionContext(ctx, emoji, slack.ItemRef{Channel: channel, Timestamp: ts})
	if err != nil && strings.Contains(err.Error(), "already_reacted") {
		return nil
	}
	return err
}

func (t *slackTransport) permalink(ctx context.Context, channel, ts string) string {
	l, err := t.api.GetPermalinkContext(ctx, &slack.PermalinkParameters{Channel: channel, Ts: ts})
	if err != nil {
		return ""
	}
	return l
}

// uploadContent posts content as a file in the thread and reports the file id and permalink.
// snippetType may be empty, in which case Slack renders by the filename's extension.
func (t *slackTransport) uploadContent(ctx context.Context, channel, threadTS, filename, title, content, snippetType string) (fileID, permalink string, err error) {
	f, err := t.api.UploadFileContext(ctx, slack.UploadFileParameters{
		Channel: channel, ThreadTimestamp: threadTS, Filename: filename, Title: title,
		Content: content, FileSize: len(content), SnippetType: snippetType,
	})
	if err != nil {
		return "", "", err
	}
	// files.upload.v2 answers with id and title only, so the permalink costs a second call.
	// The file is already in the thread by then: a lookup failure loses the link, not the file.
	info, _, _, err := t.api.GetFileInfoContext(ctx, f.ID, 0, 0)
	if err != nil {
		slog.Warn("file uploaded but permalink lookup failed", "file", f.ID, "err", err)
		return f.ID, "", nil
	}
	return f.ID, info.Permalink, nil
}

// setStatus shows "is thinking…"-style status on a thread. Empty status clears it.
// Only assistant (DM) threads render it; channel threads return an error we ignore.
func (t *slackTransport) setStatus(ctx context.Context, channel, threadTS, status string) {
	p := slack.AssistantThreadsSetStatusParameters{ChannelID: channel, ThreadTS: threadTS, Status: status}
	if err := t.api.SetAssistantThreadsStatusContext(ctx, p); err != nil {
		slog.Debug("setStatus", "err", err)
	}
}

func (t *slackTransport) suggestPrompts(ctx context.Context, channel, threadTS string, prompts [][2]string) {
	p := slack.AssistantThreadsSetSuggestedPromptsParameters{ChannelID: channel, ThreadTS: threadTS, Title: "Try asking"}
	for _, pr := range prompts {
		p.AddPrompt(pr[0], pr[1])
	}
	if err := t.api.SetAssistantThreadsSuggestedPromptsContext(ctx, p); err != nil {
		slog.Debug("suggestedPrompts", "err", err)
	}
}

func (t *slackTransport) setTitle(ctx context.Context, channel, threadTS, title string) {
	if err := t.api.SetAssistantThreadsTitleContext(ctx, slack.AssistantThreadsSetTitleParameters{
		ChannelID: channel, ThreadTS: threadTS, Title: title}); err != nil {
		slog.Debug("setTitle", "err", err)
	}
}

// startStream opens a streamed message. Slack addresses a stream to a person in a workspace, which
// is what recipient and recipientTeam are.
func (t *slackTransport) startStream(ctx context.Context, channel, threadTS, recipient, recipientTeam string) (string, error) {
	if t.api == nil {
		return "", errNotSlack
	}
	opts := []slack.MsgOption{
		slack.MsgOptionRecipientUserID(recipient),
		slack.MsgOptionRecipientTeamID(recipientTeam),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := t.api.StartStreamContext(ctx, channel, opts...)
	return ts, err
}

// appendStream writes Markdown into an open stream, always as a chunk: once a task chunk has been
// sent, Slack refuses markdown_text appends.
func (t *slackTransport) appendStream(ctx context.Context, channel, ts, markdown string) error {
	_, _, err := t.api.AppendStreamContext(ctx, channel, ts, slack.MsgOptionChunks(slack.NewMarkdownTextChunk(markdown)))
	return err
}

// streamTask posts the task card with this id on an open stream, or rewrites it in place.
func (t *slackTransport) streamTask(ctx context.Context, channel, ts, id, title string, status taskStatus, details string) error {
	c := slack.NewTaskUpdateChunk(id, title)
	c.Status, c.Details = slackTaskStatus(status), details
	_, _, err := t.api.AppendStreamContext(ctx, channel, ts, slack.MsgOptionChunks(c))
	return err
}

func slackTaskStatus(s taskStatus) slack.TaskCardStatus {
	switch s {
	case taskDone:
		return slack.TaskCardStatusComplete
	case taskFailed:
		return slack.TaskCardStatusError
	}
	return slack.TaskCardStatusInProgress
}

// stopStream closes a stream with whatever Markdown is left and the footer as a context line.
func (t *slackTransport) stopStream(ctx context.Context, channel, ts, markdown, footer string) (string, error) {
	var chunks []slack.StreamChunk
	if markdown != "" {
		chunks = append(chunks, slack.NewMarkdownTextChunk(markdown))
	}
	if footer != "" {
		chunks = append(chunks, slack.NewBlocksChunk(slack.NewContextBlock(replyFooterBlockID, slack.NewTextBlockObject(slack.MarkdownType, footer, false, false))))
	}
	var opts []slack.MsgOption
	if len(chunks) > 0 {
		opts = append(opts, slack.MsgOptionChunks(chunks...))
	}
	_, out, err := t.api.StopStreamContext(ctx, channel, ts, opts...)
	return out, err
}
