package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"
)

// The Microsoft Teams half of the store: the conversation log attest_tag keeps because Teams gives
// an application no way to read a thread back, the people the bot has met in each tenant, and the
// codes that join a tenant to an organisation. migrations/sqlite/0018_msteams.sql says why each of
// them exists.

// msteamsChatThread is the thread every message in a Teams chat belongs to. A channel post starts
// a reply chain that is its own thread; a chat has none, and treating each message in one as a new
// thread would make the bot forget the message before it.
const msteamsChatThread = "chat"

// msTimeLayout is fixed-width so that the log sorts as text. RFC3339Nano drops trailing zeros,
// and "…00.1Z" sorts after "…00.123Z" as a string while being before it in time.
const msTimeLayout = "2006-01-02T15:04:05.000000Z"

func msTime(t time.Time) string { return t.UTC().Format(msTimeLayout) }

// msMessage is one message in a Teams tenant's log.
type msMessage struct {
	Channel, Thread, ID string
	UserID, UserName    string
	IsBot               bool
	Text, At            string
	Files               []msFile
	Replies             int // filled by TeamsHistory only
}

// msFile is something that came attached to a Teams message, and where it can be fetched again.
type msFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"` // a MIME type, where Teams said or the name does
	URL  string `json:"url,omitempty"`
	// Kind is how the bytes are reached: "download", a pre-authorised link Teams hands a bot for a
	// file sent to it in a chat; "inline", an image pasted into a message, fetched with the bot's
	// own token; "reference", a file in SharePoint, which this app has no permission to read.
	Kind string `json:"kind"`
}

// LogTeamsMessage records a message the bot received or posted. A message seen twice — a
// redelivery, or an edit — keeps one row, with the newer text.
func (s *Store) LogTeamsMessage(ctx context.Context, teamID string, m msMessage) error {
	if teamID == "" || m.Channel == "" || m.ID == "" {
		return errors.New("a Teams message needs a workspace, a conversation and an id")
	}
	if m.At == "" {
		m.At = msTime(time.Now())
	}
	files := ""
	if len(m.Files) > 0 {
		raw, err := json.Marshal(m.Files)
		if err != nil {
			return err
		}
		files = string(raw)
	}
	_, err := s.db.ExecContext(ctx, `insert into msteams_messages
		(team_id, channel, thread, message_id, user_id, user_name, is_bot, text, files, at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(team_id, channel, message_id) do update set text=excluded.text, user_name=excluded.user_name,
			files=excluded.files`,
		teamID, m.Channel, m.Thread, m.ID, m.UserID, m.UserName, m.IsBot, m.Text, files, m.At)
	return err
}

// TeamsMessageThread is the thread a logged message is in, or "" when it is not in the log.
func (s *Store) TeamsMessageThread(ctx context.Context, teamID, channel, id string) string {
	var thread string
	_ = s.db.QueryRowContext(ctx, `select thread from msteams_messages where team_id=? and channel=? and message_id=?`,
		teamID, channel, id).Scan(&thread)
	return thread
}

// SetTeamsMessageText keeps the log in step with a post the bot has edited.
func (s *Store) SetTeamsMessageText(ctx context.Context, teamID, channel, id, text string) error {
	_, err := s.db.ExecContext(ctx, `update msteams_messages set text=? where team_id=? and channel=? and message_id=?`,
		text, teamID, channel, id)
	return err
}

// DeleteTeamsMessage takes a post the bot deleted out of the log, so it is not read back.
func (s *Store) DeleteTeamsMessage(ctx context.Context, teamID, channel, id string) error {
	_, err := s.db.ExecContext(ctx, `delete from msteams_messages where team_id=? and channel=? and message_id=?`,
		teamID, channel, id)
	return err
}

const msMessageCols = `channel, thread, message_id, user_id, user_name, is_bot, text, files, at`

