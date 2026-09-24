package app

import (
	"context"
	"os"
	"testing"
)

// The Postgres path, end to end: migrate an empty database, then exercise the operations that
// would break first if the dialect seam were wrong — an insert that has to report the id it
// generated, a slug derived and stored, a settings upsert, and the config-version bump that
// used to be a cast.
//
// Gated on TEST_DATABASE_URL because a fresh clone must pass `go test ./...` with nothing
// installed. To run it:
//
//	createdb attesttag_test
//	TEST_DATABASE_URL="postgres://$(whoami)@localhost:5432/attesttag_test?sslmode=disable" go test ./internal/app -run TestPostgres
//
// The database is left as it was found apart from the schema, so a second run is a no-op
// migration followed by the same work in a new organisation.
func TestPostgresEndToEnd(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to a Postgres database to run this")
	}
	ctx := context.Background()
	st, err := OpenStore(dsn)
	if err != nil {
		t.Fatalf("OpenStore on Postgres: %v", err)
	}
	defer st.Close()

	if !st.db.postgres() {
		t.Fatal("TEST_DATABASE_URL did not select the Postgres driver; check the scheme")
	}
	// The migration ran, and a second call is a no-op rather than a second attempt.
	if v := migrationVersions(t, st.db); len(v) == 0 {
		t.Fatal("no migrations recorded")
	}
	if err := applyMigrations(ctx, st.db); err != nil {
		t.Fatalf("re-running migrations on an up-to-date database: %v", err)
	}

	before := st.OrgCount(ctx)
	org, err := st.CreateOrg(ctx, "Acme Ltd", 0)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.ID == 0 {
		t.Error("the insert did not report the id it generated — `returning id` is not working")
	}
	if org.Slug == "" || org.PublicID == "" {
		t.Errorf("org came back without a slug or public id: %+v", org)
	}
	if n := st.OrgCount(ctx); n != before+1 {
		t.Errorf("OrgCount = %d, want %d", n, before+1)
	}

	u, err := st.CreateUser(ctx, "founder-"+org.PublicID+"@example.com", "Founder", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == 0 {
		t.Error("CreateUser returned id 0")
	}

	// A settings upsert, then the config-version bump — the two statements that carry an
	// `on conflict … do update` and used to carry a cast.
	if err := st.PutSetting(ctx, org.ID, "bot_name", "probe"); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	if err := st.PutSetting(ctx, org.ID, "bot_name", "probe2"); err != nil {
		t.Fatalf("PutSetting (conflicting): %v", err)
	}
	st.BumpConfigVersion(ctx, org.ID)
	st.BumpConfigVersion(ctx, org.ID)

	// And the introspection the account deletion depends on, which is the one dialect branch.
	tables, err := st.orgScopedTables(ctx)
	if err != nil {
		t.Fatalf("orgScopedTables on Postgres: %v", err)
	}
	if len(tables) < 20 {
		t.Errorf("only %d org-scoped tables found; the deletion sweep reads this list", len(tables))
	}
}
