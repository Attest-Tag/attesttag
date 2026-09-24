package app

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Versioned migrations, one set per dialect.
//
// What this replaces is worth stating, because it was a real defect rather than an
// inconvenience. The old migrate() was a list of ADD COLUMNs applied idempotently — all it
// could express. The composite primary keys multi-tenancy needed could not be expressed that
// way, so they existed only on a database created fresh, and a database created before them
// simply did not have them. The production replica had to be abandoned for exactly this
// reason: a container restoring it died on "no such column: org_id". Shipping that to people
// who will upgrade a database with their own data in it was not an option.
//
// The window to do this cleanly is now and does not come back: the only SQLite databases in
// existence are the production one and developers' own, every fold has already run on both,
// and after the repository is public there is an installed base forever.

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationFS embed.FS

// migrationAdvisoryLock is an arbitrary constant; it only has to be the same number in every
// process. Two instances booting together then migrate once, and the second waits rather than
// racing. SQLite needs no equivalent: that mode is single-instance and says so at startup.
const migrationAdvisoryLock = 8274461903

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int64
	name     string
	sql      string
	checksum string
}

// loadMigrations reads one dialect's set, in version order, and refuses anything malformed —
// a file nobody can parse is a migration nobody can be sure ran.
func loadMigrations(dialect string) ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations/"+dialect)
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int64]string{}
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations/%s/%s: name must be NNNN_lower_snake.sql", dialect, e.Name())
		}
		v, _ := strconv.ParseInt(m[1], 10, 64)
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations/%s: version %04d is used by both %s and %s", dialect, v, prev, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(migrationFS, "migrations/"+dialect+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{version: v, name: m[2], sql: string(body), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// applyMigrations brings the database up to the newest version it has a file for.
func applyMigrations(ctx context.Context, db *database) error {
	dialect := "sqlite"
	if db.postgres() {
		dialect = "postgres"
	}
	all, err := loadMigrations(dialect)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("no migrations embedded for %s", dialect)
	}

	// One migrating process at a time. The lock is held to the end of the transaction that
	// takes it, and here that transaction is the whole run.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if db.postgres() {
		if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(?)`, migrationAdvisoryLock); err != nil {
			return fmt.Errorf("taking the migration lock: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `create table if not exists schema_migrations (
		version bigint primary key,
		checksum text not null,
		applied_at text not null
	)`); err != nil {
		return err
	}

	applied := map[int64]string{}
	rows, err := tx.QueryContext(ctx, `select version, checksum from schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	rows.Close()

	// Baseline adoption. A database from before this existed has every one of the old ADD
	// COLUMNs already applied and has never seen schema_migrations, so running 0001 against it
	// would try to create tables that are already there. Record it as applied instead.
	//
	// The test is whether `orgs` exists, because a database with organisations in it is a
	// database that predates this file. Getting this wrong is not a small mistake: it is the
	// difference between a clean upgrade and a failed boot on somebody's production data.
	if len(applied) == 0 && tableExists(ctx, tx, "orgs") {
		base := all[0]
		if _, err := tx.ExecContext(ctx, `insert into schema_migrations (version, checksum, applied_at) values (?, ?, ?)`,
			base.version, base.checksum, now()); err != nil {
			return err
		}
		applied[base.version] = base.checksum
		slog.Info("adopted an existing schema as the baseline; it was created before versioned migrations",
			"version", base.version, "dialect", dialect)
	}

	for _, m := range all {
		if sum, done := applied[m.version]; done {
			// An applied file that has since been edited means the database and the repository
			// disagree about what ran. Refusing to start is the only honest answer.
			if sum != m.checksum {
				// truncate, not sum[:12]: a checksum column holding something shorter than
				// that is exactly the corruption this branch exists to report, and slicing it
				// would panic the process at boot instead of saying so.
				return fmt.Errorf("migration %04d_%s has changed since it was applied (recorded %s, file %s); "+
					"an applied migration must never be edited — add a new one",
					m.version, m.name, truncate(sum, 12), truncate(m.checksum, 12))
			}
			continue
		}
		if err := execScript(ctx, tx, m.sql); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `insert into schema_migrations (version, checksum, applied_at) values (?, ?, ?)`,
			m.version, m.checksum, now()); err != nil {
			return err
		}
		slog.Info("applied migration", "version", m.version, "name", m.name, "dialect", dialect)
	}
	return tx.Commit()
}

// execScript runs a file's statements. Postgres takes a whole script in one Exec, but the SQLite
// driver here does not, so the script is split on the semicolons that end a statement — ignoring
// the ones inside strings and comments, which the schema has plenty of.
func execScript(ctx context.Context, tx *dbTx, script string) error {
	for _, stmt := range splitStatements(script) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w\nin: %s", err, truncate(strings.TrimSpace(stmt), 200))
		}
	}
	return nil
}

func splitStatements(script string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case c == '\'' || c == '"':
			q := c
			cur.WriteByte(c)
			for i++; i < len(script); i++ {
				cur.WriteByte(script[i])
				if script[i] == q {
					if i+1 < len(script) && script[i+1] == q {
						i++
						cur.WriteByte(script[i])
						continue
					}
					break
				}
			}
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			for ; i < len(script) && script[i] != '\n'; i++ {
			}
			cur.WriteByte('\n')
		case c == ';':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}
