// SQLite storage. Schema mirrors the plan's Postgres tables so a later swap is mechanical:
// every query is plain SQL with ? placeholders, and vectors are stored as float32 blobs
// (swap for pgvector when moving).
package app

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"time"

	"errors"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *database

	origin       string // public_origin.go: the learned console origin, re-read on a TTL
	originLoaded bool
	originAt     time.Time
}

// OpenStore opens the database named by dsn and brings its schema up to date.
//
// dsn is either a Postgres URL or a path to a SQLite file, and the scheme is what decides:
//
//	postgres://… or postgresql://…   Postgres
//	sqlite://path or file:path       SQLite at that path
//	anything else                    SQLite at that path, which is what DB_PATH has always been
//
// SQLite stays the default so that a bare `docker run` with one volume works with nothing
// configured, which is the whole of the one-command promise.
func OpenStore(dsn string) (*Store, error) {
	driver, conn, pg := parseDSN(dsn)
	handle, err := sql.Open(driver, conn)
	if err != nil {
		return nil, err
	}
	if pg {
		// A pool, because Postgres has no single-writer rule to work around. Modest, because
		// the ceiling that matters is the server's max_connections shared across instances.
		handle.SetMaxOpenConns(8)
		handle.SetMaxIdleConns(4)
		handle.SetConnMaxIdleTime(5 * time.Minute)
	} else {
		handle.SetMaxOpenConns(1) // sqlite: single writer, keeps "database is locked" away
	}
	// Everything below goes through the wrapper rather than the handle, so that the schema is
	// applied the same way the queries are — one translation, in one place (db.go).
	db := &database{sql: handle, pg: pg}
	if err := applyMigrations(context.Background(), db); err != nil {
		db.Close()
		return nil, err
	}
	// Transitional, and only on SQLite. A database adopted as the baseline was created before
	// versioned migrations and may be missing one of the old additive columns; migrate() is
	// idempotent, so running it once more costs a handful of pragma queries at boot and
	// removes the risk of adopting a database that was not quite up to date. It goes away once
	// every deployment has booted at least once on this version. Postgres never needs it:
	// there is no Postgres database that predates the baseline.
	if !pg {
		if err := migrate(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

// describeDSN is what the startup line says about the database, with nothing in it that has to
// be kept out of a log.
//
// The line used to print cfg.DBPath unconditionally, and the Dockerfile sets DB_PATH to
// /data/attesttag.db — so a Postgres deployment announced itself as running on a SQLite file it
// had never opened. "Which database is this process actually using" is the question a bad
// afternoon starts with; the process should not answer it wrongly in its first line.
//
// A Postgres DSN carries a password, so it is never printed. What comes back is the engine and
// the host — for Cloud Run that is the Unix socket, which names the instance and is the useful
// half anyway.
func describeDSN(dsn string) string {
	_, _, pg := parseDSN(dsn)
	if !pg {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "postgres"
	}
	where := u.Host // host:port, when it is reached over TCP
	if sock := u.Query().Get("host"); sock != "" {
		where = sock // /cloudsql/<project>:<region>:<instance>
	}
	db := strings.TrimPrefix(u.Path, "/")
	if where == "" {
		return "postgres:" + db
	}
	return "postgres " + db + " at " + where
}

// parseDSN maps what a deployment configured onto a driver. It takes no decisions beyond that:
// an unrecognised string is a file path, because that is what DB_PATH has always been and a
// deployment that has been setting it should not have to change anything.
func parseDSN(dsn string) (driver, conn string, pg bool) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "pgx", dsn, true
	case strings.HasPrefix(dsn, "sqlite://"):
		dsn = strings.TrimPrefix(dsn, "sqlite://")
	case strings.HasPrefix(dsn, "file:"):
		dsn = strings.TrimPrefix(dsn, "file:")
	}
	return "sqlite", dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", false
}

// migrateAdds are the columns migrate() applies, in order: table, column, definition.
//
// A package-level var rather than a local, so that TestEveryMigrateColumnReachesPostgres can
// read it. That test exists because this list is the SQLite-only path — OpenStore skips
// migrate() entirely on Postgres — and a column added here after the Postgres baseline was
// frozen therefore reaches one dialect and not the other. connections.secret_fp did exactly
// that and took every read of the connections table down on Postgres.
var migrateAdds = [][3]string{
	{"admin_sessions", "mfa_verified", "integer not null default 0"},
	{"login_challenges", "via", "text not null default 'password'"},
	{"sessions", "team_id", "text"},
	{"sessions", "title", "text"},
	{"sessions", "tool_calls", "integer default 0"},
	{"turns", "slack_ts", "text"},
	{"scopes", "read_all", "text default 'inherit'"},
	{"scopes", "link_epoch", "integer not null default 0"},
	{"approval_members", "team_id", "text not null default ''"},
	{"approval_members", "slack_user_id", "text not null default ''"},
	{"slack_deliveries", "org_id", "integer not null default 0"},
	{"slack_deliveries", "attempts", "integer not null default 0"},
	{"slack_deliveries", "dead_at", "integer not null default 0"},
	{"slack_deliveries", "last_error", "text not null default ''"},
	{"seen_events", "owner", "text not null default ''"},
	{"scopes", "monthly_budget_usd", "real default 0"},
	{"sessions", "summary", "text default ''"},
	{"sessions", "summary_upto", "text default ''"},
	{"usage", "user_id", "text default ''"},
	{"routines", "result_ts", "text"},
	{"routines", "notify", "text not null default 'always'"},
	{"routines", "notify_when", "text default ''"},
	{"routines", "last_status", "text default ''"},
	{"scopes", "allow_rules", "text default '[]'"},
	{"connections", "repo", "text default ''"},
	// 0 is the pasted-token path, which is every row that existed before GitHub App
	// installs; a non-zero id means the credential is minted, not stored.
	{"connections", "github_installation_id", "integer not null default 0"},
	// A short digest of the token a repository was connected with, so repositories sharing
	// one token can be recognised as one credential — the console groups by it, and it is
	// how "rotate this token" knows what else it touches. A digest, never the token: sealed
	// secrets are opened one at a time and only to spend them, and comparing them would
	// otherwise mean opening every one on every page load. Empty on rows connected before
	// the column existed and on every app-backed row, which the installation id groups.
	{"connections", "secret_fp", "text not null default ''"},
	{"scopes", "default_repo", "text default ''"},
	{"tool_calls", "result", "text default ''"},
	{"connections", "allow_grants", "integer default 0"},
	{"scopes", "approvers", "text default ''"},
	{"proxy_audit", "access_request_id", "integer default 0"},
	{"access_requests", "self_approved", "integer default 0"},
	{"access_requests", "role_id", "integer default 0"},
	{"connections", "test_cmd", "text default ''"},
	{"connections", "recipe", "text default ''"},
	{"scopes", "is_private", "integer not null default 0"},
	{"scopes", "max_tool_rounds", "integer not null default 0"},
	// Plans (plans.go). An organisation from before the column existed is free, like a new one.
	{"orgs", "plan", "text not null default 'free'"},
	{"orgs", "plan_budget_usd", "real not null default 0"},
	// A routine's pinned steps, and what it does once they have run. Both default to what a
	// routine without them has always done: no steps, and a model turn that finds its own.
	{"routines", "auto_confirm", "integer not null default 0"},
	{"routines", "steps", "text not null default '[]'"},
	{"routines", "finish", "text not null default 'answer'"},
	// Which model a routine's runs answer on. '' is what every routine did before: the
	// channel's model, then the default.
	{"routines", "model", "text not null default ''"},
	// Multi-workspace. NOTE: these ADD COLUMNs are all migrate() can express — the
	// composite keys (scopes, sessions, console_users, documents, file_texts) exist
	// only in the ddl above and therefore only on a database created fresh.
	{"turns", "team_id", "text not null default ''"},
	{"memories", "team_id", "text not null default ''"},
	{"routines", "team_id", "text not null default ''"},
	{"tool_calls", "team_id", "text not null default ''"},
	{"usage", "team_id", "text not null default ''"},
	{"proxy_audit", "team_id", "text not null default ''"},
	{"artifacts", "team_id", "text not null default ''"},
	{"access_requests", "team_id", "text not null default ''"},
	{"pending_writes", "team_id", "text not null default ''"},
	{"jobs", "team_id", "text not null default ''"},
	{"scopes", "team_id", "text not null default ''"},
	{"scopes", "org_id", "integer not null default 1"},
	{"console_users", "team_id", "text not null default ''"},
	{"console_users", "org_id", "integer not null default 1"},
	{"console_invites", "team_id", "text not null default ''"},
	{"bundles", "org_id", "integer not null default 1"},
	{"connections", "org_id", "integer not null default 1"},
	{"domains", "org_id", "integer not null default 1"},
	{"skills", "org_id", "integer not null default 1"},
	{"documents", "org_id", "integer not null default 1"},
	{"settings", "org_id", "integer not null default 1"},
	{"setup_links", "org_id", "integer not null default 1"},
	// How this session was signed in ('password' | 'slack' | 'signup'). The organisation's
	// sign-in policy is checked against it on every request, so a session predating the
	// column reads as '' and is treated as unrestricted rather than locked out.
	{"admin_sessions", "via", "text not null default ''"},
	// How this one person wants their own account used — how they sign off an email, which
	// calendar is the real one, whose invitations to decline. It belongs on the row that
	// holds their grant rather than in channel settings: the bot acts as them, so the
	// instruction is theirs and nobody else in the channel should see it or inherit it.
	{"user_connections", "instructions", "text not null default ''"},
	// Invitations grew two addressees and a shareable form. A row written before this
	// reads as a single-use invitation to a mailbox, which is exactly what it was.
	{"email_tokens", "slack_user_id", "text default ''"},
	{"email_tokens", "label", "text default ''"},
	{"email_tokens", "domain", "text default ''"},
	{"email_tokens", "max_uses", "integer not null default 1"},
	{"email_tokens", "uses", "integer not null default 0"},
	// When the bot was removed from this channel, from the console or from Slack. The row
	// is kept rather than deleted: a channel's instructions, budget and attached bundles
	// are work somebody did, and inviting the bot back should return them rather than ask
	// for them again. Empty means the bot is in the channel, which is every row until one
	// is removed, so the default is what every existing channel already means.
	{"scopes", "left_at", "text not null default ''"},
	// The outside's name for an organisation and for a person. Added empty, filled by
	// backfillPublicIDs below, and unique from then on.
	{"orgs", "public_id", "text not null default ''"},
	{"users", "public_id", "text not null default ''"},
}

// migrate adds columns introduced after a table was first created (SQLite has no
// "add column if not exists"). SQLite only: see migrateAdds.
func migrate(db *database) error {
	for _, a := range migrateAdds {
		have, err := hasColumn(db, a[0], a[1])
		if err != nil {
			return err
		}
		if !have {
			if _, err := db.Exec(`alter table ` + a[0] + ` add column ` + a[1] + ` ` + a[2]); err != nil {
				return err
			}
		}
	}
	// A tier used to name one bundle in a column; it now lists bundles and one-off connections
	// in their own tables, the way a channel does.
	if err := foldRoleBundleIntoGrants(db); err != nil {
		return err
	}
	// "Respond automatically" was a second, narrower version of "read every message"; a channel
	// that had it on keeps answering under the one setting that is left.
	if err := foldAutoRespondIntoReadAll(db); err != nil {
		return err
	}
	// Rows that predate public_id have none. They get one before the unique index goes on,
	// because '' is a value and a second empty row would collide with the first.
	if err := backfillPublicIDs(db); err != nil {
		return err
	}
	// Sessions written before tokens were hashed at rest still hold the credential itself.
	return rehashSessionTokens(db)
}

// backfillPublicIDs gives every organisation and every person the identifier the outside world
// uses, then makes it unique. The index is created here rather than in the ddl because on an
// existing database the column does not exist until the ADD COLUMN above has run.
//
// One row at a time rather than one UPDATE: SQLite has no per-row random hex of its own, and a
// single expression would hand every row the same value — which the unique index would then
// refuse, loudly but too late to explain itself.
func backfillPublicIDs(db *database) error {
	for _, table := range []string{"orgs", "users"} {
		rows, err := db.Query(`select id from ` + table + ` where public_id = '' or public_id is null`)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		// One SQLite connection: the cursor closes before the updates run, or the two wait
		// on each other and the process never finishes starting.
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := db.Exec(`update `+table+` set public_id=? where id=?`, newPublicID(), id); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`create unique index if not exists ` + table + `_public_id on ` + table + `(public_id)`); err != nil {
			return err
		}
	}
	return nil
}

