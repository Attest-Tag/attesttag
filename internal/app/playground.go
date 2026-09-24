package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The playground runs a real turn against a channel's real configuration and hands the answer
// back over HTTP: the same access resolution, the same tools, the same model routing, the same
// budget. It exists because until now the only way to find out what a channel's settings do was
// to change them and wait for somebody to ask something in the channel.
//
// Two things are deliberately not real. Nothing that changes data is sent — a write stops at the
// gate and comes back as "this is what would have been asked for" — and the shared memory is not
// written, because a test that leaves things behind in the channel it was testing is not a test.
// Everything else, including every read against real credentials, happens exactly as it would.

const playgroundPrefix = "playground:"

// newPlaygroundThread mints a thread key for a playground conversation. Deliberately not
// ts-shaped: these keys are stored next to real Slack ones, and anything downstream that tried
// to resolve this against Slack should fail loudly rather than fetch some unrelated message.
func newPlaygroundThread() string {
	b := make([]byte, 8)
	rand.Read(b)
	return playgroundPrefix + hex.EncodeToString(b)
}

func isPlaygroundThread(ts string) bool { return strings.HasPrefix(ts, playgroundPrefix) }

type ThreadTurn struct {
	Role, UserID, Content, At string
}

// ThreadTurns replays one thread from the turns table, oldest first. The playground's history
// lives here rather than in Slack, because for a playground thread there is no Slack.
func (s *Store) ThreadTurns(ctx context.Context, teamID, channel, threadTS string) []ThreadTurn {
	rows, err := s.db.QueryContext(ctx, `select role, coalesce(user_id,''), content, created_at
		from turns where team_id=? and channel=? and thread_ts=? and role in ('user','assistant') order by id`,
		teamID, channel, threadTS)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ThreadTurn
	for rows.Next() {
		var t ThreadTurn
		if err := rows.Scan(&t.Role, &t.UserID, &t.Content, &t.At); err != nil {
			return out
		}
		out = append(out, t)
	}
	return out
}

// ForgetThread drops a playground conversation. The tool calls it made are left where they are:
// they ran against real credentials and belong in the audit trail whether or not the person who
// ran them wants the transcript back.
func (s *Store) ForgetThread(ctx context.Context, teamID, channel, threadTS string) {
	s.db.ExecContext(ctx, `delete from turns where team_id=? and channel=? and thread_ts=?`, teamID, channel, threadTS)
	s.db.ExecContext(ctx, `delete from sessions where team_id=? and channel=? and thread_ts=?`, teamID, channel, threadTS)
}

// LastToolCallID is the high-water mark taken before a run, so the calls that run makes can be
// read back afterwards without guessing from timestamps.
func (s *Store) LastToolCallID(ctx context.Context, orgID int64) int64 {
	var id int64
	s.db.QueryRowContext(ctx, `select coalesce(max(id),0) from tool_calls where org_id=?`, orgID).Scan(&id)
	return id
}

// ToolCallsAfter returns one thread's calls above a watermark, oldest first — the order they ran in.
func (s *Store) ToolCallsAfter(ctx context.Context, orgID int64, threadTS string, afterID int64) []ToolCallRow {
	rows, err := s.db.QueryContext(ctx, toolCallCols+` where org_id=? and thread_ts=? and id>? order by id`, orgID, threadTS, afterID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []ToolCallRow{}
	for rows.Next() {
		t, err := scanToolCall(rows)
		if err != nil {
			return out
		}
		out = append(out, t)
	}
	return out
}

type playgroundRequest struct {
	Channel string `json:"channel"` // the Slack channel id whose settings are being tried
	Thread  string `json:"thread"`  // an earlier playground thread to carry on in; "" starts one
	Text    string `json:"text"`
}

type playgroundTool struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Result string `json:"result"`
	OK     bool   `json:"ok"`
	MS     int64  `json:"ms"`
}

type playgroundReply struct {
	Thread string           `json:"thread"`
	Reply  string           `json:"reply"`
	Model  string           `json:"model"`
	Rounds int              `json:"rounds"`
	Tools  []playgroundTool `json:"tools"`
	// Held is what the turn stopped at: every write it asked for, including the ones the channel
	// would have run without asking — through a connection whose writes are automatic, or under
	// an allow rule — since a preview is not the channel and sends none of them.
	Held      []string `json:"held"`
	TokensIn  int      `json:"tokens_in"`
	TokensOut int      `json:"tokens_out"`
	CostUSD   float64  `json:"cost_usd"`
	// Error is a turn that failed rather than a request that was refused: the transcript and
	// the tool calls are still worth showing, so it rides along with a 200 instead of replacing it.
	Error string `json:"error"`
}

