package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Removing one connected workspace: what it reaches, what it must not reach, and who may ask.

// TestRemovingAWorkspaceReachesEveryTeamKeyedTable is the twin of the account deletion's
// coverage test. A table carrying a team_id holds rows belonging to one workspace, so it is
// either swept when that workspace goes or named with the reason it stays — never forgotten.
func TestRemovingAWorkspaceReachesEveryTeamKeyedTable(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	tables, err := st.teamScopedTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 10 {
		t.Fatalf("only %d table(s) carry a team_id; the schema question is not asking what it thinks", len(tables))
	}
	// Every one of them is swept unless it is named as surviving, which is the whole of the
	// rule — so what this checks is the other direction: that the names in the survivors map
	// are real tables, and that a table renamed out from under it fails here rather than
	// silently going unswept.
	have := map[string]bool{}
	for _, tb := range tables {
		have[tb] = true
	}
	for tb, why := range teamTablesThatSurvive {
		if !have[tb] {
			t.Errorf("teamTablesThatSurvive names %q (%s), which carries no team_id — it is excusing a table that does not exist", tb, why)
		}
	}
	for _, c := range teamChildTables {
		if !have[c.parent] {
			t.Errorf("%s is reached through %s, which carries no team_id", c.table, c.parent)
		}
		if have[c.table] {
			t.Errorf("%s carries a team_id of its own; sweep it directly rather than through %s", c.table, c.parent)
		}
	}
}

