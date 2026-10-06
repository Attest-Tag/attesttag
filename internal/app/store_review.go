package app

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"attesttag/internal/review"
)

// Code review's configuration: the settings tree people edit under Reviews › Settings, and the
// review types — rubrics — under Reviews › Types. migrations/*/0027_code_review.sql says what each
// column holds; this file is the statements, and the few rules the database cannot hold itself.
//
// The tree is keyed by GitHub App installation, never by an attest_tag connection. That is the
// decision about DeleteConnection (store_admin.go), which is left as it was: deleting a
// connection — even the last App connection for an installation — touches no review row, and
// nothing here points at a connections.id, so nothing is left dangling. What a review tree hangs
// from is the installation's binding to the organisation, github_installs, and the binding
// outlives any one connection. Suspended at GitHub, it still counts. Uninstalled at GitHub — a
// 404 when a token is minted (installFailure), or the installation webhook's "deleted" — it is
// recorded as revoked, and ReviewedInstallation reads that, so its reviews stop at once; nothing
// is deleted, so the tree stays in the console. A reinstall is a new installation id, which is
// added to the tree afresh. Whether a repository additionally needs an attest_tag connection of
// its own before it is reviewed is the engine's question to ask at run time, not something
// storage can settle by deleting rows.
//
// Settings are stored as people set them and resolved in Go: each row holds only the keys set at
// its own level, so "inherit" is a key's absence rather than a value that could be mistaken for
// one, and a connection's change reaches every repository that never overrode it without a
// single row being rewritten.

const (
	reviewKindConnection = "connection"
	reviewKindGroup      = "group"
	reviewKindRepo       = "repo"

	// A settings object holds a few dozen keys and some instructions. The ceiling is a backstop
	// against a body nobody meant to send, not a policy — the API validates what the keys hold.
	reviewSettingsMaxBytes = 64 << 10
	reviewGroupNameMax     = 80
)

