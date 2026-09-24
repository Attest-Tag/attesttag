package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// Chat is one connected workspace as the rest of the app sees it: who is in it, what its
// conversations are called, and how to read and post in them. It owns the caches and the rules
// about stale or missing answers; the platform calls themselves go through t, so those rules
// hold the same way for a workspace on any platform.
type Chat struct {
	t transport
	// Platform is the chat platform the workspace is on, as the teams row records it.
	Platform  string
	BotUserID string
	BotID     string
	TeamID    string
	TeamName  string
	// OrgID is the organisation this workspace was connected to. It rides on the client so a
	// turn never has to re-derive it, and cannot derive it differently in two places.
	OrgID   int64
	names   sync.Map // userID -> display name
	chans   sync.Map // channelID -> name
	emails  sync.Map // userID -> userFacts, expiring
	byEmail sync.Map // email -> userID
	convs   sync.Map // channelID -> convInfo
	members sync.Map // channelID -> memberSet
}

// transport is what a workspace needs from the chat platform it is on. Every method is a plain
// question or a plain post: the caching, the expiry and the fail-closed rules sit on *Chat
// above it, so a second platform inherits them instead of reimplementing them.
type transport interface {
	userName(ctx context.Context, id string) (string, error)
	userFacts(ctx context.Context, id string) (userFacts, error)
	userByEmail(ctx context.Context, email string) (string, error)
	conversation(ctx context.Context, id string) (name string, info convInfo, err error)
	members(ctx context.Context, channel string) (map[string]bool, error)
	replies(ctx context.Context, channel, threadTS string) ([]rawMessage, error)
	history(ctx context.Context, channel string, since time.Time, limit int) ([]rawMessage, error)

	postText(ctx context.Context, channel, threadTS, text string) (string, error)
	postMarkdown(ctx context.Context, channel, threadTS, md, footer string) (string, error)
	updateMarkdown(ctx context.Context, channel, ts, md, footer, fallback string) error
	postCard(ctx context.Context, channel, threadTS string, card Card, fallback string) (string, string, error)
	updateCard(ctx context.Context, channel, ts string, card Card, fallback string) error
	deleteMessage(ctx context.Context, channel, ts string) error
	addReaction(ctx context.Context, channel, ts, emoji string) error
	permalink(ctx context.Context, channel, ts string) string
	uploadContent(ctx context.Context, channel, threadTS, filename, title, content, snippetType string) (fileID, permalink string, err error)

	setStatus(ctx context.Context, channel, threadTS, status string)
	suggestPrompts(ctx context.Context, channel, threadTS string, prompts [][2]string)
	setTitle(ctx context.Context, channel, threadTS, title string)

	// A streamed answer: one message opened, written into as the answer arrives, and closed. A
	// platform or a conversation that cannot stream refuses startStream, and the streamer posts
	// the whole answer once instead.
	startStream(ctx context.Context, channel, threadTS, recipient, recipientTeam string) (string, error)
	appendStream(ctx context.Context, channel, ts, markdown string) error
	streamTask(ctx context.Context, channel, ts, id, title string, status taskStatus, details string) error
	stopStream(ctx context.Context, channel, ts, markdown, footer string) (string, error)
}

// taskStatus is how far the tool call a streamed answer is showing has got.
type taskStatus int

const (
	taskRunning taskStatus = iota
	taskDone
	taskFailed
)

// rawMessage is a message as its platform hands it over: text already flattened out of any card
// it arrived in, and nobody in it named yet.
type rawMessage struct {
	UserID, BotID, Username, Text, TS string
	Replies                           int          // on a top-level message, how many hang off it
	Files                             []slack.File // Slack's own file objects, until files are platform-neutral
}

// Card is a message with answers on it: a few paragraphs of Markdown, the buttons someone can
// press, and a line of small print under them. Every card attest_tag posts has that shape, so it
// is described once here and each platform draws it its own way — which also means a card that
// has been answered is the same card with its buttons gone, rather than a list of blocks with the
// last two cut off and a hope that they were the right two.
type Card struct {
	Parts   []CardPart
	Buttons []Button
	Footer  string // small print under the buttons, Markdown
}

// CardPart is one paragraph of a card, or a line of small print between paragraphs.
type CardPart struct {
	Markdown string
	Small    bool
}

