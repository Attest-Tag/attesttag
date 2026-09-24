package app

import (
	"context"
	"errors"
	"fmt"
)

// Erasing an organisation: the SQL half of "delete this account".
//
// Three things make this more than a list of deletes.
//
// The list of tables is read from the schema rather than written out here. A hand-kept list is a
// list somebody adds a table to six months later without noticing, and the rows left behind are
// a customer's — kept after they asked to be forgotten, in a file nobody opens again. So every
// table carrying org_id is swept by it, whatever it is called and whenever it was added. The
// eleven that do not carry one are named below with the rule that reaches them, and
// TestDeletingAnAccountReachesEveryTable refuses a table that is on neither list.
//
// The statements are built from a table name rather than written as literals, which puts them
// out of reach of TestEveryPerOrgQueryIsScoped. Nothing here is unscoped — every one carries
// org_id, a team belonging to the organisation, or a user id this function has just proved has
// nowhere else to belong — but the usual guard is not the thing checking that, so the coverage
// test above stands in its place.
//
// The people are not the organisation's to delete. An account can belong to several, so a member
// survives this unless the organisation being deleted was their only one. Then the row has to go
// too: left behind, it is an account that can sign in and is told it belongs nowhere, holding an
// address that can never be signed up again.

// teamKeyedTables hold rows belonging to one connected workspace rather than to the organisation
// directly — a thread and its turns are keyed by the workspace they happened in, and so are a
// Teams tenant's conversation log and the people the bot met there. They are deleted through the
// teams table, which is why they go first: the sweep below is about to delete the rows this
// subquery reads.
var teamKeyedTables = []string{"sessions", "turns", "file_texts", "console_invites", "msteams_messages", "msteams_users"}

// accountTables are one person's own rows, keyed by user id and belonging to no organisation.
// They go only with the account itself, and only when that account has nowhere left to be.
var accountTables = []string{"user_identities", "user_totp", "user_recovery_codes", "login_challenges"}

// tablesThatSurvive are the two the sweep deliberately leaves alone, each with the reason.
// Named here rather than left out, so the coverage test has something to check them against.
var tablesThatSurvive = map[string]string{
	"orgs":        "deleted last, by its own id: it is the organisation, so it has no org_id",
	"users":       "an account can belong to several organisations; only one with nowhere left to be is deleted",
	"seen_events": "Slack event dedup keys, no content of their own, swept by their own one-day GC",
	"schema_migrations": "the database's own version, not anybody's data; deleting it would make " +
		"the next boot try to create a schema that is already there",
	"leader_leases": "which instance is running the deployment's singleton loops; no tenant's rows, " +
		"and expiring on its own within a minute",
	"throttle_events": "rate-limit counters keyed by IP or address rather than by organisation, " +
		"swept by their own window",
	"mcp_clients": "MCP clients that registered themselves before anybody signed in. A client is " +
		"not an organisation's -- one is used by people in many -- and holds only a name and its " +
		"callback addresses. What an organisation gave one, its grants, carries org_id and goes.",
	"billing_events": "Stripe webhook dedup keys, swept by their own age. They carry no tenant " +
		"content and no organisation -- an event arrives before anyone knows whose it is -- and " +
		"keeping them is what stops a redelivery arriving after a deletion from being treated as new. " +
		"The ledger itself is NOT here: it carries org_id and goes with the organisation, because " +
		"Stripe is the system of record for money taken and a customer who asked to be deleted " +
		"does not get an exception (billing.go logs the final balance on the way out).",
}

// Erased is what a deletion actually removed. It exists for the line this writes to the log:
// "deleted organisation 12" says nothing about whether the sweep found anything, and a deletion
// is the one operation nobody can go back afterwards and check.
type Erased struct {
	Rows     int64            `json:"rows"`
	Accounts int64            `json:"accounts"` // people whose only organisation this was
	Tables   map[string]int64 `json:"-"`
}

// DeleteOrg removes an organisation and everything belonging to it, in one transaction: either
// the account is gone or nothing happened, never half of each.
func (s *Store) DeleteOrg(ctx context.Context, orgID int64) (Erased, error) {
	out := Erased{Tables: map[string]int64{}}
	if orgID == 0 {
		return out, errors.New("no organisation to delete")
	}
	scoped, err := s.orgScopedTables(ctx)
	if err != nil {
		return out, err
	}
	members, err := s.memberIDs(ctx, orgID)
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

	for _, t := range teamKeyedTables {
		if err := del(t, `where team_id in (select team_id from teams where org_id=?)`, orgID); err != nil {
			return out, err
		}
	}
	for _, t := range scoped {
		if err := del(t, `where org_id=?`, orgID); err != nil {
			return out, err
		}
	}
	if err := del("orgs", `where id=?`, orgID); err != nil {
		return out, err
	}

	// The memberships are gone by now, so this asks the question in its simplest form: is there
	// anywhere left for this person to be?
	for _, id := range members {
		var left int
		if err := tx.QueryRowContext(ctx, `select count(*) from memberships where user_id=?`, id).Scan(&left); err != nil {
			return out, err
		}
		if left > 0 {
			continue
		}
		for _, t := range accountTables {
			if err := del(t, `where user_id=?`, id); err != nil {
				return out, err
			}
		}
		// Sessions and one-time links are keyed differently: a session row names its account in
		// "id", and an unspent verification or reset link carries no organisation at all.
		if err := del("admin_sessions", `where id=?`, id); err != nil {
			return out, err
		}
		if err := del("email_tokens", `where user_id=?`, id); err != nil {
			return out, err
		}
		if err := del("users", `where id=?`, id); err != nil {
			return out, err
		}
		out.Accounts++
	}
	return out, tx.Commit()
}

// orgScopedTables asks the database which tables carry an org_id, rather than asking a list in
// this file — see the note at the top for why that matters.
func (s *Store) orgScopedTables(ctx context.Context) ([]string, error) {
	return s.tableNames(ctx, orgScopedTablesQuery(s.db.postgres()))
}

// tableNames runs one of the schema questions above and collects the answer. Shared with the
// workspace sweep in store_team_delete.go, which asks the same thing about team_id.
func (s *Store) tableNames(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// memberIDs is everyone in one organisation, read before the memberships are deleted so the
// accounts can be looked at afterwards.
func (s *Store) memberIDs(ctx context.Context, orgID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `select user_id from memberships where org_id=?`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