// foldRoleBundleIntoGrants moves the bundle a tier named in approval_roles.bundle_id into
// approval_role_bundles. It runs once: the column is blanked as it goes, so an admin who later
// takes that bundle off the tier does not find it back at the next start. The column itself is
// left where it is — dropping a column rewrites the table, and an unread column costs nothing.
func foldRoleBundleIntoGrants(db *database) error {
	if _, err := db.Exec(`insert into approval_role_bundles (org_id, role_id, bundle_id)
		select org_id, id, bundle_id from approval_roles where bundle_id <> 0 on conflict do nothing`); err != nil {
		return err
	}
	_, err := db.Exec(`update approval_roles set bundle_id=0 where bundle_id <> 0`)
	return err
}

// foldAutoRespondIntoReadAll retires the old "respond automatically" setting. Both its per-scope
// column and its global default (the unprompted_replies setting) become "read every message",
// which asked the wider question and always subsumed the narrow one. It runs once: the values it
// reads are blanked as it goes, so a second start finds nothing to fold and leaves an admin's
// later "off" alone. The auto_respond column itself is left where it is — dropping a column
// rewrites the table, and an unread column costs nothing.
func foldAutoRespondIntoReadAll(db *database) error {
	if has, err := hasColumn(db, "scopes", "auto_respond"); err != nil || !has {
		return err
	}
	if _, err := db.Exec(`update scopes set read_all='on'
		where auto_respond='on' and coalesce(read_all,'') in ('', 'inherit')`); err != nil {
		return err
	}
	// The global default applied to every channel that inherited, so it becomes the account
	// scope — the last link the inheritance chain reads. Only organisations that had it on get
	// a row, and only if they have no account scope already.
	if _, err := db.Exec(`insert into scopes (org_id, kind, team_id, slack_id, name, read_all)
		select s.org_id, 'workspace', '', '',
		       coalesce((select o.name from orgs o where o.id=s.org_id), 'Organisation'), 'on'
		from settings s where s.key='unprompted_replies' and s.value='1' on conflict do nothing`); err != nil {
		return err
	}
	if _, err := db.Exec(`update scopes set read_all='on'
		where kind='workspace' and coalesce(read_all,'') in ('', 'inherit')
		  and org_id in (select org_id from settings where key='unprompted_replies' and value='1')`); err != nil {
		return err
	}
	if _, err := db.Exec(`delete from settings where key='unprompted_replies'`); err != nil {
		return err
	}
	_, err := db.Exec(`update scopes set auto_respond='' where coalesce(auto_respond,'') <> ''`)
	return err
}

