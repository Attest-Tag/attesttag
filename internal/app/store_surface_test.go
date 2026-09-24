package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// SQL lint aid only: an org/team identifier in a literal does not prove isolation.
// Adversarial multi-tenant endpoint, cache, retention and file tests are required.

// Tables whose rows belong to one organisation. A query touching one of these has to say which.
var perOrgTables = []string{
	"orgs", "scopes", "bundles", "connections", "domains", "skills", "documents", "doc_chunks",
	"document_folders", "settings", "setup_links", "console_roles", "console_invites",
	"approval_roles", "approval_members", "approval_role_bundles", "approval_role_connections",
	"scope_bundles", "scope_connections", "alerts_sent", "memories", "routines", "routine_runs", "usage",
	"tool_calls", "proxy_audit", "artifacts", "access_requests", "access_request_cards",
	"assistant_turns",
	"pending_writes", "jobs", "job_events", "job_files", "sessions", "turns", "file_texts",
	"teams", "memberships", "api_keys", "investigations", "slack_deliveries",
	// Added late, and the reason they are named here rather than left out: user_connections
	// holds one person's own Google credential and web_keys holds an organisation's paid search
	// key, which makes them the last two tables in the database that should be reachable
	// without saying whose they are. This list is the whole of what the check below looks at, so
	// a table missing from it is a table nothing is checking.
	"user_connections", "web_keys", "connect_states",
	// personal_memories is per-organisation like the rest, and per-person on top of that --
	// see perUserTables below, which is the half of its isolation this list cannot express.
	"personal_memories",
	// Who used the bot, per person per day. Per organisation like the rest, and the one table
	// whose rows decide a figure on an invoice, so a query that forgot whose they were would
	// bill one customer for another's people.
	"active_users",
	// Money. An organisation's subscription and the statement of what it has paid and spent: the
	// two tables where a query that forgot to say whose rows it wanted would hand one customer
	// another customer's balance. billing_events is deliberately not here -- it holds Stripe
	// event ids and no tenant content, and an event arrives before anyone knows whose it is.
	"billing_accounts", "credit_ledger",
	// An enterprise deal: what one account was sold and what it pays, and the operator's note
	// about it. A statement that forgot whose deal it read would show one customer another's price.
	"enterprise_terms",
	// A Drive sync names a folder in somebody's Drive and the credential it is spent on, and
	// drive_sync_files says which documents it may delete — so both are per-organisation for
	// the same reason documents is.
	"drive_syncs", "drive_sync_files",
	// github_installs binds a GitHub App installation to one organisation, and that binding is
	// the only thing standing between an installation id — which is not secret, it rides in a
	// query string — and somebody else's repositories. Per-organisation, emphatically.
	"github_installs",
	// audit_log is who did what in one organisation, addresses included. Readable by its own
	// admins and nobody else, which the predicate is the whole of.
	"audit_log",
	// A Teams tenant's conversations as the bot saw them, and the people in it. Keyed by the
	// tenant's team_id like every other chat table, so a statement that forgot it would read one
	// customer's channel into another customer's thread.
	"msteams_messages", "msteams_users",
	// sso_providers holds one organisation's IdP registration and the domain it claims. The
	// claim is what routes a sign-in, so a row reachable without saying whose it is would be a
	// way to read, or to take over, somebody else's front door.
	"sso_providers",
	// org_model_keys is where one organisation's model calls go and the key they are paid with. A
	// statement that forgot whose row it read would send one customer's conversations to another
	// customer's endpoint, on the other customer's bill.
	"org_model_keys",
	// An MCP client somebody connected: it acts as that person in that organisation, exactly as a
	// developer key does, so it is per-organisation for the same reason api_keys is.
	"mcp_grants",
}

// Predicates that narrow a statement to one organisation. team_id counts because a Slack
// workspace belongs to exactly one organisation (teams.team_id is the primary key), so a
// team-keyed row cannot be reached from another.
var orgPredicates = []string{"org_id", "team_id"}

// Tables whose rows belong to one person inside one organisation, where an org-wide predicate is
// not enough. org_id and team_id both pass the check above, so a personal note narrowed only to
// its workspace would look scoped while returning everybody in it -- which is the whole failure
// the table exists to prevent. These need the owner named as well.
var perUserTables = []string{"personal_memories"}

// The column that says whose row it is. user_connections uses slack_user_id; personal_memories
// uses owner.
var userPredicates = []string{"owner", "slack_user_id", "user_id"}

