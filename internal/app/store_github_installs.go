package app

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// One GitHub App installation belongs to exactly one attest_tag organisation, and the first to
// claim it wins. This is the whole multi-tenant guard: an installation id is not secret — it
// rides in a query string and shows in GitHub's own URLs — so without first-claim-wins, knowing
// one would be enough to attach somebody else's repositories to your account.
//
// The same shape and the same reasoning as SaveTeam (store_teams.go), deliberately: two ways of
// saying "this external account is spoken for" would be two places to get it wrong.

// ErrInstallOwnedElsewhere says the installation is already bound to a different organisation.
var ErrInstallOwnedElsewhere = errors.New("this GitHub App installation belongs to another organisation")

// GitHubInstall is one installation as the console shows it.
type GitHubInstall struct {
	ID            int64  `json:"installation_id"`
	OrgID         int64  `json:"-"`
	AccountLogin  string `json:"account_login"`
	AccountID     int64  `json:"account_id"`
	AccountType   string `json:"account_type"`
	RepoSelection string `json:"repo_selection"`
	Permissions   string `json:"permissions,omitempty"`
	AppSlug       string `json:"app_slug,omitempty"`
	InstalledBy   string `json:"installed_by,omitempty"`
	SuspendedAt   string `json:"suspended_at,omitempty"`
	Status        string `json:"status"`
	LastError     string `json:"last_error,omitempty"`
	InstalledAt   string `json:"installed_at,omitempty"`
}

const installCols = `installation_id, org_id, coalesce(account_login,''), coalesce(account_id,0), coalesce(account_type,''),
	coalesce(repo_selection,''), coalesce(permissions,''), coalesce(app_slug,''), coalesce(installed_by,''),
	coalesce(suspended_at,''), coalesce(status,'active'), coalesce(last_error,''), coalesce(installed_at,'')`

func scanInstall(row interface{ Scan(...any) error }) (*GitHubInstall, error) {
	var g GitHubInstall
	if err := row.Scan(&g.ID, &g.OrgID, &g.AccountLogin, &g.AccountID, &g.AccountType, &g.RepoSelection,
		&g.Permissions, &g.AppSlug, &g.InstalledBy, &g.SuspendedAt, &g.Status, &g.LastError, &g.InstalledAt); err != nil {
		return nil, err
	}
	return &g, nil
}

// SaveGitHubInstall records an installation against an organisation, or refreshes what GitHub
// says about one this organisation already holds. Another organisation's installation is
// refused rather than moved: reinstalling cannot take it away from whoever set it up first.
func (s *Store) SaveGitHubInstall(ctx context.Context, g *GitHubInstall) error {
	res, err := s.db.ExecContext(ctx, `insert into github_installs
		(installation_id, org_id, account_login, account_id, account_type, repo_selection, permissions, app_slug, installed_by, suspended_at, status, last_error)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', '')
		on conflict(installation_id) do update set
			account_login=excluded.account_login, account_id=excluded.account_id, account_type=excluded.account_type,
			repo_selection=excluded.repo_selection, permissions=excluded.permissions, app_slug=excluded.app_slug,
			suspended_at=excluded.suspended_at, status='active', last_error='', revoked_at=null
		where github_installs.org_id = excluded.org_id`,
		g.ID, g.OrgID, g.AccountLogin, g.AccountID, g.AccountType, g.RepoSelection, g.Permissions, g.AppSlug,
		g.InstalledBy, nullIfEmpty(g.SuspendedAt))
	if err != nil {
		return err
	}
	// The where clause above is what makes this safe, and a zero row count is how it says no.
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInstallOwnedElsewhere
	}
	return nil
}