func scanMSMessages(rows *sql.Rows, withReplies bool) ([]msMessage, error) {
	defer rows.Close()
	var out []msMessage
	for rows.Next() {
		var m msMessage
		var files string
		dest := []any{&m.Channel, &m.Thread, &m.ID, &m.UserID, &m.UserName, &m.IsBot, &m.Text, &files, &m.At}
		if withReplies {
			dest = append(dest, &m.Replies)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if files != "" {
			_ = json.Unmarshal([]byte(files), &m.Files) // a row that will not parse reads as having none
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// teamsThreadCap bounds how much of one thread is read back. A chat is one thread for as long as
// it exists, so it is the newest messages that are kept: the one being answered is always among
// them. The agent windows and summarises what it is given; this only stops a year-old chat from
// being read whole on every turn.
const teamsThreadCap = 1000

// TeamsThread reads one thread back oldest-first — in a chat, the whole chat — keeping the newest
// messages when there are more than the cap, and the root of a channel thread even then, because
// the post that started a thread is what the rest of it is about.
func (s *Store) TeamsThread(ctx context.Context, teamID, channel, thread string) ([]msMessage, error) {
	rows, err := s.db.QueryContext(ctx, `select `+msMessageCols+` from msteams_messages
		where team_id=? and channel=? and thread=? order by at desc, message_id desc limit ?`,
		teamID, channel, thread, teamsThreadCap)
	if err != nil {
		return nil, err
	}
	newest, err := scanMSMessages(rows, false)
	if err != nil {
		return nil, err
	}
	out := make([]msMessage, 0, len(newest)+1)
	for i := len(newest) - 1; i >= 0; i-- {
		out = append(out, newest[i])
	}
	if thread != msteamsChatThread && len(out) > 0 && out[0].ID != thread {
		rows, err := s.db.QueryContext(ctx, `select `+msMessageCols+` from msteams_messages
			where team_id=? and channel=? and message_id=?`, teamID, channel, thread)
		if err != nil {
			return nil, err
		}
		root, err := scanMSMessages(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(root, out...)
	}
	return out, nil
}

// TeamsHistory is a channel's top-level posts since a time, oldest-first, each with how many
// replies hang off it — the shape Slack's conversations.history has, read from the log.
func (s *Store) TeamsHistory(ctx context.Context, teamID, channel string, since time.Time, limit int) ([]msMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `select `+msMessageCols+`,
		(select count(*) from msteams_messages r where r.team_id=m.team_id and r.channel=m.channel
			and r.thread=m.message_id and r.message_id<>m.message_id)
		from msteams_messages m where m.team_id=? and m.channel=? and m.thread=m.message_id and m.at>=?
		order by m.at desc, m.message_id desc limit ?`, teamID, channel, msTime(since), limit)
	if err != nil {
		return nil, err
	}
	newest, err := scanMSMessages(rows, true)
	if err != nil {
		return nil, err
	}
	out := make([]msMessage, 0, len(newest))
	for i := len(newest) - 1; i >= 0; i-- {
		out = append(out, newest[i])
	}
	return out, nil
}

// msUser is somebody the bot has met in a Teams tenant.
type msUser struct {
	UserID       string // Entra object id: the id attest_tag knows them by
	TeamsID      string // 29:…, which Teams addresses them by and which differs per bot
	Name, Email  string
	Conversation string // a conversation they were last seen in, to ask the roster about them
}

// SaveTeamsUser records what an activity or the roster said about someone. A field this sighting
// did not carry keeps what an earlier one did: a message has a name but no email, the roster the
// other way round, and neither should erase the other.
func (s *Store) SaveTeamsUser(ctx context.Context, teamID string, u msUser) error {
	if teamID == "" || u.UserID == "" {
		return errors.New("a Teams user needs a workspace and an object id")
	}
	_, err := s.db.ExecContext(ctx, `insert into msteams_users
		(team_id, user_id, teams_id, name, email, conversation, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)
		on conflict(team_id, user_id) do update set
			teams_id=coalesce(nullif(excluded.teams_id, ''), msteams_users.teams_id),
			name=coalesce(nullif(excluded.name, ''), msteams_users.name),
			email=coalesce(nullif(excluded.email, ''), msteams_users.email),
			conversation=coalesce(nullif(excluded.conversation, ''), msteams_users.conversation),
			updated_at=excluded.updated_at`,
		teamID, u.UserID, u.TeamsID, u.Name, strings.ToLower(strings.TrimSpace(u.Email)), u.Conversation, msTime(time.Now()))
	return err
}

const msUserCols = `user_id, teams_id, name, email, conversation`

func (s *Store) teamsUserWhere(ctx context.Context, teamID, col, val string) (*msUser, error) {
	var u msUser
	err := s.db.QueryRowContext(ctx, `select `+msUserCols+` from msteams_users where team_id=? and `+col+`=? limit 1`,
		teamID, val).Scan(&u.UserID, &u.TeamsID, &u.Name, &u.Email, &u.Conversation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// TeamsUser is someone by their object id; nil when the bot has not met them.
func (s *Store) TeamsUser(ctx context.Context, teamID, userID string) (*msUser, error) {
	return s.teamsUserWhere(ctx, teamID, "user_id", userID)
}

// TeamsUserByTeamsID is someone by the 29: id a mention carries.
func (s *Store) TeamsUserByTeamsID(ctx context.Context, teamID, teamsID string) (*msUser, error) {
	return s.teamsUserWhere(ctx, teamID, "teams_id", teamsID)
}

// TeamsUserByEmail is someone by address, among the people the bot has met: the Bot Framework has
// no lookup by email, and Graph's needs a consent the tenant may not have given.
func (s *Store) TeamsUserByEmail(ctx context.Context, teamID, email string) (*msUser, error) {
	return s.teamsUserWhere(ctx, teamID, "email", strings.ToLower(strings.TrimSpace(email)))
}

// SetTeamServiceURL keeps the Bot Framework service URL a tenant's conversations are served from,
// for the posts that have no incoming message to take it from.
func (s *Store) SetTeamServiceURL(ctx context.Context, teamID, serviceURL string) error {
	_, err := s.db.ExecContext(ctx, `update teams set service_url=? where team_id=? and service_url<>?`,
		serviceURL, teamID, serviceURL)
	return err
}

// ---- link codes ----

// linkCodeAlphabet leaves out the characters people misread for each other: 0 and O, 1, I and L.
const linkCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// linkCodeTTL is long enough to walk from the console to Teams and back, and short enough that a
// code left in a screenshot is useless by the time anyone finds it.
const linkCodeTTL = 30 * time.Minute

// ErrLinkCode is every way a code can fail to link. One answer on purpose: which of them it was
// is not something to tell whoever sent the code.
var ErrLinkCode = errors.New("that link code is not valid: it may have expired or been used already")

func normalLinkCode(code string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r == '-' || r == ' ':
			return -1
		}
		return r
	}, strings.TrimSpace(code))
}

func linkCodeHash(code string) string {
	sum := sha256.Sum256([]byte(normalLinkCode(code)))
	return hex.EncodeToString(sum[:])
}

// NewLinkCode mints a single-use code that joins a workspace on platform to orgID. Only its hash
// is stored; the code itself is shown once, to the person who asked for it.
func (s *Store) NewLinkCode(ctx context.Context, orgID int64, platform string, createdBy int64) (string, time.Time, error) {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(linkCodeAlphabet))))
		if err != nil {
			return "", time.Time{}, err
		}
		if i == 4 {
			b.WriteByte('-')
		}
		b.WriteByte(linkCodeAlphabet[n.Int64()])
	}
	code, expires := b.String(), time.Now().Add(linkCodeTTL)
	_, err := s.db.ExecContext(ctx, `insert into link_codes (code_hash, org_id, platform, created_by, expires_at)
		values (?, ?, ?, ?, ?)`, linkCodeHash(code), orgID, platform, createdBy, msTime(expires))
	return code, expires, err
}

