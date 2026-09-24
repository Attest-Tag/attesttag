package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// A streamed answer in Teams, which Microsoft offers in one-to-one chats only. A stream is a run of
// typing activities under one stream id, each carrying the whole answer so far — Teams replaces the
// text rather than appending to it, and refuses an update that does not begin with what it already
// shows — closed by a message carrying the finished answer. Before the first words it can carry
// informative updates, the line Teams shows while the bot works: that is where the tool call in
// flight is said, as the task card says it in Slack.
//
// Three rules from Microsoft shape the rest. A stream takes one request a second, and Microsoft
// asks for the words to be buffered for a second and a half, so an update that comes sooner is
// folded into the next one — which loses nothing when every update carries everything. A stream
// must close within two minutes of opening, so a turn that has spent most of that on tools does not
// start its answer on one, and closes with a plain message instead: what it would have posted had it
// never streamed. And a person can press Stop, after which nothing more may be sent on the stream,
// and the answer is not posted some other way either, because they asked for it not to be.
//
// Channels and group chats cannot stream at all, and their answers are posted whole.

const (
	msStreamGap   = 1500 * time.Millisecond
	msStreamWords = 90 * time.Second  // the oldest a stream may be and still start showing words
	msStreamClose = 110 * time.Second // and still be closed as a stream rather than superseded
)

// msStreamInfo is the entity that makes an activity part of a stream.
type msStreamInfo struct {
	Type       string `json:"type"` // always streaminfo
	StreamID   string `json:"streamId,omitempty"`
	StreamType string `json:"streamType"`               // informative | streaming | final
	Sequence   int    `json:"streamSequence,omitempty"` // from 1, and never on the final message
}

// msStream is one answer being streamed.
type msStream struct {
	conv      string
	id        string          // the stream's id: the id Teams gave its first update
	seq       int             // the streamSequence of the last update sent
	opened    time.Time       // when the first update was sent
	last      time.Time       // when the latest was
	raw       strings.Builder // the answer so far, in the internal dialect
	shown     string          // what the latest streaming update carried, which every later one extends
	shownRaw  string          // the same, before rendering
	stopped   bool            // the person pressed Stop
	broken    bool            // Teams refused an update, so the answer ends as a plain message
	wordsSent bool            // the answer has begun, after which informative updates are not shown
}

var msStreamKeys atomic.Int64

func (t *msteamsTransport) stream(key string) (*msStream, error) {
	if v, ok := t.streams.Load(key); ok {
		return v.(*msStream), nil
	}
	return nil, errors.New("no open stream " + key)
}

// startStream opens a stream in a one-to-one chat and refuses one anywhere else. Nothing is sent
// yet: Teams refuses a stream that opens without text, and there is none until the first tool call
// or the first words. Until then the key is the streamer's own.
func (t *msteamsTransport) startStream(ctx context.Context, channel, threadTS, _, _ string) (string, error) {
	if !isTeamsPersonal(channel) {
		return "", errStreamUnsupported
	}
	conv, err := t.conversationFor(ctx, channel, threadTS)
	if err != nil {
		return "", err
	}
	key := "stream-" + strconv.FormatInt(msStreamKeys.Add(1), 10)
	t.streams.Store(key, &msStream{conv: conv})
	return key, nil
}

// streamTask says what the bot is doing, while it has not started answering. A finished tool call
// says nothing: the next one, or the answer, replaces it soon enough.
func (t *msteamsTransport) streamTask(ctx context.Context, _, key, _, title string, status taskStatus, _ string) error {
	s, err := t.stream(key)
	if err != nil {
		return err
	}
	if status != taskRunning || title == "" || s.wordsSent || !s.open(msStreamWords) || s.tooSoon() {
		return nil
	}
	if err := t.streamUpdate(ctx, s, "informative", truncate(title, 200)+"…", nil); err != nil && !s.stopped {
		return err
	}
	return nil
}

// appendStream adds words to the answer, and shows as much of it as can be shown for good.
func (t *msteamsTransport) appendStream(ctx context.Context, _, key, markdown string) error {
	s, err := t.stream(key)
	if err != nil {
		return err
	}
	s.raw.WriteString(markdown)
	if !s.open(msStreamWords) || s.tooSoon() {
		return nil
	}
	p := msStreamable(strings.TrimLeft(s.raw.String(), " \t\n"))
	if p == "" || p == s.shownRaw {
		return nil
	}
	text, mentions := t.messageText(ctx, p, false, "")
	text = strings.TrimRight(text, " \t\n")
	if !strings.HasPrefix(text, s.shown) { // cannot happen by construction; Teams would refuse it
		return nil
	}
	if err := t.streamUpdate(ctx, s, "streaming", text, mentions); err != nil {
		if s.stopped { // the person pressed Stop, which is not a failure
			return nil
		}
		return err
	}
	s.shown, s.shownRaw, s.wordsSent = text, p, true
	return nil
}