// hasColumn reports whether a table already carries a column, the same question the migration
// list above asks of every column it adds.
func hasColumn(db *database, table, column string) (bool, error) {
	rows, err := db.Query(`pragma table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.DateTime) }

// nowMinus and today are the same stamp for a moment in the past and for midnight this
// morning. They exist so that no query has to say datetime('now','-5 minutes') or date('now'):
// those are SQLite's spelling, Postgres spells them differently, and a clock the process owns
// is also a clock a test can reason about. Timestamps are stored as text in this format and
// compared as text, so "after" is a string comparison and these have to sort with the column.
func nowMinus(d time.Duration) string { return time.Now().UTC().Add(-d).Format(time.DateTime) }
func today() string                   { return time.Now().UTC().Format(time.DateOnly) }

// ---- sessions ----

type Session struct {
	Channel, ThreadTS, TeamID, Kind, Model, Status, Title string
	Muted                                                 bool
	ToolCalls                                             int
	// RestartTS is the message `!restart` was said in; a turn reads the thread only after it.
	RestartTS string
}

// A thread is identified by its workspace as well as its channel: a Slack Connect channel
// carries the same C-id in every workspace it is shared into, so (channel, thread_ts) alone
// would merge two workspaces' conversations into one.
func (s *Store) GetSession(ctx context.Context, teamID, channel, threadTS string) (*Session, error) {
	var se Session
	var muted int
	var model, title, team sql.NullString
	err := s.db.QueryRowContext(ctx, `select channel, thread_ts, team_id, kind, model, status, muted, title, tool_calls,
		coalesce(restart_ts,'') from sessions where team_id=? and channel=? and thread_ts=?`, teamID, channel, threadTS).
		Scan(&se.Channel, &se.ThreadTS, &team, &se.Kind, &model, &se.Status, &muted, &title, &se.ToolCalls, &se.RestartTS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	se.Muted, se.Model, se.Title, se.TeamID = muted == 1, model.String, title.String, team.String
	return &se, nil
}

// EnsureSession is called where the bot starts, or carries on, talking in a thread, so it also
// makes the session active again. It used to leave status alone, and a session archived once —
// `!restart` did that — stayed archived for good: a mention still got an answer, but every
// follow-up that did not mention the bot was taken for chatter in a thread it had left.
func (s *Store) EnsureSession(ctx context.Context, teamID, channel, threadTS, kind, model string) (*Session, error) {
	_, err := s.db.ExecContext(ctx, `
		insert into sessions (team_id, channel, thread_ts, kind, model) values (?, ?, ?, ?, ?)
		on conflict(team_id, channel, thread_ts) do update set last_active = ?, status = 'active'`,
		teamID, channel, threadTS, kind, model, now())
	if err != nil {
		return nil, err
	}
	return s.GetSession(ctx, teamID, channel, threadTS)
}

func (s *Store) SetSessionField(ctx context.Context, teamID, channel, threadTS, field string, val any) error {
	switch field {
	case "muted", "status", "model", "title", "tool_calls":
	default:
		panic("bad session field " + field)
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`update sessions set %s=? where team_id=? and channel=? and thread_ts=?`, field), val, teamID, channel, threadTS)
	return err
}

// RequestStop records that somebody asked this thread's work to stop. It is read between tool
// rounds by whichever instance is running the turn, which may not be the one that took the
// message — a Slack event goes to whichever container answered the webhook.
func (s *Store) RequestStop(ctx context.Context, teamID, channel, threadTS string) error {
	_, err := s.db.ExecContext(ctx, `update sessions set stop_requested_at=? where team_id=? and channel=? and thread_ts=?`,
		now(), teamID, channel, threadTS)
	return err
}

// StopRequestedAt is what a running turn checks between rounds. Empty means carry on.
func (s *Store) StopRequestedAt(ctx context.Context, teamID, channel, threadTS string) string {
	var at string
	s.db.QueryRowContext(ctx, `select coalesce(stop_requested_at,'') from sessions where team_id=? and channel=? and thread_ts=?`,
		teamID, channel, threadTS).Scan(&at)
	return at
}

// ClearStopRequest is called when a turn starts, so a stop from an hour ago does not kill the
// next thing somebody asks in the same thread.
func (s *Store) ClearStopRequest(ctx context.Context, teamID, channel, threadTS string) error {
	_, err := s.db.ExecContext(ctx, `update sessions set stop_requested_at=null where team_id=? and channel=? and thread_ts=?`,
		teamID, channel, threadTS)
	return err
}

func (s *Store) ArchiveSession(ctx context.Context, teamID, channel, threadTS string) error {
	// Keep the rows for audit; the router treats an archived session as "start fresh".
	_, err := s.db.ExecContext(ctx, `update sessions set status='archived' where team_id=? and channel=? and thread_ts=?`, teamID, channel, threadTS)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `update turns set role='archived:'||role where team_id=? and channel=? and thread_ts=? and role not like 'archived:%'`, teamID, channel, threadTS)
	return err
}

// RestartSession is `!restart`: the thread carries on, read from after the message that asked
// (fromTS) — Slack keeps every message and hands them all back, so forgetting has to be a cut on
// this side. The running summary of what came before is dropped with it, and the turns are
// archived as ArchiveSession archives them. The session stays active, so the conversation goes on
// without anybody having to mention the bot again.
func (s *Store) RestartSession(ctx context.Context, teamID, channel, threadTS, kind, fromTS string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into sessions (team_id, channel, thread_ts, kind, restart_ts) values (?, ?, ?, ?, ?)
		on conflict(team_id, channel, thread_ts) do update set status = 'active', restart_ts = ?,
			summary = '', summary_upto = '', last_active = ?`,
		teamID, channel, threadTS, kind, fromTS, fromTS, now())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `update turns set role='archived:'||role where team_id=? and channel=? and thread_ts=? and role not like 'archived:%'`, teamID, channel, threadTS)
	return err
}

// ---- turns ----

type Turn struct {
	Role, UserID, Content, SlackTS string
	CreatedAt                      string
}

func (s *Store) AddTurn(ctx context.Context, teamID, channel, threadTS, role, userID, content, slackTS string, in, out int) error {
	_, err := s.db.ExecContext(ctx, `
		insert into turns (team_id, channel, thread_ts, role, user_id, content, slack_ts, tokens_in, tokens_out)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?)`, teamID, channel, threadTS, role, userID, content, slackTS, in, out)
	return err
}

func (s *Store) Notes(ctx context.Context, teamID, channel, threadTS string) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, `select role, coalesce(user_id,''), content, coalesce(slack_ts,''), created_at
		from turns where team_id=? and channel=? and thread_ts=? and role='note' order by id`, teamID, channel, threadTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.Role, &t.UserID, &t.Content, &t.SlackTS, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- memories ----

type Memory struct {
	ID                         int64
	TeamID                     string
	Scope, Text, CreatedBy, At string
}

func (s *Store) AddMemory(ctx context.Context, orgID int64, teamID, scope, text, by string) error {
	if s.countRows(ctx, "memories", orgID) >= maxMemoriesPerOrg {
		return errOrgCap
	}
	_, err := s.db.ExecContext(ctx, `insert into memories (org_id, team_id, scope, text, created_by) values (?, ?, ?, ?, ?)`,
		orgID, teamID, scope, text, by)
	return err
}

func (s *Store) Memories(ctx context.Context, orgID int64, scopes ...string) ([]Memory, error) {
	var out []Memory
	for _, sc := range scopes {
		rows, err := s.db.QueryContext(ctx, `select id, coalesce(team_id,''), scope, text, coalesce(created_by,''), created_at from memories where org_id=? and scope=? order by id`, orgID, sc)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m Memory
			if err := rows.Scan(&m.ID, &m.TeamID, &m.Scope, &m.Text, &m.CreatedBy, &m.At); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, m)
		}
		rows.Close()
	}
	return out, nil
}

func (s *Store) AllMemories(ctx context.Context, orgID int64) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, `select id, coalesce(team_id,''), scope, text, coalesce(created_by,''), created_at from memories
		where org_id=? order by scope, id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.TeamID, &m.Scope, &m.Text, &m.CreatedBy, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMemory(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `delete from memories where org_id=? and id=?`, orgID, id)
	return err
}

// UpdateMemory rewrites one memory's text in place. The row keeps its id, scope and author, so
// the bot reads the corrected fact on its next turn without the console having to forget and
// re-add it. It reports false when the organisation has no memory with that id.
func (s *Store) UpdateMemory(ctx context.Context, orgID, id int64, text string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `update memories set text=? where org_id=? and id=?`, text, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// escapeLike escapes the LIKE metacharacters in a literal that is being spliced into a pattern,
// so % and _ in user- or model-supplied text match themselves rather than acting as wildcards.
// It pairs with `escape '\'` on the query. Only the literal part is passed through it; a wildcard
// the query itself adds (a trailing "/%") is concatenated after.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Store) ForgetMemory(ctx context.Context, orgID int64, scope, needle string) (int64, error) {
	// "Delete everything" is not something this is ever asked for. The escaping below stops % and
	// _ widening the match, which is the bug forget("%") was; it never stopped an empty needle
	// becoming "%%", which matches every row just the same and is reached by leaving the argument
	// out rather than by choosing it. required in a JSON schema is advice to the model, not a
	// check, and an unparseable tool call arrives here as the zero value. Refused at the store,
	// because there are three callers and the next one would have to remember too.
	if strings.TrimSpace(needle) == "" {
		return 0, errors.New("say which words the memory contains")
	}
	// The needle is the model's, and % or _ in it would be wildcards: forget("%") emptied a
	// scope. Escaped, so it matches the text it names and nothing more.
	needle = escapeLike(needle)
	res, err := s.db.ExecContext(ctx, `delete from memories where org_id=? and scope=? and lower(text) like lower(?) escape '\'`, orgID, scope, "%"+needle+"%")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- artifacts ----