// PeekLinkCode is the organisation a code would link to, without using it up: unknown, expired,
// already used or for another platform is ErrLinkCode. The Teams link checks a code with this and
// spends it only once the link stands, so a redeemer refused on the way — not a member of the
// tenant, a save that failed — leaves the code for the person it was meant for.
func (s *Store) PeekLinkCode(ctx context.Context, code, platform string) (int64, error) {
	var orgID int64
	var expires string
	err := s.db.QueryRowContext(ctx, `select org_id, expires_at from link_codes
		where code_hash=? and platform=? and used_at is null`, linkCodeHash(code), platform).Scan(&orgID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrLinkCode
	}
	if err != nil {
		return 0, err
	}
	if expires <= msTime(time.Now()) {
		return 0, ErrLinkCode
	}
	return orgID, nil
}

// SpendLinkCode uses a code up and says which organisation it was for. The update is what makes
// it single use: two messages carrying the same code cannot both find it unspent.
func (s *Store) SpendLinkCode(ctx context.Context, code, platform, usedBy string) (int64, error) {
	orgID, err := s.PeekLinkCode(ctx, code, platform)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `update link_codes set used_at=?, used_by=? where code_hash=? and used_at is null`,
		msTime(time.Now()), usedBy, linkCodeHash(code))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrLinkCode
	}
	return orgID, nil
}
