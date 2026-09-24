package app

import (
	"context"
	"errors"
	"time"
)

// The half-finished MCP sign-in, in the database rather than in one process's memory.
//
// It lived in a map, which was correct while one process both started the flow and received the
// callback. With two instances it is a coin toss: the provider redirects the browser to
// /api/oauth/callback — registered outside requireAdmin, because a provider sends no session
// cookie — and whichever container answers that request is the one that has to know which
// organisation, which connection and which PKCE verifier the state token stands for. Half the
// time that is the other one, and the person is told "unknown or expired state; start the
// sign-in again", forever, with nothing in the logs to explain it.
//
// The verifier is sealed. It is the secret half of the PKCE pair: anybody holding it and the
// authorisation code can complete somebody else's sign-in, so it is stored the way every other
// credential here is rather than in the clear. connect_states, which solves the same problem for
// personal connections, is the shape this follows.

const oauthPendingTTL = 15 * time.Minute

// oauthPending is the half-finished sign-in, held between the redirect out and the callback
// back. It carries the organisation because the callback cannot: the provider redirects the
// browser to /api/oauth/callback, which is registered outside requireAdmin (the provider will
// not send a session cookie or a CSRF token), so orgOf(r) there is 0 and no connection would
// ever be found. The state token is unguessable and single-use, and it is what ties the callback
// to the organisation that started the flow.
type oauthPending struct {
	userID   int64
	orgID    int64
	connID   int64
	verifier string
	created  time.Time
}

// PutOAuthPending records a flow that has just been sent out to the provider, and sweeps the
// ones nobody came back from. The sweep is here rather than on a timer because this is the only
// moment the table is known to be in use, and an abandoned row is worth nothing to anybody.
func (s *Store) PutOAuthPending(ctx context.Context, sealer *Sealer, state string, p oauthPending) error {
	if sealer == nil {
		return errors.New("no sealer: the PKCE verifier cannot be stored in the clear")
	}
	enc, err := sealer.Seal([]byte(p.verifier))
	if err != nil {
		return err
	}
	s.db.ExecContext(ctx, `delete from oauth_pendings where expires_at < ?`, now())
	_, err = s.db.ExecContext(ctx,
		`insert into oauth_pendings (state, org_id, user_id, conn_id, verifier_enc, expires_at) values (?, ?, ?, ?, ?, ?)`,
		state, p.orgID, p.userID, p.connID, enc, time.Now().UTC().Add(oauthPendingTTL).Format(time.DateTime))
	return err
}

// TakeOAuthPending redeems a state token exactly once, whichever instance holds the row.
//
// One statement, because "read it then delete it" is two callers completing the same sign-in
// twice — and the second would exchange an authorisation code that has already been spent,
// which reads as a provider error rather than as what it is.
func (s *Store) TakeOAuthPending(ctx context.Context, sealer *Sealer, state string) (oauthPending, error) {
	var p oauthPending
	var enc []byte
	var expires string
	err := s.db.QueryRowContext(ctx,
		`delete from oauth_pendings where state=? returning org_id, user_id, conn_id, verifier_enc, expires_at`,
		state).Scan(&p.orgID, &p.userID, &p.connID, &enc, &expires)
	if err != nil {
		return p, errors.New("unknown or expired state; start the sign-in again")
	}
	if expires < now() {
		return p, errors.New("that sign-in took too long; start it again")
	}
	raw, err := sealer.Open(enc)
	if err != nil {
		// The master key has rotated under a flow in progress. Saying so beats "invalid grant"
		// from the provider three steps later.
		return p, errors.New("that sign-in cannot be completed after a key rotation; start it again")
	}
	p.verifier = string(raw)
	return p, nil
}
