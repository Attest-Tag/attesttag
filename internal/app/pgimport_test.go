package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The cutover, rehearsed. Build a SQLite database with the kinds of row that matter — sealed
// bytes, a float, a NULL, ids that must survive — copy it into an empty Postgres database, and
// check every value arrived. Then check the things that go wrong after a copy rather than
// during one: a second import refused, and the identity sequences moved past the imported ids
// so the first insert afterwards does not collide with a row from before the cutover.
func TestPGImportRoundTrip(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to a Postgres database to run this")
	}
	ctx := context.Background()

	// A source database with data in it.
	srcPath := filepath.Join(t.TempDir(), "snap.db")
	src, err := OpenStore(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	org, err := src.CreateOrg(ctx, "Acme Ltd", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.CreateUser(ctx, "founder@example.com", "Founder", "hash"); err != nil {
		t.Fatal(err)
	}
	// A sealed credential and a float, which are the two shapes a copy loses quietly.
	sealed := []byte{0x00, 0xff, 0x10, 0x7f, 0x80, 0xfe}
	if err := src.SaveTeam(ctx, &Team{TeamID: "T1", OrgID: org.ID, Name: "Acme"}, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.ExecContext(ctx,
		`insert into usage (org_id, team_id, channel, cost_usd) values (?, 'T1', 'C1', 0.123456789)`, org.ID); err != nil {
		t.Fatal(err)
	}
	if err := src.PutSetting(ctx, org.ID, "bot_name", "probe"); err != nil {
		t.Fatal(err)
	}
	src.Close()

	// An empty target.
	schema := "imp" + strings.ReplaceAll(t.Name(), "/", "_")
	admin := openPGAdmin(t, dsn)
	admin.Exec(`drop schema if exists ` + schema + ` cascade`)
	if _, err := admin.Exec(`create schema ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`drop schema ` + schema + ` cascade`) })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	target := dsn + sep + "search_path=" + schema

	if err := pgImport(ctx, srcPath, target, 500, false); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Everything arrived, and the sealed bytes are the same bytes.
	dst, err := OpenStore(target)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if n := dst.OrgCount(ctx); n != 1 {
		t.Errorf("OrgCount = %d, want 1", n)
	}
	var got []byte
	if err := dst.db.QueryRowContext(ctx, `select bot_token_enc from teams where team_id='T1'`).Scan(&got); err != nil {
		t.Fatalf("reading the sealed token: %v", err)
	}
	if string(got) != string(sealed) {
		t.Errorf("sealed bytes changed: %x -> %x", sealed, got)
	}
	var cost float64
	dst.db.QueryRowContext(ctx, `select cost_usd from usage where channel='C1'`).Scan(&cost)
	if cost != 0.123456789 {
		t.Errorf("cost_usd = %v, want 0.123456789", cost)
	}

	// The next insert must not collide with an imported id. Without setval it asks for 1.
	next, err := dst.CreateOrg(ctx, "Beta Inc", 0)
	if err != nil {
		t.Fatalf("inserting after the import: %v", err)
	}
	if next.ID <= org.ID {
		t.Errorf("the sequence was not moved: new org id %d, imported id %d", next.ID, org.ID)
	}

	// And a second import is refused rather than doubling every row.
	err = pgImport(ctx, srcPath, target, 500, false)
	if err == nil {
		t.Error("a second import into a populated database was allowed")
	} else if !strings.Contains(err.Error(), "import into an empty database") {
		t.Errorf("the refusal should say why: %v", err)
	}
}

func openPGAdmin(t *testing.T, dsn string) *database {
	t.Helper()
	h, err := sqlOpenPG(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return &database{sql: h, pg: true}
}

// A database that has been replicated carries two tables of Litestream's own, and they are not
// this deployment's data. Only a restored replica has them — every development copy is
// unreplicated — so an import rehearsed against anything but a real snapshot will not meet
// them, and the first time they are met is the cutover, where the copy fails partway through
// with `relation "_litestream_lock" does not exist`. Found exactly that way, against a snapshot
// of production, on 2026-09-14. No Postgres needed to hold this: the listing is the bug.
func TestImportSkipsLitestreamsOwnTables(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "replicated.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, ddl := range []string{
		`create table _litestream_seq (id integer primary key, seq integer)`,
		`create table _litestream_lock (id integer)`,
	} {
		if _, err := st.db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	tables, err := importTables(ctx, st.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range tables {
		if strings.HasPrefix(n, "_litestream") {
			t.Errorf("pg-import would carry %q across, and the Postgres schema has no such table", n)
		}
	}
	// And the query still returns the deployment's own tables, so this did not pass by
	// excluding everything.
	var found bool
	for _, n := range tables {
		if n == "orgs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the deployment's own tables in the import list, got %d: %v", len(tables), tables)
	}
}

// Truncation used to cut at a byte offset, which can land inside a character and leave half of
// one in the database. SQLite stores it happily and Slack renders the fragment as a single
// replacement glyph, so nothing complains for years — and then Postgres, which validates
// encoding, refuses the row and stops an entire import. Both halves are pinned here: the cut no
// longer produces such a value, and a value produced before the fix still imports.
func TestTruncationCutsAtCharacterBoundaries(t *testing.T) {
	// "…" is three bytes (e2 80 a6), so every cap that is not a multiple of three lands inside
	// one of them. The old code produced invalid UTF-8 for two caps out of every three.
	body := strings.Repeat("…", 40)
	for n := 1; n < len(body); n++ {
		if got := cutAtRune(body, n); !utf8.ValidString(got) {
			t.Fatalf("cutAtRune(…, %d) left half a character: % x", n, got[len(got)-3:])
		}
	}
	if got := truncate(body, 10); !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: % x", got)
	}
	if got := truncateToolOutput(strings.Repeat("…", toolOutputCap)); !utf8.ValidString(got) {
		t.Errorf("truncateToolOutput produced invalid UTF-8")
	}
	// Nothing is lost that was whole: a cut that lands on a boundary keeps everything before it.
	if got := cutAtRune("abc…def", 3); got != "abc" {
		t.Errorf("cutAtRune at a boundary = %q, want \"abc\"", got)
	}
}

// And the repair itself, which is what lets a database written before the fix still move. The
// digest coerces both sides, so a repaired value compares equal rather than reading as a copy
// that lost something.
func TestImportRepairsInvalidUTF8(t *testing.T) {
	broken := "result\xe2\nmore" // a lone lead byte, exactly what a byte-offset cut leaves
	if utf8.ValidString(broken) {
		t.Fatal("the fixture is not actually invalid")
	}
	repaired := strings.ToValidUTF8(broken, "�")
	if !utf8.ValidString(repaired) {
		t.Fatal("repair did not produce valid UTF-8")
	}
	if canonical(broken) != canonical(repaired) {
		t.Error("the digest treats a repaired value as different from its original, so a verified import would report a mismatch it cannot fix")
	}
	if !strings.Contains(canonical(broken), "result") {
		t.Errorf("the repair ate more than the bad bytes: %q", canonical(broken))
	}
}

// The verifier must compare content, not catalogue order. SQLite appends a column added by
// ALTER TABLE; the Postgres baseline was dumped rather than replayed, so it holds the same
// column somewhere else entirely. Digesting in catalogue order made six tables of a real
// production snapshot report "contents differ" while every row matched — checked here with two
// SQLite tables, since the property is about ordering and needs no second engine to show it.
func TestTableDigestIgnoresColumnOrder(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, ddl := range []string{
		`create table first_order (a text, b integer, c text)`,
		`create table other_order (c text, a text, b integer)`,
		`insert into first_order (a, b, c) values ('x', 1, 'z'), ('y', 2, NULL)`,
		`insert into other_order (a, b, c) values ('x', 1, 'z'), ('y', 2, NULL)`,
	} {
		if _, err := st.db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	d1, n1, err := tableDigest(ctx, st.db, "first_order", nil)
	if err != nil {
		t.Fatal(err)
	}
	d2, n2, err := tableDigest(ctx, st.db, "other_order", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n1 != n2 || d1 != d2 {
		t.Errorf("same rows, different column order, different digest: %s (%d rows) vs %s (%d rows)", d1[:12], n1, d2[:12], n2)
	}
}