// An artifact is a file the bot made on request: uploaded to the thread so people can open it
// straight away, and kept here so the console can show everything that has been produced.
type Artifact struct {
	ID                             int64
	TeamID                         string
	Channel, ThreadTS, CreatedBy   string
	Title, Kind                    string
	Bytes                          int
	Content, FileID, Permalink, At string
}

func (s *Store) AddArtifact(ctx context.Context, orgID int64, a *Artifact) error {
	err := s.db.QueryRowContext(ctx, `insert into artifacts
		(org_id, team_id, channel, thread_ts, created_by, title, kind, bytes, content, file_id, permalink)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id`,
		orgID, a.TeamID, a.Channel, a.ThreadTS, a.CreatedBy, a.Title, a.Kind, a.Bytes, a.Content, a.FileID, a.Permalink).Scan(&a.ID)
	return err
}

// Artifacts lists newest first without the bodies: the index only needs the metadata, and a
// list of 200 reports would otherwise be megabytes of JSON.
func (s *Store) Artifacts(ctx context.Context, orgID int64, limit int) ([]Artifact, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `select id, coalesce(team_id,''), coalesce(channel,''), coalesce(thread_ts,''), coalesce(created_by,''),
		title, kind, bytes, coalesce(file_id,''), coalesce(permalink,''), created_at
		from artifacts where org_id=? order by id desc limit ?`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.TeamID, &a.Channel, &a.ThreadTS, &a.CreatedBy, &a.Title, &a.Kind,
			&a.Bytes, &a.FileID, &a.Permalink, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ArtifactByID(ctx context.Context, orgID, id int64) (*Artifact, error) {
	var a Artifact
	err := s.db.QueryRowContext(ctx, `select id, coalesce(team_id,''), coalesce(channel,''), coalesce(thread_ts,''), coalesce(created_by,''),
		title, kind, bytes, content, coalesce(file_id,''), coalesce(permalink,''), created_at
		from artifacts where org_id=? and id=?`, orgID, id).
		Scan(&a.ID, &a.TeamID, &a.Channel, &a.ThreadTS, &a.CreatedBy, &a.Title, &a.Kind, &a.Bytes,
			&a.Content, &a.FileID, &a.Permalink, &a.At)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) DeleteArtifact(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `delete from artifacts where org_id=? and id=?`, orgID, id)
	return err
}

// ---- routines ----

type Routine struct {
	ID                                   int64
	OrgID                                int64  // whose budget and connections it spends
	TeamID                               string // which connected workspace its channel is in
	Channel, Cron, TZ, Prompt, CreatedBy string
	Enabled                              bool
	NextRun, LastRun, LastError          string
	// Notify is "always" or "when_needed": whether every run posts to the channel, or only the
	// ones that clear NotifyWhen. LastStatus is the outcome of the most recent run, denormalised
	// for the same reason LastRun and LastError are — the console list must not need a join.
	Notify, NotifyWhen, LastStatus string
	// Steps are the calls this routine makes before the model is asked anything, as the JSON
	// array parseRoutineSteps reads. Finish is what happens afterwards: "answer" (one model
	// call, no tools), "agent" (it carries on with its tools), or "raw" (no model at all).
	Steps, Finish string
	// Model is which model answers this routine's runs. "" follows the channel, then Settings,
	// as any turn does; "heavy" is the advanced model; anything else is an id from the short
	// list Settings offers to channels. The editor only shows that list, and the API refuses
	// anything outside it, for the reason the channel page does: a model name typed into a
	// form is not a model anyone chose to pay for.
	Model string
	// RunLease is when the run currently holding this routine gives it up, as UnixNano; 0 or a
	// time in the past means nobody holds it. Read here rather than asked for separately, so
	// the scheduler can skip a routine somebody is already running without another query
	// (routines.go, routineBusy).
	RunLease int64
	// AutoConfirm pre-approves this routine's writes. A Confirm card posted at 6am is nobody's
	// decision — it expires unpressed — so a routine that is meant to change something has to
	// be told once, when it is set up, rather than asked every time it runs. It is narrower
	// than the two settings that already do this: a connection set to Writes = Automatic covers
	// every channel, and an allow rule covers every turn in a scope, while this covers one
	// routine. What it never covers is a connection that hands out access; that write is not
	// the person who wrote the routine's to approve, whenever it happens.
	AutoConfirm bool
}

// countRows is the per-organisation size of one table, for the caps in limits.go. The table
// name is one of a fixed set named in code, never input.
func (s *Store) countRows(ctx context.Context, table string, orgID int64) int {
	var n int
	s.db.QueryRowContext(ctx, `select count(*) from `+table+` where org_id=?`, orgID).Scan(&n)
	return n
}

func (s *Store) AddRoutine(ctx context.Context, r Routine) (int64, error) {
	if s.countRows(ctx, "routines", r.OrgID) >= maxRoutinesPerOrg {
		return 0, errOrgCap
	}
	steps, err := parseRoutineSteps(r.Steps)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `insert into routines (org_id, team_id, channel, cron, tz, prompt, created_by, enabled, next_run, notify, notify_when, steps, finish, model, auto_confirm)
		values (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?) returning id`, r.OrgID, r.TeamID, r.Channel, r.Cron, r.TZ, r.Prompt, r.CreatedBy, r.NextRun, notifyMode(r.Notify), r.NotifyWhen, encodeRoutineSteps(steps), routineFinish(r.Finish), r.Model, boolInt(r.AutoConfirm)).Scan(&id)
	return id, err
}

const routineCols = `id, coalesce(org_id,0), coalesce(team_id,''), channel, cron, tz, prompt, coalesce(created_by,''), enabled, coalesce(next_run,''), coalesce(last_run,''), coalesce(last_error,''), coalesce(notify,'always'), coalesce(notify_when,''), coalesce(last_status,''), coalesce(steps,'[]'), coalesce(finish,'answer'), coalesce(model,''), coalesce(run_lease,0), coalesce(auto_confirm,0)`

// Routines lists one organisation's routines, optionally narrowed to a channel. A channel id is
// not an organisation: Slack Connect puts the same channel in two workspaces, so the org is the
// predicate and the channel only refines it.
func (s *Store) Routines(ctx context.Context, orgID int64, channel string) ([]Routine, error) {
	q := `select ` + routineCols + ` from routines where org_id=?`
	args := []any{orgID}
	if channel != "" {
		q += ` and channel=?`
		args = append(args, channel)
	}
	return scanRoutines(s.db.QueryContext(ctx, q+` order by id`, args...))
}

// DueRoutines is the scheduler's sweep across every organisation. It is the one read here that
// is deliberately deployment-wide: one process fires every tenant's routines, and each row
// carries the organisation that everything after this is scoped by.
func (s *Store) DueRoutines(ctx context.Context) ([]Routine, error) {
	return scanRoutines(s.db.QueryContext(ctx, `select `+routineCols+` from routines order by id`))
}

