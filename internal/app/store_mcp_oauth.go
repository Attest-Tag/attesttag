package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// The store side of the MCP server's OAuth (mcp_oauth.go): the clients that registered, the codes
// between the consent screen and the token endpoint, and the grants people approved. Tokens are
// kept as hashes only, as developer keys are, so a copy of the database is not a set of working
// credentials.

// MCPClient is a client as it registered itself.
type MCPClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	// AuthMethod is how it authenticates at the token endpoint: "none", or a client secret for
	// the few clients that register as confidential. SecretHash is that secret's hash.
	AuthMethod string
	SecretHash string
	CreatedAt  string
}

func (s *Store) CreateMCPClient(ctx context.Context, c MCPClient) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `insert into mcp_clients (id, name, redirect_uris, auth_method, secret_hash, created_at)
		values (?, ?, ?, ?, ?, ?)`, c.ID, c.Name, string(uris), c.AuthMethod, c.SecretHash, c.CreatedAt)
	return err
}

// MCPClient is nil, nil for a client_id nobody registered.
func (s *Store) MCPClient(ctx context.Context, id string) (*MCPClient, error) {
	var c MCPClient
	var uris string
	err := s.db.QueryRowContext(ctx, `select id, name, redirect_uris, auth_method, secret_hash, created_at
		from mcp_clients where id=?`, id).Scan(&c.ID, &c.Name, &uris, &c.AuthMethod, &c.SecretHash, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	return &c, nil
}

// TouchMCPClient stamps a client as used, at most once a day: what the sweep reads to tell a
// client somebody connected from one that registered and was never heard from again.
func (s *Store) TouchMCPClient(ctx context.Context, id string) {
	s.db.ExecContext(ctx, `update mcp_clients set last_used_at=? where id=? and last_used_at < ?`,
		now(), id, time.Now().Add(-24*time.Hour).UTC().Format(time.DateTime))
}

// SweepMCP drops what nobody can use any more: codes past their ten minutes, and clients that
// registered and never had a grant made for them in a week. Registration is open to anybody, so
// without this the second table would grow for as long as somebody's script felt like calling it.
func (s *Store) SweepMCP(ctx context.Context) {
	s.db.ExecContext(ctx, `delete from mcp_codes where expires_at < ?`, time.Now().Add(-time.Hour).UTC().Format(time.DateTime))
	s.db.ExecContext(ctx, `delete from mcp_clients where last_used_at='' and created_at < ?`,
		time.Now().Add(-7*24*time.Hour).UTC().Format(time.DateTime))
}

// mcpCode is an authorization code's row.
type mcpCode struct {
	Hash, ClientID         string
	OrgID, UserID          int64
	RedirectURI, Challenge string
	Scope, Resource        string
	ExpiresAt              string
	GrantID                int64
}

func (s *Store) CreateMCPCode(ctx context.Context, c mcpCode) error {
	_, err := s.db.ExecContext(ctx, `insert into mcp_codes (code_hash, client_id, org_id, user_id, redirect_uri,
		code_challenge, scope, resource, expires_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Hash, c.ClientID, c.OrgID, c.UserID, c.RedirectURI, c.Challenge, c.Scope, c.Resource, c.ExpiresAt)
	return err
}

// UseMCPCode spends a code. first is false when it had been spent already; the row still comes
// back then, so the caller can end the grant the first exchange made.
func (s *Store) UseMCPCode(ctx context.Context, hash string) (c *mcpCode, first bool, err error) {
	res, err := s.db.ExecContext(ctx, `update mcp_codes set used_at=? where code_hash=? and used_at=''`, now(), hash)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	var row mcpCode
	err = s.db.QueryRowContext(ctx, `select code_hash, client_id, org_id, user_id, redirect_uri, code_challenge,
		scope, resource, expires_at, grant_id from mcp_codes where code_hash=?`, hash).
		Scan(&row.Hash, &row.ClientID, &row.OrgID, &row.UserID, &row.RedirectURI, &row.Challenge,
			&row.Scope, &row.Resource, &row.ExpiresAt, &row.GrantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &row, n == 1, nil
}

func (s *Store) SetMCPCodeGrant(ctx context.Context, hash string, grantID int64) error {
	_, err := s.db.ExecContext(ctx, `update mcp_codes set grant_id=? where code_hash=?`, grantID, hash)
	return err
}

// MCPGrant is one connected app as the console lists it.
type MCPGrant struct {
	ID     int64 `json:"id"`
	OrgID  int64 `json:"-"`
	UserID int64 `json:"-"`
	// Owner is the email of the person it acts as, resolved for display.
	Owner       string `json:"owner"`
	ClientID    string `json:"-"`
	ClientName  string `json:"client_name"`
	RedirectURI string `json:"-"`
	// ReturnsTo is the host the client's redirect address names: what a person can check a
	// client by, where its name is whatever it said about itself.
	ReturnsTo  string `json:"returns_to"`
	Scope      string `json:"-"`
	Resource   string `json:"-"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
	// ExpiresAt is when the grant lapses if its client stops refreshing it.
	ExpiresAt string `json:"expires_at"`
	RevokedAt string `json:"revoked_at"`
}

// mcpGrantAuth is what checking a token needs, and nothing else.
type mcpGrantAuth struct {
	ID, OrgID, UserID int64
	ClientID          string
	Scope, Resource   string
	AccessExpiresAt   string
	RefreshExpiresAt  string
	RevokedAt         string
	LastUsed          string
}

// CreateMCPGrant records a person's consent and the first pair of tokens. It ends any grant the
// same person already gave the same client in the same organisation: connecting again replaces
// the old connection rather than leaving two.
func (s *Store) CreateMCPGrant(ctx context.Context, g MCPGrant, accessHash, accessExp, refreshHash, refreshExp string) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `update mcp_grants set revoked_at=? where org_id=? and user_id=? and client_id=? and revoked_at=''`,
		now(), g.OrgID, g.UserID, g.ClientID); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into mcp_grants (org_id, user_id, client_id, client_name, redirect_uri, scope,
		resource, access_hash, access_expires_at, refresh_hash, refresh_expires_at, created_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id`,
		g.OrgID, g.UserID, g.ClientID, g.ClientName, g.RedirectURI, g.Scope, g.Resource,
		accessHash, accessExp, refreshHash, refreshExp, now()).Scan(&id)
	return id, err
}

const mcpGrantAuthCols = `id, org_id, user_id, client_id, scope, resource, access_expires_at, refresh_expires_at, revoked_at, last_used_at`

func scanMCPGrantAuth(row *sql.Row) (*mcpGrantAuth, error) {
	var g mcpGrantAuth
	err := row.Scan(&g.ID, &g.OrgID, &g.UserID, &g.ClientID, &g.Scope, &g.Resource, &g.AccessExpiresAt,
		&g.RefreshExpiresAt, &g.RevokedAt, &g.LastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// MCPGrantByAccess is the lookup an MCP request makes. Like APIKeyByHash it names no
// organisation, because the token is the authority and finding out whose it is is the point; the
// row it returns carries the organisation, and everything after is scoped by that.
func (s *Store) MCPGrantByAccess(ctx context.Context, hash string) (*mcpGrantAuth, error) {
	return scanMCPGrantAuth(s.db.QueryRowContext(ctx, `select `+mcpGrantAuthCols+` from mcp_grants where access_hash=?`, hash))
}

// MCPGrantByRefresh finds the grant a refresh token belongs to. replayed says the token is one a
// refresh already replaced: the client was handed a new one, so whoever is presenting the old
// one is not the client, or not the only holder.
func (s *Store) MCPGrantByRefresh(ctx context.Context, hash string) (g *mcpGrantAuth, replayed bool, err error) {
	g, err = scanMCPGrantAuth(s.db.QueryRowContext(ctx, `select `+mcpGrantAuthCols+` from mcp_grants where refresh_hash=?`, hash))
	if g != nil || err != nil {
		return g, false, err
	}
	g, err = scanMCPGrantAuth(s.db.QueryRowContext(ctx, `select `+mcpGrantAuthCols+` from mcp_grants where prev_refresh_hash=? and prev_refresh_hash<>''`, hash))
	return g, g != nil, err
}

// RotateMCPGrant swaps a grant's tokens for new ones, on the condition that the refresh token
// being spent is still the current one: of two refreshes racing with the same token, one wins
// and the other finds nothing to update.
func (s *Store) RotateMCPGrant(ctx context.Context, orgID, id int64, spent, access, accessExp, refresh, refreshExp string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `update mcp_grants set access_hash=?, access_expires_at=?, refresh_hash=?,
		refresh_expires_at=?, prev_refresh_hash=? where org_id=? and id=? and refresh_hash=? and revoked_at=''`,
		access, accessExp, refresh, refreshExp, spent, orgID, id, spent)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RevokeMCPGrant ends a grant. Its tokens stop working with the next request that presents them.
func (s *Store) RevokeMCPGrant(ctx context.Context, orgID, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `update mcp_grants set revoked_at=? where org_id=? and id=? and revoked_at=''`,
		now(), orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MCPGrants lists one organisation's connected apps, newest first, ended ones included, for the
// same reason revoked keys stay on the keys page: who had access last week is part of the story.
func (s *Store) MCPGrants(ctx context.Context, orgID int64) ([]MCPGrant, error) {
	rows, err := s.db.QueryContext(ctx, `select g.id, g.org_id, g.user_id, coalesce(u.email,''), g.client_id, g.client_name,
		g.redirect_uri, g.scope, g.resource, g.created_at, g.last_used_at, g.refresh_expires_at, g.revoked_at
		from mcp_grants g left join users u on u.id = g.user_id where g.org_id=? order by g.id desc limit 200`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPGrant{}
	for rows.Next() {
		var g MCPGrant
		if err := rows.Scan(&g.ID, &g.OrgID, &g.UserID, &g.Owner, &g.ClientID, &g.ClientName, &g.RedirectURI,
			&g.Scope, &g.Resource, &g.CreatedAt, &g.LastUsedAt, &g.ExpiresAt, &g.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// TouchMCPGrant stamps when a grant was last used, at most once a minute, like TouchAPIKey.
func (s *Store) TouchMCPGrant(ctx context.Context, orgID, id int64, last string) {
	if last != "" && last > time.Now().Add(-time.Minute).UTC().Format(time.DateTime) {
		return
	}
	s.db.ExecContext(ctx, `update mcp_grants set last_used_at=? where org_id=? and id=?`, now(), orgID, id)
}
