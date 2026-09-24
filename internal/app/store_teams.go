package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// One row per connected Slack workspace. The bot token arrives from oauth.v2.access and is
// sealed with the same AES-GCM sealer that protects connection credentials; it is never
// stored in the clear and never leaves the process. key_version records which MASTER_KEY
// sealed it, so a future re-key can tell resealed rows from untouched ones.
type Team struct {
	TeamID       string `json:"team_id"`
	Platform     string `json:"platform"` // which chat platform: platformSlack, until there is another
	OrgID        int64  `json:"-"`        // the owning account; a join key, never part of a response
	Name         string `json:"name"`
	Domain       string `json:"domain"`
	Icon         string `json:"icon"`
	EnterpriseID string `json:"enterprise_id"`
	BotUserID    string `json:"bot_user_id"`
	BotID        string `json:"bot_id"`
	Scopes       string `json:"scopes"`
	EmailScope   bool   `json:"email_scope"`
	DMScope      bool   `json:"dm_scope"`
	InstalledBy  string `json:"installed_by"`
	Status       string `json:"status"` // active | revoked
	LastError    string `json:"last_error"`
	InstalledAt  string `json:"installed_at"`
	RevokedAt    string `json:"revoked_at"`

	tokenEnc   []byte
	KeyVersion int `json:"-"`
	// ServiceURL is the Bot Framework host a Teams tenant's conversations are served from; empty
	// on a Slack workspace, which has one API host for everybody.
	ServiceURL string `json:"-"`
}

const teamCols = `team_id, org_id, coalesce(name,''), coalesce(domain,''), coalesce(icon,''), coalesce(enterprise_id,''),
	coalesce(bot_user_id,''), coalesce(bot_id,''), coalesce(scopes,''), email_scope, dm_scope, coalesce(installed_by,''),
	coalesce(status,'active'), coalesce(last_error,''), coalesce(installed_at,''), coalesce(revoked_at,''),
	bot_token_enc, key_version, platform, service_url`

func scanTeam(row interface{ Scan(...any) error }) (*Team, error) {
	var t Team
	var revoked sql.NullString
	if err := row.Scan(&t.TeamID, &t.OrgID, &t.Name, &t.Domain, &t.Icon, &t.EnterpriseID,
		&t.BotUserID, &t.BotID, &t.Scopes, &t.EmailScope, &t.DMScope, &t.InstalledBy,
		&t.Status, &t.LastError, &t.InstalledAt, &revoked, &t.tokenEnc, &t.KeyVersion, &t.Platform, &t.ServiceURL); err != nil {
		return nil, err
	}
	t.RevokedAt = revoked.String
	return &t, nil
}

// SaveTeam records an install. Re-installing the same workspace overwrites the token and
// clears any revoked state, which is exactly what "reinstall to fix it" has to mean.
//
// It has to mean that only for the organisation that already holds the workspace, though.
// team_id is the primary key, so without the org in the conflict clause a second organisation
// installing the same workspace would quietly overwrite the first one's bot token and adopt
// its channels. The upsert therefore only updates a row that already belongs to this
// organisation, and a workspace someone else holds is refused rather than taken.
func (s *Store) SaveTeam(ctx context.Context, t *Team, tokenEnc []byte) error {
	if t.OrgID == 0 {
		return errors.New("refusing to save a Slack install with no organisation")
	}
	// platform is written on insert and never updated: a workspace does not change platform.
	res, err := s.db.ExecContext(ctx, `insert into teams
		(team_id, org_id, name, domain, icon, enterprise_id, bot_token_enc, key_version,
		 bot_user_id, bot_id, scopes, email_scope, dm_scope, installed_by, status, last_error, revoked_at, platform, service_url)
		values (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, 'active', '', null, ?, ?)
		on conflict(team_id) do update set
			name=excluded.name, domain=excluded.domain, icon=excluded.icon, enterprise_id=excluded.enterprise_id,
			bot_token_enc=excluded.bot_token_enc, key_version=excluded.key_version,
			bot_user_id=excluded.bot_user_id, bot_id=excluded.bot_id, scopes=excluded.scopes,
			email_scope=excluded.email_scope, dm_scope=excluded.dm_scope, installed_by=excluded.installed_by,
			status='active', last_error='', revoked_at=null
		where teams.org_id = excluded.org_id`,
		t.TeamID, t.OrgID, t.Name, t.Domain, t.Icon, t.EnterpriseID, tokenEnc,
		t.BotUserID, t.BotID, t.Scopes, t.EmailScope, t.DMScope, t.InstalledBy, nonEmpty(t.Platform, platformSlack), t.ServiceURL)
	if err != nil {
		return err
	}
	// The conflict clause skips the update when the row belongs to somebody else, and a skipped
	// upsert is not an error to SQLite. Say so, or the install screen reports success on a
	// workspace this organisation does not have.
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTeamOwnedElsewhere
	}
	return nil
}

// ErrTeamOwnedElsewhere is returned when a Slack workspace is already installed by a different
// organisation. Connecting it here would mean taking it from them.
var ErrTeamOwnedElsewhere = errors.New("that Slack workspace is already connected to another organisation")