// Button is one answer on a card. A press comes back as ActionID and Value; a button with a URL
// opens the link as well. Who may press is never the card's to decide: every press is checked
// against the presser when it arrives, because no platform restricts a button to a person.
type Button struct {
	ActionID, Value, Label string
	Primary                bool // the answer the card expects, drawn in the platform's accent
	URL                    string
}

// platformSlack is the platform every workspace was on before there was a column to say so.
const platformSlack = "slack"

// workspaceNoun is what a workspace is called on its platform, in sentences written for people
// and for the model: a Slack workspace, a Microsoft Teams organisation.
func workspaceNoun(sl *Chat) string {
	if sl != nil && sl.Platform == platformMSTeams {
		return "Microsoft Teams organisation"
	}
	return "Slack workspace"
}

// isDirectConversation says whether a conversation is a one-to-one chat with the bot: a Slack D…
// id or a Teams a:… one. It decides whether an approval card gets buttons and how a place is named
// in a sentence, so a conversation it does not recognise is treated as a channel.
func isDirectConversation(channel string) bool {
	return strings.HasPrefix(channel, "D") || isTeamsPersonal(channel)
}

// platformName is the platform itself, for "couldn't reach Slack"-shaped sentences.
func platformName(sl *Chat) string {
	if sl != nil && sl.Platform == platformMSTeams {
		return "Microsoft Teams"
	}
	return "Slack"
}

var errNotSlack = errors.New("this workspace is not on Slack")

// warnStreamStart reports a stream that could not be opened — unless the conversation simply
// cannot stream, which is every Teams channel on every tool call and not worth a warning each time.
func warnStreamStart(err error) {
	if !errors.Is(err, errStreamUnsupported) {
		slog.Warn("startStream failed", "err", err)
	}
}

// slackAPI is the raw Slack client behind a workspace, for the few things only Slack does:
// search, pins, the channel roster sync, revoking a token. A workspace that is not on Slack — or
// a test double with no client at all — gets an error to handle rather than a nil pointer to
// trip over.
func (s *Chat) slackAPI() (*slack.Client, error) {
	if t, ok := s.t.(*slackTransport); ok && t.api != nil {
		return t.api, nil
	}
	return nil, errNotSlack
}

// download reads the bytes of a file attached in workspace teamID, from wherever its platform
// keeps them: Slack's file host with the workspace's token, or wherever Teams said the file is.
func (s *Chat) download(ctx context.Context, slacks *ChatRegistry, teamID string, f slack.File) ([]byte, error) {
	if s != nil {
		if t, ok := s.t.(*msteamsTransport); ok {
			return t.downloadFile(ctx, f)
		}
	}
	return downloadSlackFile(ctx, slacks, teamID, f)
}

func NewSlack(api *slack.Client) (*Chat, error) {
	auth, err := api.AuthTest()
	if err != nil {
		return nil, err
	}
	return &Chat{t: &slackTransport{api: api}, Platform: platformSlack, BotUserID: auth.UserID, BotID: auth.BotID,
		TeamID: auth.TeamID, TeamName: auth.Team}, nil
}

func (s *Chat) UserName(ctx context.Context, id string) string {
	if id == "" {
		return "someone"
	}
	if id == s.BotUserID {
		return "assistant"
	}
	if isEmailRequester(id) {
		return "a forwarded email" // no account behind it; users.info would 404 on every call
	}
	return s.DisplayName(ctx, id)
}

// DisplayName is UserName without the shims a transcript wants. UserName answers "someone" for
// an empty id and "assistant" for the bot's own — right when narrating a conversation, wrong on
// a console surface that has to name the installed app itself. Both share the one name cache,
// and both fall back to the raw id when Slack cannot be reached.
func (s *Chat) DisplayName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if v, ok := s.names.Load(id); ok {
		return v.(string)
	}
	name, err := s.t.userName(ctx, id)
	if err != nil {
		return id
	}
	s.names.Store(id, name)
	return name
}

// maxNameLookups caps the users.info round trips one rewrite pays for. Names are cached for the
// life of the process, so this only ever bites the first time a thread names a crowd — and a
// mention past the cap keeps its id, which is still a person the model can look up on purpose.
const maxNameLookups = 20

