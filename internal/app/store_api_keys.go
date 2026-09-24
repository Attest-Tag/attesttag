package app

import (
	"context"
	"database/sql"
	"time"
)

// The store side of developer API keys. A key is a credential belonging to one person acting in
// one organisation, so the row holds both: org_id says whose data it reaches, user_id says whose
// authority it carries. Only the hash is here — minting and checking live in api_keys.go.

// APIKey is one key as the console lists it. The raw key is not a field: it exists exactly once,
// in the response that created it.
type APIKey struct {
	ID int64 `json:"id"`
	// Whose data the key reaches and whose authority it carries. Both are join keys and neither
	// goes out: a listing is already scoped to the caller's own organisation, so the numbers
	// told the reader nothing they did not know and told them how many accounts exist.
	OrgID  int64  `json:"-"`
	UserID int64  `json:"-"`
	Name   string `json:"name"`
	// Prefix is the first few characters of the raw key — enough to recognise which key a log
	// line or a broken deploy is talking about, never enough to authenticate with.
	Prefix string `json:"prefix"`
	// Owner is the email of the account the key acts as, resolved for display.
	Owner      string `json:"owner"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
	ExpiresAt  string `json:"expires_at"`
	RevokedAt  string `json:"revoked_at"`
}

// keyAuth is what authentication needs and nothing else: which row, whose authority, and the
// dates that decide whether it is still good.
type keyAuth struct {
	ID        int64
	OrgID     int64
	UserID    int64
	Hash      string
	ExpiresAt string
	RevokedAt string
	LastUsed  string
}

// CreateAPIKey records a minted key. The caller has the raw one and must show it now: this side
// keeps only the hash, so nothing here can ever hand it back.
func (s *Store) CreateAPIKey(ctx context.Context, orgID, userID int64, name, prefix, hash, expiresAt, createdBy string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into api_keys (org_id, user_id, name, key_prefix, key_hash, expires_at, created_by)
		values (?, ?, ?, ?, ?, ?, ?) returning id`, orgID, userID, name, prefix, hash, nullable(expiresAt), createdBy).Scan(&id)
	return id, err
}

// APIKeys lists one organisation's keys, newest first, revoked ones included: a key that was
// turned off last week is part of the story of who had access.
func (s *Store) APIKeys(ctx context.Context, orgID int64) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `select k.id, k.org_id, k.user_id, k.name, coalesce(k.key_prefix,''),
		coalesce(u.email,''), coalesce(k.created_by,''), coalesce(k.created_at,''), coalesce(k.last_used_at,''),
		coalesce(k.expires_at,''), coalesce(k.revoked_at,'')
		from api_keys k left join users u on u.id = k.user_id
		where k.org_id=? order by k.id desc`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.OrgID, &k.UserID, &k.Name, &k.Prefix, &k.Owner, &k.CreatedBy,
			&k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// APIKeyByHash is the lookup authentication makes. It names no organisation in its predicate
// because it cannot: the key IS the authority, and finding out whose it is happens to be the
// whole point of the query. The row it returns carries the org, and everything downstream is
// scoped by that.
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (*keyAuth, error) {
	var k keyAuth
	err := s.db.QueryRowContext(ctx, `select id, org_id, user_id, key_hash, coalesce(expires_at,''),
		coalesce(revoked_at,''), coalesce(last_used_at,'') from api_keys where key_hash=?`, hash).
		Scan(&k.ID, &k.OrgID, &k.UserID, &k.Hash, &k.ExpiresAt, &k.RevokedAt, &k.LastUsed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &k, err
}

// RevokeAPIKey turns a key off for good. Revoking is not deleting: the row stays so the console
// can still say who held it and when it stopped.
func (s *Store) RevokeAPIKey(ctx context.Context, orgID, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `update api_keys set revoked_at=? where org_id=? and id=? and revoked_at is null`,
		now(), orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RevokeAPIKeysOf retires every key an account holds in one organisation. Called when somebody
// is removed from the console: taking away a person's access has to take their integrations
// with them, or "removed" is a half-truth. Authentication refuses a key whose owner has no
// membership anyway; this is the same answer written down, so the console can show it.
func (s *Store) RevokeAPIKeysOf(ctx context.Context, orgID, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `update api_keys set revoked_at=? where org_id=? and user_id=? and revoked_at is null`,
		now(), orgID, userID); err != nil {
		return err
	}
	// And the MCP clients they connected, which are keys made in a browser (mcp_oauth.go).
	_, err := s.db.ExecContext(ctx, `update mcp_grants set revoked_at=? where org_id=? and user_id=? and revoked_at=''`,
		now(), orgID, userID)
	return err
}

// RevokeAPIKeysForUser revokes every developer key the person holds, in every organisation. A
// password reset or change is the account-wide "lock everyone out" action — like
// DeleteSessionsFor — so, like it, this is keyed by the person rather than scoped to one tenant:
// a key an attacker minted while holding a stolen session must not outlive the reset that evicts
// their cookies.
func (s *Store) RevokeAPIKeysForUser(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `update api_keys set revoked_at=? where user_id=? and revoked_at is null`,
		now(), userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `update mcp_grants set revoked_at=? where user_id=? and revoked_at=''`, now(), userID)
	return err
}

// TouchAPIKey stamps when a key was last used, at most once a minute. A public API is mostly
// reads, and a write per read would put every GET behind SQLite's single writer for the sake of
// a timestamp nobody reads to the second.
func (s *Store) TouchAPIKey(ctx context.Context, id int64, last string) {
	if last != "" && last > time.Now().Add(-time.Minute).UTC().Format(time.DateTime) {
		return
	}
	s.db.ExecContext(ctx, `update api_keys set last_used_at=? where id=?`, now(), id)
}

// nullable keeps an empty string out of the database as NULL, so "no expiry" is one value
// rather than two that every query would have to test for.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