// GitHubInstall looks one up without an organisation filter, so the caller can compare and tell
// "not ours" apart from "does not exist" — the console must answer both the same way, but the
// install flow needs to know which it is.
func (s *Store) GitHubInstall(ctx context.Context, id int64) (*GitHubInstall, error) {
	g, err := scanInstall(s.db.QueryRowContext(ctx, `select `+installCols+` from github_installs where installation_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// GitHubInstalls lists an organisation's installations, newest first.
func (s *Store) GitHubInstalls(ctx context.Context, orgID int64) ([]*GitHubInstall, error) {
	rows, err := s.db.QueryContext(ctx, `select `+installCols+` from github_installs
		where org_id=? and coalesce(status,'active')<>'revoked' order by installed_at desc`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*GitHubInstall{}
	for rows.Next() {
		g, err := scanInstall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeGitHubInstall forgets an installation here. It cannot uninstall it at GitHub — only an
// account admin can do that — so the console says so rather than implying otherwise.
func (s *Store) RevokeGitHubInstall(ctx context.Context, orgID, id int64, why string) error {
	_, err := s.db.ExecContext(ctx, `update github_installs set status='revoked', last_error=?, revoked_at=?
		where org_id=? and installation_id=?`, why, time.Now().UTC().Format(time.DateTime), orgID, id)
	return err
}

// MarkGitHubInstallError records why an installation stopped answering, so the console can say
// "suspended" or "uninstalled at GitHub" instead of leaving every repository under it looking
// individually broken. With no webhook this is how we find out: the next call tells us.
func (s *Store) MarkGitHubInstallError(ctx context.Context, orgID, id int64, status, why string) error {
	_, err := s.db.ExecContext(ctx, `update github_installs set status=?, last_error=?
		where org_id=? and installation_id=?`, status, why, orgID, id)
	return err
}

// SetGitHubInstallPermissions records what an installation has been granted, as GitHub's
// installation webhook reports it (github_webhook.go): on install, and again whenever an owner
// accepts permissions the App asked for later. The install flow writes the same column once, at
// binding; without this it would go on describing the grant as it was that day, and code review
// reads it to tell "accept the new permissions at GitHub" apart from "this installation can post".
func (s *Store) SetGitHubInstallPermissions(ctx context.Context, orgID, id int64, permissions string) error {
	_, err := s.db.ExecContext(ctx, `update github_installs set permissions=?
		where org_id=? and installation_id=?`, permissions, orgID, id)
	return err
}

// SetGitHubInstallSelection records whether the installation reaches every repository in its
// account or the ones its admin chose, as GitHub's installation webhooks report it: the console
// labels the installation with it, and an owner can switch it at GitHub at any time. Anything but
// GitHub's two values — an event that did not carry the field — leaves it as it was.
func (s *Store) SetGitHubInstallSelection(ctx context.Context, orgID, id int64, selection string) error {
	if selection != "all" && selection != "selected" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `update github_installs set repo_selection=?
		where org_id=? and installation_id=?`, selection, orgID, id)
	return err
}

// SuspendGitHubInstall records that an owner suspended the installation at GitHub, which the
// webhook says as it happens rather than the next token mint finding out (installFailure).
//
// Not MarkGitHubInstallError, because of the one status it must not overwrite: an uninstall —
// the installation webhook's "deleted", or a 404 when a token is minted — can be recorded as
// 'revoked' between a "suspend" delivery's receipt and its dispatch, and a suspension written over
// 'revoked' would hand an uninstalled installation back — reviewInstallLinked and
// installBelongsTo both read anything but 'revoked' as still this organisation's.
func (s *Store) SuspendGitHubInstall(ctx context.Context, orgID, id int64, at string) error {
	_, err := s.db.ExecContext(ctx, `update github_installs set status='suspended', last_error='suspended at GitHub',
			suspended_at=coalesce(suspended_at, ?)
		where org_id=? and installation_id=? and coalesce(status,'active')<>'revoked'`, at, orgID, id)
	return err
}

// UnsuspendGitHubInstall clears a suspension, and only a suspension: an installation recorded as
// uninstalled meanwhile stays exactly as it is.
func (s *Store) UnsuspendGitHubInstall(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `update github_installs set status='active', last_error='', suspended_at=null
		where org_id=? and installation_id=? and status='suspended'`, orgID, id)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