// TestRemovingAWorkspaceSweepsOnlyItsOwnRows seeds a row in every table a workspace can own, for
// three workspaces — two in the same account — and removes one. Seeding from the schema rather
// than from a list is the point: a table added next year is seeded, swept and checked here
// without anybody remembering this test exists.
func TestRemovingAWorkspaceSweepsOnlyItsOwnRows(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()
	// A second workspace in the same account: the neighbour that must not feel a thing.
	if err := x.st.SaveTeam(ctx, &Team{TeamID: "T_A2", OrgID: x.a, Name: "Acme second"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}

	tables, err := x.st.teamScopedTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	teams := map[string]int64{"T_A": x.a, "T_A2": x.a, "T_B": x.b}
	for _, tb := range tables {
		if tb == "teams" { // the workspaces themselves are seeded above and by the fixture
			continue
		}
		for team, org := range teams {
			seedRow(t, x.st, tb, team, org)
		}
	}
	// Rows that belong to a workspace through a parent row: one card on a request, one event and
	// one file on a job, and the bundle the fixture already attached to T_A's channel scope.
	for _, c := range teamChildTables {
		for team := range teams {
			seedChild(t, x.st, c.table, c.key, c.parent, team)
		}
	}

	erased, err := x.st.DeleteTeam(ctx, x.a, "T_A")
	if err != nil {
		t.Fatal(err)
	}
	if erased.Rows == 0 {
		t.Fatal("the removal reported no rows at all")
	}

	for _, tb := range tables {
		n := countTeamRows(t, x.st, tb, "T_A")
		switch {
		case tb == "teams":
			// The workspace's own row, deleted last rather than swept. Its neighbours are
			// checked by name below.
			if n > 0 {
				t.Error("the workspace row survived its own removal")
			}
			continue
		case teamTablesThatSurvive[tb] != "":
			if n == 0 {
				t.Errorf("%s was swept, but it is named as surviving a removal: %s", tb, teamTablesThatSurvive[tb])
			}
		case n > 0:
			t.Errorf("%s still holds %d row(s) of the removed workspace", tb, n)
		}
		// The neighbour in the same account, and the other account's workspace, keep theirs.
		for _, kept := range []string{"T_A2", "T_B"} {
			if countTeamRows(t, x.st, tb, kept) == 0 {
				t.Errorf("%s lost %s's row when T_A was removed", tb, kept)
			}
		}
	}
	for _, c := range teamChildTables {
		if n := countChildRows(t, x.st, c.table, c.key, c.parent, "T_A"); n > 0 {
			t.Errorf("%s still holds %d row(s) hanging off the removed workspace's %s", c.table, n, c.parent)
		}
		for _, kept := range []string{"T_A2", "T_B"} {
			if n := countChildRows(t, x.st, c.table, c.key, c.parent, kept); n == 0 {
				t.Errorf("%s lost the row hanging off %s's %s", c.table, kept, c.parent)
			}
		}
	}

	// The account keeps everything that was never the workspace's: its bundles, its credentials,
	// its documents, its people.
	if tm, _ := x.st.Team(ctx, "T_A"); tm != nil {
		t.Error("the workspace row survived its own removal")
	}
	if tm, _ := x.st.Team(ctx, "T_A2"); tm == nil {
		t.Error("removing one workspace took the account's other workspace with it")
	}
	if bs, _ := x.st.Bundles(ctx, x.a); len(bs) != 1 {
		t.Errorf("the account has %d bundles left, want 1: a bundle belongs to the account, not to a workspace", len(bs))
	}
	if ds, _ := x.st.Documents(ctx, x.a); len(ds) != 1 {
		t.Errorf("the account has %d documents left, want 1", len(ds))
	}
	if o, _ := x.st.Org(ctx, x.a); o == nil {
		t.Fatal("removing a workspace deleted the account")
	}
	if u, _ := x.st.UserByEmail(ctx, "a@example.com"); u == nil {
		t.Error("removing a workspace deleted the person who connected it")
	}
}

// Another organisation's workspace is not this one's to remove, whatever the request says.
func TestRemovingAWorkspaceStopsAtTheOrganisation(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()
	if _, err := x.st.DeleteTeam(ctx, x.a, "T_B"); err != errNotThisOrgsTeam {
		t.Errorf("deleting another organisation's workspace = %v, want it refused", err)
	}
	if tm, _ := x.st.Team(ctx, "T_B"); tm == nil {
		t.Fatal("another organisation's workspace was removed by a tenant that does not hold it")
	}
	if _, err := x.st.DeleteTeam(ctx, x.a, "T_NOPE"); err != errNotThisOrgsTeam {
		t.Errorf("deleting a workspace that was never connected = %v, want the same answer", err)
	}
}

// ---- the route: the name typed out, the permission, and what the rail says afterwards ----

func TestRemoveWorkspaceRoute(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	token := seedAdmin(t, st)
	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: 1, Name: "Acme"}, enc); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTeam(ctx, &Team{TeamID: "TB", OrgID: 1, Name: "Beta"}, enc); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertScope(ctx, 1, "channel", "TA", "C1", "#eng"); err != nil {
		t.Fatal(err)
	}

	// A connected workspace is not removable at all: ending the install is its own step, and the
	// reversible one.
	code, out := authReq(t, mux, "POST", "/api/teams/TA/delete", map[string]string{"confirm": "Acme"}, token)
	if code != 409 {
		t.Fatalf("removing a connected workspace = %d, want 409: %v", code, out)
	}
	if tm, _ := st.Team(ctx, "TA"); tm == nil {
		t.Fatal("a connected workspace was removed without being disconnected")
	}
	if w := do(t, mux, "POST", "/api/teams/TA/disconnect", token); w.Code != 200 {
		t.Fatalf("disconnect = %d: %s", w.Code, w.Body.String())
	}

	// The name is the pause: without it, or with the wrong one, nothing happens.
	for _, typed := range []string{"", "acme ltd", "TB"} {
		code, out := authReq(t, mux, "POST", "/api/teams/TA/delete", map[string]string{"confirm": typed}, token)
		if code != 400 {
			t.Fatalf("confirm=%q = %d, want 400: %v", typed, code, out)
		}
	}
	if tm, _ := st.Team(ctx, "TA"); tm == nil {
		t.Fatal("the workspace went without its name being typed")
	}
	// Case and surrounding space are forgiven — it is a confirmation, not a password.
	code, out = authReq(t, mux, "POST", "/api/teams/TA/delete", map[string]string{"confirm": "  acme "}, token)
	if code != 200 {
		t.Fatalf("remove = %d: %v", code, out)
	}
	if tm, _ := st.Team(ctx, "TA"); tm != nil {
		t.Error("the workspace row is still there")
	}
	if n := countTeamRows(t, st, "scopes", "TA"); n != 0 {
		t.Errorf("%d scope(s) of the removed workspace are still in the rail", n)
	}
	// Gone from the rail, and the account's other workspace is still in it.
	w := do(t, mux, "GET", "/api/teams", token)
	if w.Code != 200 {
		t.Fatalf("GET /api/teams = %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, `"TA"`) || !strings.Contains(body, `"TB"`) {
		t.Errorf("after the removal the rail says %s", body)
	}
	// A workspace that is not connected any more, and one that never was, answer the same.
	if code, _ := authReq(t, mux, "POST", "/api/teams/TA/delete", map[string]string{"confirm": "Acme"}, token); code != 404 {
		t.Errorf("removing it twice = %d, want 404", code)
	}
	if code, _ := authReq(t, mux, "POST", "/api/teams/TNOPE/delete", map[string]string{"confirm": "whatever"}, token); code != 404 {
		t.Errorf("removing a workspace that was never connected = %d, want 404", code)
	}

	// The audit log is the account's, not the workspace's, so it survives the sweep and carries
	// what was done.
	events, err := st.AuditEvents(ctx, 1, AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == "workspace.removed" && e.TargetID == "TA" {
			found = true
		}
	}
	if !found {
		t.Error("the removal left no trace in the audit log")
	}
}

