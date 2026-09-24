package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func openRaw(t *testing.T, path string) *database {
	t.Helper()
	driver, conn, pg := parseDSN(path)
	h, err := sql.Open(driver, conn)
	if err != nil {
		t.Fatal(err)
	}
	h.SetMaxOpenConns(1)
	t.Cleanup(func() { h.Close() })
	return &database{sql: h, pg: pg}
}

func migrationVersions(t *testing.T, db *database) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	rows, err := db.QueryContext(context.Background(), `select version, checksum from schema_migrations`)
	if err != nil {
		t.Fatalf("no schema_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int64
		var sum string
		rows.Scan(&v, &sum)
		out[v] = sum
	}
	return out
}

// The upgrade path, and the one that would have been expensive to get wrong. A database created
// before versioned migrations existed has every table already and has never heard of
// schema_migrations. Running the baseline against it would try to create tables that are there.
// It must be adopted instead: recorded as applied, not run.
func TestAnExistingSchemaIsAdoptedNotRebuilt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a database the way the old code did: the ddl constant, the inbox, then the
	// additive migrator. This is what every database in existence before this commit looks like.
	old := openRaw(t, path)
	if _, err := old.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(slackDeliveryDDL); err != nil {
		t.Fatal(err)
	}
	if err := migrate(old); err != nil {
		t.Fatal(err)
	}
	// Put an organisation in it, because that is what adoption keys on — and because losing it
	// is the failure this test exists to rule out.
	if _, err := old.ExecContext(ctx, `insert into orgs (public_id, name, slug) values ('p1','Acme','acme')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	// Now open it with the new code.
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("an existing database failed to open: %v", err)
	}
	defer st.Close()

	if n := st.OrgCount(ctx); n != 1 {
		t.Fatalf("the organisation did not survive the upgrade: OrgCount = %d", n)
	}
	// The baseline is recorded as adopted — not run — and every migration after it IS run,
	// which is the whole point: an old database gets the new tables without being rebuilt.
	all, err := loadMigrations("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	v := migrationVersions(t, st.db)
	if _, ok := v[1]; !ok {
		t.Fatalf("the baseline was not recorded as applied: %v", v)
	}
	if len(v) != len(all) {
		t.Fatalf("%d migrations recorded, %d exist; an adopted database must still get the later ones", len(v), len(all))
	}
	// leader_leases arrives in 0002 and cannot exist in a database built the old way, so its
	// presence is proof the later migrations actually ran against the adopted schema.
	if _, err := st.db.ExecContext(ctx, `select count(*) from leader_leases`); err != nil {
		t.Errorf("a migration after the baseline did not run on the adopted database: %v", err)
	}
	// And opening it again is a no-op rather than a second adoption.
	st.Close()
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	defer st2.Close()
	if n := st2.OrgCount(ctx); n != 1 {
		t.Errorf("a second open changed the data: OrgCount = %d", n)
	}
}

// A database with nothing in it gets the baseline run, not adopted.
func TestAFreshDatabaseRunsTheBaseline(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "new.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	all, err := loadMigrations("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if v := migrationVersions(t, st.db); len(v) != len(all) {
		t.Fatalf("%d migrations recorded, %d exist", len(v), len(all))
	}
	// The schema is actually there: the composite keys that the old additive migrator could
	// never express are the whole reason this exists.
	if _, err := st.db.ExecContext(ctx, `insert into orgs (public_id, name, slug) values ('p','N','n')`); err != nil {
		t.Fatalf("the baseline did not produce a usable schema: %v", err)
	}
}

// An applied migration that has since been edited means the database and the repository
// disagree about what ran. That has to stop the process, not be papered over.
func TestAnEditedMigrationRefusesToStart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "edited.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	db := openRaw(t, path)
	if _, err := db.ExecContext(ctx, `update schema_migrations set checksum='tampered' where version=1`); err != nil {
		t.Fatal(err)
	}
	err = applyMigrations(ctx, db)
	if err == nil {
		t.Fatal("a changed migration was accepted")
	}
	if !strings.Contains(err.Error(), "has changed since it was applied") {
		t.Errorf("the refusal should say what is wrong: %v", err)
	}
}

// Every version has a file in both dialects, with the same name. A migration that exists for
// one and not the other is a deployment that works on one database and not the other, found in
// production rather than here.
func TestMigrationsArePairedAcrossDialects(t *testing.T) {
	s, err := loadMigrations("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadMigrations("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != len(p) {
		t.Fatalf("%d sqlite migrations, %d postgres", len(s), len(p))
	}
	for i := range s {
		if s[i].version != p[i].version || s[i].name != p[i].name {
			t.Errorf("migration %d: sqlite has %04d_%s, postgres has %04d_%s",
				i, s[i].version, s[i].name, p[i].version, p[i].name)
		}
	}
	// Versions start at 1 and have no gaps, so "the newest version" is also "how many ran".
	for i, m := range s {
		if m.version != int64(i+1) {
			t.Errorf("migration %d has version %d; versions must run 1..n with no gaps", i, m.version)
		}
	}
}

// splitStatements is what feeds the SQLite driver one statement at a time. The schema is full
// of semicolons inside comments and strings, and one of those ending a statement early would
// produce a half-built database.
func TestSplitStatements(t *testing.T) {
	got := splitStatements(`
-- a comment; with a semicolon
create table a (x text default 'hi; there');
create table b (y text);
`)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2: %q", len(got), got)
	}
	if !strings.Contains(got[0], "'hi; there'") {
		t.Errorf("a semicolon inside a string ended the statement: %q", got[0])
	}
	if !strings.HasPrefix(got[1], "create table b") {
		t.Errorf("second statement = %q", got[1])
	}
	// A trailing statement with no semicolon still counts.
	if n := len(splitStatements(`select 1`)); n != 1 {
		t.Errorf("an unterminated final statement was dropped (%d)", n)
	}
}

// skipUnlessSQLite marks a test as being about the transitional SQLite path: the old additive
// migrator and the one-shot folds it ran, which exist only to bring a database created before
// versioned migrations up to date. There is no Postgres database that predates the baseline, so
// on Postgres these test a code path that is never taken.
func skipUnlessSQLite(t *testing.T, st *Store) {
	t.Helper()
	if st.db.postgres() {
		t.Skip("this covers the SQLite-only upgrade path; Postgres has no database that predates the baseline")
	}
}
