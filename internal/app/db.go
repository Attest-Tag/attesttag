package app

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// The seam between the code and whichever SQL dialect is underneath.
//
// Every statement in this package is written once, with `?` placeholders, in the shape SQLite
// understands. That is not sentiment about SQLite: it is what keeps the guard tests honest.
// TestEveryPerOrgQueryIsScoped and TestEveryTableIsClassified read the SQL literals out of these
// files as text, so a statement built by string-concatenation per dialect, or rewritten at the
// call site, is a statement those tests can no longer see. One shape, translated here, at the
// last possible moment.
//
// Postgres wants $1…$n instead of ?, so that is what rebind does, and it is the entire
// difference at this layer. Everything else — `on conflict … do update set excluded.…`,
// `returning id`, the timestamp text format — is written in the form both dialects already
// accept, which is why there is no dialect switch anywhere in the store files.

// database wraps *sql.DB and translates on the way through. The method set is deliberately the
// one database/sql already offers, so that the several hundred existing call sites compile
// untouched: they keep calling ExecContext, QueryContext and the rest, and keep writing `?`.
type database struct {
	sql *sql.DB
	pg  bool
}

func (d *database) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.sql.ExecContext(ctx, d.bind(q), d.args(args)...)
}

func (d *database) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.sql.QueryContext(ctx, d.bind(q), d.args(args)...)
}

func (d *database) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return d.sql.QueryRowContext(ctx, d.bind(q), d.args(args)...)
}

// The context-free three exist because a few dozen call sites — mostly tests, a few one-shot
// statements at startup — use them. They are the same translation; keeping them means this
// change touches no call site at all.
func (d *database) Exec(q string, args ...any) (sql.Result, error) {
	return d.sql.Exec(d.bind(q), d.args(args)...)
}

func (d *database) Query(q string, args ...any) (*sql.Rows, error) {
	return d.sql.Query(d.bind(q), d.args(args)...)
}

func (d *database) QueryRow(q string, args ...any) *sql.Row {
	return d.sql.QueryRow(d.bind(q), d.args(args)...)
}

func (d *database) Close() error { return d.sql.Close() }

// Postgres is what this exists for. Kept as a method rather than read off the struct at call
// sites, so that the one place allowed to care about the dialect stays one place.
func (d *database) postgres() bool { return d != nil && d.pg }

func (d *database) BeginTx(ctx context.Context, opts *sql.TxOptions) (*dbTx, error) {
	tx, err := d.sql.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &dbTx{tx: tx, pg: d.pg}, nil
}

func (d *database) bind(q string) string {
	if !d.pg {
		return q
	}
	return rebind(q)
}

func (d *database) args(a []any) []any { return boolArgs(d.pg, a) }

// dbTx is the same wrapper for a transaction. `*sql.Tx` appears in no signature in this package
// — every transaction begins and ends inside one function — so wrapping it changes nothing but
// the translation.
type dbTx struct {
	tx *sql.Tx
	pg bool
}

func (t *dbTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.bind(q), t.args(args)...)
}

func (t *dbTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.bind(q), t.args(args)...)
}

func (t *dbTx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.bind(q), t.args(args)...)
}

func (t *dbTx) Exec(q string, args ...any) (sql.Result, error) {
	return t.tx.Exec(t.bind(q), t.args(args)...)
}

func (t *dbTx) Commit() error   { return t.tx.Commit() }
func (t *dbTx) Rollback() error { return t.tx.Rollback() }

func (t *dbTx) bind(q string) string {
	if !t.pg {
		return q
	}
	return rebind(q)
}

func (t *dbTx) args(a []any) []any { return boolArgs(t.pg, a) }

// boolArgs turns Go bools into the 0 and 1 this schema stores them as.
//
// There is no boolean column anywhere in it — the types are text, integer, real and blob, and
// booleans have always been written as 0 or 1, read back with boolInt and scanned into ints.
// SQLite went along with a Go bool arriving at one of those columns and stored it as 0 or 1
// itself. Postgres does not: pgx refuses with "unable to encode false into binary format for
// int8", which is correct of it and is how CreateAdminSession, and a hundred and forty-odd
// tests downstream of it, failed on the first real Postgres run.
//
// Doing it here rather than at the call sites is the same argument as rebind: one translation,
// in the one place that knows which dialect this is, so that no statement anywhere has to care.
func boolArgs(pg bool, a []any) []any {
	if !pg {
		return a
	}
	var out []any
	for i, v := range a {
		b, ok := v.(bool)
		if !ok {
			continue
		}
		if out == nil {
			out = make([]any, len(a))
			copy(out, a)
		}
		out[i] = int64(0)
		if b {
			out[i] = int64(1)
		}
	}
	if out == nil {
		return a
	}
	return out
}

// rebind turns `?` placeholders into `$1…$n`.
//
// The whole difficulty is that a `?` inside a string literal is a question mark, not a
// placeholder, and rewriting it corrupts the query silently — the statement still runs, against
// different data. This package has exactly one such literal today:
//
//	delete from memories where org_id=? and scope=? and text like ? escape '\'
//
// which is also the case that rules out a regular expression: `'\'` is a complete SQL string
// containing a backslash. SQL escapes a quote by doubling it (`”`), not with a backslash, so a
// scanner that treats `\` as an escape reads that literal as unterminated and everything after
// it as string — and the two real placeholders before it keep their `?`, while the one after is
// missed. Hence a scanner that knows only the three things SQL actually has: quoted strings,
// quoted identifiers, and comments.
func rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'' || c == '"':
			// A string ('…') or a quoted identifier ("…"). Either ends at the matching quote,
			// and a doubled quote inside is a literal one rather than the end.
			quote := c
			b.WriteByte(c)
			for i++; i < len(q); i++ {
				b.WriteByte(q[i])
				if q[i] == quote {
					if i+1 < len(q) && q[i+1] == quote {
						i++
						b.WriteByte(q[i])
						continue
					}
					break
				}
			}
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			// -- to end of line. The schema is full of these and they are prose.
			for ; i < len(q) && q[i] != '\n'; i++ {
				b.WriteByte(q[i])
			}
			if i < len(q) {
				b.WriteByte(q[i])
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			b.WriteString("/*")
			for i += 2; i < len(q); i++ {
				if q[i] == '*' && i+1 < len(q) && q[i+1] == '/' {
					b.WriteString("*/")
					i++
					break
				}
				b.WriteByte(q[i])
			}
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
