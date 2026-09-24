package app

import "context"

// The only file in this package allowed to contain SQL that differs by dialect.
//
// Everything else is written once, with `?`, in a form both accept — see db.go. What cannot be
// written once is asking the database about itself: SQLite keeps its catalogue in
// sqlite_master, Postgres in information_schema, and there is no spelling that reaches both.
// Two statements, in one place, is the cost of that; TestNoDialectSpecificSQL allows them here
// and nowhere else.

// tableExists answers whether a table is present, without touching it.
//
// The obvious implementation — select from it and see whether that errors — works on SQLite and
// is a trap on Postgres, where a failed statement aborts the surrounding transaction and every
// statement after it fails with "current transaction is aborted". The migration runner asks
// this question inside the transaction that then creates the schema, so the obvious
// implementation broke the first Postgres boot at the first table.
func tableExists(ctx context.Context, tx *dbTx, table string) bool {
	q := `select 1 from sqlite_master where type='table' and name=?`
	if tx.pg {
		q = `select 1 from information_schema.tables where table_schema=current_schema() and table_name=?`
	}
	var one int
	return tx.QueryRowContext(ctx, q, table).Scan(&one) == nil
}

// orgScopedTablesQuery lists the tables carrying an org_id, which is how a deletion finds every
// table it has to sweep rather than trusting a list in a file to have been kept up to date.
func orgScopedTablesQuery(pg bool) string {
	if pg {
		return `select table_name from information_schema.columns
			where table_schema=current_schema() and column_name='org_id' order by table_name`
	}
	return `select m.name from sqlite_master m
		join pragma_table_info(m.name) p on p.name='org_id'
		where m.type='table' order by m.name`
}

// teamScopedTablesQuery is the same question about one connected Slack workspace: which tables
// carry a team_id, and so hold rows that go when the workspace does. Asked of the schema for
// the reason above — a list in a file is a list somebody adds a table to without noticing.
func teamScopedTablesQuery(pg bool) string {
	if pg {
		return `select table_name from information_schema.columns
			where table_schema=current_schema() and column_name='team_id' order by table_name`
	}
	return `select m.name from sqlite_master m
		join pragma_table_info(m.name) p on p.name='team_id'
		where m.type='table' order by m.name`
}

// everyColumnQuery names every table's every column. TestSchemasMatchAcrossDialects asks it of
// a database built by each dialect's own migrations and compares the two answers, which is what
// stands between the schemas and the kind of drift that put a column in one of them and not the
// other — connections.secret_fp, which Postgres never received and which took every read of the
// connections table down there. Here for the reason the rest of this file is: asking a database
// about itself is the one question with no spelling that reaches both.
func everyColumnQuery(pg bool) string {
	if pg {
		return `select table_name, column_name from information_schema.columns
			where table_schema=current_schema()`
	}
	return `select m.name, p.name from sqlite_master m
		join pragma_table_info(m.name) p where m.type='table'`
}

// requiredColumnsQuery describes one table's columns: the name, the type, whether a value has to
// be supplied, and whether the engine supplies one of its own (a default, or an identity). Like
// allTablesQuery it is here for a test — the workspace-removal test seeds a row into every table
// a workspace can own, and builds each insert from this, which is how it covers a table added
// long after it was written.
func requiredColumnsQuery(pg bool) string {
	if pg {
		return `select column_name, data_type,
			case when is_nullable='NO' then 1 else 0 end,
			case when column_default is not null or is_identity='YES' then 1 else 0 end
			from information_schema.columns where table_schema=current_schema() and table_name=?`
	}
	return `select p.name, p.type, p."notnull", case when p.dflt_value is null then 0 else 1 end
		from sqlite_master m join pragma_table_info(m.name) p where m.name=?`
}

// emailTokenIDCol is the surrogate key an invitation is listed and revoked by. SQLite has given
// every row a rowid since forever, and the databases already in existence rely on it; Postgres
// has no such thing, so its baseline carries a real identity column instead. One name, chosen
// here, rather than two spellings of the same query.
func emailTokenIDCol(pg bool) string {
	if pg {
		return "id"
	}
	return "rowid"
}

// allTablesQuery lists the deployment's own tables, skipping whatever the engine keeps for
// itself. Used by the test that refuses a table no account deletion would ever reach, and by
// pg-import to decide what to carry across.
//
// _litestream_lock and _litestream_seq belong to the replication, not to this deployment, and
// they exist in exactly the database that matters most: a snapshot restored from the production
// replica has them, an unreplicated development copy does not. Importing them is not merely
// untidy — the Postgres schema has no such tables, so the copy fails partway with
// `relation "_litestream_lock" does not exist`, which reads like a broken migration and is not
// one. The underscore is LIKE's own single-character wildcard, hence the escape.
func allTablesQuery(pg bool) string {
	if pg {
		return `select table_name from information_schema.tables
			where table_schema=current_schema() and table_type='BASE TABLE' order by table_name`
	}
	return `select name from sqlite_master where type='table' and name not like 'sqlite_%'
		and name not like '\_litestream\_%' escape '\' order by name`
}
