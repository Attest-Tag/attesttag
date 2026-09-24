package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// One organisation's identity provider, and the domain it claims.
//
// Two facts live here and they are not the same fact. The row says which IdP an organisation
// has pointed at; domain_verified says whether the claim on the email domain has been proved by
// DNS. Only the second one lets anybody in — see ssoProviderByDomain, which will not look at an
// unproved row. Registering is a configuration change; verifying is the security boundary.
type SSOProvider struct {
	ID         int64  `json:"-"`
	OrgID      int64  `json:"-"`
	ProviderID string `json:"provider_id"`
	Kind       string `json:"kind"`
	Domain     string `json:"domain"`
	Issuer     string `json:"issuer"`
	ClientID   string `json:"client_id"`
	// The value of the TXT record the organisation has to publish. Not a secret — it proves
	// control of the domain to us, and it is worthless to anybody who cannot write that zone.
	DomainToken    string `json:"-"`
	DomainVerified bool   `json:"domain_verified"`
	CreatedBy      int64  `json:"-"`
	CreatedAt      string `json:"created_at"`
	VerifiedAt     string `json:"verified_at,omitempty"`
}

const ssoCols = `id, org_id, provider_id, kind, domain, issuer, client_id, domain_token,
	domain_verified, created_by, created_at, coalesce(verified_at, '')`

func scanSSO(row interface{ Scan(...any) error }) (*SSOProvider, error) {
	var p SSOProvider
	err := row.Scan(&p.ID, &p.OrgID, &p.ProviderID, &p.Kind, &p.Domain, &p.Issuer, &p.ClientID,
		&p.DomainToken, &p.DomainVerified, &p.CreatedBy, &p.CreatedAt, &p.VerifiedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SSOProviderForOrg is the console's read: what this organisation has registered, proved or not.
func (s *Store) SSOProviderForOrg(ctx context.Context, orgID int64) (*SSOProvider, error) {
	p, err := scanSSO(s.db.QueryRowContext(ctx, `select `+ssoCols+` from sso_providers where org_id=?`, orgID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// SSOProviderByDomain routes a sign-in: an address at this domain belongs to this organisation.
//
// The predicate that matters is domain_verified. A row without it has claimed a domain and
// proved nothing, and on a deployment where anybody may sign up that is a stranger's claim on
// somebody else's staff — so it is not merely ranked lower here, it is invisible.
func (s *Store) SSOProviderByDomain(ctx context.Context, domain string) (*SSOProvider, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, nil
	}
	p, err := scanSSO(s.db.QueryRowContext(ctx,
		`select `+ssoCols+` from sso_providers where domain=? and domain_verified=1`, domain))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// SSOProviderByID finds the provider a callback URL names. Unverified rows are returned, so the
// callback can say "this provider is not switched on yet" rather than "no such provider"; the
// sign-in itself is refused a step later.
func (s *Store) SSOProviderByID(ctx context.Context, providerID string) (*SSOProvider, error) {
	p, err := scanSSO(s.db.QueryRowContext(ctx, `select `+ssoCols+` from sso_providers where provider_id=?`, providerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// CreateSSOProvider registers one, sealing the client secret on the way in. It arrives
// unverified: nothing about writing this row lets anybody sign in.
func (s *Store) CreateSSOProvider(ctx context.Context, sealer *Sealer, p *SSOProvider, clientSecret string) error {
	if sealer == nil {
		return errors.New("no sealer: a client secret cannot be stored in the clear")
	}
	enc, err := sealer.Seal([]byte(clientSecret))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`insert into sso_providers (org_id, provider_id, kind, domain, issuer, client_id, client_secret_enc, domain_token, created_by)
		 values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.OrgID, p.ProviderID, p.Kind, p.Domain, p.Issuer, p.ClientID, enc, p.DomainToken, p.CreatedBy)
	if err != nil && isUniqueViolation(err) {
		// Which index tripped is worth telling apart: one is "you already have one" and the
		// other is "somebody else proved that domain first", and the fixes are different.
		if other, _ := s.SSOProviderByDomain(ctx, p.Domain); other != nil {
			return errors.New("another organisation has already proved it controls that domain")
		}
		return errors.New("this organisation already has an identity provider; remove it first")
	}
	return err
}

// SSOClientSecret unseals the secret for a token exchange. It is never returned to a browser —
// the only caller is the callback, on the server side of the flow.
func (s *Store) SSOClientSecret(ctx context.Context, sealer *Sealer, id int64) (string, error) {
	if sealer == nil {
		return "", errors.New("no sealer: the client secret cannot be read")
	}
	var enc []byte
	if err := s.db.QueryRowContext(ctx, `select client_secret_enc from sso_providers where id=?`, id).Scan(&enc); err != nil {
		return "", err
	}
	raw, err := sealer.Open(enc)
	if err != nil {
		return "", errors.New("the stored client secret cannot be read after a key rotation; register the provider again")
	}
	return string(raw), nil
}

// MarkSSODomainVerified flips the one flag that switches sign-in on.
//
// The unique index over verified rows is what makes the race safe: two organisations that both
// publish a TXT record for the same domain cannot both land here, and the loser is told so
// rather than quietly sharing a domain with the winner.
func (s *Store) MarkSSODomainVerified(ctx context.Context, orgID int64) error {
	_, err := s.db.ExecContext(ctx,
		`update sso_providers set domain_verified=1, verified_at=? where org_id=?`, now(), orgID)
	if err != nil && isUniqueViolation(err) {
		return errors.New("another organisation has already proved it controls that domain")
	}
	return err
}

func (s *Store) DeleteSSOProvider(ctx context.Context, orgID int64) error {
	_, err := s.db.ExecContext(ctx, `delete from sso_providers where org_id=?`, orgID)
	return err
}

// isUniqueViolation reads a constraint failure out of either driver. SQLite says "UNIQUE
// constraint failed", pgx says "SQLSTATE 23505"; neither is worth a driver-specific type
// assertion for the two call sites above.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToUpper(err.Error())
	return strings.Contains(s, "UNIQUE") || strings.Contains(s, "23505") || strings.Contains(s, "DUPLICATE")
}
