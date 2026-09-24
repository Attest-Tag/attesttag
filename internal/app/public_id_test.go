package app

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// A new organisation and a new person each get a public id, and no two are alike. This is the
// whole point of the column: the serial says how many accounts came before you, so nothing
// outside the process is allowed to see it.
func TestPublicIDsAreOpaqueAndUnique(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seen := map[string]string{}
	for i := 0; i < 20; i++ {
		u, err := st.CreateUser(ctx, strings.Repeat("a", i+1)+"@example.com", "Someone", "")
		if err != nil {
			t.Fatal(err)
		}
		o, err := st.CreateOrg(ctx, "Org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		for what, id := range map[string]string{"user": u.PublicID, "org": o.PublicID} {
			if !hex32.MatchString(id) {
				t.Fatalf("%s public id %q is not 32 hex characters", what, id)
			}
			if prev, dup := seen[id]; dup {
				t.Fatalf("%s reused the public id already given to %s", what, prev)
			}
			seen[id] = what
		}
		// And it is not the row number wearing a disguise: neighbouring rows must not be
		// adjacent, orderable, or derivable from one another.
		if strings.Contains(o.PublicID, u.PublicID) || o.PublicID == u.PublicID {
			t.Fatal("an organisation's id is a function of its founder's")
		}
	}
}

// Both ids are round-trippable, and an id that names nobody reads as nobody rather than as the
// first row — the failure mode a "default 1" would have produced.
func TestLookupByPublicID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	u, _ := st.CreateUser(ctx, "founder@example.com", "Founder", "")
	o, _ := st.CreateOrg(ctx, "Acme", u.ID)

	if got, _ := st.OrgByPublicID(ctx, o.PublicID); got == nil || got.ID != o.ID {
		t.Fatalf("org by public id = %v", got)
	}
	if got, _ := st.UserByPublicID(ctx, u.PublicID); got == nil || got.ID != u.ID {
		t.Fatalf("user by public id = %v", got)
	}
	// Case and surrounding space are a paste artefact, not a different account.
	if got, _ := st.OrgByPublicID(ctx, "  "+strings.ToUpper(o.PublicID)+"  "); got == nil || got.ID != o.ID {
		t.Error("a pasted id with padding or capitals did not resolve")
	}
	for _, miss := range []string{"", "   ", "1", "ffffffffffffffffffffffffffffffff", o.PublicID + "0"} {
		if got, _ := st.OrgByPublicID(ctx, miss); got != nil {
			t.Errorf("org lookup for %q found %q", miss, got.Name)
		}
		if got, _ := st.UserByPublicID(ctx, miss); got != nil {
			t.Errorf("user lookup for %q found %q", miss, got.Email)
		}
	}
}

// Production is a database written before the column existed. Its rows have no public id, and
// the first start after this change has to give them one — without which every one of them
// would share the empty string, and the unique index would refuse to be created at all.
func TestMigrationBackfillsRowsThatPredateTheColumn(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	skipUnlessSQLite(t, st)
	u, _ := st.CreateUser(ctx, "old@example.com", "Old", "")
	o, _ := st.CreateOrg(ctx, "Old Org", u.ID)
	u2, _ := st.CreateUser(ctx, "older@example.com", "Older", "")
	o2, _ := st.CreateOrg(ctx, "Older Org", u2.ID)

	// Wind the database back to what it looked like before: no ids, no index.
	for _, table := range []string{"orgs", "users"} {
		if _, err := st.db.Exec(`drop index if exists ` + table + `_public_id`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`update ` + table + ` set public_id=''`); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(st.db); err != nil {
		t.Fatalf("migrating a database that predates public_id: %v", err)
	}

	filled := map[string]bool{}
	for _, o := range []*Org{o, o2} {
		got, _ := st.Org(ctx, o.ID)
		if got == nil || !hex32.MatchString(got.PublicID) {
			t.Fatalf("org %d was not given a public id: %v", o.ID, got)
		}
		if filled[got.PublicID] {
			t.Fatal("two organisations were backfilled with the same id")
		}
		filled[got.PublicID] = true
	}
	for _, u := range []*User{u, u2} {
		got, _ := st.User(ctx, u.ID)
		if got == nil || !hex32.MatchString(got.PublicID) {
			t.Fatalf("user %d was not given a public id: %v", u.ID, got)
		}
		if filled[got.PublicID] {
			t.Fatal("two rows were backfilled with the same id")
		}
		filled[got.PublicID] = true
	}

	// Running again is a no-op: a second start must not hand everybody new ids, or every link
	// in an already-sent support email would stop resolving.
	before, _ := st.Org(ctx, o.ID)
	if err := migrate(st.db); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.Org(ctx, o.ID); after.PublicID != before.PublicID {
		t.Error("a second start changed an organisation's public id")
	}

	// And the index is really there, so a duplicate is refused by the database and not merely
	// by the generator that has so far never produced one.
	if _, err := st.db.Exec(`update orgs set public_id=? where id=?`, before.PublicID, o2.ID); err == nil {
		t.Error("two organisations were allowed to share a public id")
	}
}

// The serial must not be able to creep back onto the wire because somebody put a json tag on it
// again. Reflection rather than review: these are the fields that used to carry it, and the
// whole change is worth nothing if any one of them starts being marshalled.
func TestSerialIDsAreNotSerialised(t *testing.T) {
	for _, f := range []struct {
		of   any
		name string
		want string
	}{
		{Org{}, "ID", "-"}, {Org{}, "PublicID", "id"},
		{User{}, "ID", "-"}, {User{}, "PublicID", "id"},
		{AdminUser{}, "ID", "-"}, {AdminUser{}, "PublicID", "id"},
		{AdminUser{}, "OrgID", "-"}, {AdminUser{}, "OrgPublic", "org_id"},
		{Membership{}, "UserID", "-"}, {Membership{}, "UserPublic", "user_id"},
		{Membership{}, "OrgID", "-"}, {Membership{}, "OrgPublic", "org_id"},
		// These three carry the caller's own organisation, which the caller already knows.
		{APIKey{}, "OrgID", "-"}, {APIKey{}, "UserID", "-"},
		{Team{}, "OrgID", "-"}, {Job{}, "OrgID", "-"},
	} {
		typ := reflect.TypeOf(f.of)
		sf, ok := typ.FieldByName(f.name)
		if !ok {
			t.Errorf("%s has no field %s any more; this guard needs updating", typ.Name(), f.name)
			continue
		}
		got, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if got != f.want {
			t.Errorf("%s.%s is marshalled as %q, want %q", typ.Name(), f.name, got, f.want)
		}
	}
}
