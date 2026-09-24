package app

import (
	"context"
	"errors"
	"fmt"
)

// Erasing one connected Slack workspace: the SQL half of "remove this workspace".
//
// It is the account deletion in store_org_delete.go, one level down, and it works the same way
// for the same reason: the tables are read from the schema rather than written out here, so a
// table added next year is swept by it without anybody remembering to come back. Every table
// carrying a team_id is deleted from by team_id; the handful that hang off one of those rows
// instead of carrying the workspace themselves are named in teamChildTables with the parent
// they are reached through; and the two that deliberately stay are named with the reason.
// TestRemovingAWorkspaceReachesEveryTeamKeyedTable refuses a table that is on none of the three.
//
// Scoping is by team_id alone, and that is not a gap. teams.team_id is the primary key, so a
// workspace belongs to exactly one organisation — the same fact store_surface_test.go leans on
// when it accepts team_id as a tenant predicate. The organisation is checked once, against the
// teams row, before a single delete runs; adding org_id to the sweep as well would only mean
// that a row whose org_id had somehow drifted survived the workspace it belongs to.

// teamChildTables hold rows that belong to a workspace through a parent row rather than by
// carrying team_id of their own: a card belongs to its access request, an event to its job, a
// grant to the scope it was made on. They go first, while the parents the subqueries read are
// still there.
var teamChildTables = []struct{ table, key, parent string }{
	{"scope_bundles", "scope_id", "scopes"},
	{"scope_connections", "scope_id", "scopes"},
	{"access_request_cards", "request_id", "access_requests"},
	{"job_events", "job_id", "jobs"},
	{"job_files", "job_id", "jobs"},
}

// teamTablesThatSurvive carry a team_id and keep their rows when the workspace goes, with the
// reason. The teams table is not one of them — it is the workspace, and it is deleted last,
// below, rather than in the sweep that reads rows out of it.
var teamTablesThatSurvive = map[string]string{
	"audit_log": "the account's record of what was done in it, this removal included. A trail a " +
		"delete can take a slice out of is not one, and the account it belongs to is still here",
	"usage": "the month's spend, which every budget is measured against. Deleting it with the " +
		"workspace would let an account reset its own free budget and the platform ceiling by " +
		"removing and re-adding a workspace; the account it belongs to is still here",
	"active_users": "the 30-day active-user count the plan is sized and billed by. A removal must " +
		"never be a way to shrink it; the account it belongs to is still here",
}

var errNotThisOrgsTeam = errors.New("that workspace is not connected")

// DeleteTeam removes one workspace and everything recorded for it, in one transaction: either
// the workspace is gone or nothing happened, never half of each. The organisation keeps
// everything of its own — its bundles, credentials, documents and skills are the account's, not
// the workspace's, and its other workspaces are untouched.
//
// Whether the workspace may be removed at all is the caller's to decide, as it is for DeleteOrg:
// the console only offers this once the install has been disconnected, which is where that rule
// is written down (team_delete.go). This is the mechanism.
func (s *Store) DeleteTeam(ctx context.Context, orgID int64, teamID string) (Erased, error) {
	out := Erased{Tables: map[string]int64{}}
	if orgID == 0 || teamID == "" {
		return out, errors.New("no workspace to delete")
	}
	// The one tenant check, made before anything is deleted: a workspace another organisation
	// holds is not this one's to remove, and answers the same as one that was never connected.
	t, err := s.Team(ctx, teamID)
	if err != nil {
		return out, err
	}
	if t == nil || t.OrgID != orgID {
		return out, errNotThisOrgsTeam
	}
	tables, err := s.teamScopedTables(ctx)
	if err != nil {
		return out, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	del := func(table, where string, args ...any) error {
		res, err := tx.ExecContext(ctx, `delete from `+table+` `+where, args...)
		if err != nil {
			return fmt.Errorf("deleting from %s: %w", table, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out.Tables[table] += n
			out.Rows += n
		}
		return nil
	}

	for _, c := range teamChildTables {
		if err := del(c.table, `where `+c.key+` in (select id from `+c.parent+` where team_id=?)`, teamID); err != nil {
			return out, err
		}
	}
	for _, tb := range tables {
		// teams is the workspace itself and goes at the end; the rest of the skips are the rows
		// that outlive it.
		if tb == "teams" || teamTablesThatSurvive[tb] != "" {
			continue
		}
		if err := del(tb, `where team_id=?`, teamID); err != nil {
			return out, err
		}
	}
	// Last, so that a failure anywhere above leaves a workspace that is still whole rather than
	// a pile of rows pointing at a workspace that no longer exists.
	if err := del("teams", `where team_id=?`, teamID); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// teamScopedTables asks the database which tables carry a team_id — the same question
// orgScopedTables asks about org_id, and asked of the schema for the same reason.
func (s *Store) teamScopedTables(ctx context.Context) ([]string, error) {
	return s.tableNames(ctx, teamScopedTablesQuery(s.db.postgres()))
}
