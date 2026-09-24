package app

// Active users: who used the bot, counted for the size they are being charged for.
//
// The definition lives in migrations/sqlite/0013_active_users.sql and is worth repeating once
// here, because it is the sort of thing that drifts: a user is a person in a connected Slack
// workspace who caused a turn to run. Not a console sign-in, not an API key's owner, not the
// creator of a routine that has been running on its own ever since. The write side enforces
// that (active_users.go); this file only stores and counts.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// activeUserWindow is the window the size is judged over. Rolling rather than calendar: a
// customer who signs up on the 28th should not be measured against two days of activity, and a
// month boundary should not quietly halve the figure on the 1st.
const activeUserWindow = 30 * 24 * time.Hour

// activeUserRetention is how long these rows are kept. They outlive the tenant's own
// data_retention_days deliberately — see the migration — but not forever: a year and a bit is
// enough to answer "what were we charging them for last year" and to draw a trend, and nothing
// reads further back than that.
const activeUserRetention = 400 * 24 * time.Hour

// ActiveMark is one person, seen once. Everything about it is a natural key except the counter.
type ActiveMark struct {
	OrgID     int64
	TeamID    string
	SlackUser string
	Email     string // '' when the install never granted users:read.email
}

// identity is how a person is named in this table: the workspace and their id in it. Never a
// bare user id — Slack only promises those are unique inside one workspace, so a bare id can
// address a different person elsewhere. personal_memory_api.go documents the same trap.
func (m ActiveMark) identity() string { return m.TeamID + ":" + m.SlackUser }

// emailHash is the dedupe key for one human in two connected workspaces. Lowercased and trimmed
// first, so the same address written two ways is one person.
//
// It is not anonymisation and the migration says so: a hash of an address at a known domain is
// guessable. Storing the hash rather than the address is about this table outliving the tenant's
// retention policy without becoming a directory of their staff.
func emailHash(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])
}

// MarkActive records that somebody used the bot today. One row per person per day, so a person
// who asks fifty questions is one user and the turn count is still there to show why.
//
// The email hash is written on first sight and never cleared by a later mark that lacks one: the
// scope can be missing for one call — a users.info that failed, a cache miss during an outage —
// and losing the dedupe key over that would silently split one person into two.
func (s *Store) MarkActive(ctx context.Context, m ActiveMark) error {
	if m.OrgID == 0 || m.TeamID == "" || m.SlackUser == "" {
		return nil
	}
	at := now()
	_, err := s.db.ExecContext(ctx,
		`insert into active_users (org_id, identity, day, team_id, email_hash, turns, first_seen, last_seen)
		 values (?, ?, ?, ?, ?, 1, ?, ?)
		 on conflict (org_id, identity, day) do update
		    set turns      = active_users.turns + 1,
		        last_seen  = ?,
		        email_hash = coalesce(nullif(excluded.email_hash, ''), active_users.email_hash)`,
		m.OrgID, m.identity(), today(), m.TeamID, emailHash(m.Email), at, at, at)
	return err
}

// activeSince is the lower bound on `day` for a window, in the same 'YYYY-MM-DD' the column holds.
func activeSince(window time.Duration) string {
	return time.Now().UTC().Add(-window).Format(time.DateOnly)
}

// dedupedIdentity is the expression that makes one human one user: their email hash where we
// know it, and the per-workspace identity where we do not. Written once and used by every count
// below, because a breakdown that dedupes differently from the total is a support ticket.
const dedupedIdentity = `coalesce(nullif(email_hash, ''), identity)`

// ActiveUsers counts the distinct people who used the bot in the window.
func (s *Store) ActiveUsers(ctx context.Context, orgID int64, window time.Duration) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`select count(distinct `+dedupedIdentity+`) from active_users where org_id=? and day >= ?`,
		orgID, activeSince(window)).Scan(&n)
	return n, err
}

// ActiveUsersByTeam is the same figure per connected workspace. It deliberately does NOT sum to
// the total: somebody in two workspaces is one user overall and appears in both rows here. That
// is the point of it — it is what lets an admin see why their total is lower than the parts, or,
// where no install granted users:read.email, why it is not.
type TeamActiveUsers struct {
	TeamID string `json:"team_id"`
	Name   string `json:"name"`
	Users  int    `json:"users"`
}

func (s *Store) ActiveUsersByTeam(ctx context.Context, orgID int64, window time.Duration) ([]TeamActiveUsers, error) {
	rows, err := s.db.QueryContext(ctx,
		`select a.team_id, coalesce(t.name, ''), count(distinct `+dedupedIdentity+`)
		   from active_users a left join teams t on t.team_id = a.team_id
		  where a.org_id=? and a.day >= ?
		  group by a.team_id, t.name
		  order by count(distinct `+dedupedIdentity+`) desc`,
		orgID, activeSince(window))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamActiveUsers
	for rows.Next() {
		var r TeamActiveUsers
		if err := rows.Scan(&r.TeamID, &r.Name, &r.Users); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveUserRow is one person on the drill-down list: the rows behind the number on the tile.
type ActiveUserRow struct {
	Identity  string `json:"identity"` // 'T…:U…'
	TeamID    string `json:"team_id"`
	TeamName  string `json:"team_name"`
	SlackUser string `json:"slack_user"`
	Name      string `json:"name"` // resolved at read time; never stored
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Days      int    `json:"days"`
	Turns     int    `json:"turns"`
}

// ActiveUserList is the list behind the count, newest activity first. One line per identity
// rather than per deduped person: an admin looking at this wants to see both workspace accounts
// and understand that they are one user, which a collapsed list could not show.
func (s *Store) ActiveUserList(ctx context.Context, orgID int64, window time.Duration, limit int) ([]ActiveUserRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`select a.identity, a.team_id, coalesce(t.name, ''),
		        count(*), sum(a.turns), min(a.first_seen), max(a.last_seen)
		   from active_users a left join teams t on t.team_id = a.team_id
		  where a.org_id=? and a.day >= ?
		  group by a.identity, a.team_id, t.name
		  order by max(a.last_seen) desc
		  limit ?`,
		orgID, activeSince(window), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveUserRow
	for rows.Next() {
		var r ActiveUserRow
		if err := rows.Scan(&r.Identity, &r.TeamID, &r.TeamName, &r.Days, &r.Turns, &r.FirstSeen, &r.LastSeen); err != nil {
			return nil, err
		}
		// The inverse of ActiveMark.identity. A team id cannot contain a colon, so the first one
		// separates them.
		if _, user, ok := strings.Cut(r.Identity, ":"); ok {
			r.SlackUser = user
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SweepActiveUsers is this table's own retention, run from the hourly billing loop. It is not in
// retentionTables and must not be: see the note there and the migration.
func (s *Store) SweepActiveUsers(ctx context.Context, older time.Duration) {
	s.db.ExecContext(ctx, `delete from active_users where day < ?`,
		time.Now().UTC().Add(-older).Format(time.DateOnly))
}