// NameMentions rewrites the mentions in a message into the form the rest of a transcript already
// uses — `Alex Kim (<@U123>)` — and drops the bot's own mention, which is only the address
// on the envelope.
//
// Every mention used to be deleted outright, so "@attest_tag please add @alex to my 6:30 call"
// reached the model as "please add to my 6:30 call" and the best it could do was ask who was
// meant. The id stays beside the name because the id is what every tool takes; the name is not a
// secret either way, since anyone who can read the message sees it rendered in Slack.
func (s *Chat) NameMentions(ctx context.Context, text string) string {
	budget := maxNameLookups
	return s.nameMentions(ctx, text, &budget)
}

// nameMentions is NameMentions over a budget shared by several messages, so listing a whole
// thread costs one allowance rather than one per line.
func (s *Chat) nameMentions(ctx context.Context, text string, budget *int) string {
	if !strings.Contains(text, "<@") {
		return text
	}
	return mentionRe.ReplaceAllStringFunc(text, func(m string) string {
		id := mentionRe.FindStringSubmatch(m)[1]
		if id == s.BotUserID {
			return ""
		}
		if _, cached := s.names.Load(id); !cached {
			if *budget <= 0 {
				return "<@" + id + ">"
			}
			*budget--
		}
		// An id Slack will not name — a deactivated account, a missing scope — comes back as
		// itself. Leave the mention alone rather than writing "U123 (<@U123>)".
		if name := s.DisplayName(ctx, id); name != "" && name != id {
			return name + " (<@" + id + ">)"
		}
		return "<@" + id + ">"
	})
}

func (s *Chat) ChannelName(ctx context.Context, id string) string {
	if v, ok := s.chans.Load(id); ok {
		return v.(string)
	}
	name, ci, err := s.t.conversation(ctx, id)
	if err != nil {
		return id
	}
	if ci.IsIM {
		name = "DM"
	}
	s.chans.Store(id, name)
	return name
}

// ---- who may see what ----

// A channel id does not say whether a channel is private: a public channel converted to
// private keeps its "C". So privacy and membership are asked of Slack and cached — briefly,
// because both change, and the answers gate what the bot will read on someone's behalf.
const (
	convInfoTTL = 10 * time.Minute
	membersTTL  = 2 * time.Minute
)

type convInfo struct {
	IsPrivate, IsIM, IsMPIM bool
	at                      time.Time
}

type memberSet struct {
	ids map[string]bool
	at  time.Time
}

func (s *Chat) conv(ctx context.Context, id string) (convInfo, error) {
	if v, ok := s.convs.Load(id); ok {
		if ci := v.(convInfo); time.Since(ci.at) < convInfoTTL {
			return ci, nil
		}
	}
	_, ci, err := s.t.conversation(ctx, id)
	if err != nil {
		return convInfo{}, err
	}
	ci.at = time.Now()
	s.convs.Store(id, ci)
	return ci, nil
}

// IsPrivateConversation reports whether a conversation is anything other than a public
// channel. A lookup that fails counts as private: "we could not tell" must not read as
// "anyone may see it".
func (s *Chat) IsPrivateConversation(ctx context.Context, id string) bool {
	ci, err := s.conv(ctx, id)
	if err != nil {
		slog.Debug("conversations.info", "channel", id, "err", err)
		return true
	}
	return ci.IsPrivate || ci.IsIM || ci.IsMPIM
}

// IsMember reports whether a user belongs to a conversation. The roster is cached for a
// couple of minutes: this runs per tool call, and the private channels it is asked about are
// small.
func (s *Chat) IsMember(ctx context.Context, channel, user string) (bool, error) {
	if v, ok := s.members.Load(channel); ok {
		if ms := v.(memberSet); time.Since(ms.at) < membersTTL {
			return ms.ids[user], nil
		}
	}
	ids, err := s.t.members(ctx, channel)
	if err != nil {
		return false, err
	}
	s.members.Store(channel, memberSet{ids: ids, at: time.Now()})
	return ids[user], nil
}

// An account is not a fixed thing: people leave, get deactivated, or are downgraded to a guest.
// The email cache used to have no expiry at all, which meant that on a long-lived instance
// (Cloud Run runs this one for weeks with --min-instances 1) someone who had left the company
// kept passing mayUseBot for the life of the process. Same reasoning as convInfoTTL: the answer
// gates what the bot will do on a person's behalf, so it has to go stale.
const userInfoTTL = 10 * time.Minute

// userFacts is everything about an account that a gate cares about.
type userFacts struct {
	Email      string // lowercased; empty when the account has none or the scope is missing
	TeamID     string
	Deleted    bool
	Bot        bool
	Restricted bool // single-channel or multi-channel guest
	at         time.Time
}