func (s *Store) Team(ctx context.Context, teamID string) (*Team, error) {
	t, err := scanTeam(s.db.QueryRowContext(ctx, `select `+teamCols+` from teams where team_id=?`, teamID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// Teams lists every install, revoked ones included, newest first. The console shows revoked
// rows so a workspace that stopped answering has somewhere to say why.
func (s *Store) Teams(ctx context.Context, orgID int64) ([]*Team, error) {
	rows, err := s.db.QueryContext(ctx, `select `+teamCols+` from teams where org_id=? order by status, installed_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Team{}
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ActiveTeams is what every fan-out loop iterates.
// ActiveTeams is every organisation's, because its callers are the schedulers and the socket
// loop: one process serving every tenant. Each Team carries its OrgID, so what they do with one
// is scoped to it.
func (s *Store) ActiveTeams(ctx context.Context) ([]*Team, error) {
	all, err := s.allTeams(ctx)
	if err != nil {
		return nil, err
	}
	out := []*Team{}
	for _, t := range all {
		if t.Status == "active" {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *Store) allTeams(ctx context.Context) ([]*Team, error) {
	rows, err := s.db.QueryContext(ctx, `select `+teamCols+` from teams order by status, installed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Team{}
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeTeam(ctx context.Context, teamID, reason string) error {
	_, err := s.db.ExecContext(ctx, `update teams set status='revoked', last_error=?, revoked_at=? where team_id=?`,
		reason, time.Now().UTC().Format(time.DateTime), teamID)
	return err
}

// TeamProbes records what this install's granted scopes actually allow. Scopes differ per
// workspace and per reinstall, so the answer cannot be a process-wide boot-time constant.
func (s *Store) TeamProbes(ctx context.Context, teamID string, email, dm bool) error {
	_, err := s.db.ExecContext(ctx, `update teams set email_scope=?, dm_scope=? where team_id=?`, email, dm, teamID)
	return err
}

// ---- install-flow state ----
//
// A table rather than a map: Cloud Run recycles the container, and an install that started
// before a restart should still complete.

var errBadState = errors.New("this install link has expired — press Add to Slack again")

// NewOAuthState records who started an install and, just as importantly, which organisation
// they started it for. The org used to be a literal 1 here, which meant the callback had no way
// to know whose install it was finishing — every workspace ended up belonging to the first
// tenant. It is the state token, consumed once, that carries the answer across Slack's redirect.
func (s *Store) NewOAuthState(ctx context.Context, orgID int64, createdBy string, ttl time.Duration) (string, error) {
	tok := randomToken()
	_, err := s.db.ExecContext(ctx, `insert into oauth_states (state, org_id, created_by, expires_at) values (?, ?, ?, ?)`,
		tok, orgID, createdBy, time.Now().UTC().Add(ttl).Format(time.DateTime))
	return tok, err
}

// TakeOAuthState consumes a state exactly once, so a replayed callback cannot install twice.
// It returns the organisation the install was started for along with the installer.
func (s *Store) TakeOAuthState(ctx context.Context, state string) (string, int64, error) {
	if state == "" {
		return "", 0, errBadState
	}
	var createdBy, expires string
	var orgID int64
	// One statement, so two callbacks racing on the same state cannot both read it before
	// either deletes it: whichever runs first gets the row, the other gets nothing.
	err := s.db.QueryRowContext(ctx, `delete from oauth_states where state=? returning coalesce(created_by,''), coalesce(org_id,0), expires_at`, state).
		Scan(&createdBy, &orgID, &expires)
	if err != nil {
		return "", 0, errBadState
	}
	if expires < time.Now().UTC().Format(time.DateTime) {
		return "", 0, errBadState
	}
	if orgID == 0 {
		return "", 0, errBadState
	}
	return createdBy, orgID, nil
}

func (s *Store) sweepOAuthStates(ctx context.Context) {
	s.db.ExecContext(ctx, `delete from oauth_states where expires_at < ?`, time.Now().UTC().Format(time.DateTime))
}

// ChannelTeam says which connected workspace a channel id belongs to, from the scope row the
// sync wrote. Channel ids are unique to a workspace, not to Slack, so a channel shared into
// two workspaces has two rows; the first is good enough for the one caller that needs this
// (posting an operational alert) and the console names the workspace everywhere else.
func (s *Store) ChannelTeam(ctx context.Context, orgID int64, channel string) (string, error) {
	var team string
	err := s.db.QueryRowContext(ctx,
		`select team_id from scopes where org_id=? and kind='channel' and slack_id=? order by id limit 1`, orgID, channel).Scan(&team)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return team, err
}

// OrgOfTeam says which organisation a connected Slack workspace belongs to. It is how the
// message path gets from an event's team id to the organisation whose configuration answers it.
func (s *Store) OrgOfTeam(ctx context.Context, teamID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `select org_id from teams where team_id=? and status='active'`, teamID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("workspace %s is not connected", teamID)
	}
	return id, err
}
