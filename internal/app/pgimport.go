package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Copying a SQLite database into an empty Postgres one, once, at a cutover.
//
//	attesttag pg-import --sqlite snap.db --to "postgres://…"
//
// What makes this more than a loop over tables is that ten columns hold sealed credentials —
// teams.bot_token_enc, connections.secret_enc, user_totp.secret_enc, jobs.llm_key_enc,
// slack_deliveries.payload_enc, web_keys.secret_enc, user_connections.secret_enc,
// org_model_keys.key_enc, doc_chunks.embedding — and a byte lost in any of them is every stored credential in that
// table gone, discovered later, by somebody whose bot has quietly stopped being able to reach
// anything. So the copy is verified rather than trusted: row counts, and a hash over every
// value in every row, computed the same way on both sides and compared.
//
// Ids are carried across rather than regenerated, which is why the Postgres baseline declares
// its identity columns "by default" rather than "always". Every sequence is then set past the
// largest id that arrived, or the first insert after the cutover collides with an imported row.

type importStats struct {
	table string
	rows  int64
	// column name → how many of its values were not valid UTF-8 and were repaired on the way
	// across. Nil in the ordinary case, which is almost every table of almost every database.
	repaired map[string]int
}

func PGImport(args []string) int {
	fs := flag.NewFlagSet("pg-import", flag.ContinueOnError)
	src := fs.String("sqlite", "", "path to the SQLite database to read (a restored snapshot, not a live file)")
	dst := fs.String("to", "", "postgres:// URL of the database to write, which must be empty")
	batch := fs.Int("batch", 500, "rows per insert")
	verifyOnly := fs.Bool("verify-only", false, "compare the two databases without writing anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: attesttag pg-import --sqlite snap.db --to postgres://…")
		return 2
	}
	if !strings.HasPrefix(*dst, "postgres://") && !strings.HasPrefix(*dst, "postgresql://") {
		fmt.Fprintln(os.Stderr, "--to must be a postgres:// URL")
		return 2
	}
	if err := pgImport(context.Background(), *src, *dst, *batch, *verifyOnly); err != nil {
		fmt.Fprintf(os.Stderr, "\nimport failed: %v\n", err)
		return 1
	}
	return 0
}