// user reads an account, cached for userInfoTTL. fresh=true skips the cache: an approval is worth
// one API call to be sure the person still works here.
func (s *Chat) user(ctx context.Context, id string, fresh bool) (userFacts, error) {
	if id == "" {
		return userFacts{}, errors.New("no user id")
	}
	if !fresh {
		if v, ok := s.emails.Load(id); ok {
			if uf := v.(userFacts); time.Since(uf.at) < userInfoTTL {
				return uf, nil
			}
		}
	}
	uf, err := s.t.userFacts(ctx, id)
	if err != nil {
		return userFacts{}, err
	}
	uf.at = time.Now()
	s.emails.Store(id, uf)
	return uf, nil
}

// UserEmail returns a user's Slack profile email, lowercased and cached. It is empty when the
// account has none (a bot) or the workspace hides email from apps; the caller decides what
// that means.
func (s *Chat) UserEmail(ctx context.Context, id string) (string, error) {
	uf, err := s.user(ctx, id, false)
	return uf.Email, err
}

// UserFacts reads an account straight from Slack, ignoring the cache. Used where a stale answer
// would be a security hole rather than a slow page.
func (s *Chat) UserFacts(ctx context.Context, id string) (userFacts, error) {
	return s.user(ctx, id, true)
}

// UserByEmail resolves an address to a Slack user id, so approvers can be named by email rather
// than by an id nobody can read. It needs the same users:read.email scope mayUseBot depends on.
func (s *Chat) UserByEmail(ctx context.Context, email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", errors.New("no email")
	}
	if v, ok := s.byEmail.Load(email); ok {
		if hit := v.(emailHit); time.Since(hit.at) < userInfoTTL {
			return hit.id, nil
		}
	}
	id, err := s.t.userByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	s.byEmail.Store(email, emailHit{id: id, at: time.Now()})
	return id, nil
}

// emailHit is one cached address lookup. It expires like the other per-user caches: an approver
// whose address moved to another account must not keep answering as the old one for the life of
// the process.
type emailHit struct {
	id string
	at time.Time
}

type ThreadMsg struct {
	UserID, Name, Text, TS string
	IsBot                  bool
	Files                  []string
	files                  []slack.File
}

func (s *Chat) toMsg(ctx context.Context, m rawMessage) ThreadMsg {
	tm := ThreadMsg{UserID: m.UserID, Text: m.Text, TS: m.TS, IsBot: m.BotID != "" || m.UserID == s.BotUserID}
	tm.Name = s.UserName(ctx, m.UserID)
	if m.BotID != "" && m.UserID == "" {
		tm.Name = m.Username
		if tm.Name == "" {
			tm.Name = "bot"
		}
	}
	for _, f := range m.Files {
		tm.Files = append(tm.Files, f.Name)
	}
	tm.files = m.Files
	return tm
}

// Thread returns a thread's messages oldest-first, at most limit (the most recent ones win).
func (s *Chat) Thread(ctx context.Context, channel, threadTS string, limit int) ([]ThreadMsg, error) {
	all, err := s.t.replies(ctx, channel, threadTS)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(all) > limit {
		root := all[0]
		all = append([]rawMessage{root}, all[len(all)-limit+1:]...)
	}
	out := make([]ThreadMsg, 0, len(all))
	for _, m := range all {
		out = append(out, s.toMsg(ctx, m))
	}
	return out, nil
}

// History returns recent top-level channel messages, oldest-first.
func (s *Chat) History(ctx context.Context, channel string, since time.Time, limit int) ([]ThreadMsg, error) {
	msgs, err := s.t.history(ctx, channel, since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ThreadMsg, 0, len(msgs))
	for _, m := range msgs {
		tm := s.toMsg(ctx, m)
		if m.Replies > 0 {
			tm.Text += fmt.Sprintf(" [thread with %d replies, ts=%s]", m.Replies, m.TS)
		}
		out = append(out, tm)
	}
	return out, nil
}

// AddReaction puts an emoji on one message.
func (s *Chat) AddReaction(ctx context.Context, channel, ts, emoji string) error {
	return s.t.addReaction(ctx, channel, ts, emoji)
}

// Permalink is a link to one message, or "" when the platform will not give one.
func (s *Chat) Permalink(ctx context.Context, channel, ts string) string {
	return s.t.permalink(ctx, channel, ts)
}

