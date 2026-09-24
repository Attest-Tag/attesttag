package app

import (
	"context"
	"database/sql"
	"time"
)

// The store side of a per-person credential. A connection of cred_type oauth_user holds the
// organisation's OAuth client and nothing that can call anything; what spends is one row per
// person, sealed the same way a connection secret is. Nothing here can read a secret it holds.
//
// Every lookup takes the organisation as well as the connection, for the reason Store.Connection
// spells out: the ids come from a URL or a Slack payload and the row holds a sealed credential,
// so a missing predicate hands one customer another's token.

// UserConnection is one person's sign-in to one connection.
type UserConnection struct {
	ID          int64  `json:"id"`
	ConnID      int64  `json:"conn_id"`
	TeamID      string `json:"team_id"`
	SlackUserID string `json:"slack_user_id"`
	Account     string `json:"account"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	LastUsed    string `json:"last_used"`
	// Instructions is how this person wants their own account used, in their words. It reaches
	// the model only on their own turns, and it is theirs to write and to delete.
	Instructions string `json:"instructions"`
	secretEnc    []byte
}

// Connected reports whether this row can actually spend anything. A row exists as soon as
// somebody writes instructions, which is before they have signed in and may be instead of ever
// signing in — so the presence of a row is not the question, and asking it that way would report
// an unconnected person as connected.
func (u *UserConnection) Connected() bool { return u != nil && len(u.secretEnc) > 0 }

const userConnCols = `id, conn_id, coalesce(team_id,''), slack_user_id, coalesce(account,''), coalesce(status,'active'),
	created_at, coalesce(last_used,''), coalesce(instructions,''), secret_enc`

func scanUserConn(row interface{ Scan(...any) error }) (*UserConnection, error) {
	var u UserConnection
	if err := row.Scan(&u.ID, &u.ConnID, &u.TeamID, &u.SlackUserID, &u.Account, &u.Status,
		&u.CreatedAt, &u.LastUsed, &u.Instructions, &u.secretEnc); err != nil {
		return nil, err
	}
	return &u, nil
}

// maxUserInstructions bounds what one person can add to every one of their own prompts. Long
// enough for a signature and a handful of house rules, short enough that it cannot crowd out the
// question being asked.
const maxUserInstructions = 2000

// SaveUserInstructions writes one person's own notes for one connection, creating the row when
// they have not signed in yet: somebody may well write down how their mail should read before
// they hand over the mailbox. The row it creates holds no secret, so it grants nothing.
func (s *Store) SaveUserInstructions(ctx context.Context, orgID, connID int64, teamID, slackUserID, text string) error {
	if len([]rune(text)) > maxUserInstructions {
		text = string([]rune(text)[:maxUserInstructions])
	}
	_, err := s.db.ExecContext(ctx, `insert into user_connections
		(org_id, conn_id, team_id, slack_user_id, instructions, status, created_at, updated_at)
		values (?, ?, ?, ?, ?, 'active', ?, ?)
		on conflict(conn_id, team_id, slack_user_id) do update set
			instructions=excluded.instructions, updated_at=excluded.updated_at`,
		orgID, connID, teamID, slackUserID, text, now(), now())
	return err
}

// UserConnection finds the person's sign-in, or nil when they have not connected yet. A nil
// result is the ordinary case on a first call, not an error: it is what makes the bot send
// them a link instead of failing.
func (s *Store) UserConnection(ctx context.Context, orgID, connID int64, teamID, slackUserID string) (*UserConnection, error) {
	if slackUserID == "" {
		return nil, nil
	}
	u, err := scanUserConn(s.db.QueryRowContext(ctx, `select `+userConnCols+` from user_connections
		where org_id=? and conn_id=? and team_id=? and slack_user_id=? and status='active'`,
		orgID, connID, teamID, slackUserID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// SaveUserConnection writes the sealed tokens for one person, replacing whatever they had. A
// second sign-in is how somebody switches Google account or re-grants a scope they declined.
func (s *Store) SaveUserConnection(ctx context.Context, orgID, connID int64, teamID, slackUserID, account string, secretEnc []byte) error {
	_, err := s.db.ExecContext(ctx, `insert into user_connections
		(org_id, conn_id, team_id, slack_user_id, account, secret_enc, status, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, 'active', ?, ?)
		on conflict(conn_id, team_id, slack_user_id) do update set
			account=excluded.account, secret_enc=excluded.secret_enc, status='active', updated_at=excluded.updated_at`,
		orgID, connID, teamID, slackUserID, account, secretEnc, now(), now())
	return err
}

// UpdateUserSecret re-seals one row in place, after a refresh grant returned a new access token.
func (s *Store) UpdateUserSecret(ctx context.Context, orgID, id int64, secretEnc []byte) error {
	_, err := s.db.ExecContext(ctx, `update user_connections set secret_enc=?, updated_at=? where org_id=? and id=?`,
		secretEnc, now(), orgID, id)
	return err
}

func (s *Store) TouchUserConnection(ctx context.Context, orgID, id int64) {
	s.db.ExecContext(ctx, `update user_connections set last_used=? where org_id=? and id=?`, now(), orgID, id)
}

// DeleteUserConnection forgets one person's tokens. The caller revokes them at the provider
// first; dropping the row on its own would leave a grant standing that nobody can see.
func (s *Store) DeleteUserConnection(ctx context.Context, orgID, connID int64, teamID, slackUserID string) error {
	_, err := s.db.ExecContext(ctx, `delete from user_connections where org_id=? and conn_id=? and team_id=? and slack_user_id=?`,
		orgID, connID, teamID, slackUserID)
	return err
}

// UserConnectionsFor lists who has connected, for the console. The sealed secret rides along on
// the struct unexported, so this is safe to hand to writeJSON.
func (s *Store) UserConnectionsFor(ctx context.Context, orgID, connID int64) ([]*UserConnection, error) {
	rows, err := s.db.QueryContext(ctx, `select `+userConnCols+` from user_connections
		where org_id=? and conn_id=? order by created_at`, orgID, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*UserConnection{}
	for rows.Next() {
		u, err := scanUserConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUserConnectionsFor drops every sign-in under a connection, for when the connection
// itself is deleted. Tokens for a connection that no longer exists are unreachable but still
// live at the provider, and nothing in the console would ever show them again.
func (s *Store) DeleteUserConnectionsFor(ctx context.Context, orgID, connID int64) error {
	_, err := s.db.ExecContext(ctx, `delete from user_connections where org_id=? and conn_id=?`, orgID, connID)
	return err
}

// connectState is one half-finished sign-in.
type connectState struct {
	OrgID       int64
	ConnID      int64
	TeamID      string
	SlackUserID string
	Verifier    string
	RedirectURI string
}

const connectStateTTL = 15 * time.Minute

func (s *Store) SaveConnectState(ctx context.Context, state string, cs connectState) error {
	s.db.ExecContext(ctx, `delete from connect_states where expires_at < ?`, now())
	_, err := s.db.ExecContext(ctx, `insert into connect_states
		(state, org_id, conn_id, team_id, slack_user_id, verifier, redirect_uri, expires_at) values (?, ?, ?, ?, ?, ?, ?, ?)`,
		state, cs.OrgID, cs.ConnID, cs.TeamID, cs.SlackUserID, cs.Verifier, cs.RedirectURI,
		time.Now().UTC().Add(connectStateTTL).Format(time.DateTime))
	return err
}

// TakeConnectState spends the state token exactly once, so a replayed callback finds nothing.
// One statement, for the reason TakeOAuthState is one: two callbacks racing on the same state
// must not both read the row before either deletes it.
func (s *Store) TakeConnectState(ctx context.Context, state string) (*connectState, error) {
	if state == "" {
		return nil, nil
	}
	var cs connectState
	var expires string
	err := s.db.QueryRowContext(ctx, `delete from connect_states where state=? returning
		org_id, conn_id, coalesce(team_id,''), slack_user_id, verifier, coalesce(redirect_uri,''), expires_at`, state).
		Scan(&cs.OrgID, &cs.ConnID, &cs.TeamID, &cs.SlackUserID, &cs.Verifier, &cs.RedirectURI, &expires)
	if err == sql.ErrNoRows || err != nil {
		return nil, nil
	}
	if expires < time.Now().UTC().Format(time.DateTime) {
		return nil, nil
	}
	return &cs, nil
}

// PersonalConnections are the organisation's per-person connections, for a turn that can reach
// none of them. "Google Workspace is set up but not attached to this channel" and "nothing here
// runs on your own account" are different answers, and a model told neither invents a third:
// that an admin has to connect somebody's mailbox for them, which is the one thing no admin can
// do. The rows come back whole because the link that connects one is minted from its id.
func (s *Store) PersonalConnections(ctx context.Context, orgID int64) ([]*Connection, error) {
	rows, err := s.db.QueryContext(ctx,
		`select `+connCols+` from connections where org_id=? and cred_type='oauth_user' and status='active' order by name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Connection
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
