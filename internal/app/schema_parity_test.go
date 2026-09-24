package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The two dialects have to describe the same database, and nothing in the build was checking
// that they did.
//
// How they came apart: migrate() is a list of ADD COLUMNs that OpenStore runs only when the
// store is not Postgres, on the reasoning that no Postgres database predates the versioned
// baseline. That holds for the baseline itself and fails for every column appended to the list
// afterwards — such a column reaches SQLite, through migrate(), and reaches Postgres through
// nothing at all. connections.secret_fp was appended that way and Postgres never got it, which
// put `column "secret_fp" does not exist` under every read of the connections table in
// production: the console drew every bundle as holding no credentials, and said nothing,
// because Bundles() discarded the error it got back.
//
// Two tests, because they cost different things. The first needs no database and so actually
// runs; the second is the rigorous one and needs a Postgres to compare against.

// TestEveryMigrateColumnReachesPostgres is the cheap half: a column that migrate() adds must at
// least be named somewhere in the Postgres migrations. Naming is a weaker claim than having —
// it does not check the table, the type or the default — but the failure that shipped was a
// column mentioned in neither file of that dialect, and this catches that in `go test ./...`
// with nothing installed. TestSchemasMatchAcrossDialects below is what checks the rest.
func TestEveryMigrateColumnReachesPostgres(t *testing.T) {
	pg, err := loadMigrations("postgres")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, m := range pg {
		sb.WriteString(m.sql)
		sb.WriteString("\n")
	}
	// Comments out: this file argues for the column by name in prose, and a test satisfied by
	// prose is a test that passes on the bug it was written for.
	text := regexp.MustCompile(`(?m)--[^\n]*`).ReplaceAllString(sb.String(), "")

	for _, a := range migrateAdds {
		table, col := a[0], a[1]
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(col) + `\b`).MatchString(text) {
			t.Errorf("migrate() adds %s.%s, which no Postgres migration mentions: "+
				"OpenStore skips migrate() on Postgres, so that column will not exist there. "+
				"Add it in a new migrations/postgres/NNNN_*.sql (with its SQLite twin).", table, col)
		}
	}
}

// TestSchemasMatchAcrossDialects is the rigorous half: build both schemas the way OpenStore
// builds them and compare the catalogues, column by column.
//
//	TEST_DATABASE_URL="postgres://$(whoami)@localhost:5432/attesttag_test?sslmode=disable" \
//	  go test ./internal/app -run TestSchemasMatchAcrossDialects
func TestSchemasMatchAcrossDialects(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to a Postgres database to run this")
	}
	lite, err := OpenStore(filepath.Join(t.TempDir(), "parity.db"))
	if err != nil {
		t.Fatalf("opening the SQLite side: %v", err)
	}
	defer lite.db.Close()
	pg, err := OpenStore(dsn)
	if err != nil {
		t.Fatalf("opening the Postgres side: %v", err)
	}
	defer pg.db.Close()

	// The differences that are meant to be there, each one a decision recorded in db_dialect.go
	// or forced by the engine. Anything else is drift.
	allowed := map[string]string{
		"sqlite_sequence":   "SQLite's own bookkeeping for autoincrement; not a table of ours",
		"email_tokens.id":   "emailTokenIDCol: Postgres has no rowid, so this dialect carries a real identity column",
		"schema_migrations": "written by applyMigrations itself, identically on both",
	}

	l, p := schemaColumns(t, lite, false), schemaColumns(t, pg, true)
	var problems []string
	for _, want := range []struct {
		have, lack map[string]map[string]bool
		dialect    string
	}{{l, p, "Postgres"}, {p, l, "SQLite"}} {
		for table, cols := range want.have {
			if allowed[table] != "" {
				continue
			}
			if want.lack[table] == nil {
				problems = append(problems, fmt.Sprintf("table %s is missing from %s", table, want.dialect))
				continue
			}
			for col := range cols {
				if !want.lack[table][col] && allowed[table+"."+col] == "" {
					problems = append(problems, fmt.Sprintf("%s.%s is missing from %s", table, col, want.dialect))
				}
			}
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	if len(problems) > 0 {
		t.Log("a column on one side and not the other means a query written once runs on one dialect only")
	}
}

// schemaColumns is every table's every column, asked of the database rather than of the files
// that built it — the files are what is in doubt.
func schemaColumns(t *testing.T, s *Store, pg bool) map[string]map[string]bool {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), everyColumnQuery(pg))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		if out[table] == nil {
			out[table] = map[string]bool{}
		}
		out[table][col] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---- indexes ----