// playgroundResultCap keeps one tool result from filling the page. The whole thing is already in
// Activity, which is where a result this long wants reading anyway.
const playgroundResultCap = 4000

func (b *Bot) handlePlayground(w http.ResponseWriter, r *http.Request) {
	var req playgroundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		bad(w, err)
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		bad(w, errors.New("nothing to ask"))
		return
	}
	ctx := r.Context()
	orgID := orgOf(r)

	// The channel is looked up inside the organisation rather than checked after the fact, so a
	// channel id from another tenant is simply not a channel this request can name.
	scopes, err := b.store.Scopes(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	var sc *Scope
	for _, s := range scopes {
		if s.Kind == "channel" && s.SlackID == req.Channel {
			sc = s
			break
		}
	}
	if sc == nil {
		writeJSON(w, 404, map[string]any{"error": "no such channel in this organisation"})
		return
	}
	sl, err := b.slacks.For(ctx, sc.TeamID)
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": "that workspace is not connected right now"})
		return
	}
	// Whose access this runs with. A console session carries a Slack id only when it signed in
	// that way, and there is deliberately no "run as" field: connections that belong to one
	// person are exactly the thing an admin must not be able to borrow from a test page.
	userID := ""
	if u := adminFromCtx(ctx); u != nil {
		userID = u.UserID
	}
	if userID == "" {
		userID = sl.BotUserID
	}

	// Every limit a turn in the channel would meet, met here too: the per-person hourly rate
	// limit, the cap on turns in flight for this organisation, the channel's budget and the
	// account's. A playground is the easiest place to spend a budget by accident — a box that
	// answers every time you press return — and it is the easiest place to hold a dozen
	// three-minute turns open at once on an instance that serves every other tenant.
	if ok, why := b.agent.allowed(ctx, &Call{TeamID: sc.TeamID, OrgID: orgID, SL: sl, Channel: sc.SlackID, UserID: userID}); !ok {
		writeJSON(w, 429, map[string]any{"error": why})
		return
	}

	thread := req.Thread
	if !isPlaygroundThread(thread) {
		thread = newPlaygroundThread()
	}
	sess, err := b.store.EnsureSession(ctx, sc.TeamID, sc.SlackID, thread, "playground", "")
	if err != nil {
		fail(w, err)
		return
	}
	if err := b.store.AddTurn(ctx, sc.TeamID, sc.SlackID, thread, "user", userID, req.Text, "", 0, 0); err != nil {
		fail(w, err)
		return
	}

	mark := b.store.LastToolCallID(ctx, orgID)
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	c := &Call{
		TeamID: sc.TeamID, OrgID: orgID, SL: sl,
		Channel: sc.SlackID, ThreadTS: thread, UserID: userID, Text: req.Text,
		Kind: "channel", Session: sess, Preview: true,
		Streamer: sl.NewSilentStreamer(sc.SlackID, thread, userID),
	}
	runErr := b.agent.Run(runCtx, c)

	out := playgroundReply{
		Thread: thread, Reply: c.FinalText, Model: c.model, Rounds: c.roundsUsed,
		Held: c.previewHeld, TokensIn: c.usage.In, TokensOut: c.usage.Out, CostUSD: c.usage.CostUSD,
		Tools: []playgroundTool{},
	}
	if out.Held == nil {
		out.Held = []string{}
	}
	for _, t := range b.store.ToolCallsAfter(ctx, orgID, thread, mark) {
		result, _ := cutRunes(t.Result, playgroundResultCap)
		args, _ := cutRunes(t.Args, 1000)
		out.Tools = append(out.Tools, playgroundTool{Name: t.Name, Args: args, Result: result, OK: t.OK, MS: t.MS})
	}
	if runErr != nil {
		out.Error = runErr.Error()
	}
	writeJSON(w, 200, out)
}

// handlePlaygroundReset forgets one playground conversation so the next message starts cold.
// Starting a fresh thread would do as much, but then the old one sits in the turns table
// forever, and "clear" that leaves the transcript behind is not what the button says.
func (b *Bot) handlePlaygroundReset(w http.ResponseWriter, r *http.Request) {
	var req playgroundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		bad(w, err)
		return
	}
	if !isPlaygroundThread(req.Thread) {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	ctx := r.Context()
	scopes, err := b.store.Scopes(ctx, orgOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	for _, s := range scopes {
		if s.Kind == "channel" && s.SlackID == req.Channel {
			b.store.ForgetThread(ctx, s.TeamID, s.SlackID, req.Thread)
			break
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