// Statements that are legitimately global, each with the reason it is safe. Anything not on
// this list and not carrying a predicate is a failure.
var globalQueries = map[string]string{
	// Identity is global by design: one person, one account, however many organisations.
	"insert into users":                                          "a user is not owned by an organisation",
	"select * from users":                                        "a user is not owned by an organisation",
	"update users set":                                           "the account's own fields, keyed by user id",
	"insert into user_identities":                                "an identity belongs to a user, not an organisation",
	"select user_id from user_identities":                        "the sign-in lookup, keyed by provider and subject",
	"delete from user_identities":                                "keyed by user id",
	"select provider, subject":                                   "one user's own sign-in methods",
	"insert into memberships":                                    "the row that creates the link, keyed by both ids",
	"delete from memberships":                                    "keyed by both ids",
	"update memberships set":                                     "keyed by both ids",
	"select m.user_id":                                           "joins memberships to orgs on ids the caller owns",
	"select count(*) from memberships where user_id=?":           "after an organisation is deleted: has this account anywhere left to be?",
	"from github_installs where installation_id=?":               "deliberately org-less: the install flow has to tell \"not ours\" from \"does not exist\" before it can refuse either. The caller compares org_id itself (handleGitHubSetup), and the console's own listing is scoped.",
	"select org_id from billing_accounts\n\t\t  where status in": "the hourly allowance sync's enumeration, deliberately deployment-wide and exactly like the roll-up's below: it asks which accounts might have an allowance out of step with the size and period they are on, then works on each one by id. It runs behind the leader lease, never on a request.",
	"update billing_accounts set allowance_micros=0":             "the allowance expiry sweep. An age comparison over every row, like the other sweeps, and deliberately not per-organisation: narrowing it would mean enumerating every account to do the same thing once each. It reveals nothing and decides nothing -- every read already treats a lapsed allowance as spent (BillingAccount.AllowanceLive), so this only stops a stored row from claiming credit the gate would refuse.",
	"select org_id from billing_accounts where lifetime_debit":   "the hourly roll-up's enumeration, deliberately deployment-wide: it asks which organisations have spent since their statement was last written, and then works on each one by id. It runs behind the leader lease, never on a request. (It happens to satisfy the predicate check by selecting org_id, so this entry is the reason written down rather than the thing permitting it.)",
	"from billing_accounts where customer_id=?":                  "deliberately org-less: an invoice or subscription webhook carries a Stripe customer and nothing of ours, so this lookup is HOW the organisation is found. The row it returns names it and everything after it is scoped by that (billing.go); a session naming a different organisation than the customer is bound to is refused there.",
	"from credit_ledger where external_id=? and kind=?":          "deliberately org-less, for the same reason as the customer lookup above: a refund or a dispute arrives carrying a payment intent and — for a dispute — no customer and none of our metadata, so the top-up that payment became is HOW its organisation is found. external_id is unique across the ledger, so it names one row, and what follows is scoped by the organisation that row names (billing.go, applyChargeEvent).",
	"insert into orgs":                                           "creating one",
	// Single sign-on routes on a domain and on a provider id, each of which names exactly one
	// organisation. Asking for the org first would mean already knowing what these answer.
	"from sso_providers where domain=? and domain_verified=1": "the sign-in lookup: the domain of the address typed is what says which organisation it belongs to",
	"from sso_providers where provider_id=?":                  "the callback, keyed by the provider id in its own URL; the row it returns names the organisation",
	"select client_secret_enc from sso_providers where id=?":  "the token exchange, keyed by the row the callback already resolved",
	"select count(*) from orgs":                               "the signup gate asks whether any exist",
	// Bearer tokens: the token is the authorisation, so the row is found by it alone.
	"insert into admin_sessions":                                   "a session is created for a known user",
	"select coalesce(a.id,0)":                                      "a session is found by its opaque token",
	"delete from admin_sessions":                                   "keyed by token or by user id",
	"update admin_sessions set":                                    "keyed by token",
	"insert into email_tokens":                                     "a one-time link is created for a known user or address",
	"update email_tokens set":                                      "keyed by the token hash, or by user and kind",
	"delete from email_tokens":                                     "the expiry sweep",
	"from api_keys where key_hash=?":                               "a developer key is found by the hash of the key itself; the row it returns names the organisation",
	"update api_keys set last_used_at=? where id=?":                "the key just authenticated; its own row is the subject",
	"update api_keys set revoked_at=? where user_id=? and revoked_at is null": "a password reset or change locks the person out everywhere, keyed by the person like DeleteSessionsFor",
	"update mcp_grants set revoked_at=? where user_id=? and revoked_at=''":    "the same reset, ending the MCP clients the person connected as it ends their keys",
	// An MCP token is found the way a developer key is: by the hash of the token itself, because
	// the token is the authority. The row each returns names the organisation.
	"from mcp_grants where access_hash=?":                          "an MCP access token is found by its hash; the row names the organisation",
	"from mcp_grants where refresh_hash=?":                         "a refresh token is found by its hash, the same way",
	"from mcp_grants where prev_refresh_hash=?":                    "a refresh token already spent, found by its hash so its replay can end the grant",
	"insert into oauth_states":                                     "install state, keyed by an unguessable token",
	"select coalesce(created_by,''), expires_at from oauth_states": "found by its token",
	"delete from oauth_states":                                     "single use, and the expiry sweep",
	// An organisation keyed by its own id, and the sweep that enumerates every one.
	"update orgs set name=?": "the row is the organisation; its id is the predicate",
	"update orgs set plan=?": "the operator moves one organisation between plans, keyed by its id (operator.go)",
	"select id from orgs":    "the schedulers walk every organisation in turn",
	"from orgs order by id":  "Orgs: the operator's listing of every tenant, behind OPERATOR_SECRET (operator.go)",
	// Shared infrastructure, deliberately counted and swept across the whole deployment. Each
	// row these touch still carries its organisation, so what happens next is scoped.
	"select count(*) from jobs where status in": "the worker pool is one shared resource, not one tenant's",
	"from jobs where status in":                 "ActiveJobs: the reconciler is one sweep for the whole deployment",
	"from jobs where id=?":                      "jobByID: the worker's per-job token is the authority, and the row names the org",
	"from teams order by":                       "allTeams: the schedulers and the socket loop serve every tenant",
	"from routines order by id":                 "DueRoutines: one scheduler fires every tenant's routines, and each row carries the org everything after it is scoped by",
	"from orgs where id=?":                      "the row is the organisation",
	"from orgs where slug=?":                    "the slug identifies one organisation",
	"from orgs where public_id=?":               "the public id identifies one organisation, and is the only name for it outside the process",
	"from users where public_id=?":              "the public id identifies one account, and callers still have to prove membership before acting on it",
	"update jobs set token_hash='', llm_key_enc=null where token_hash<>'' and status in": "an expired credential expires for everyone",
	// The digging lane is one sweep for the whole deployment, like the worker pool above it: the
	// claim picks the oldest queued row from any tenant, and every statement after it is keyed by
	// the id that claim returned. None of these ids comes from a request — the console's own
	// reads (CountOpenInvestigations, OpenInvestigationsInThread, StopQueuedInvestigations) are
	// scoped, and they are the only ones a person can reach.
	"update investigations set status='running'":  "the claim: one lane serves every tenant, and the row it takes names the org",
	"from investigations where status in":         "the sweep for leases that expired while a container was being replaced",
	"update investigations set lease_until=?":     "the heartbeat of a run already claimed, keyed by its own id",
	"update investigations set status=?, error=?": "finishing the run this process claimed, keyed by its own id",
	"update investigations set status='queued'":   "handing a claimed run back, keyed by its own id",
	// The Slack inbox is one queue for the whole deployment, and delivery_key embeds the
	// workspace ("event:<team>:<id>") exactly as seen_events does. The row carries org_id, which
	// is what everything downstream of a claim is scoped by.
	"from slack_deliveries where delivery_key=?": "the dedup check; the key embeds the workspace",
	"update slack_deliveries set":                "one delivery, keyed by a key that embeds its workspace",
	"delete from slack_deliveries where done_at": "the GC for finished deliveries",
	"delete from slack_deliveries where dead_at": "the GC for dead-lettered deliveries",
	// A sign-in that was started and never finished, swept by age. The row is found by its
	// single-use state token everywhere else.
	"delete from connect_states where expires_at": "the expiry sweep for unfinished personal sign-ins",
	// Platform-wide bookkeeping.
	"delete from seen_events":                              "the event-dedup GC; keys already embed the workspace",
	"delete from active_users":                             "this table's own retention ceiling, deliberately deployment-wide: it is an age sweep and not a read, it runs behind the leader lease in the hourly billing loop and never on a request, and narrowing it to one organisation would mean enumerating every organisation to do the same thing once each. Note which sweep this is NOT -- active_users is absent from retentionTables on purpose, because a tenant's own data_retention_days must not be able to shrink the figure their size is counted from (retention.go).",
	"insert into seen_events":                              "the key embeds the workspace",
	"select 1 from seen_events":                            "the key embeds the workspace",
	"select value from settings where key='public_origin'": "the console's own origin, one per deployment",
	// A schema migration runs once over the whole file, before any request has an organisation
	// to be scoped to. Each of these retires the old "respond automatically" setting.
	"update scopes set read_all='on' where auto_respond='on'": "the one-time fold in foldAutoRespondIntoReadAll, applied to every tenant's existing choice",
	"delete from settings where key='unprompted_replies'":     "the same fold, retiring the global default it just carried over",
	"update scopes set auto_respond='' where":                 "the same fold, blanking what it read so it never runs twice",
	// The same shape for approval tiers: the single bundle_id column becomes rows in
	// approval_role_bundles (that insert names org_id), and the column is blanked so a bundle an
	// admin later takes off the tier is not put back at the next start.
	"update approval_roles set bundle_id=0": "the one-time fold in foldRoleBundleIntoGrants, blanking what it read so it never runs twice",
	// Public ids are given to rows that predate the column, at startup, before any request has
	// an organisation to be scoped to. Both statements name one row by its primary key.
	"where public_id = '' or public_id is null": "backfillPublicIDs: the one-time fill, over every row that has no public id yet",
	"set public_id=? where id=?":                "the same fill, one row at a time, keyed by that row's own id",
}