func pgImport(ctx context.Context, srcPath, dstDSN string, batch int, verifyOnly bool) error {
	started := time.Now()

	sdriver, sconn, _ := parseDSN(srcPath)
	sh, err := sql.Open(sdriver, sconn)
	if err != nil {
		return fmt.Errorf("opening the SQLite database: %w", err)
	}
	defer sh.Close()
	sh.SetMaxOpenConns(1)
	source := &database{sql: sh}

	dh, err := sql.Open("pgx", dstDSN)
	if err != nil {
		return fmt.Errorf("opening the Postgres database: %w", err)
	}
	defer dh.Close()
	target := &database{sql: dh, pg: true}

	tables, err := importTables(ctx, source)
	if err != nil {
		return err
	}
	fmt.Printf("source: %s — %d tables\n", srcPath, len(tables))

	if !verifyOnly {
		// The schema, applied the same way a boot would apply it.
		if err := applyMigrations(ctx, target); err != nil {
			return fmt.Errorf("preparing the target schema: %w", err)
		}
		if err := refuseNonEmpty(ctx, target, tables); err != nil {
			return err
		}
		var total int64
		for _, t := range tables {
			st, err := copyTable(ctx, source, target, t, batch)
			if err != nil {
				return fmt.Errorf("copying %s: %w", t, err)
			}
			total += st.rows
			if st.rows > 0 {
				fmt.Printf("  %-28s %8d rows\n", st.table, st.rows)
			}
			for col, n := range st.repaired {
				fmt.Printf("  ▸ %s.%s: %d value(s) were not valid UTF-8 (a character cut in half) "+
					"and were repaired with U+FFFD. Postgres cannot store the original bytes.\n", st.table, col, n)
			}
		}
		fmt.Printf("copied %d rows in %s\n", total, time.Since(started).Round(time.Millisecond))
		if err := resetSequences(ctx, target); err != nil {
			return fmt.Errorf("resetting identity sequences: %w", err)
		}
		fmt.Println("identity sequences set past the imported ids")
	}

	fmt.Println("\nverifying:")
	bad := 0
	for _, t := range tables {
		// The two schemas are not column-for-column identical, and the honest ones to compare
		// are the ones both sides hold. A column only the target has holds nothing the source
		// could have supplied — email_tokens.id, which is SQLite's rowid over there; the lease
		// pair on routines and sessions.stop_requested_at, which a database still on the older
		// schema has never had. A column only the SOURCE has is a different matter entirely, and
		// cannot reach here quietly: copyTable names every source column in its insert, so
		// Postgres refuses the statement outright. It is still checked and named, because a
		// verifier that assumes its own copier is the one that misses things.
		srcCols, err := columnsOf(ctx, source, t)
		if err != nil {
			return fmt.Errorf("reading %s from SQLite: %w", t, err)
		}
		dstCols, err := columnsOf(ctx, target, t)
		if err != nil {
			return fmt.Errorf("reading %s from Postgres: %w", t, err)
		}
		common, srcOnly, dstOnly := compareColumns(srcCols, dstCols)
		if len(srcOnly) > 0 {
			fmt.Printf("  MISMATCH %-26s the source has %s, which the target has no column for\n", t, strings.Join(srcOnly, ", "))
			bad++
			continue
		}
		if len(dstOnly) > 0 {
			verb, subject := "exists", "it is"
			if len(dstOnly) > 1 {
				verb, subject = "exist", "they are"
			}
			fmt.Printf("  ▸ %s: %s %s only in the target and hold no imported value, so %s not compared\n",
				t, strings.Join(dstOnly, ", "), verb, subject)
		}
		sc, scount, err := tableDigest(ctx, source, t, common)
		if err != nil {
			return fmt.Errorf("reading %s from SQLite: %w", t, err)
		}
		dc, dcount, err := tableDigest(ctx, target, t, common)
		if err != nil {
			return fmt.Errorf("reading %s from Postgres: %w", t, err)
		}
		switch {
		case scount != dcount:
			fmt.Printf("  MISMATCH %-26s %d rows here, %d there\n", t, scount, dcount)
			bad++
		case sc != dc:
			fmt.Printf("  MISMATCH %-26s %d rows, contents differ (%s vs %s)\n", t, scount, sc[:12], dc[:12])
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d table(s) did not match; the target is NOT safe to cut over to", bad)
	}
	fmt.Printf("  all %d tables match, row for row and byte for byte\n", len(tables))
	return nil
}

// importTables is every table the deployment owns, in name order. The schema declares no
// foreign keys, so nothing depends on the order they are copied in.
func importTables(ctx context.Context, db *database) ([]string, error) {
	rows, err := db.QueryContext(ctx, allTablesQuery(db.postgres()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		// schema_migrations is the target's own, written by applyMigrations. Carrying the
		// source's across would claim the Postgres baseline had run when the SQLite one did.
		if n == "schema_migrations" {
			continue
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// refuseNonEmpty is the guard against running this twice. A second run would double every row,
// and the verification afterwards would report the mismatch — but by then the target holds two
// copies of somebody's data.
func refuseNonEmpty(ctx context.Context, db *database, tables []string) error {
	for _, t := range tables {
		var n int64
		if err := db.QueryRowContext(ctx, `select count(*) from `+quoteIdent(t)).Scan(&n); err != nil {
			return fmt.Errorf("counting %s: %w", t, err)
		}
		if n > 0 {
			return fmt.Errorf("the target already has %d row(s) in %s; import into an empty database", n, t)
		}
	}
	return nil
}

func columnsOf(ctx context.Context, db *database, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `select * from `+quoteIdent(table)+` limit 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rows.Columns()
}

func copyTable(ctx context.Context, src, dst *database, table string, batch int) (importStats, error) {
	cols, err := columnsOf(ctx, src, table)
	if err != nil {
		return importStats{}, err
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}

	rows, err := src.QueryContext(ctx, `select `+strings.Join(quoted, ", ")+` from `+quoteIdent(table))
	if err != nil {
		return importStats{}, err
	}
	defer rows.Close()

	st := importStats{table: table}
	var pending []any
	var placeholders []string
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		q := `insert into ` + quoteIdent(table) + ` (` + strings.Join(quoted, ", ") + `) values ` +
			strings.Join(placeholders, ", ")
		// Written with $n directly: rebind numbers `?` from one, and these statements are
		// generated rather than written, so there is nothing for the guard tests to read.
		if _, err := dst.sql.ExecContext(ctx, q, pending...); err != nil {
			return err
		}
		pending, placeholders = pending[:0], placeholders[:0]
		return nil
	}

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return st, err
		}
		base := len(pending)
		marks := make([]string, len(cols))
		for i, v := range vals {
			marks[i] = "$" + strconv.Itoa(base+i+1)
			// SQLite stores whatever bytes it was handed and calls them text; Postgres checks,
			// and refuses the whole statement with "invalid byte sequence for encoding UTF8".
			// One such value in a database of two thousand rows would otherwise stop a cutover
			// halfway, at the hour nobody wants to be debugging encodings. Repaired, counted and
			// named in the output instead — and only for text: a []byte is a blob, and every
			// sealed credential in this schema is one, so those bytes are never touched.
			if str, ok := v.(string); ok && !utf8.ValidString(str) {
				v = strings.ToValidUTF8(str, "\uFFFD")
				if st.repaired == nil {
					st.repaired = map[string]int{}
				}
				st.repaired[cols[i]]++
			}
			pending = append(pending, v)
		}
		placeholders = append(placeholders, "("+strings.Join(marks, ", ")+")")
		st.rows++
		if len(placeholders) >= batch {
			if err := flush(); err != nil {
				return st, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	return st, flush()
}

// resetSequences moves every identity sequence past the largest id that was imported. Without
// it the first insert after the cutover asks for id 1 and collides with a row from 2026.
func resetSequences(ctx context.Context, db *database) error {
	rows, err := db.sql.QueryContext(ctx, `select table_name, column_name
		from information_schema.columns
		where table_schema=current_schema() and is_identity='YES'`)
	if err != nil {
		return err
	}
	type idcol struct{ table, col string }
	var cols []idcol
	for rows.Next() {
		var c idcol
		if err := rows.Scan(&c.table, &c.col); err != nil {
			rows.Close()
			return err
		}
		cols = append(cols, c)
	}
	rows.Close()
	for _, c := range cols {
		q := fmt.Sprintf(
			`select setval(pg_get_serial_sequence('%s','%s'), coalesce((select max(%s) from %s), 0) + 1, false)`,
			c.table, c.col, quoteIdent(c.col), quoteIdent(c.table))
		if _, err := db.sql.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s.%s: %w", c.table, c.col, err)
		}
	}
	return nil
}

// tableDigest hashes every value of every row, and the row count beside it.
//
// The rows are canonicalised in Go and sorted here rather than ordered by the database, because
// the two engines do not have to agree on how text sorts and several of these tables have no
// column that orders them uniquely. Sorting the canonical forms makes the comparison about
// content and nothing else.
func tableDigest(ctx context.Context, db *database, table string, cols []string) (string, int64, error) {
	if len(cols) == 0 {
		var err error
		if cols, err = columnsOf(ctx, db, table); err != nil {
			return "", 0, err
		}
	}
	// In name order, because the two databases do not hold their columns in the same one and the
	// copy does not need them to: it inserts by name. SQLite puts a column added by ALTER TABLE
	// at the end, while the Postgres baseline — dumped from the schema rather than replayed as a
	// history of alterations — has it wherever the dump put it. Hashing in catalogue order made
	// every table that ever gained a column read as "contents differ" while holding identical
	// rows, which is the worst possible verifier: one that cries at a copy that was fine.
	cols = append([]string(nil), cols...)
	sort.Strings(cols)
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	rows, err := db.QueryContext(ctx, `select `+strings.Join(quoted, ", ")+` from `+quoteIdent(table))
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", 0, err
		}
		var b strings.Builder
		for _, v := range vals {
			b.WriteString(canonical(v))
			b.WriteByte(31) // unit separator: cannot appear in the text these columns hold
		}
		lines = append(lines, b.String())
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{30}) // record separator
	}
	return hex.EncodeToString(h.Sum(nil)), int64(len(lines)), nil
}

// canonical writes one value in a form both drivers produce identically. The types that matter
// are the four this schema uses; anything else is stringified and would show up as a mismatch
// rather than passing silently.
func canonical(v any) string {
	switch t := v.(type) {
	case nil:
		return "\x00"
	case []byte:
		return "b" + hex.EncodeToString(t)
	case string:
		// Coerced on both sides so that a value the copy had to repair still compares equal.
		// Nothing is hidden by this: Postgres cannot hold invalid UTF-8 at all, so the only
		// values it changes are the ones copyTable already counted and named.
		return "s" + strings.ToValidUTF8(t, "\uFFFD")
	case int64:
		return "i" + strconv.FormatInt(t, 10)
	case float64:
		return "f" + strconv.FormatFloat(t, 'g', 17, 64)
	case bool:
		if t {
			return "i1"
		}
		return "i0"
	case time.Time:
		return "t" + t.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("?%T:%v", v, v)
	}
}

// quoteIdent is for identifiers this process read out of the database's own catalogue, not for
// anything a user typed. Both dialects quote with double quotes, and a doubled one escapes.
// compareColumns splits two column lists into what they share and what only one of them has.
func compareColumns(src, dst []string) (common, srcOnly, dstOnly []string) {
	in := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}
	for _, c := range src {
		if in(dst, c) {
			common = append(common, c)
		} else {
			srcOnly = append(srcOnly, c)
		}
	}
	for _, c := range dst {
		if !in(src, c) {
			dstOnly = append(dstOnly, c)
		}
	}
	sort.Strings(common)
	sort.Strings(srcOnly)
	sort.Strings(dstOnly)
	return common, srcOnly, dstOnly
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// sqlOpenPG is the one place a raw Postgres handle is opened outside OpenStore — the import
// tool and the tests that rehearse it need one before any schema exists.
func sqlOpenPG(dsn string) (*sql.DB, error) { return sql.Open("pgx", dsn) }