func scanRoutines(rows *sql.Rows, err error) ([]Routine, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Routine
	for rows.Next() {
		var r Routine
		var en, auto int
		if err := rows.Scan(&r.ID, &r.OrgID, &r.TeamID, &r.Channel, &r.Cron, &r.TZ, &r.Prompt, &r.CreatedBy, &en, &r.NextRun, &r.LastRun, &r.LastError, &r.Notify, &r.NotifyWhen, &r.LastStatus, &r.Steps, &r.Finish, &r.Model, &r.RunLease, &auto); err != nil {
			return nil, err
		}
		r.Enabled, r.AutoConfirm = en == 1, auto == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateRoutineRun(ctx context.Context, orgID, id int64, nextRun, lastErr, status string) error {
	_, err := s.db.ExecContext(ctx, `update routines set next_run=?, last_run=?, last_error=?, last_status=? where org_id=? and id=?`, nextRun, now(), lastErr, status, orgID, id)
	return err
}

// RoutinePatch is an edit to one routine: every field nil but the ones being changed. A struct
// rather than a row of pointer arguments, which stopped being readable at five.
type RoutinePatch struct {
	Cron, TZ, Prompt, Notify, NotifyWhen, Steps, Finish, Model *string
	// Channel moves the routine; TeamID goes with it, because a channel id only means
	// something inside one workspace. The API resolves the pair before it gets here.
	Channel, TeamID *string
	AutoConfirm     *bool
	// EditedBy is the Slack id of whoever is making this change, and empty when they have no
	// Slack identity — a password console session, or an API key. Nothing stores it: it decides
	// whether the routine goes on running as the person it belongs to, or passes to the person
	// rewriting it. See changesWhatItDoes.
	EditedBy string
}

// changesWhatItDoes reports whether this patch alters what the routine does, or where what it
// finds ends up — as opposed to only when it runs. Pausing a routine, widening its schedule or
// moving it to another timezone is housekeeping, and anyone holding routines.manage may do it
// to anyone's routine. Rewriting its prompt or its steps, moving the channel it posts into, or
// pre-approving its writes is authorship, and authorship is what decides whose accounts a run
// reaches. Notify is on the authorship side for a reason that is easy to miss: a routine set to
// when_needed keeps what it finds to itself, and flipping it to always publishes that without
// touching a word of the prompt.
func (p RoutinePatch) changesWhatItDoes() bool {
	return p.Prompt != nil || p.Steps != nil || p.Finish != nil || p.Channel != nil ||
		p.TeamID != nil || p.AutoConfirm != nil || p.Notify != nil || p.NotifyWhen != nil
}

func (s *Store) UpdateRoutine(ctx context.Context, orgID, id int64, p RoutinePatch) error {
	rs, err := s.Routines(ctx, orgID, "")
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.ID != id {
			continue
		}
		if p.Channel != nil {
			r.Channel = *p.Channel
		}
		if p.TeamID != nil {
			r.TeamID = *p.TeamID
		}
		if p.Cron != nil {
			r.Cron = *p.Cron
		}
		if p.TZ != nil {
			r.TZ = *p.TZ
		}
		if p.Prompt != nil {
			r.Prompt = *p.Prompt
		}
		if p.Notify != nil {
			r.Notify = notifyMode(*p.Notify)
		}
		if p.NotifyWhen != nil {
			r.NotifyWhen = *p.NotifyWhen
		}
		if p.Finish != nil {
			r.Finish = routineFinish(*p.Finish)
		}
		if p.Model != nil {
			r.Model = strings.TrimSpace(*p.Model)
		}
		if p.AutoConfirm != nil {
			r.AutoConfirm = *p.AutoConfirm
		}
		if p.Steps != nil {
			steps, err := parseRoutineSteps(*p.Steps)
			if err != nil {
				return err
			}
			r.Steps = encodeRoutineSteps(steps)
		}
		// Who a routine runs as follows whoever last said what it does. Each run spends the
		// personal connections of the person in created_by (routines.go, oauth_user.go), so
		// rewriting a prompt is writing an instruction for somebody else's Google account to
		// carry out: without this, anyone holding routines.manage could point a colleague's
		// routine at their mail and read the answer in a channel, and the colleague would never
		// see the instruction that did it. playground.go refuses the same borrow by having no
		// "run as" field at all, and this is that rule arriving at the other door.
		//
		// The edit itself is allowed — an admin fixing a typo in somebody's routine is the
		// ordinary case — and it is the ownership that moves. An editor with no Slack identity
		// leaves it empty, which is already what a run treats as the bot itself and reaches
		// nobody's personal account.
		if p.changesWhatItDoes() && p.EditedBy != r.CreatedBy {
			r.CreatedBy = p.EditedBy
		}
		next, err := nextRun(r.Cron, r.TZ, time.Now())
		if err != nil {
			return err
		}
		if routineTooFrequent(r.Cron, r.TZ) {
			return fmt.Errorf("routines run at most every %s; pick a wider schedule", minRoutineInterval)
		}
		_, err = s.db.ExecContext(ctx, `update routines set team_id=?, channel=?, cron=?, tz=?, prompt=?, next_run=?, notify=?, notify_when=?, steps=?, finish=?, model=?, auto_confirm=?, created_by=? where org_id=? and id=?`,
			r.TeamID, r.Channel, r.Cron, r.TZ, r.Prompt, next.UTC().Format(time.DateTime), r.Notify, r.NotifyWhen, r.Steps, routineFinish(r.Finish), r.Model, boolInt(r.AutoConfirm), r.CreatedBy, orgID, id)
		return err
	}
	return sql.ErrNoRows
}

func (s *Store) DeleteRoutine(ctx context.Context, orgID, id int64) error {
	// The run log goes with it; there is nothing left to read it against.
	s.db.ExecContext(ctx, `delete from routine_runs where org_id=? and routine_id=?`, orgID, id)
	_, err := s.db.ExecContext(ctx, `delete from routines where org_id=? and id=?`, orgID, id)
	return err
}

// RoutineRun is one execution of a routine, including the ones that stayed out of the channel.
type RoutineRun struct {
	ID, RoutineID, OrgID          int64
	TeamID, Channel, ThreadTS     string
	Status, Reason, Output, Error string
	TokensIn, TokensOut           int
	CostUSD                       float64
	StartedAt, FinishedAt         string
	MS                            int
}

// routineRunsKept bounds the log: a routine may run every 15 minutes, so without a ceiling the
// table grows without end for no one's benefit. Trimming on insert keeps it deterministic —
// no sweeper to schedule, and nothing to go wrong while nobody is looking.
const routineRunsKept = 100

func (s *Store) AddRoutineRun(ctx context.Context, r RoutineRun) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into routine_runs
		(org_id, routine_id, team_id, channel, thread_ts, status, reason, output, error, tokens_in, tokens_out, cost_usd, started_at, finished_at, ms)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id`,
		r.OrgID, r.RoutineID, r.TeamID, r.Channel, r.ThreadTS, r.Status, r.Reason, r.Output, r.Error,
		r.TokensIn, r.TokensOut, r.CostUSD, r.StartedAt, r.FinishedAt, r.MS).Scan(&id)
	if err != nil {
		return 0, err
	}
	s.db.ExecContext(ctx, `delete from routine_runs where org_id=? and routine_id=? and id <= (
		select id from routine_runs where org_id=? and routine_id=? order by id desc limit 1 offset ?)`,
		r.OrgID, r.RoutineID, r.OrgID, r.RoutineID, routineRunsKept)
	return id, nil
}

const routineRunCols = `id, routine_id, coalesce(org_id,0), coalesce(team_id,''), coalesce(channel,''), coalesce(thread_ts,''),
	coalesce(status,''), coalesce(reason,''), coalesce(output,''), coalesce(error,''),
	coalesce(tokens_in,0), coalesce(tokens_out,0), coalesce(cost_usd,0), coalesce(started_at,''), coalesce(finished_at,''), coalesce(ms,0)`

// RoutineRuns is one routine's history, newest first.
func (s *Store) RoutineRuns(ctx context.Context, orgID, routineID int64, limit int) ([]RoutineRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	return scanRoutineRuns(s.db.QueryContext(ctx, `select `+routineRunCols+` from routine_runs where org_id=? and routine_id=? order by id desc limit ?`, orgID, routineID, limit))
}

// RoutineRunByID is one run with its output untruncated.
func (s *Store) RoutineRunByID(ctx context.Context, orgID, runID int64) (*RoutineRun, error) {
	rs, err := scanRoutineRuns(s.db.QueryContext(ctx, `select `+routineRunCols+` from routine_runs where org_id=? and id=?`, orgID, runID))
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, sql.ErrNoRows
	}
	return &rs[0], nil
}

func scanRoutineRuns(rows *sql.Rows, err error) ([]RoutineRun, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineRun
	for rows.Next() {
		var r RoutineRun
		if err := rows.Scan(&r.ID, &r.RoutineID, &r.OrgID, &r.TeamID, &r.Channel, &r.ThreadTS,
			&r.Status, &r.Reason, &r.Output, &r.Error,
			&r.TokensIn, &r.TokensOut, &r.CostUSD, &r.StartedAt, &r.FinishedAt, &r.MS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// boolInt is how a Go bool goes into a SQLite integer column.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) SetRoutineEnabled(ctx context.Context, orgID, id int64, enabled bool) (int64, error) {
	en := 0
	if enabled {
		en = 1
	}
	res, err := s.db.ExecContext(ctx, `update routines set enabled=? where org_id=? and id=?`, en, orgID, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetRoutineEnabledInChannel is the chat-facing toggle, and it reaches only a routine that lives
// in the channel the request came from. `!routine off <id>` and the delete_routine tool take an
// id straight from a member (or the model), and routine ids are guessable, so without the channel
// predicate a member in one channel could disable or re-enable another channel's routine — a
// billing digest, a monitor — by number. The console (SetRoutineEnabled, permission-gated) is
// where org-wide control belongs.
func (s *Store) SetRoutineEnabledInChannel(ctx context.Context, orgID int64, channel string, id int64, enabled bool) (int64, error) {
	en := 0
	if enabled {
		en = 1
	}
	res, err := s.db.ExecContext(ctx, `update routines set enabled=? where org_id=? and id=? and channel=?`, en, orgID, id, channel)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- doc chunks (RAG) ----

type DocChunk struct {
	ID                                       int64
	Source, DocID, URL, Title, Heading, Text string
	Hash                                     string
	Embedding                                []float32
	// EmbedID is which endpoint and model made Embedding: '' for the deployment's own, and
	// "<host>|<model>" for an organisation's (Indexer.embedID). Vectors from two of them are not
	// comparable, so a search reads only the chunks embedded the way its query is.
	EmbedID string
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// DocHashes is the chunks of one document already embedded the way embedID embeds. A chunk
// embedded some other way counts as missing, so its document is embedded again: its text may be
// the same, and its vector no longer means anything to the model searching it.
func (s *Store) DocHashes(ctx context.Context, orgID int64, docID, embedID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `select hash from doc_chunks where org_id=? and doc_id=? and embed_id=?`, orgID, docID, embedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		m[h] = true
	}
	return m, rows.Err()
}

// ReplaceDoc swaps one document's chunks. The delete is scoped to the organisation as well as
// the document: doc ids are relative paths, so two organisations that both upload "handbook.md"
// share one, and without the predicate re-indexing in one would wipe the other's.
func (s *Store) ReplaceDoc(ctx context.Context, orgID int64, docID string, chunks []DocChunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `delete from doc_chunks where org_id=? and doc_id=?`, orgID, docID); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, `insert into doc_chunks (org_id, source, doc_id, url, title, heading, chunk, hash, embedding, embed_id)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, orgID, c.Source, c.DocID, c.URL, c.Title, c.Heading, c.Text, c.Hash, encodeVec(c.Embedding), c.EmbedID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteDocsNotIn(ctx context.Context, orgID int64, source string, keep map[string]bool) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `select distinct doc_id from doc_chunks where org_id=? and source=?`, orgID, source)
	if err != nil {
		return 0, err
	}
	var gone []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		if !keep[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	var n int64
	for _, id := range gone {
		res, err := s.db.ExecContext(ctx, `delete from doc_chunks where org_id=? and doc_id=?`, orgID, id)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += k
	}
	return n, nil
}