// stopStream closes the stream with the whole answer, or posts the answer as a plain message when
// there is no stream to close: none was ever opened, Teams refused it, or it is too old to close.
func (t *msteamsTransport) stopStream(ctx context.Context, channel, key, markdown, footer string) (string, error) {
	s, err := t.stream(key)
	if err != nil {
		return "", err
	}
	defer t.streams.Delete(key)
	s.raw.WriteString(markdown)
	if s.stopped {
		t.logStreamed(ctx, s, s.id, s.shownRaw)
		return s.id, nil
	}
	raw := strings.TrimSpace(s.raw.String())
	if raw == "" {
		return s.id, nil
	}
	body, mentions := t.messageText(ctx, raw, false, footer)
	if s.id == "" || s.broken || time.Since(s.opened) > msStreamClose || !strings.HasPrefix(body, s.shown) {
		return t.postMarkdown(ctx, channel, "", raw, footer)
	}
	info := msStreamInfo{Type: "streaminfo", StreamID: s.id, StreamType: "final"}
	act := msOutgoing{Type: "message", Text: body, TextFormat: "markdown", Entities: entities(mentions, info)}
	var out struct {
		ID string `json:"id"`
	}
	if err := t.call(ctx, "POST", activitiesPath(s.conv), act, &out); err != nil {
		if streamCanceled(err) {
			t.logStreamed(ctx, s, s.id, s.shownRaw)
			return s.id, nil
		}
		return "", err // the streamer posts the answer plainly instead
	}
	id := nonEmpty(out.ID, s.id)
	t.logStreamed(ctx, s, id, raw)
	return id, nil
}

// streamUpdate sends one typing activity on the stream, opening it if it is the first.
func (t *msteamsTransport) streamUpdate(ctx context.Context, s *msStream, kind, text string, mentions []msMention) error {
	info := msStreamInfo{Type: "streaminfo", StreamID: s.id, StreamType: kind, Sequence: s.seq + 1}
	act := msOutgoing{Type: "typing", Text: text, Entities: entities(mentions, info)}
	if kind == "streaming" {
		act.TextFormat = "markdown"
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := t.call(ctx, "POST", activitiesPath(s.conv), act, &out); err != nil {
		if streamCanceled(err) {
			s.stopped = true
		} else {
			s.broken = true
		}
		return err
	}
	now := time.Now()
	if s.id == "" {
		if out.ID == "" {
			s.broken = true
			return errors.New("Teams opened a stream without saying its id")
		}
		s.id, s.opened = out.ID, now
	}
	s.seq++
	s.last = now
	return nil
}

// logStreamed keeps a streamed answer in the conversation log, as send does for a posted one, so
// the next turn reads back what this one said.
func (t *msteamsTransport) logStreamed(ctx context.Context, s *msStream, id, text string) {
	if id == "" || text == "" {
		return
	}
	_ = t.c.store.LogTeamsMessage(ctx, t.teamID, msMessage{Channel: s.conv, Thread: msteamsChatThread, ID: id,
		UserID: t.c.botID(), UserName: "assistant", IsBot: true, Text: text})
}

// open is whether the stream can still take an update: it has not been stopped or refused, and it
// is younger than limit — or has not been opened yet, since its clock starts with its first update.
func (s *msStream) open(limit time.Duration) bool {
	return !s.stopped && !s.broken && (s.id == "" || time.Since(s.opened) < limit)
}

// tooSoon is an update that would come faster than Teams takes them.
func (s *msStream) tooSoon() bool { return !s.last.IsZero() && time.Since(s.last) < msStreamGap }

// streamCanceled is Teams saying the person pressed Stop.
func streamCanceled(err error) bool { return strings.Contains(err.Error(), "canceled by user") }

// msStreamable is the longest start of an answer being written that Teams can be shown now and will
// never have to take back. The renderer rewrites <@id> and <url|label> — only whole ones, and none
// inside code — so text cut through one of those, or inside a code span not yet closed, renders
// differently from the same text finished, and Teams refuses an update that does not begin with
// what it already shows. So the cut is at a space, before any of them.
func msStreamable(raw string) string {
	s := raw
	for {
		cut := strings.LastIndexAny(s, " \t\n")
		if cut < 0 {
			return ""
		}
		s = strings.TrimRight(s[:cut], " \t\n")
		end := len(s)
		if i := openCode(s); i >= 0 {
			end = i
		}
		if i := strings.LastIndexByte(s[:end], '<'); i >= 0 && strings.IndexByte(s[i:end], '>') < 0 {
			end = i
		}
		if end == len(s) {
			return s
		}
		s = s[:end]
	}
}

// openCode is where a code span or fence begins that text never closes, reading the markers the way
// convertOutsideCode does, or -1 when every one is closed.
func openCode(text string) int {
	off := 0
	for {
		tick := strings.IndexByte(text[off:], '`')
		if tick < 0 {
			return -1
		}
		start := off + tick
		marker := "`"
		if strings.HasPrefix(text[start:], "```") {
			marker = "```"
		}
		end := strings.Index(text[start+len(marker):], marker)
		if end < 0 {
			return start
		}
		off = start + len(marker) + end + len(marker)
	}
}