// ---- assistant (agent) UI ----

// SetStatus shows "is thinking…"-style status on a thread. Empty status clears it. A thread that
// cannot show one ignores it.
func (s *Chat) SetStatus(ctx context.Context, channel, threadTS, status string) {
	s.t.setStatus(ctx, channel, threadTS, status)
}

func (s *Chat) SuggestPrompts(ctx context.Context, channel, threadTS string, prompts [][2]string) {
	s.t.suggestPrompts(ctx, channel, threadTS, prompts)
}

func (s *Chat) SetTitle(ctx context.Context, channel, threadTS, title string) {
	s.t.setTitle(ctx, channel, threadTS, title)
}

// PostMarkdown posts a complete (non-streamed) reply as Markdown, with an optional small grey
// context footer ("model · Configure").
func (s *Chat) PostMarkdown(ctx context.Context, channel, threadTS, md, footer string) (string, error) {
	return s.t.postMarkdown(ctx, channel, threadTS, md, footer)
}

func (s *Chat) PostText(ctx context.Context, channel, threadTS, text string) (string, error) {
	return s.t.postText(ctx, channel, threadTS, text)
}

// ---- streaming ----

// Streamer is one answer as it is written: a message opened on the platform and filled in as the
// words arrive, or — where the platform or the conversation cannot stream — the whole answer
// posted once at the end. The buffering, the filtering and that fallback are the same everywhere,
// so they live here; only opening, appending to and closing the stream are the transport's.
// Deltas are buffered and flushed about once a second (Slack's appendStream is Tier 4).
type Streamer struct {
	s          *Chat
	channel    string
	threadTS   string
	userID     string
	ts         string
	mu         sync.Mutex
	pending    strings.Builder
	lastFlush  time.Time
	started    bool
	closed     bool
	tasks      map[string]string
	failed     bool
	fallbackMD strings.Builder
	filter     markupFilter
	// silent is a streamer that keeps the answer and posts none of it. It is not the same as
	// one that failed to start: a failed streamer still falls back to a plain post, which is
	// exactly what a quiet routine must never do.
	silent bool
}

func (s *Chat) NewStreamer(channel, threadTS, userID string) *Streamer {
	st := &Streamer{s: s, channel: channel, threadTS: threadTS, userID: userID, tasks: map[string]string{}}
	// A stream is addressed to a person, and an email turn has none: Slack would be asked to
	// resolve "email:C0123" and would answer invalid_arguments. Starting already failed skips
	// that round trip and takes the fallback a failed streamer already has — the finished answer
	// posted as one message, which is the right shape here anyway. Nobody is watching it arrive.
	st.failed = isEmailRequester(userID)
	return st
}

// NewSilentStreamer collects the answer without showing any of it. A quiet routine runs on one
// of these: the turn writes into it exactly as it always does, and whether any of it reaches
// the channel is decided afterwards, by the routine, from the finished text.
func (s *Chat) NewSilentStreamer(channel, threadTS, userID string) *Streamer {
	st := s.NewStreamer(channel, threadTS, userID)
	st.silent = true
	return st
}

func (st *Streamer) start(ctx context.Context) error {
	if st.started || st.silent {
		return nil
	}
	ts, err := st.s.t.startStream(ctx, st.channel, st.threadTS, st.userID, st.s.TeamID)
	if err != nil {
		st.failed = true
		return err
	}
	st.ts, st.started, st.lastFlush = ts, true, time.Now()
	return nil
}

// activityCard is the id every tool call on a turn shares. Slack keys task cards by id, so
// reusing one rewrites that row in place: the stream carries what the bot is doing now instead
// of a checklist of everything it has already done, which grows past the height of the message
// and pushes the answer out of sight on a turn that takes many tools.
const activityCard = "activity"

// Task posts/updates a task card (the visible row for the tool call in flight).
func (st *Streamer) Task(ctx context.Context, id, title string, status taskStatus, details string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.failed || st.silent {
		return
	}
	if err := st.start(ctx); err != nil {
		warnStreamStart(err)
		return
	}
	st.flushLocked(ctx)
	if err := st.s.t.streamTask(ctx, st.channel, st.ts, id, title, status, details); err != nil {
		slog.Warn("task chunk failed", "err", err)
	}
}