// AllChunks loads one organisation's chunks with their vectors. Brute-force cosine is fine up to
// ~100k chunks; the pgvector index replaces this when the database moves.
//
// The org_id predicate here is the single most load-bearing one in the schema: this feeds every
// answer the bot gives, so a chunk retrieved from the wrong organisation is that organisation's
// private document read out loud in somebody else's Slack.
func (s *Store) AllChunks(ctx context.Context, orgID int64) ([]DocChunk, error) {
	rows, err := s.db.QueryContext(ctx, `select id, source, doc_id, coalesce(url,''), coalesce(title,''), coalesce(heading,''), chunk, embedding, embed_id
		from doc_chunks where org_id=?`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocChunk
	for rows.Next() {
		var c DocChunk
		var emb []byte
		if err := rows.Scan(&c.ID, &c.Source, &c.DocID, &c.URL, &c.Title, &c.Heading, &c.Text, &emb, &c.EmbedID); err != nil {
			return nil, err
		}
		c.Embedding = decodeVec(emb)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ChunkCountsByDoc(ctx context.Context, orgID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `select doc_id, count(*) from doc_chunks where org_id=? group by doc_id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		rows.Scan(&id, &n)
		m[id] = n
	}
	return m, nil
}

func (s *Store) DocStats(ctx context.Context, orgID int64) (docs, chunks int, err error) {
	err = s.db.QueryRowContext(ctx, `select count(distinct doc_id), count(*) from doc_chunks where org_id=?`, orgID).Scan(&docs, &chunks)
	return
}

// ---- audit + usage + budget ----

// LogToolCall records one tool call. result is what the tool handed back to the
// model (already redacted and capped), so the console can show the same output
// the model saw.
func (s *Store) LogToolCall(ctx context.Context, orgID int64, teamID, channel, threadTS, name, args, result string, ok bool, ms int64) {
	okv := 0
	if ok {
		okv = 1
	}
	s.db.ExecContext(ctx, `insert into tool_calls (org_id, team_id, channel, thread_ts, name, args, result, ok, ms) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		orgID, teamID, channel, threadTS, name, args, result, okv, ms)
	s.db.ExecContext(ctx, `update sessions set tool_calls = tool_calls + 1 where team_id=? and channel=? and thread_ts=?`, teamID, channel, threadTS)
}

// ThreadToolNames lists the distinct tool names called so far in a thread, most recent first.
// The agent uses it to reload an MCP connection's tools on later turns of a thread that already
// used them, without spending a round on use_connection again.
func (s *Store) ThreadToolNames(ctx context.Context, teamID, channel, threadTS string) []string {
	if threadTS == "" {
		return nil
	}
	return s.toolNames(ctx, `select name from tool_calls where team_id=? and channel=? and thread_ts=? group by name order by max(id) desc limit 50`,
		teamID, channel, threadTS)
}

// ChannelToolNames is the same question asked of the channel instead of one thread, over a recent
// window: what is this room in the habit of calling?
//
// It exists for the thread that has no history of its own, which is every thread on its first
// turn. Without it, a channel that uses the same MCP server every day pays for the habit twice
// over on each new thread — one round spent on use_connection, and then a round at full price,
// because the tools that call loads change the tool array and the tool array sits ahead of
// everything else in the prefix a provider has cached.
//
// The window is what keeps this from becoming "send every connection to everybody": a channel
// that has not touched a server in a fortnight stops carrying its definitions, and one that never
// touched it never carries them at all. The evidence is the channel's own.
func (s *Store) ChannelToolNames(ctx context.Context, teamID, channel string, within time.Duration) []string {
	if channel == "" {
		return nil
	}
	since := time.Now().Add(-within).UTC().Format(time.DateTime)
	return s.toolNames(ctx, `select name from tool_calls where team_id=? and channel=? and created_at >= ? group by name order by max(id) desc limit 50`,
		teamID, channel, since)
}

func (s *Store) toolNames(ctx context.Context, q string, args ...any) []string {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) LogUsage(ctx context.Context, orgID int64, teamID, channel, threadTS, model string, u Usage) {
	s.LogUsageBy(ctx, orgID, teamID, channel, threadTS, "", model, u)
}

// LogUsageBy records what one turn cost and, on an account with prepaid credit, takes it off the
// balance. Seven call sites reach this, including the fix-job worker reporting from its own
// container, so it is the one place spend is written and therefore the one place it can be charged.
//
// It takes the whole Usage rather than three numbers out of it because two of the five — the
// tokens served from cache, and the tokens spent on reasoning nobody ever reads — are the two
// that say whether the prompt-cache breakpoints and REASONING are earning their keep, and they
// were previously logged and thrown away. A worker reporting over the wire has neither and sends
// zero, which is honest: JobUsage does not carry them.
//
// Both writes are in one transaction. Split, a failed debit would be free model spend and a failed
// insert would be an uncharged turn; together, a failure loses the usage row instead, which is the
// wrong answer in the customer's favour rather than ours, and it is logged rather than swallowed.
//
// The context is detached first. The spend has already happened — the tokens are bought and the
// provider will invoice for them — so a caller whose request was cancelled must not be able to
// un-record it.
func (s *Store) LogUsageBy(ctx context.Context, orgID int64, teamID, channel, threadTS, userID, model string, u Usage) {
	ctx = context.WithoutCancel(ctx)
	cost := u.CostUSD
	// The second of the two places a cost is checked before it becomes money. validCost (llm.go)
	// guards the budget; this guards the balance, because the paths that reach here are not all
	// the path that goes through there — the worker's costs arrive over the wire from another
	// container.
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		slog.Error("usage cost is not a number of dollars; recording the turn as free", "org", orgID, "model", model, "cost", cost)
		cost = 0
	}
	// Spend on the organisation's own key is billed to it by its provider. It is recorded like any
	// other — it is what the organisation's own monthly budget is measured against — and charged
	// to nothing here.
	owner := keyOwnerPlatform
	if u.KeyOwner == keyOwnerOrg {
		owner = keyOwnerOrg
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("usage not recorded", "org", orgID, "err", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `insert into usage (org_id, team_id, channel, thread_ts, user_id, model, tokens_in, tokens_out, cached_in, tokens_reasoning, cost_usd, key_owner)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, orgID, teamID, channel, threadTS, userID, model,
		u.In, u.Out, u.CachedIn, u.Reasoning, cost, owner); err != nil {
		slog.Error("usage not recorded", "org", orgID, "err", err)
		return
	}
	if owner == keyOwnerOrg {
		cost = 0
	}
	if err := chargeCredit(ctx, tx, orgID, usdToMicros(cost)); err != nil {
		// Loud, because the alternative to noticing here is noticing in a month's reconciliation.
		slog.Error("credit not debited; the turn is not recorded either", "org", orgID, "cost_usd", cost, "err", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("usage not recorded", "org", orgID, "err", err)
	}
}

// TurnsByUserSince counts a user's turns in the last window (per-user rate limit).
func (s *Store) TurnsByUserSince(ctx context.Context, teamID, userID string, since time.Duration) int {
	var n int
	s.db.QueryRowContext(ctx, `select count(*) from usage where team_id=? and user_id=? and created_at >= ?`, teamID, userID,
		time.Now().Add(-since).UTC().Format(time.DateTime)).Scan(&n)
	return n
}

// ConsoleTurnsSince counts one console account's assistant turns in the window. Its own query
// rather than TurnsByUserSince, which is keyed on team_id and user_id with no organisation in
// it: a console account has no workspace, so that predicate would match every tenant at once,
// and an admin who also uses Slack would spend one allowance on both. The channel narrows it to
// the assistant, so asking questions in the console and working in a channel are separate
// budgets — which is what an admin expects, since one of them is their own tab and the other is
// a room full of people.
func (s *Store) ConsoleTurnsSince(ctx context.Context, orgID int64, actor string, since time.Duration) int {
	var n int
	s.db.QueryRowContext(ctx, `select count(*) from usage where org_id=? and user_id=? and channel=? and created_at >= ?`,
		orgID, actor, assistantChannel, time.Now().Add(-since).UTC().Format(time.DateTime)).Scan(&n)
	return n
}

// AlertOnce returns true the first time key is seen within the window.
func (s *Store) AlertOnce(ctx context.Context, orgID int64, key string, window time.Duration) bool {
	var at string
	err := s.db.QueryRowContext(ctx, `select created_at from alerts_sent where org_id=? and key=?`, orgID, key).Scan(&at)
	if err == nil {
		if t, perr := time.Parse(time.DateTime, at); perr == nil && time.Since(t) < window {
			return false
		}
	}
	s.db.ExecContext(ctx, `insert into alerts_sent (org_id, key, created_at) values (?, ?, ?)
		on conflict(org_id, key) do update set created_at=excluded.created_at`, orgID, key, now())
	return true
}

// ClearAlert forgets one alert key, so the next occurrence warns again inside the window that
// would otherwise have suppressed it. A top-up is exactly that case: the account was told it was
// running low, they fixed it, and the next time it runs low is news rather than a repeat.
func (s *Store) ClearAlert(ctx context.Context, orgID int64, key string) {
	s.db.ExecContext(ctx, `delete from alerts_sent where org_id=? and key=?`, orgID, key)
}

// ---- thread summaries (long-thread windowing) ----

func (s *Store) SessionSummary(ctx context.Context, teamID, channel, threadTS string) (summary, upto string) {
	s.db.QueryRowContext(ctx, `select coalesce(summary,''), coalesce(summary_upto,'') from sessions where team_id=? and channel=? and thread_ts=?`, teamID, channel, threadTS).Scan(&summary, &upto)
	return
}

func (s *Store) SetSessionSummary(ctx context.Context, teamID, channel, threadTS, summary, upto string) {
	s.db.ExecContext(ctx, `update sessions set summary=?, summary_upto=? where team_id=? and channel=? and thread_ts=?`, summary, upto, teamID, channel, threadTS)
}

// ---- file text cache ----

func (s *Store) FileText(ctx context.Context, teamID, id string) (string, bool) {
	var t string
	if err := s.db.QueryRowContext(ctx, `select text from file_texts where team_id=? and file_id=?`, teamID, id).Scan(&t); err != nil {
		return "", false
	}
	return t, true
}

func (s *Store) PutFileText(ctx context.Context, teamID, id, name, text string) {
	s.db.ExecContext(ctx, `insert into file_texts (team_id, file_id, name, text) values (?, ?, ?, ?)
		on conflict (team_id, file_id) do update set name=excluded.name, text=excluded.text`, teamID, id, name, text)
}

// ---- skills ----

type Skill struct {
	ID        int64  `json:"id"`
	BundleID  int64  `json:"bundle_id"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	Enabled   bool   `json:"enabled"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Store) SkillsForBundle(ctx context.Context, orgID, bundleID int64) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, `select id, bundle_id, name, content, enabled, updated_at from skills where org_id=? and bundle_id=? order by name`, orgID, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Skill{}
	for rows.Next() {
		var k Skill
		var en int
		if err := rows.Scan(&k.ID, &k.BundleID, &k.Name, &k.Content, &en, &k.UpdatedAt); err != nil {
			return nil, err
		}
		k.Enabled = en == 1
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) UpsertSkill(ctx context.Context, orgID int64, k Skill) (int64, error) {
	en := 0
	if k.Enabled {
		en = 1
	}
	if k.ID == 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `insert into skills (org_id, bundle_id, name, content, enabled) values (?, ?, ?, ?, ?) returning id`,
			orgID, k.BundleID, k.Name, k.Content, en).Scan(&id)
		return id, err
	}
	_, err := s.db.ExecContext(ctx, `update skills set name=?, content=?, enabled=?, updated_at=? where org_id=? and id=?`,
		k.Name, k.Content, en, now(), orgID, k.ID)
	return k.ID, err
}

func (s *Store) DeleteSkill(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `delete from skills where org_id=? and id=?`, orgID, id)
	return err
}

// MonthSpendOn is the month's spend on one key: the deployment's (keyOwnerPlatform) or the
// organisation's own (keyOwnerOrg). See budgetSpend for why an account's limits read it.
func (s *Store) MonthSpendOn(ctx context.Context, orgID int64, keyOwner string) (float64, error) {
	start := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	var v float64
	err := s.db.QueryRowContext(ctx, `select coalesce(sum(cost_usd),0) from usage where org_id=? and key_owner=? and created_at >= ?`,
		orgID, keyOwner, start).Scan(&v)
	return v, err
}

// MonthSpend returns USD spent since the first of the current UTC month. An empty teamID or
// channel means "do not filter on it", so the same call serves the account total, one
// workspace's total and one channel's.
func (s *Store) MonthSpend(ctx context.Context, orgID int64, teamID, channel string) (float64, error) {
	start := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	q, args := `select coalesce(sum(cost_usd),0) from usage where org_id=? and created_at >= ?`, []any{orgID, start}
	if teamID != "" {
		q += ` and team_id=?`
		args = append(args, teamID)
	}
	if channel != "" {
		q += ` and channel=?`
		args = append(args, channel)
	}
	var v float64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&v)
	return v, err
}

type UsageRow struct {
	TeamID  string
	Channel string
	// ChannelName is what the console calls the conversation (conversationNamer). Only the
	// console's overview fills it in. /v1/usage sends its own snake_case rows instead
	// (v1ChannelUsageJSON), since these PascalCase keys are the console's.
	ChannelName    string `json:",omitempty"`
	Turns, In, Out int
	Cost           float64
}

func (s *Store) UsageByChannel(ctx context.Context, orgID int64) ([]UsageRow, error) {
	start := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	rows, err := s.db.QueryContext(ctx, `select coalesce(team_id,''), channel, count(*), sum(tokens_in), sum(tokens_out), sum(cost_usd)
		from usage where org_id=? and created_at >= ? group by team_id, channel order by sum(cost_usd) desc`, orgID, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.TeamID, &r.Channel, &r.Turns, &r.In, &r.Out, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ConversationSpeakers is somebody who has spoken in each conversation, keyed "team|channel". In
// a direct message that can only be the person on the other end, which is what it is for: never
// sent anywhere, because on a channel it would single out one member for no reason. One pass over
// the organisation's usage, since that table is indexed by time and not by channel.
func (s *Store) ConversationSpeakers(ctx context.Context, orgID int64) map[string]string {
	speakers := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `select coalesce(team_id,''), coalesce(channel,''), max(user_id)
		from usage where org_id=? and user_id<>'' group by team_id, channel`, orgID)
	if err != nil {
		return speakers
	}
	defer rows.Close()
	for rows.Next() {
		var team, channel, user string
		if rows.Scan(&team, &channel, &user) == nil {
			speakers[team+"|"+channel] = user
		}
	}
	return speakers
}

type TurnRow struct {
	ID                                        int64
	TeamID, TeamName                          string
	At, Channel, ChannelName, ThreadTS, Model string
	In, Out                                   int
	Cost                                      float64
}

// TurnFilter narrows a read of the turns. Before and After are id cursors in the audit log's
// sense: Before walks back through history newest first, After walks forward oldest first from
// the last row a caller saw, and with neither the newest rows come first.
type TurnFilter struct {
	Channel string
	// Since is a lower bound on created_at, "" for none. It is how a count on the overview and
	// the rows behind it stay the same set: the tile passes the very window it counted.
	Since         string
	Limit         int
	Before, After int64
}

func (s *Store) RecentTurns(ctx context.Context, orgID int64, channel string, limit int, since string) ([]TurnRow, error) {
	return s.Turns(ctx, orgID, TurnFilter{Channel: channel, Since: since, Limit: limit})
}

func (s *Store) Turns(ctx context.Context, orgID int64, f TurnFilter) ([]TurnRow, error) {
	q := `select id, coalesce(team_id,''), created_at, coalesce(channel,''), coalesce(thread_ts,''), coalesce(model,''), tokens_in, tokens_out, cost_usd
		from usage where org_id=?`
	args := []any{orgID}
	if f.Channel != "" {
		q += ` and channel=?`
		args = append(args, f.Channel)
	}
	if f.Since != "" {
		q += ` and created_at >= ?`
		args = append(args, f.Since)
	}
	if f.Before > 0 {
		q += ` and id < ?`
		args = append(args, f.Before)
	}
	order := ` order by id desc limit ?`
	if f.After > 0 {
		q += ` and id > ?`
		args = append(args, f.After)
		order = ` order by id asc limit ?`
	}
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TurnRow{}
	for rows.Next() {
		var t TurnRow
		if err := rows.Scan(&t.ID, &t.TeamID, &t.At, &t.Channel, &t.ThreadTS, &t.Model, &t.In, &t.Out, &t.Cost); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type ToolCallRow struct {
	ID                                        int64
	TeamID                                    string
	At, Channel, ThreadTS, Name, Args, Result string
	// ChannelName is what the console calls the conversation; only the Activity list fills it in.
	ChannelName string `json:",omitempty"`
	OK          bool
	MS          int64
	// More is set when Result was cut for the list; the console fetches the
	// whole thing by ID.
	More bool
	// Private is a call whose arguments and result were one person's, so Args and Result are
	// what the log kept instead (privateMark). Only the console's reads set it (unmarkPrivate).
	Private bool
}

const toolCallCols = `id, coalesce(team_id,''), created_at, coalesce(channel,''), coalesce(thread_ts,''), name, coalesce(args,''), coalesce(result,''), ok, ms`

func scanToolCall(rows *sql.Rows) (ToolCallRow, error) {
	var t ToolCallRow
	var ok int
	err := rows.Scan(&t.ID, &t.TeamID, &t.At, &t.Channel, &t.ThreadTS, &t.Name, &t.Args, &t.Result, &ok, &t.MS)
	t.OK = ok == 1
	return t, err
}

// RecentToolCalls returns the newest calls first. failedOnly narrows to the ones that
// errored, which is what the console's error filter reads: filtering here rather than in
// the browser means a failure stays reachable however many successful calls came after it.
func (s *Store) RecentToolCalls(ctx context.Context, orgID int64, channel string, limit int, failedOnly bool, since string) ([]ToolCallRow, error) {
	q := `select ` + toolCallCols + ` from tool_calls where org_id=?`
	args := []any{orgID}
	if channel != "" {
		q += ` and channel=?`
		args = append(args, channel)
	}
	if failedOnly {
		q += ` and ok=0`
	}
	if since != "" {
		q += ` and created_at >= ?`
		args = append(args, since)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q+` order by id desc limit ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallRow{}
	for rows.Next() {
		t, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ToolCall returns one call with its full result. The organisation is part of the lookup rather
// than a check on the row afterwards, so a miss is indistinguishable from a wrong id.
func (s *Store) ToolCall(ctx context.Context, orgID, id int64) (ToolCallRow, error) {
	rows, err := s.db.QueryContext(ctx, `select `+toolCallCols+` from tool_calls where org_id=? and id=?`, orgID, id)
	if err != nil {
		return ToolCallRow{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return ToolCallRow{}, sql.ErrNoRows
	}
	return scanToolCall(rows)
}

// SeenEvent records an event key; returns true the first time (survives restarts and multi-replica).
//
// owner is the inbox delivery that claimed the key, and it is what makes a retry different from a
// duplicate. Slack sends two events for one mention (app_mention and message) which share a
// channel and timestamp, and only the first may be answered — that is what this guards. But a
// delivery re-dispatched after the process died mid-turn is the *same* work, and refusing it here
// would swallow the inbox's retry and lose the message for good. So a key already held by this
// delivery is allowed through; a key held by anything else is not.
//
// An empty owner keeps the old behaviour: seen once, never again.
func (s *Store) SeenEvent(ctx context.Context, key, owner string) bool {
	res, err := s.db.ExecContext(ctx, `insert into seen_events (key, owner) values (?,?) on conflict do nothing`, key, owner)
	if err != nil {
		return true
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.db.ExecContext(ctx, `delete from seen_events where created_at < ?`, nowMinus(24*time.Hour))
		s.sweepOAuthStates(ctx)
		return true
	}
	if owner == "" {
		return false
	}
	var prior string
	if err := s.db.QueryRowContext(ctx, `select owner from seen_events where key=?`, key).Scan(&prior); err != nil {
		return false
	}
	return prior == owner
}
