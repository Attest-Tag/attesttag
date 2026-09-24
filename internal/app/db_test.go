package app

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rebind rewrites the placeholders every statement in this package is written with. Getting it
// wrong is the worst kind of bug available here: a `?` inside a string literal that gets
// renumbered does not fail, it runs — against a query that no longer means what it says.
func TestRebind(t *testing.T) {
	cases := []struct{ in, want string }{
		// The ordinary shape, which is most of the package.
		{`select id from orgs where id=?`, `select id from orgs where id=$1`},
		{`insert into usage (org_id, cost_usd) values (?, ?)`,
			`insert into usage (org_id, cost_usd) values ($1, $2)`},
		{`update memberships set role=? where user_id=? and org_id=?`,
			`update memberships set role=$1 where user_id=$2 and org_id=$3`},
		// Numbering runs past nine, and $10 must not be read as $1 followed by a 0.
		{`values (?,?,?,?,?,?,?,?,?,?,?)`, `values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`},
		// Nothing to do.
		{`select count(*) from orgs`, `select count(*) from orgs`},
		{``, ``},

		// The literal that rules out a regular expression, and the reason this is a scanner.
		// `'\'` is a complete SQL string holding one backslash — SQL escapes a quote by
		// doubling it, not with a backslash — so a scanner that believes in backslash escapes
		// reads everything after it as string and renumbers nothing beyond.
		{`delete from memories where org_id=? and scope=? and text like ? escape '\'`,
			`delete from memories where org_id=$1 and scope=$2 and text like $3 escape '\'`},

		// A question mark inside a string is punctuation. Rewriting it changes the data the
		// statement matches, silently.
		{`select ? where note='really?'`, `select $1 where note='really?'`},
		{`select 'a?b', ?, 'c?d', ?`, `select 'a?b', $1, 'c?d', $2`},
		// A doubled quote is a quote, not the end of the string.
		{`select ? where s='it''s a ? here' and t=?`,
			`select $1 where s='it''s a ? here' and t=$2`},
		// Quoted identifiers follow the same rule.
		{`select "odd?name" from t where id=?`, `select "odd?name" from t where id=$1`},

		// Comments are prose. The schema is full of them.
		{"select ? -- why? because\nand x=?", "select $1 -- why? because\nand x=$2"},
		{`select ? /* is this ? a placeholder */ , ?`, `select $1 /* is this ? a placeholder */ , $2`},
		// A comment that runs to the end without a newline must not drop characters.
		{"select ? -- trailing?", "select $1 -- trailing?"},
		// An unterminated string is malformed SQL either way; it must not panic or lose text.
		{`select ? where s='unterminated ?`, `select $1 where s='unterminated ?`},
	}
	for _, c := range cases {
		if got := rebind(c.in); got != c.want {
			t.Errorf("rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// On SQLite the wrapper must hand the statement through untouched, character for character.
// Every statement in the package is written in SQLite's shape, so any rewriting here is a
// change in behaviour on the dialect that is in production today.
func TestSQLiteStatementsAreNotRewritten(t *testing.T) {
	sqlite := &database{pg: false}
	pg := &database{pg: true}
	for _, q := range []string{
		`select id from orgs where id=?`,
		`delete from memories where org_id=? and scope=? and text like ? escape '\'`,
		`insert into usage (org_id) values (?) on conflict do nothing`,
	} {
		if got := sqlite.bind(q); got != q {
			t.Errorf("SQLite rewrote a statement:\n got %q\nwant %q", got, q)
		}
		if strings.Contains(q, "?") {
			if got := pg.bind(q); got == q {
				t.Errorf("Postgres left the placeholders alone: %q", q)
			}
		}
	}
}

// Every `?` becomes exactly one placeholder and they are numbered 1..n with no gaps — which is
// what database/sql is about to match against the argument list. An off-by-one here surfaces as
// "expected N arguments, got M" at runtime, on one statement, in production.
func TestRebindNumbersEveryArgumentOnce(t *testing.T) {
	for _, q := range []string{
		`insert into t (a,b,c,d,e) values (?,?,?,?,?)`,
		`select ? where s='a?b' and t=? and u='c?d' and v=?`,
		`update t set a=?, b=? where c=? and d like ? escape '\'`,
	} {
		got := rebind(q)
		want := strings.Count(q, "?") - strings.Count(got, "?") // placeholders consumed
		n := 0
		for i := 1; ; i++ {
			if !strings.Contains(got, "$"+strconv.Itoa(i)) {
				break
			}
			n++
		}
		if n != want {
			t.Errorf("rebind(%q) = %q: %d placeholders numbered, %d consumed", q, got, n, want)
		}
	}
}

// BumpConfigVersion is a cache key, not a counter: the resolver and the proxy hold it and
// refetch when it differs. It stopped being "the old value plus one" because
// cast(value as integer) is a silent 0 in SQLite and an error in Postgres, and that difference
// sat underneath the cache that decides which credentials a channel may reach.
func TestConfigVersionChangesAndParses(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const orgID = int64(1)

	read := func() (string, int64) {
		t.Helper()
		var v string
		if err := st.db.QueryRowContext(ctx,
			`select value from settings where org_id=? and key='config_version'`, orgID).Scan(&v); err != nil {
			t.Fatalf("no config_version row: %v", err)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("config_version %q does not parse as the int64 the settings cache reads: %v", v, err)
		}
		return v, n
	}

	st.BumpConfigVersion(ctx, orgID)
	first, n1 := read()
	if n1 <= 0 {
		t.Errorf("first version = %d, want something positive", n1)
	}
	// Bumping again must produce a different value — that is the entire contract.
	time.Sleep(time.Millisecond)
	st.BumpConfigVersion(ctx, orgID)
	second, n2 := read()
	if second == first {
		t.Errorf("two bumps produced the same version %q; nothing downstream would refetch", first)
	}
	if n2 <= n1 {
		t.Errorf("version went backwards: %d then %d", n1, n2)
	}
	// And it upserts rather than inserting a second row for the same key.
	var rows int
	st.db.QueryRowContext(ctx, `select count(*) from settings where org_id=? and key='config_version'`, orgID).Scan(&rows)
	if rows != 1 {
		t.Errorf("config_version has %d rows, want 1", rows)
	}
}