// The column tests above compare what the two dialects declare; nothing compared their indexes,
// and an index is not cosmetic. One dialect getting a UNIQUE the other does not means a row pair
// that is legal in production and rejected in a developer's SQLite, or the reverse — the
// secret_fp failure again, in the half of the schema that the catalogue comparison never reached.
//
// This is deliberately the cheap kind of check: it reads the migration files rather than a live
// database, so it runs in `go test ./...` with nothing installed, which is what the rigorous half
// above cannot claim. It compares the whole definition and not just the name, because the defect
// that prompted it — 0009's unique index over billing_accounts.customer_id counting the empty
// string, which let only one organisation per deployment hold a row with no Stripe customer — was
// a predicate, and 0012 fixes it by adding a WHERE to an index whose name does not change.
//
// Worth being honest about the limit: that defect was identical on both dialects, so this test
// would not have caught it. What it catches is the next one — a fix or an index applied to one
// file and not its twin, which is the shape every schema divergence in this repository has had.
func TestIndexesMatchAcrossDialects(t *testing.T) {
	lite, err := declaredIndexes("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	pg, err := declaredIndexes("postgres")
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	// Both directions, because "missing" has to name the side that lacks it. A definition that
	// merely differs is the same fact read twice, so only the first pass reports it.
	for _, side := range []struct {
		have, lack map[string]string
		dialect    string
		compare    bool
	}{{lite, pg, "Postgres", true}, {pg, lite, "SQLite", false}} {
		for name, def := range side.have {
			other, ok := side.lack[name]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("index %s is declared for the other dialect but not for %s", name, side.dialect))
			case side.compare && other != def:
				problems = append(problems, fmt.Sprintf("index %s differs between the dialects: SQLite has %q, Postgres has %q", name, def, other))
			}
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	if len(problems) > 0 {
		t.Log("an index on one side and not the other is a constraint that holds in one deployment and not another; " +
			"add the twin in migrations/<dialect>/NNNN_*.sql")
	}
}

// declaredIndexes replays one dialect's migrations and returns the indexes left standing, by name.
// Replayed rather than collected, because a migration may drop an index to recreate it — 0012 does
// exactly that — and the answer wanted here is the final state, not every statement ever written.
func declaredIndexes(dialect string) (map[string]string, error) {
	ms, err := loadMigrations(dialect)
	if err != nil {
		return nil, err
	}
	createIdx := regexp.MustCompile(`(?is)^create\s+(unique\s+)?index\s+(?:if\s+not\s+exists\s+)?(\w+)\s+on\s+(.*)$`)
	dropIdx := regexp.MustCompile(`(?is)^drop\s+index\s+(?:if\s+exists\s+)?(\w+)`)
	comment := regexp.MustCompile(`(?m)--[^\n]*`)
	space := regexp.MustCompile(`\s+`)

	out := map[string]string{}
	for _, m := range ms {
		for _, stmt := range splitStatements(comment.ReplaceAllString(m.sql, "")) {
			stmt = strings.TrimSpace(space.ReplaceAllString(stmt, " "))
			if g := createIdx.FindStringSubmatch(stmt); g != nil {
				kind := "index"
				if strings.TrimSpace(g[1]) != "" {
					kind = "unique index"
				}
				// Lower-cased and with the spaces normalised, so the two files may differ in
				// layout — they are written by hand, months apart — and not in meaning.
				out[g[2]] = strings.ToLower(kind + " on " + strings.TrimSpace(g[3]))
				continue
			}
			if g := dropIdx.FindStringSubmatch(stmt); g != nil {
				delete(out, g[1])
			}
		}
	}
	return out, nil
}