var (
	// Raw string literals are also used for prompts, HTML templates and help text, so a
	// statement is recognised by starting with a SQL verb rather than merely containing one.
	sqlLiteral = regexp.MustCompile("(?s)`([^`]*)`")
	// "from" is in the list because a column list is often a separate const — `select `+cols+`
	// from teams where …` reaches here as two literals, and the half naming the table starts
	// with "from". Without it that whole shape is invisible to this check.
	sqlStart  = regexp.MustCompile(`^(select|insert(\s+or\s+(ignore|replace))?|update|delete|with|from)\s`)
	wordSplit = regexp.MustCompile(`[^a-z_]+`)
)

func TestEveryPerOrgQueryIsScoped(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range sqlLiteral.FindAllStringSubmatch(string(src), -1) {
			q := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
			if !sqlStart.MatchString(q) || !touchesPerOrgTable(q) || hasOrgPredicate(q) || isAllowedGlobal(q) {
				continue
			}
			offenders = append(offenders, f+": "+trimQuery(q))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("%d statement(s) touch per-organisation data without narrowing to one.\n"+
			"Add the predicate, or add the statement to globalQueries with the reason it is safe:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func touchesPerOrgTable(q string) bool {
	words := map[string]bool{}
	for _, w := range wordSplit.Split(q, -1) {
		words[w] = true
	}
	for _, tbl := range perOrgTables {
		if words[tbl] {
			return true
		}
	}
	return false
}

func touchesPerUserTable(q string) bool {
	words := map[string]bool{}
	for _, w := range wordSplit.Split(q, -1) {
		words[w] = true
	}
	for _, tbl := range perUserTables {
		if words[tbl] {
			return true
		}
	}
	return false
}
func hasUserPredicate(q string) bool {
	for _, p := range userPredicates {
		if strings.Contains(q, p) {
			return true
		}
	}
	return false
}
func hasOrgPredicate(q string) bool {
	for _, p := range orgPredicates {
		if strings.Contains(q, p) {
			return true
		}
	}
	return false
}

func isAllowedGlobal(q string) bool {
	for prefix := range globalQueries {
		if strings.Contains(q, prefix) {
			return true
		}
	}
	return false
}

func trimQuery(q string) string {
	if len(q) > 150 {
		return q[:150] + "…"
	}
	return q
}

// Tables that belong to nobody in particular, with the reason each one is not a tenant's. A
// table is on this list or on perOrgTables; the test below refuses a third option, because the
// third option is what happened — user_connections, web_keys, investigations, document_folders
// and slack_deliveries were all added to the schema and to none of these lists, and the check
// above then walked straight past every statement touching them.
var globalTables = map[string]string{
	"users":               "one person, one account, however many organisations",
	"user_identities":     "an identity belongs to a user, not an organisation",
	"user_totp":           "a second factor is the account's, not the organisation's",
	"user_recovery_codes": "the same: recovery is for the account",
	"admin_sessions":      "a session is found by its token; the row it returns names the organisation",
	"email_tokens":        "a one-time link is found by its hash and names what it is for",
	"oauth_states":        "install state, found by an unguessable token, single use",
	"login_challenges":    "a half-finished sign-in, found by its own token",
	"seen_events":         "event dedup; the key embeds the workspace",
	"console_users":       "superseded by users/memberships; no statement touches it",
	"console_invites":     "superseded by email_tokens; no statement touches it",
	"schema_migrations":   "the database's own version; it belongs to the deployment, not a tenant",
	"leader_leases":       "which instance runs the deployment's singleton loops; one row per role, no tenant data",
	"throttle_events":     "rate-limit counters keyed by IP or address — the thing being limited is a stranger who has no organisation yet",
	"oauth_pendings":      "a half-finished sign-in, found by an unguessable single-use state, exactly like oauth_states above",
	"link_codes":          "a one-time code, found by its hash like email_tokens, that names the organisation a chat tenant is joining; single use and short-lived",
	"mcp_clients":         "an MCP client registered itself (RFC 7591) before anybody signed in, and one client is used by people in many organisations; it holds a name and its callback addresses, no tenant data and no secret",
	"mcp_codes":           "an authorization code, found by its hash like email_tokens, that names the organisation and person it was approved for; single use and ten minutes long",
	"billing_events":      "Stripe webhook dedup keys, found by the event id Stripe made unique. An event arrives before anyone knows which organisation it concerns — resolving that is what the handler does next — so a column saying whose it is could only be written after the row it is meant to protect. Holds no tenant content, swept by its own age.",
}

// TestEveryTableIsClassified is the guard on the guard. Adding a table is a decision about whose
// rows it holds, and this is where that decision is written down — so the answer cannot be given
// by forgetting.
func TestEveryTableIsClassified(t *testing.T) {
	// The schema is in migrations/ now rather than in a Go constant, and it is read from the
	// SQLite set — the Postgres one is its twin and TestSchemasMatch is what holds them
	// together. `if not exists` is optional because the baseline does not use it: a migration
	// that has already run is skipped by version, so a half-applied one should be loud.
	files, err := filepath.Glob(filepath.Join("migrations", "sqlite", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	create := regexp.MustCompile(`(?i)create table (?:if not exists )?([a-z_]+)`)
	seen := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range create.FindAllStringSubmatch(string(src), -1) {
			seen[strings.ToLower(m[1])] = true
		}
	}
	if len(seen) < 20 {
		t.Fatalf("only found %d tables; the schema is not being read", len(seen))
	}
	for table := range seen {
		if slices.Contains(perOrgTables, table) || globalTables[table] != "" {
			continue
		}
		t.Errorf("table %q is in neither perOrgTables nor globalTables: say which it is, "+
			"or every statement touching it goes unchecked", table)
	}
	for _, table := range perOrgTables {
		if !seen[table] {
			t.Errorf("perOrgTables names %q, which no longer exists in the schema", table)
		}
	}
}

// TestEveryPerUserQueryNamesItsPerson is the companion to TestEveryPerOrgQueryIsScoped, and it
// exists because that one cannot see this mistake: orgPredicates contains team_id, so a statement
// reading personal_memories by workspace alone passes it while returning every person in that
// workspace. A note that one person wrote and nobody else may read is narrowed by three things or
// it is not narrowed at all.
func TestEveryPerUserQueryNamesItsPerson(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range sqlLiteral.FindAllStringSubmatch(string(src), -1) {
			q := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
			if !sqlStart.MatchString(q) || !touchesPerUserTable(q) {
				continue
			}
			// The one shape that may name a table without naming a person: dropping a whole
			// workspace's rows when the workspace itself is gone. It still carries org and team.
			if strings.HasPrefix(q, "delete from personal_memories where org_id=? and team_id=?") {
				continue
			}
			if hasOrgPredicate(q) && hasUserPredicate(q) {
				continue
			}
			offenders = append(offenders, f+": "+trimQuery(q))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("%d statement(s) read one person's rows without saying which person.\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

// dialectOnly is what must not appear in a SQL literal anywhere in this package: spellings that
// one of the two dialects understands and the other does not.
//
// This is what makes supporting both affordable. Without it the cost of a second dialect is
// paid forever, in production, one statement at a time — somebody writes `ilike` or
// `datetime('now')`, it works on their machine, and it fails on the other database weeks later
// in front of somebody else. With it the cost is paid once, here, by whoever wrote the line.
//
// Every one of these was found in this codebase, and the fix for each is in the commit that
// removed it: the clock comes from the process, the conflict clauses are ANSI, an insert says
// `returning id`, and the two questions a database can only answer about itself live in
// db_dialect.go — the one file this test exempts.
var dialectOnly = []string{
	"pragma", "sqlite_master", "sqlite_version", "autoincrement", "last_insert_rowid",
	"insert or ", "update or ", "replace into", "rowid",
	"datetime(", "strftime(", "julianday(", "ifnull(", "group_concat(",
	"ilike", "nextval(", "greatest(", "generated by default", "skip locked", "jsonb", "::text",
}

// TestNoInsertReadsItsIdFromTheDriver is the same rule as dialectOnly's "last_insert_rowid",
// one level up.
//
// That list scans SQL text, so it catches the SQLite function by name and cannot see
// `res.LastInsertId()` — a Go call, on a result the driver returns, that means exactly the same
// thing and is exactly as portable. The pgx driver does not implement it: it answers with an
// error, and the idiom for reading past one is `id, _ := res.LastInsertId()`, which turns an
// unsupported operation into a silent zero. LogAudit did that from the day the audit log was
// written until this test was added, on the one insert in the package that had not been
// converted to `returning id` when the rest were.
//
// Every insert says `returning id`. A driver is not asked what it just did.
func TestNoInsertReadsItsIdFromTheDriver(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// This test's own prose names it; the check is about calls, so require the parenthesis
		// and skip the file that is arguing for the rule.
		if filepath.Base(f) == "store_surface_test.go" {
			continue
		}
		if strings.Contains(string(src), "LastInsertId(") {
			t.Errorf("%s calls LastInsertId, which the Postgres driver does not implement and "+
				"which the usual `id, _ :=` turns into a silent 0. Write `returning id` on the "+
				"insert and Scan it, the way every other insert in this package does.", filepath.Base(f))
		}
	}
}

func TestNoDialectSpecificSQL(t *testing.T) {
	// db_dialect.go answers the questions a database can only be asked in its own words.
	// legacy_schema_test.go is the pre-migrations schema, kept verbatim so the upgrade path can
	// be tested against it — it is SQLite's by definition and is never run on Postgres.
	exempt := map[string]bool{"db_dialect.go": true, "legacy_schema_test.go": true}

	// Test files are scanned too, and not as an afterthought: two of the statements this rule
	// was written for were in them. A test that writes SQLite-only SQL and ignores the error
	// passes on Postgres by doing nothing — which is how an expired access request came to be
	// approved, and how a five-minute hold came to be confirmable ten minutes later. Both were
	// found by running the suite on Postgres, not by reading it.
	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, f := range files {
		base := filepath.Base(f)
		if exempt[base] {
			continue
		}
		for _, lit := range goStringLiterals(t, f) {
			low := strings.ToLower(lit)
			// Only judge things that are actually SQL, and only outside their comments. The
			// length floor is what stops this rule reporting its own deny list: "update or "
			// is a fragment that contains a SQL keyword and is not a statement.
			if len(lit) < 24 || !looksLikeSQL(low) {
				continue
			}
			body := stripSQLComments(low)
			for _, bad := range dialectOnly {
				if strings.Contains(body, bad) {
					found = append(found, fmt.Sprintf("%s: %q in %.70s…", base, bad, strings.Join(strings.Fields(lit), " ")))
					break
				}
			}
		}
	}
	if len(found) > 0 {
		t.Errorf("%d statement(s) written in one dialect's words:\n  %s\n\n"+
			"Write it in the form both accept, or — if the database can only be asked in its own "+
			"words — put it in db_dialect.go, which this test exempts.",
			len(found), strings.Join(found, "\n  "))
	}
}

func looksLikeSQL(low string) bool {
	for _, k := range []string{"select ", "insert into", "update ", "delete from", "create table", "create index", "create unique"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// goStringLiterals returns the string literals in a file, parsed rather than matched. A regular
// expression over the source cannot tell a backtick that opens a SQL literal from one inside a
// comment — and this file's own comments quote SQL constantly, which is how the first version
// of the dialect guard reported a sentence explaining `update or replace` as an offence.
func goStringLiterals(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0) // 0: comments not attached
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}