// ReviewSetting is one node of an organisation's review settings tree.
type ReviewSetting struct {
	ID             int64           `json:"-"`
	PublicID       string          `json:"id"`
	Kind           string          `json:"kind"` // connection | group | repo
	ParentID       int64           `json:"-"`
	ParentPublicID string          `json:"parent_id,omitempty"`
	InstallationID int64           `json:"installation_id,omitempty"` // a connection's
	Name           string          `json:"name,omitempty"`            // a group's
	Repo           string          `json:"repo,omitempty"`            // a repository's, owner/name lower-cased
	Settings       json.RawMessage `json:"settings"`                  // only what is set at this level
	RemovedAt      string          `json:"removed_at,omitempty"`      // a connection whose reviews were stopped
	UpdatedBy      string          `json:"updated_by,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

var (
	// ErrReviewInstallNotLinked is an installation this organisation does not hold — never
	// installed here, bound to another organisation, or uninstalled. The console must answer all
	// three alike, for the reason ErrInstallOwnedElsewhere gives: an installation id is not secret.
	ErrReviewInstallNotLinked = errors.New("this GitHub App installation is not connected to this organisation")
	// ErrReviewSettingExists is adding a connection that is already in the tree.
	ErrReviewSettingExists = errors.New("this connection is already reviewed")
	// ErrReviewSettingNotFound is a node that is not this organisation's, or not of the kind asked for.
	ErrReviewSettingNotFound = errors.New("no such review setting")
	// ErrReviewGroupNameTaken is a second group of one name under the same connection.
	ErrReviewGroupNameTaken = errors.New("this connection already has a group of that name")
	// ErrReviewMoveAcrossConnections is moving a repository to a group or connection of another
	// installation. Which installation reaches a repository is GitHub's fact, not a setting.
	ErrReviewMoveAcrossConnections = errors.New("a repository can only move within the connection it belongs to")
	// ErrReviewSettingsInvalid is a settings body that is not one JSON object — and, under its own
	// words rather than these (invalidReviewSettings), every other reason a level's settings are
	// refused.
	ErrReviewSettingsInvalid = errors.New("review settings must be a JSON object")
	// ErrReviewSettingsStale is a write that said what it read a field as (reviewSettingsInput.Expect)
	// where that field reads otherwise now: somebody changed it since, here or above where it is
	// inherited from, and saving would put their change back without anybody seeing it.
	ErrReviewSettingsStale = errors.New("these review settings changed since they were read; nothing was saved")
	// ErrReviewName is a repository not written owner/name, or a group with no usable name.
	ErrReviewName = errors.New("invalid name")
)

const reviewSettingCols = `r.id, r.public_id, r.kind, coalesce(r.parent_id,0), coalesce(p.public_id,''), r.installation_id,
	r.name, r.repo, r.settings_json, coalesce(r.removed_at,''), r.updated_by, r.created_at, r.updated_at`

// The parent's public id comes from a self-join that names the organisation on both sides, so a
// parent_id can only ever resolve to a row of the same organisation.
const reviewSettingFrom = ` from review_settings r
	left join review_settings p on p.org_id=r.org_id and p.id=r.parent_id`

func scanReviewSetting(row interface{ Scan(...any) error }) (*ReviewSetting, error) {
	var r ReviewSetting
	var settings string
	if err := row.Scan(&r.ID, &r.PublicID, &r.Kind, &r.ParentID, &r.ParentPublicID, &r.InstallationID,
		&r.Name, &r.Repo, &settings, &r.RemovedAt, &r.UpdatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Settings = json.RawMessage(settings)
	return &r, nil
}

// reviewQuerier is what both the store's handle and a transaction offer, so the checks below run
// inside whichever one the caller is in. SQLite has a single connection: a read issued on the
// handle while a transaction holds it would wait on itself forever.
type reviewQuerier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// ReviewSettingsTree is every node of an organisation's tree, removed connections included,
// connections first, then groups by name, then repositories by name. The console builds the tree
// from parent ids; one flat read is cheaper than a query per level and the tree is small.
func (s *Store) ReviewSettingsTree(ctx context.Context, orgID int64) ([]*ReviewSetting, error) {
	rows, err := s.db.QueryContext(ctx, `select `+reviewSettingCols+reviewSettingFrom+`
		where r.org_id=?
		order by case r.kind when 'connection' then 0 when 'group' then 1 else 2 end, r.name, r.repo, r.id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReviewSetting{}
	for rows.Next() {
		r, err := scanReviewSetting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReviewSetting reads one node by id. Another organisation's node is nil, like a missing one.
func (s *Store) ReviewSetting(ctx context.Context, orgID, id int64) (*ReviewSetting, error) {
	r, err := scanReviewSetting(s.db.QueryRowContext(ctx, `select `+reviewSettingCols+reviewSettingFrom+`
		where r.org_id=? and r.id=?`, orgID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ReviewSettingByPublicID reads one node by the id the console and the API know it by.
func (s *Store) ReviewSettingByPublicID(ctx context.Context, orgID int64, publicID string) (*ReviewSetting, error) {
	r, err := scanReviewSetting(s.db.QueryRowContext(ctx, `select `+reviewSettingCols+reviewSettingFrom+`
		where r.org_id=? and r.public_id=?`, orgID, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// reviewInstallLinked says whether this organisation holds the installation: bound to it in
// github_installs and not revoked. Suspended at GitHub still counts — GitHub sends a suspended
// installation nothing anyway, and unsuspending it should bring its reviews back, not mean adding
// it to the tree again. Uninstalled at GitHub is recorded as revoked (a 404 on mint, or the
// installation.deleted webhook) and does not count; a reinstall arrives under a new installation
// id, added to the tree afresh.
func reviewInstallLinked(ctx context.Context, q reviewQuerier, orgID, installationID int64) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `select 1 from github_installs
		where org_id=? and installation_id=? and coalesce(status,'active')<>'revoked'`, orgID, installationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AddReviewConnection puts one of the organisation's GitHub App installations into the review
// tree, with settings as its starting point (built-in defaults, or a copy of another
// connection's — the caller's choice). An installation whose reviews were stopped earlier is
// restored instead, with the settings and the tree under it as they were, and restored says so:
// the caller decides whether the starting point it was given should replace them.
func (s *Store) AddReviewConnection(ctx context.Context, orgID, installationID int64, settings json.RawMessage, by string) (r *ReviewSetting, restored bool, err error) {
	body, err := reviewSettingsJSON(settings)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if ok, err := reviewInstallLinked(ctx, tx, orgID, installationID); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, ErrReviewInstallNotLinked
	}
	at := now()
	var id int64
	var removed string
	err = tx.QueryRowContext(ctx, `select id, coalesce(removed_at,'') from review_settings
		where org_id=? and kind='connection' and installation_id=?`, orgID, installationID).Scan(&id, &removed)
	switch {
	case err == nil && removed == "":
		return nil, false, ErrReviewSettingExists
	case err == nil:
		if _, err := tx.ExecContext(ctx, `update review_settings set removed_at=null, updated_by=?, updated_at=?
			where org_id=? and id=?`, by, at, orgID, id); err != nil {
			return nil, false, err
		}
		restored = true
	case errors.Is(err, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `insert into review_settings
			(org_id, public_id, kind, installation_id, settings_json, updated_by, created_at, updated_at)
			values (?, ?, 'connection', ?, ?, ?, ?, ?) returning id`,
			orgID, newPublicID(), installationID, body, by, at, at).Scan(&id)
		if isUniqueViolation(err) { // somebody added it in the moment since the read above
			return nil, false, ErrReviewSettingExists
		}
		if err != nil {
			return nil, false, err
		}
	default:
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	r, err = s.ReviewSetting(ctx, orgID, id)
	return r, restored, err
}

// RemoveReviewConnection stops reviews on a connection. Nothing is deleted: its settings, groups
// and repositories stay, so adding it again is a restore. The first removal's time is kept.
func (s *Store) RemoveReviewConnection(ctx context.Context, orgID, id int64, by string) error {
	at := now()
	res, err := s.db.ExecContext(ctx, `update review_settings set removed_at=coalesce(removed_at, ?), updated_by=?, updated_at=?
		where org_id=? and id=? and kind='connection'`, at, by, at, orgID, id)
	return reviewRowWritten(res, err)
}

// RestoreReviewConnection starts reviews again on a connection that was stopped — provided the
// organisation still holds its installation, which is checked here as it is when one is added.
func (s *Store) RestoreReviewConnection(ctx context.Context, orgID, id int64, by string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var installationID int64
	err = tx.QueryRowContext(ctx, `select installation_id from review_settings
		where org_id=? and id=? and kind='connection'`, orgID, id).Scan(&installationID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewSettingNotFound
	}
	if err != nil {
		return err
	}
	if ok, err := reviewInstallLinked(ctx, tx, orgID, installationID); err != nil {
		return err
	} else if !ok {
		return ErrReviewInstallNotLinked
	}
	if _, err := tx.ExecContext(ctx, `update review_settings set removed_at=null, updated_by=?, updated_at=?
		where org_id=? and id=?`, by, now(), orgID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReviewedInstallation says whether this installation is in the organisation's review tree, not
// stopped, and still the organisation's. The webhook's receipt filter asks one step more, of the
// repository (ReviewModeAt), since a repository switched off under a reviewed installation is not
// to be written down either.
func (s *Store) ReviewedInstallation(ctx context.Context, orgID, installationID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `select 1 from review_settings r
		join github_installs g on g.org_id=r.org_id and g.installation_id=r.installation_id
		where r.org_id=? and r.kind='connection' and r.installation_id=? and r.removed_at is null
		  and coalesce(g.status,'active')<>'revoked'`, orgID, installationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AddReviewGroup makes a group under one of the organisation's connections.
func (s *Store) AddReviewGroup(ctx context.Context, orgID, connectionID int64, name, by string) (*ReviewSetting, error) {
	name, err := reviewGroupName(name)
	if err != nil {
		return nil, err
	}
	at := now()
	var id int64
	// The parent is checked in the statement that relies on it: one statement cannot be told the
	// id is this organisation's connection and then insert under somebody else's.
	err = s.db.QueryRowContext(ctx, `insert into review_settings
		(org_id, public_id, kind, parent_id, name, updated_by, created_at, updated_at)
		select ?, ?, 'group', ?, ?, ?, ?, ?
		where exists (select 1 from review_settings where org_id=? and id=? and kind='connection')
		returning id`,
		orgID, newPublicID(), connectionID, name, by, at, at, orgID, connectionID).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrReviewSettingNotFound
	case isUniqueViolation(err):
		return nil, ErrReviewGroupNameTaken
	case err != nil:
		return nil, err
	}
	return s.ReviewSetting(ctx, orgID, id)
}

// RenameReviewGroup renames a group; the name stays unique within its connection.
func (s *Store) RenameReviewGroup(ctx context.Context, orgID, groupID int64, name, by string) error {
	name, err := reviewGroupName(name)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `update review_settings set name=?, updated_by=?, updated_at=?
		where org_id=? and id=? and kind='group'`, name, by, now(), orgID, groupID)
	if isUniqueViolation(err) {
		return ErrReviewGroupNameTaken
	}
	return reviewRowWritten(res, err)
}

// DeleteReviewGroup removes a group and moves its repositories up to the group's connection,
// where they inherit straight from it. Their own settings go with them; only the group's go.
func (s *Store) DeleteReviewGroup(ctx context.Context, orgID, groupID int64, by string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var connectionID int64
	err = tx.QueryRowContext(ctx, `select coalesce(parent_id,0) from review_settings
		where org_id=? and id=? and kind='group'`, orgID, groupID).Scan(&connectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewSettingNotFound
	}
	if err != nil {
		return err
	}
	at := now()
	if _, err := tx.ExecContext(ctx, `update review_settings set parent_id=?, updated_by=?, updated_at=?
		where org_id=? and parent_id=? and kind='repo'`, connectionID, by, at, orgID, groupID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from review_settings where org_id=? and id=? and kind='group'`, orgID, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureReviewRepo returns a repository's own row, making it under parentID — a connection or a
// group — the first time one is needed: when something is set on the repository, or it is put in
// a group. A repository that already has a row is returned where it is; moving it is
// MoveReviewRepo's job, so that a settings save can never quietly move a repository as well.
//
// Whether the repository really is reached through that connection's installation is GitHub's to
// say and the caller's to have asked; storage holds one row per repository per organisation.
func (s *Store) EnsureReviewRepo(ctx context.Context, orgID, parentID int64, repo, by string) (*ReviewSetting, error) {
	repo, err := reviewRepoName(repo)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `select id from review_settings where org_id=? and kind='repo' and repo=?`, orgID, repo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		kind, _, perr := reviewNode(ctx, tx, orgID, parentID)
		if perr != nil {
			return nil, perr
		}
		if kind == reviewKindRepo {
			return nil, ErrReviewSettingNotFound // a repository is never a parent
		}
		at := now()
		err = tx.QueryRowContext(ctx, `insert into review_settings
			(org_id, public_id, kind, parent_id, repo, updated_by, created_at, updated_at)
			values (?, ?, 'repo', ?, ?, ?, ?, ?) on conflict do nothing returning id`,
			orgID, newPublicID(), parentID, repo, by, at, at).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			// Made by a concurrent request since the read above. On Postgres this statement sees
			// what that one committed, so the row it made is the answer here too.
			err = tx.QueryRowContext(ctx, `select id from review_settings where org_id=? and kind='repo' and repo=?`, orgID, repo).Scan(&id)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ReviewSetting(ctx, orgID, id)
}

// MoveReviewRepo puts a repository in another group of its connection, or back directly under
// the connection. Moving it to another connection is refused: the installation that reaches a
// repository is decided at GitHub, and a tree that said otherwise would resolve settings for a
// repository from a connection its events never come through.
func (s *Store) MoveReviewRepo(ctx context.Context, orgID, repoID, parentID int64, by string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	err = tx.QueryRowContext(ctx, `select coalesce(parent_id,0) from review_settings
		where org_id=? and id=? and kind='repo'`, orgID, repoID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewSettingNotFound
	}
	if err != nil {
		return err
	}
	_, from, err := reviewNode(ctx, tx, orgID, current)
	if err != nil {
		return err
	}
	kind, to, err := reviewNode(ctx, tx, orgID, parentID)
	if err != nil {
		return err
	}
	if kind == reviewKindRepo {
		return ErrReviewSettingNotFound
	}
	if from != to {
		return ErrReviewMoveAcrossConnections
	}
	if _, err := tx.ExecContext(ctx, `update review_settings set parent_id=?, updated_by=?, updated_at=?
		where org_id=? and id=? and kind='repo'`, parentID, by, now(), orgID, repoID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteReviewRepo removes a repository's own row: what it set, and the group it was put in. It goes
// back to inheriting straight from its connection, as a repository nobody ever touched does — rows
// are made lazily (EnsureReviewRepo), so having none is the ordinary state, not a missing one.
//
// The connection is marked updated in the same transaction, by whoever removed the row. The row
// going can switch the repository back on — its own "off", or its group's, may be what it held —
// and the catch-up reviews a missed pull request only if it was opened after the newest change on
// the repository's chain (reviewChainSince). With the row gone that chain is the connection alone,
// and without this its last change could be months old: every pull request opened while the
// repository was off would then read as one the deployment missed, and be reviewed unasked.
func (s *Store) DeleteReviewRepo(ctx context.Context, orgID, id int64, by string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var parent int64
	err = tx.QueryRowContext(ctx, `select coalesce(parent_id,0) from review_settings
		where org_id=? and id=? and kind='repo'`, orgID, id).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewSettingNotFound
	}
	if err != nil {
		return err
	}
	_, conn, err := reviewNode(ctx, tx, orgID, parent)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from review_settings where org_id=? and id=? and kind='repo'`, orgID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update review_settings set updated_by=?, updated_at=?
		where org_id=? and id=? and kind='connection'`, by, now(), orgID, conn); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveReviewRepo takes one repository out of code review: nothing on it is reviewed, its pull
// requests' deliveries are not even kept (ReviewModeAt), and it leaves the console's tree for its
// connection's "Removed" list. Its row is made first if it has none (EnsureReviewRepo), and is kept
// with its settings and group, so restoring it puts it back as it was. The organisation's saved
// connection for it is not touched: the bot's GitHub tools and fix jobs go on reaching it, which is
// what tells removing it from reviews apart from disconnecting it. The first removal's time is kept.
func (s *Store) RemoveReviewRepo(ctx context.Context, orgID, id int64, by string) error {
	at := now()
	res, err := s.db.ExecContext(ctx, `update review_settings set removed_at=coalesce(removed_at, ?), updated_by=?, updated_at=?
		where org_id=? and id=? and kind='repo'`, at, by, at, orgID, id)
	return reviewRowWritten(res, err)
}

// RestoreReviewRepo puts a repository removed from code review back in it. The row is marked updated,
// which is what keeps the catch-up from reviewing the pull requests opened while it was out
// (reviewChainSince): restoring a repository is never a reason to review what is already open on it.
func (s *Store) RestoreReviewRepo(ctx context.Context, orgID, id int64, by string) error {
	res, err := s.db.ExecContext(ctx, `update review_settings set removed_at=null, updated_by=?, updated_at=?
		where org_id=? and id=? and kind='repo'`, by, now(), orgID, id)
	return reviewRowWritten(res, err)
}

// reviewRepoRemoved reports whether a chain (ReviewSettingsChain) ends in a repository taken out of
// code review, which reviews nothing whatever its levels say.
func reviewRepoRemoved(chain []*ReviewSetting) bool {
	n := len(chain)
	return n > 0 && chain[n-1].Kind == reviewKindRepo && chain[n-1].RemovedAt != ""
}

// ReviewChannels is every channel a review may be announced in: those of the organisation's own
// connected workspaces, active ones, that the bot is in, as (team, channel) pairs — the pair the
// chat layer posts with. A Slack Connect channel shared into two of them is listed under each.
func (s *Store) ReviewChannels(ctx context.Context, orgID int64) ([]review.NotifyChannel, error) {
	rows, err := s.db.QueryContext(ctx, `select c.team_id, c.slack_id from scopes c
		join teams t on t.org_id=c.org_id and t.team_id=c.team_id
		where c.org_id=? and c.kind='channel' and c.left_at='' and t.status='active'
		order by c.team_id, c.slack_id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []review.NotifyChannel{}
	for rows.Next() {
		var n review.NotifyChannel
		if err := rows.Scan(&n.Team, &n.Channel); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpdateReviewSettings replaces what one node sets, whole: the object given is everything set at
// this level from now on, and a key it leaves out goes back to inheriting. Whether each key and
// value is allowed — and whether this person may set it — is the API's to check before calling.
func (s *Store) UpdateReviewSettings(ctx context.Context, orgID, id int64, settings json.RawMessage, by string) error {
	body, err := reviewSettingsJSON(settings)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `update review_settings set settings_json=?, updated_by=?, updated_at=?
		where org_id=? and id=?`, body, by, now(), orgID, id)
	return reviewRowWritten(res, err)
}

// UpdateReviewSettingsFrom is UpdateReviewSettings made only over was, the settings the caller read
// the node as, exactly as they were stored. The console's save works out what it writes from that
// read and judges it against that read — which fields change, and whether they need
// connections.manage — so a save of any field landing in between would be put back whole, by
// somebody the check never judged for it. That is ErrReviewSettingsStale instead, and nothing is
// written. One statement, so no save can land between the comparison and the write.
func (s *Store) UpdateReviewSettingsFrom(ctx context.Context, orgID, id int64, was string, settings json.RawMessage, by string) error {
	body, err := reviewSettingsJSON(settings)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `update review_settings set settings_json=?, updated_by=?, updated_at=?
		where org_id=? and id=? and settings_json=?`, body, by, now(), orgID, id, was)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	var one int
	err = s.db.QueryRowContext(ctx, `select 1 from review_settings where org_id=? and id=?`, orgID, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewSettingNotFound
	}
	if err != nil {
		return err
	}
	return ErrReviewSettingsStale
}

// ReviewSettingsChain is what a repository's effective settings are resolved from, nearest last:
// its connection, the group it sits in if any, and its own row if it has one. Nil when the
// installation is not in the tree or its reviews were stopped — there is nothing to resolve.
//
// A repository row found under a different connection is left out, group and all. That is a
// repository transferred between two accounts this organisation has both installed: its settings
// were made for the installation it used to come through, and do not follow it to another.
func (s *Store) ReviewSettingsChain(ctx context.Context, orgID, installationID int64, repo string) ([]*ReviewSetting, error) {
	conn, err := scanReviewSetting(s.db.QueryRowContext(ctx, `select `+reviewSettingCols+reviewSettingFrom+`
		where r.org_id=? and r.kind='connection' and r.installation_id=? and r.removed_at is null`, orgID, installationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	chain := []*ReviewSetting{conn}
	own, err := scanReviewSetting(s.db.QueryRowContext(ctx, `select `+reviewSettingCols+reviewSettingFrom+`
		where r.org_id=? and r.kind='repo' and r.repo=?`, orgID, strings.ToLower(strings.TrimSpace(repo))))
	if errors.Is(err, sql.ErrNoRows) {
		return chain, nil
	}
	if err != nil {
		return nil, err
	}
	if own.ParentID == conn.ID {
		return append(chain, own), nil
	}
	group, err := s.ReviewSetting(ctx, orgID, own.ParentID)
	if err != nil {
		return nil, err
	}
	if group != nil && group.Kind == reviewKindGroup && group.ParentID == conn.ID {
		return append(chain, group, own), nil
	}
	return chain, nil
}

// ReviewModeAt is the webhook's receipt filter: the effective mode of one repository under one
// installation, resolved down its connection, group and own row. "" when the installation is not
// in the organisation's tree or its reviews were stopped — nothing is reviewed there at all. Only
// a mode other than "" and off lets a pull-request delivery be stored, so another customer's pull
// requests, this customer's on an installation nobody asked to have reviewed, and a repository
// somebody switched off are never written down.
//
// A node whose settings do not read as settings is an error, not a node that sets nothing: the API
// validates what it saves, so this is a bug, and reading past it could resolve to "on" for a
// repository whose own row said "off".
func (s *Store) ReviewModeAt(ctx context.Context, orgID, installationID int64, repo string) (review.Mode, error) {
	chain, err := s.ReviewSettingsChain(ctx, orgID, installationID, repo)
	if err != nil || chain == nil {
		return "", err
	}
	if reviewRepoRemoved(chain) {
		return review.ModeOff, nil // taken out of code review: as off as a mode can say
	}
	levels, err := reviewLevels(chain)
	if err != nil {
		return "", err
	}
	return review.Resolve(levels).Mode, nil
}

// reviewLevels reads a chain, broadest first, as review.Resolve folds it. A node's kind is its
// level: the tree's three kinds are the review package's three levels by name.
func reviewLevels(chain []*ReviewSetting) ([]review.LevelSettings, error) {
	out := make([]review.LevelSettings, 0, len(chain))
	for _, n := range chain {
		var set review.Settings
		if err := json.Unmarshal(n.Settings, &set); err != nil {
			return nil, fmt.Errorf("review settings %s (%s): %w", n.PublicID, n.Kind, err)
		}
		out = append(out, review.LevelSettings{Level: review.Level(n.Kind), Settings: set})
	}
	return out, nil
}

// reviewNode reads a node's kind and the connection it belongs to: itself for a connection, its
// parent for a group, and 0 for a repository, which is never asked to be anybody's parent.
func reviewNode(ctx context.Context, q reviewQuerier, orgID, id int64) (kind string, connectionID int64, err error) {
	var parent int64
	err = q.QueryRowContext(ctx, `select kind, coalesce(parent_id,0) from review_settings where org_id=? and id=?`, orgID, id).
		Scan(&kind, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrReviewSettingNotFound
	}
	if err != nil {
		return "", 0, err
	}
	switch kind {
	case reviewKindConnection:
		return kind, id, nil
	case reviewKindGroup:
		return kind, parent, nil
	}
	return kind, 0, nil
}

// reviewRowWritten turns an update that matched nothing into ErrReviewSettingNotFound: every
// write here names the organisation, so another tenant's id and a missing one look the same.
func reviewRowWritten(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrReviewSettingNotFound
	}
	return nil
}

// reviewSettingsJSON is the stored form of a settings body: one JSON object, compacted. Nothing
// is "{}"; an array, a string or a null is refused, because every resolution under this node would
// otherwise fail to read it, and the failure would surface far from the save that caused it.
func reviewSettingsJSON(raw json.RawMessage) (string, error) {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "{}", nil
	}
	if len(t) > reviewSettingsMaxBytes || !strings.HasPrefix(t, "{") {
		return "", ErrReviewSettingsInvalid
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(t)); err != nil {
		return "", ErrReviewSettingsInvalid
	}
	return buf.String(), nil
}

// reviewRepoName is the key a repository is stored under: owner/name, lower-cased, because GitHub
// treats names without regard to case and so does every comparison of connections.repo.
func reviewRepoName(repo string) (string, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(name, "/ ") || strings.Contains(owner, " ") {
		return "", fmt.Errorf("%w: a repository is written owner/name", ErrReviewName)
	}
	return repo, nil
}

func reviewGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > reviewGroupNameMax {
		return "", fmt.Errorf("%w: a group needs a name of at most %d characters", ErrReviewName, reviewGroupNameMax)
	}
	return name, nil
}

// ---- review types ----

var (
	ErrReviewTypeNotFound = errors.New("no such review type")
	ErrReviewTypeKeyTaken = errors.New("a review type with that key already exists")
	// ErrReviewTypeStale is a save made against a version somebody else has since replaced.
	ErrReviewTypeStale = errors.New("this review type was changed while it was being edited; reload it and try again")
	// ErrReviewTypeInvalid wraps whatever made a type or one of its rules unsaveable.
	ErrReviewTypeInvalid = errors.New("invalid review type")
)

// ReviewType is a rubric and its rules. Key and BuiltinKey are fixed when the row is made: a run
// records the key it ran, and a branch rule names it, so changing it would orphan both.
//
// For the same reason a delete only hides the row (deleted_at, DeleteReviewType). A type nobody
// wants for now is turned off (Enabled false); one nobody wants again is deleted, and either way it
// stays, with its history, so every run that names {key, version} can still be shown the rubric it
// ran with — and the key stays taken: a new type made under it would start again at version 1 and
// make those names mean two different things.
type ReviewType struct {
	ID                int64            `json:"-"`
	PublicID          string           `json:"id"`
	Key               string           `json:"key"`
	BuiltinKey        string           `json:"builtin_key,omitempty"` // the built-in this is the organisation's copy of
	Name              string           `json:"name"`
	Purpose           string           `json:"purpose"`
	PathGlobs         []string         `json:"path_globs"`
	Strictness        string           `json:"strictness"`
	Model             string           `json:"model"`
	MaxUSD            float64          `json:"max_usd"`
	InlineMinSeverity string           `json:"inline_min_severity"`
	Enabled           bool             `json:"enabled"`
	Version           int              `json:"version"` // on a save, the version the edit was made against
	UpdatedBy         string           `json:"updated_by,omitempty"`
	CreatedAt         string           `json:"created_at,omitempty"`
	UpdatedAt         string           `json:"updated_at,omitempty"`
	Rules             []ReviewTypeRule `json:"rules"`

	// Skills are where the type's skills are; what they say is read at review time (review.SkillLink).
	Skills []review.SkillLink `json:"skills"`
}

// ReviewTypeRule is one line of a type, in the order the finder is given them.
type ReviewTypeRule struct {
	ID             int64    `json:"-"`
	PublicID       string   `json:"id,omitempty"`
	Position       int      `json:"position"`
	Text           string   `json:"text"`
	SeverityCap    string   `json:"severity_cap"`
	PathGlobs      []string `json:"path_globs"`
	ExampleBad     string   `json:"example_bad,omitempty"`
	ExampleGood    string   `json:"example_good,omitempty"`
	Enabled        bool     `json:"enabled"`
	Source         string   `json:"source"` // builtin | team | learned
	Status         string   `json:"status"` // active | proposed | rejected
	FromCommentURL string   `json:"from_comment_url,omitempty"`
	CreatedAt      string   `json:"created_at,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
}

// ReviewTypeVersion is one entry of a type's history.
type ReviewTypeVersion struct {
	Version   int    `json:"version"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

const reviewTypeCols = `id, public_id, key, coalesce(builtin_key,''), name, purpose, path_globs_json, strictness, model,
	max_usd, inline_min_severity, enabled, version, updated_by, created_at, updated_at, skills_json`

const reviewRuleCols = `id, public_id, type_id, position, text, severity_cap, path_globs_json, example_bad, example_good,
	enabled, source, status, from_comment_url, created_at, updated_at`

func scanReviewType(row interface{ Scan(...any) error }) (*ReviewType, error) {
	var t ReviewType
	var globs, skills string
	var enabled int
	if err := row.Scan(&t.ID, &t.PublicID, &t.Key, &t.BuiltinKey, &t.Name, &t.Purpose, &globs, &t.Strictness, &t.Model,
		&t.MaxUSD, &t.InlineMinSeverity, &enabled, &t.Version, &t.UpdatedBy, &t.CreatedAt, &t.UpdatedAt, &skills); err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	t.PathGlobs = decodeGlobs(globs)
	t.Skills = decodeSkillLinks(skills)
	t.Rules = []ReviewTypeRule{}
	return &t, nil
}

func scanReviewRule(row interface{ Scan(...any) error }) (int64, ReviewTypeRule, error) {
	var r ReviewTypeRule
	var typeID int64
	var globs string
	var enabled int
	err := row.Scan(&r.ID, &r.PublicID, &typeID, &r.Position, &r.Text, &r.SeverityCap, &globs, &r.ExampleBad, &r.ExampleGood,
		&enabled, &r.Source, &r.Status, &r.FromCommentURL, &r.CreatedAt, &r.UpdatedAt)
	r.Enabled = enabled == 1
	r.PathGlobs = decodeGlobs(globs)
	return typeID, r, err
}

func decodeGlobs(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func encodeGlobs(g []string) string {
	if len(g) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(g)
	return string(b)
}

func decodeSkillLinks(s string) []review.SkillLink {
	out := []review.SkillLink{}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []review.SkillLink{}
	}
	return out
}

func encodeSkillLinks(l []review.SkillLink) string {
	if len(l) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// ReviewTypes is every type the organisation has a row for — its own, and its copies of built-ins
// — with their rules, less the deleted ones. Built-ins it never edited are not here; the caller lays these over them.
func (s *Store) ReviewTypes(ctx context.Context, orgID int64) ([]*ReviewType, error) {
	rows, err := s.db.QueryContext(ctx, `select `+reviewTypeCols+` from review_types where org_id=? and deleted_at is null order by id`, orgID)
	if err != nil {
		return nil, err
	}
	out := []*ReviewType{}
	byID := map[int64]*ReviewType{}
	for rows.Next() {
		t, err := scanReviewType(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
		byID[t.ID] = t
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rules, err := s.db.QueryContext(ctx, `select `+reviewRuleCols+` from review_type_rules
		where org_id=? order by type_id, position, id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rules.Close()
	for rules.Next() {
		typeID, r, err := scanReviewRule(rules)
		if err != nil {
			return nil, err
		}
		if t := byID[typeID]; t != nil {
			t.Rules = append(t.Rules, r)
		}
	}
	return out, rules.Err()
}

// ReviewTypeByPublicID reads one type and its rules; nil when it is not this organisation's.
func (s *Store) ReviewTypeByPublicID(ctx context.Context, orgID int64, publicID string) (*ReviewType, error) {
	return reviewTypeWhere(ctx, s.db, orgID, `public_id=?`, publicID)
}

// ReviewTypeByKey reads the organisation's row for a key, its own type or its copy of a built-in;
// nil when it has none, which for a built-in key means it runs as shipped.
func (s *Store) ReviewTypeByKey(ctx context.Context, orgID int64, key string) (*ReviewType, error) {
	return reviewTypeWhere(ctx, s.db, orgID, `key=?`, key)
}

// reviewTypeWhere reads one type and its rules; a deleted type is not found. cond is one of the
// constant conditions above, never anything a request supplied.
func reviewTypeWhere(ctx context.Context, q reviewQuerier, orgID int64, cond string, arg any) (*ReviewType, error) {
	t, err := scanReviewType(q.QueryRowContext(ctx, `select `+reviewTypeCols+` from review_types where org_id=? and deleted_at is null and `+cond, orgID, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `select `+reviewRuleCols+` from review_type_rules
		where org_id=? and type_id=? order by position, id`, orgID, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		_, r, err := scanReviewRule(rows)
		if err != nil {
			return nil, err
		}
		t.Rules = append(t.Rules, r)
	}
	return t, rows.Err()
}

// CopyBuiltinReviewType makes the organisation's own copy of a built-in type, the first time
// somebody edits it — copy-on-write. builtin is the content as shipped, which lives in Go; this
// only stores it. The copy is version 1, so the history starts from what shipped and the first
// edit shows as a change to it.
//
// Copying twice returns the copy that exists, unchanged: two people opening the same built-in to
// edit it must end up editing one row. A key the organisation already uses for a type of its own
// is refused rather than adopted.
func (s *Store) CopyBuiltinReviewType(ctx context.Context, orgID int64, builtin *ReviewType, by string) (*ReviewType, error) {
	existing, err := s.ReviewTypeByKey(ctx, orgID, builtin.Key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.BuiltinKey != builtin.Key {
			return nil, ErrReviewTypeKeyTaken
		}
		return existing, nil
	}
	c := *builtin
	c.BuiltinKey = builtin.Key
	c.Rules = make([]ReviewTypeRule, len(builtin.Rules))
	for i, r := range builtin.Rules {
		r.Source, r.Status, r.FromCommentURL = "builtin", "active", ""
		c.Rules[i] = r
	}
	t, err := s.CreateReviewType(ctx, orgID, &c, by)
	if errors.Is(err, ErrReviewTypeKeyTaken) { // copied by somebody else in the same moment
		return s.ReviewTypeByKey(ctx, orgID, builtin.Key)
	}
	return t, err
}

// CreateReviewType stores a new type — the organisation's own, or, through CopyBuiltinReviewType,
// its copy of a built-in — with its rules, as version 1. Storage does not know which keys ship
// with the binary, so refusing a built-in's key for a type of the organisation's own is the
// caller's job; a key already taken here is ErrReviewTypeKeyTaken.
func (s *Store) CreateReviewType(ctx context.Context, orgID int64, t *ReviewType, by string) (*ReviewType, error) {
	if err := checkReviewType(t, t.Key); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	at := now()
	var id int64
	err = tx.QueryRowContext(ctx, `insert into review_types
		(org_id, public_id, key, builtin_key, name, purpose, path_globs_json, strictness, model, max_usd,
		 inline_min_severity, enabled, version, updated_by, created_at, updated_at, skills_json)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?) returning id`,
		orgID, newPublicID(), t.Key, nullIfEmpty(t.BuiltinKey), strings.TrimSpace(t.Name), t.Purpose, encodeGlobs(t.PathGlobs),
		t.Strictness, t.Model, t.MaxUSD, t.InlineMinSeverity, t.Enabled, by, at, at, encodeSkillLinks(t.Skills)).Scan(&id)
	if isUniqueViolation(err) {
		tx.Rollback()
		var deleted bool
		if s.db.QueryRowContext(ctx, `select 1 from review_types where org_id=? and key=? and deleted_at is not null`,
			orgID, t.Key).Scan(&deleted) == nil {
			return nil, fmt.Errorf("%w: a deleted type had the key %q, and the reviews it ran still name it; choose another key",
				ErrReviewTypeKeyTaken, t.Key)
		}
		return nil, ErrReviewTypeKeyTaken
	}
	if err != nil {
		return nil, err
	}
	if err := saveReviewTypeRules(ctx, tx, orgID, id, t.Rules, at); err != nil {
		return nil, err
	}
	saved, err := s.snapshotReviewType(ctx, tx, orgID, id, by, at)
	if err != nil {
		return nil, err
	}
	return saved, tx.Commit()
}

// DeleteReviewType deletes a type of the organisation's own, against the version the person was
// looking at, as ErrReviewTypeStale when somebody saved it since. Only the row is marked: its
// rules and versions stay, so the runs that name it keep explaining themselves and its key stays
// taken. A copy of a built-in is never deleted — the built-in would come straight back in its
// place — so it is ErrReviewTypeNotFound here like a type that does not exist.
func (s *Store) DeleteReviewType(ctx context.Context, orgID, typeID int64, version int, by string) error {
	at := now()
	res, err := s.db.ExecContext(ctx, `update review_types set deleted_at=?, updated_by=?, updated_at=?
		where org_id=? and id=? and version=? and builtin_key is null and deleted_at is null`,
		at, by, at, orgID, typeID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var one int
	err = s.db.QueryRowContext(ctx, `select 1 from review_types where org_id=? and id=? and builtin_key is null and deleted_at is null`,
		orgID, typeID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewTypeNotFound
	}
	if err != nil {
		return err
	}
	return ErrReviewTypeStale
}

// SaveReviewType replaces a type's fields and its rule list, whole, and records the result as the
// next version. t.ID names the type and t.Version is the version the edit was made against: a save
// on top of somebody else's newer one is refused with ErrReviewTypeStale rather than undoing it.
//
// Rules are matched to the stored ones by public id, so a rule that was edited keeps its identity
// — and its source, and the comment it was learned from, which say where it came from and are not
// the editor's to change. A rule without a known id is new (source 'team' unless said otherwise);
// a stored rule missing from the list is deleted. Turning a rule off is enabled=false, not leaving
// it out, and that is the console's choice to make.
//
// Resetting a copy to the built-in, and reverting to an old version, are both this: save the
// built-in's content, or ReviewTypeAtVersion's snapshot, as the newest version. The history keeps
// growing either way, so a run's {key, version} always names exactly what it ran with.
func (s *Store) SaveReviewType(ctx context.Context, orgID int64, t *ReviewType, by string) (*ReviewType, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Checked under the stored key, which a save never changes, so an edit that left the key out
	// is judged on what it does change.
	var key string
	err = tx.QueryRowContext(ctx, `select key from review_types where org_id=? and id=? and deleted_at is null`, orgID, t.ID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReviewTypeNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := checkReviewType(t, key); err != nil {
		return nil, err
	}
	at := now()
	res, err := tx.ExecContext(ctx, `update review_types set name=?, purpose=?, path_globs_json=?, strictness=?, model=?,
		max_usd=?, inline_min_severity=?, enabled=?, skills_json=?, version=version+1, updated_by=?, updated_at=?
		where org_id=? and id=? and version=? and deleted_at is null`,
		strings.TrimSpace(t.Name), t.Purpose, encodeGlobs(t.PathGlobs), t.Strictness, t.Model, t.MaxUSD, t.InlineMinSeverity,
		t.Enabled, encodeSkillLinks(t.Skills), by, at, orgID, t.ID, t.Version)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if err := tx.QueryRowContext(ctx, `select 1 from review_types where org_id=? and id=? and deleted_at is null`, orgID, t.ID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReviewTypeNotFound
		} else if err != nil {
			return nil, err
		}
		return nil, ErrReviewTypeStale
	}
	if err := saveReviewTypeRules(ctx, tx, orgID, t.ID, t.Rules, at); err != nil {
		return nil, err
	}
	saved, err := s.snapshotReviewType(ctx, tx, orgID, t.ID, by, at)
	if err != nil {
		return nil, err
	}
	return saved, tx.Commit()
}

// saveReviewTypeRules makes a type's stored rules the list given, in its order.
func saveReviewTypeRules(ctx context.Context, tx *dbTx, orgID, typeID int64, rules []ReviewTypeRule, at string) error {
	// Read whole before writing: SQLite has one connection, and a cursor left open across the
	// writes below would have them wait on it.
	rows, err := tx.QueryContext(ctx, `select public_id, id, status from review_type_rules where org_id=? and type_id=?`, orgID, typeID)
	if err != nil {
		return err
	}
	type storedRule struct {
		id     int64
		status string
	}
	stored := map[string]storedRule{}
	for rows.Next() {
		var pub string
		var r storedRule
		if err := rows.Scan(&pub, &r.id, &r.status); err != nil {
			rows.Close()
			return err
		}
		stored[pub] = r
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	kept := map[int64]bool{}
	for i, r := range rules {
		// A save that says nothing about a rule's status keeps the one it has. Defaulting to
		// active here would approve a proposed learned rule as a side effect of somebody saving
		// the type around it — through a form, an API body or an MCP call that left the field
		// out — and only an Approve may turn one on.
		if old, ok := stored[r.PublicID]; ok && r.PublicID != "" && !kept[old.id] {
			kept[old.id] = true
			if _, err := tx.ExecContext(ctx, `update review_type_rules set position=?, text=?, severity_cap=?, path_globs_json=?,
				example_bad=?, example_good=?, enabled=?, status=?, updated_at=?
				where org_id=? and id=?`,
				i, strings.TrimSpace(r.Text), r.SeverityCap, encodeGlobs(r.PathGlobs), r.ExampleBad, r.ExampleGood,
				r.Enabled, cmp.Or(r.Status, old.status), at, orgID, old.id); err != nil {
				return err
			}
			continue
		}
		// A new rule a person wrote is active; one learned from a reply is proposed until approved.
		source := cmp.Or(r.Source, "team")
		status := r.Status
		if status == "" {
			status = "active"
			if source == "learned" {
				status = "proposed"
			}
		}
		if _, err := tx.ExecContext(ctx, `insert into review_type_rules
			(org_id, public_id, type_id, position, text, severity_cap, path_globs_json, example_bad, example_good,
			 enabled, source, status, from_comment_url, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			orgID, newPublicID(), typeID, i, strings.TrimSpace(r.Text), r.SeverityCap, encodeGlobs(r.PathGlobs),
			r.ExampleBad, r.ExampleGood, r.Enabled, source, status, r.FromCommentURL, at, at); err != nil {
			return err
		}
	}
	for _, r := range stored {
		if kept[r.id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `delete from review_type_rules where org_id=? and id=?`, orgID, r.id); err != nil {
			return err
		}
	}
	return nil
}

// snapshotReviewType reads a type back as it now stands and files it as its current version.
func (s *Store) snapshotReviewType(ctx context.Context, tx *dbTx, orgID, typeID int64, by, at string) (*ReviewType, error) {
	t, err := reviewTypeWhere(ctx, tx, orgID, `id=?`, typeID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrReviewTypeNotFound
	}
	snap, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `insert into review_type_versions (org_id, type_id, version, snapshot_json, created_by, created_at)
		values (?, ?, ?, ?, ?, ?)`, orgID, typeID, t.Version, string(snap), by, at); err != nil {
		return nil, err
	}
	return t, nil
}

// ReviewTypeVersions is a type's history, newest first.
func (s *Store) ReviewTypeVersions(ctx context.Context, orgID, typeID int64) ([]ReviewTypeVersion, error) {
	rows, err := s.db.QueryContext(ctx, `select version, created_by, created_at from review_type_versions
		where org_id=? and type_id=? order by version desc`, orgID, typeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewTypeVersion{}
	for rows.Next() {
		var v ReviewTypeVersion
		if err := rows.Scan(&v.Version, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ReviewRuleFirstVersion is the first version of a type whose rules hold one learned from the comment
// fromURL: who saved that version, and the rule's text as it was saved there. found is false when no
// version holds it. One query whatever the history's length: the snapshots are matched on the comment
// as their JSON spells it, in version order, and the first is read. SQLite's like ignores case, so the
// version found is held to the comment exactly, and one that only matched without case is not it.
func (s *Store) ReviewRuleFirstVersion(ctx context.Context, orgID, typeID int64, fromURL string) (by, text string, found bool, err error) {
	spelled, err := json.Marshal(fromURL)
	if err != nil {
		return "", "", false, err
	}
	var snap string
	err = s.db.QueryRowContext(ctx, `select created_by, snapshot_json from review_type_versions
		where org_id=? and type_id=? and snapshot_json like ? escape '\' order by version limit 1`,
		orgID, typeID, `%"from_comment_url":`+escapeLike(string(spelled))+`%`).Scan(&by, &snap)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	var t struct {
		Rules []ReviewTypeRule `json:"rules"`
	}
	if err := json.Unmarshal([]byte(snap), &t); err != nil {
		return "", "", false, fmt.Errorf("review type %d: %w", typeID, err)
	}
	for _, r := range t.Rules {
		if r.FromCommentURL == fromURL {
			return by, r.Text, true, nil
		}
	}
	return "", "", false, nil
}

// ReviewTypeAtVersion is a type exactly as it was saved at one version, rules included, read from
// its snapshot; nil when there is no such version. What an old run ran with, and what a revert
// saves again.
func (s *Store) ReviewTypeAtVersion(ctx context.Context, orgID, typeID int64, version int) (*ReviewType, error) {
	var snap string
	err := s.db.QueryRowContext(ctx, `select snapshot_json from review_type_versions
		where org_id=? and type_id=? and version=?`, orgID, typeID, version).Scan(&snap)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t ReviewType
	if err := json.Unmarshal([]byte(snap), &t); err != nil {
		return nil, fmt.Errorf("review type %d version %d: %w", typeID, version, err)
	}
	t.ID = typeID
	return &t, nil
}

// checkReviewType holds a type to review.ValidateType, under key: the rules the engine reads a
// type under, so storage can never hold one the engine would refuse at run time — a name the
// summary would cut short, a rule spread over lines — or a key that `@… review <key>` and a branch
// rule could never name. One set of rules, in one place; a second copy here had already drifted
// from it. On top of them goes the one column the engine never reads: a rule's status. Which
// models a type may name, and how much it may spend, is the API's to decide.
func checkReviewType(t *ReviewType, key string) error {
	rt := review.Type{Key: key, Name: t.Name, Purpose: t.Purpose, Strictness: review.Strictness(t.Strictness),
		PathGlobs: t.PathGlobs, InlineMinSeverity: review.Severity(t.InlineMinSeverity), Skills: t.Skills}
	var errs []error
	for i, r := range t.Rules {
		rt.Rules = append(rt.Rules, review.TypeRule{Text: r.Text, SeverityCap: review.Severity(r.SeverityCap),
			PathGlobs: r.PathGlobs, Source: r.Source, ExampleBad: r.ExampleBad, ExampleGood: r.ExampleGood, Off: !r.Enabled})
		if !slices.Contains([]string{"", "active", "proposed", "rejected"}, r.Status) {
			errs = append(errs, fmt.Errorf("rule %s: status is active, proposed or rejected", rt.RuleID(i)))
		}
	}
	if err := review.ValidateType(rt); err != nil {
		errs = append([]error{err}, errs...)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrReviewTypeInvalid, errors.Join(errs...))
	}
	return nil
}