// Removing a workspace hands its token back, so it needs the permission that covers credentials
// — the one Disconnect needs, not the one that edits a channel's instructions.
func TestRemovingAWorkspaceNeedsTheCredentialPermission(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	admin := seedAdmin(t, st)
	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: 1, Name: "Acme"}, enc); err != nil {
		t.Fatal(err)
	}
	if w := do(t, mux, "POST", "/api/teams/TA/disconnect", admin); w.Code != 200 {
		t.Fatalf("disconnect = %d: %s", w.Code, w.Body.String())
	}
	_, editor := member(t, st, 1, "editor@example.com", RoleEditor)
	if code, _ := authReq(t, mux, "POST", "/api/teams/TA/delete", map[string]string{"confirm": "Acme"}, editor); code != 403 {
		t.Errorf("an editor removing a workspace = %d, want 403", code)
	}
	if tm, _ := st.Team(ctx, "TA"); tm == nil {
		t.Fatal("an editor removed a workspace")
	}
}

// ---- seeding from the schema ----

// seedRow puts one row into any table carrying a team_id, filling whatever else that table
// insists on. It exists so that "nothing of this workspace is left" is a claim about every
// table there is, including the ones added after this was written.
func seedRow(t *testing.T, st *Store, table, teamID string, orgID int64) {
	t.Helper()
	cols := []string{"team_id"}
	vals := []any{teamID}
	for _, c := range requiredColumns(t, st, table) {
		switch c.name {
		case "team_id", "id":
			continue
		case "org_id":
			cols, vals = append(cols, c.name), append(vals, orgID)
		default:
			cols, vals = append(cols, c.name), append(vals, fillerFor(c, teamID))
		}
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	q := fmt.Sprintf(`insert into %s (%s) values (%s)`, table, strings.Join(cols, ","), marks)
	if _, err := st.db.ExecContext(context.Background(), q, vals...); err != nil {
		t.Fatalf("seeding %s for %s: %v", table, teamID, err)
	}
}

// seedChild puts one row into a table that hangs off a team-keyed parent.
func seedChild(t *testing.T, st *Store, table, key, parent, teamID string) {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := st.db.QueryRowContext(ctx,
		fmt.Sprintf(`select id from %s where team_id=? order by id limit 1`, parent), teamID).Scan(&id); err != nil {
		t.Fatalf("no %s row for %s to hang a %s off: %v", parent, teamID, table, err)
	}
	cols := []string{key}
	vals := []any{id}
	for _, c := range requiredColumns(t, st, table) {
		if c.name == key || c.name == "id" {
			continue
		}
		cols, vals = append(cols, c.name), append(vals, fillerFor(c, teamID))
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	q := fmt.Sprintf(`insert into %s (%s) values (%s)`, table, strings.Join(cols, ","), marks)
	if _, err := st.db.ExecContext(ctx, q, vals...); err != nil {
		t.Fatalf("seeding %s for %s: %v", table, teamID, err)
	}
}

func countTeamRows(t *testing.T, st *Store, table, teamID string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(),
		fmt.Sprintf(`select count(*) from %s where team_id=?`, table), teamID).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

func countChildRows(t *testing.T, st *Store, table, key, parent, teamID string) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`select count(*) from %s where %s in (select id from %s where team_id=?)`, table, key, parent)
	if err := st.db.QueryRowContext(context.Background(), q, teamID).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// column is what the seeder needs to know about one column: its name, whether a value has to be
// supplied, and what kind of value.
type column struct {
	name    string
	kind    string
	notNull bool
}

// requiredColumns lists the columns an insert has to fill: not null, with no default or identity
// of the engine's own. Both engines are supported and only one of them is SQLite, so the
// question itself lives in db_dialect.go.
func requiredColumns(t *testing.T, st *Store, table string) []column {
	t.Helper()
	ctx := context.Background()
	rows, err := st.db.QueryContext(ctx, requiredColumnsQuery(st.db.postgres()), table)
	if err != nil {
		t.Fatalf("reading the columns of %s: %v", table, err)
	}
	defer rows.Close()
	var out []column
	for rows.Next() {
		var c column
		var notNull, hasDefault int
		if err := rows.Scan(&c.name, &c.kind, &notNull, &hasDefault); err != nil {
			t.Fatal(err)
		}
		if notNull == 1 && hasDefault == 0 {
			c.notNull = true
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fillerFor is a value of the right shape for a column whose content this test does not care
// about — distinct per workspace, so that a unique index across two seeded rows does not
// collide.
func fillerFor(c column, teamID string) any {
	switch k := strings.ToLower(c.kind); {
	case strings.Contains(k, "int"), strings.Contains(k, "real"), strings.Contains(k, "double"), strings.Contains(k, "numeric"):
		return 0
	case strings.Contains(k, "blob"), strings.Contains(k, "bytea"):
		return []byte(teamID)
	default:
		return "seed-" + teamID
	}
}