// Write buffers a markdown delta, less any markup that must never be shown. The filtering
// happens here rather than on the finished answer because an append-once stream has no way back:
// whatever this writes is what the thread keeps.
func (st *Streamer) Write(ctx context.Context, delta string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	safe := st.filter.push(delta)
	if safe == "" {
		return
	}
	st.fallbackMD.WriteString(safe)
	if st.failed || st.silent {
		return // kept, so streamTail still works; never sent
	}
	if err := st.start(ctx); err != nil {
		warnStreamStart(err)
		return
	}
	st.pending.WriteString(safe)
	if time.Since(st.lastFlush) > 900*time.Millisecond {
		st.flushLocked(ctx)
	}
}

// Replay shows text that has already been generated the way a live stream would show it.
//
// It exists because the alternative was generating the answer twice. The turn loop calls the
// model without streaming, and then, to make the words appear rather than land in one block, it
// used to send the entire conversation back for a second, streamed generation of the same reply
// -- the largest prompt of the turn, bought twice, for a visual effect. Worse, the second
// generation is a different sample than the first: the text that was logged and the text the
// channel saw could differ. Replaying the finished text costs nothing and is exactly what the
// turn decided to say.
//
// The pacing is what Write already does -- it flushes at most every 900ms -- so the chunks are
// spread over a budget rather than sent as fast as the loop can write them. The budget is short
// on purpose: this is the tail of a turn somebody is waiting on.
func (st *Streamer) Replay(ctx context.Context, text string) {
	if text == "" || st.silent {
		return
	}
	// Nothing to pace for: a failed streamer falls back to one plain post at the end, so spacing
	// the writes out would buy a delay and no reveal.
	if st.failed {
		st.Write(ctx, text)
		return
	}
	const (
		budget = 3 * time.Second
		chunks = 5
	)
	parts := replayChunks(text, chunks)
	for i, part := range parts {
		if i > 0 {
			select {
			case <-time.After(budget / chunks):
			case <-ctx.Done():
				// Out of time: write the rest in one go rather than losing it.
				st.Write(ctx, strings.Join(parts[i:], ""))
				return
			}
		}
		st.Write(ctx, part)
	}
}

// replayChunks splits text into at most n pieces that join back into exactly text. It splits on
// runes rather than bytes: a chunk that ends halfway through a multi-byte character would post
// the replacement glyph and lose the character, which is the one thing a replay must not do.
func replayChunks(text string, n int) []string {
	runes := []rune(text)
	if len(runes) == 0 || n < 1 {
		return nil
	}
	size := max((len(runes)+n-1)/n, 1)
	out := make([]string, 0, n)
	for i := 0; i < len(runes); i += size {
		out = append(out, string(runes[i:min(i+size, len(runes))]))
	}
	return out
}

func (st *Streamer) flushLocked(ctx context.Context) {
	if st.pending.Len() == 0 || !st.started {
		return
	}
	if err := st.s.t.appendStream(ctx, st.channel, st.ts, st.pending.String()); err != nil {
		slog.Warn("appendStream failed", "err", err)
	}
	st.pending.Reset()
	st.lastFlush = time.Now()
}

// Stop closes the stream with the remaining text and an optional footer (rendered as a
// small grey context line, like "model · Configure"). If streaming never worked it falls
// back to a plain post so the user always gets an answer. Returns the message ts.
func (st *Streamer) Stop(ctx context.Context, tail, footer string) (string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed { // a stopped run can race the turn it interrupted; only one of them closes
		return st.ts, nil
	}
	st.closed = true
	st.fallbackMD.WriteString(tail)
	if st.silent {
		return "", nil
	}
	if st.failed || !st.started {
		text := strings.TrimSpace(st.fallbackMD.String())
		if text == "" {
			return "", nil
		}
		return st.s.PostMarkdown(ctx, st.channel, st.threadTS, text, footer)
	}
	st.pending.WriteString(tail)
	ts, err := st.s.t.stopStream(ctx, st.channel, st.ts, st.pending.String(), footer)
	st.pending.Reset()
	if err != nil {
		slog.Warn("stopStream failed, falling back", "err", err)
		return st.s.PostMarkdown(ctx, st.channel, st.threadTS, strings.TrimSpace(st.fallbackMD.String()), footer)
	}
	if ts == "" {
		ts = st.ts
	}
	return ts, nil
}

// Started reports whether a streamed message exists yet.
func (st *Streamer) Started() bool { st.mu.Lock(); defer st.mu.Unlock(); return st.started }
