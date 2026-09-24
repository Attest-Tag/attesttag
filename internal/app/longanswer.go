package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// Long answers go up as a Markdown file with a short lead in the message, so a 4,000-word
// report doesn't become a wall of text in the thread.

func (s *Chat) uploadMarkdown(ctx context.Context, channel, threadTS, title, md string) error {
	_, _, err := s.uploadContent(ctx, channel, threadTS, slug(title)+".md", title, md, "markdown")
	return err
}

// uploadContent posts content as a file in the thread and reports the file id and permalink.
// snippetType may be empty, in which case the platform renders by the filename's extension.
func (s *Chat) uploadContent(ctx context.Context, channel, threadTS, filename, title, content, snippetType string) (fileID, permalink string, err error) {
	return s.t.uploadContent(ctx, channel, threadTS, filename, title, content, snippetType)
}

// filesInThread says whether the platform takes a file the bot uploads into a thread. Teams does not
// — a bot's file in a Teams channel means SharePoint and a consent this app does not ask for
// (msteamsTransport.uploadContent) — so an answer there is never cut to a lead and a file.
func (s *Chat) filesInThread() bool {
	return s != nil && s.Platform != platformMSTeams
}

// splitLongAnswer returns (lead, rest): the first paragraph(s) up to ~600 chars as the message,
// the whole thing as the file when the answer is longer than the threshold.
func splitLongAnswer(text string, threshold int) (lead string, full string, isLong bool) {
	if threshold <= 0 || len(text) <= threshold {
		return text, "", false
	}
	paras := strings.Split(text, "\n\n")
	var b strings.Builder
	for _, p := range paras {
		if b.Len()+len(p) > 600 && b.Len() > 0 {
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p)
	}
	lead = strings.TrimSpace(b.String())
	if lead == "" {
		lead = truncate(text, 600)
	}
	return lead + "\n\n_Full answer attached as a file._", text, true
}

// messageAnswerCap is the most text one message may carry: Slack's markdown block holds 12,000
// characters, and the header shares the block with the answer.
const messageAnswerCap = 11_500

// editWithResults replaces a routine's header message with the outcome, Claude-Tag style.
func (s *Chat) editWithResults(ctx context.Context, channel, ts, header, result, footer string) {
	md := header + "\n\n" + truncate(result, messageAnswerCap)
	note := "_edited with results_" + map[bool]string{true: " · " + footer, false: ""}[footer != ""]
	if err := s.t.updateMarkdown(ctx, channel, ts, md, note, truncate(oneLine(result), 200)); err != nil {
		slog.Warn("edit with results failed", "err", err)
	}
}

// settleAnswer leaves the answer in one place and only one. Everything the run wrote while it
// worked went into the thread; now that it has finished, an answer short enough to read in the
// channel is moved up into the message itself and the thread copy comes down, and one too long
// for that stays in the thread, with the message saying where it is. Both at once — which is
// what this used to do, with the copy in the message truncated — meant reading the same brief
// twice, and the first time you read it it stopped halfway.
func (s *Chat) settleAnswer(ctx context.Context, channel, ts, header, answer, answerTS string, msgLimit int) {
	if answer == "" {
		return
	}
	if msgLimit <= 0 || msgLimit > messageAnswerCap {
		msgLimit = messageAnswerCap
	}
	if len(answer) > msgLimit {
		s.settleLongAnswer(ctx, channel, ts, header, answer, answerTS)
		return
	}
	s.editWithResults(ctx, channel, ts, header, answer, "")
	if answerTS == "" {
		return // nothing went to the thread, so there is nothing to take down
	}
	// Taking the copy down second is deliberate: an edit that fails leaves the answer in the
	// thread, where a delete that fails leaves it in both places — and twice beats nowhere.
	if err := s.t.deleteMessage(ctx, channel, answerTS); err != nil {
		slog.Warn("the thread copy of the answer could not be removed", "err", err)
	}
}

// settleLongAnswer ends a run whose answer will not fit in a message. The whole of it goes up as
// a file in the thread and the message keeps the lead, so what the channel sees is a summary it
// can read where it stands and a file it opens only if it wants the rest. Before this the
// message said "the full answer is in the thread" and the thread held the wall of text that was
// too long to read in the first place — the same reading problem, moved one click away.
//
// The file goes first because everything after it is tidying up, and each step is ordered so
// that failing leaves the answer somewhere rather than nowhere: no file, and the thread copy
// stays with the message pointing at it, exactly as it behaved before files were an option.
func (s *Chat) settleLongAnswer(ctx context.Context, channel, ts, header, answer, answerTS string) {
	lead, full, _ := splitLongAnswer(answer, 1) // 1: the decision to file it is already made
	if err := s.uploadMarkdown(ctx, channel, ts, "Full answer", full); err != nil {
		slog.Warn("long answer upload failed", "err", err)
		if answerTS != "" {
			s.editWithResults(ctx, channel, ts, header, answerIsInThread(s.Permalink(ctx, channel, answerTS)), "")
			return
		}
		s.editWithResults(ctx, channel, ts, header, answer, "") // truncated by the edit, and all there is
		return
	}
	s.editWithResults(ctx, channel, ts, header, lead, "")
	if answerTS == "" {
		return // nothing went to the thread, so there is nothing to take down
	}
	if err := s.t.deleteMessage(ctx, channel, answerTS); err != nil {
		slog.Warn("the thread copy of the answer could not be removed", "err", err)
	}
}

// answerIsInThread is what the message says instead of the answer when the answer was too long
// to bring into it.
func answerIsInThread(permalink string) string {
	if permalink == "" {
		return "_Too long for one message — the full answer is in the thread._"
	}
	return fmt.Sprintf("_Too long for one message — <%s|the full answer is in the thread>._", permalink)
}

// preview is a lead for a longer text: the first n bytes, cut at a rune boundary and, where one
// is close by, a word boundary. Unlike truncate it does not say how much was left out — a
// routine's header is not where anyone needs to learn that its prompt runs to nine thousand
// characters.
func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if i := strings.LastIndexAny(s[:cut], " \t\n"); i > 0 && i > cut-40 {
		cut = i
	}
	return strings.TrimRight(s[:cut], " \t\n") + "…"
}
