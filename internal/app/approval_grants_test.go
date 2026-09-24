package app

import (
	"context"
	"testing"
)

// A tier grants from what it lists — whole bundles, and connections picked on their own — the
// way a channel's access is recorded. Being listed is the whole authorisation. These pin down
// what that list means when a request is routed and when it runs.

// grantConn is one active connection in a bundle, on one host.
func grantConn(t *testing.T, st *Store, org, bundleID int64, name, host string) int64 {
	t.Helper()
	id, err := st.InsertConnection(context.Background(), org, &Connection{
		BundleID: bundleID, Name: name, Preset: "custom", CredType: "bearer", Status: "active",
		AllowedHosts: []string{host}, Methods: []string{"PUT"}, PathPrefixes: []string{"/"},
		Writes: "confirm",
	}, []byte("x"))
	if err != nil || id == 0 {
		t.Fatalf("connection %s: %v", name, err)
	}
	return id
}

func TestTierGrantsFromOneOffConnections(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := &Agent{store: st, proxy: NewProxy(nil, st)}
	bl, err := st.CreateBundle(ctx, orgID, "tools", "test")
	if err != nil {
		t.Fatal(err)
	}
	billing := grantConn(t, st, orgID, bl.ID, "billing", "billing.example.com")
	grantConn(t, st, orgID, bl.ID, "crm", "crm.example.com")
	wiki := grantConn(t, st, orgID, bl.ID, "wiki", "wiki.example.com")

	// Two connections on their own, no bundle: the tier reaches exactly those, whatever else
	// their bundle holds, and needs no further switch on the connection to do it.
	r := &ApprovalRole{Name: "Billing approver", Rank: 1, ConnectionIDs: []int64{billing, wiki}}
	if err := st.AddApprovalRole(ctx, orgID, r); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ApprovalRole(ctx, orgID, r.ID)
	if len(got.ConnectionIDs) != 2 || len(got.BundleIDs) != 0 {
		t.Fatalf("stored grants: bundles %v connections %v", got.BundleIDs, got.ConnectionIDs)
	}
	acc := a.roleGrants(ctx, orgID, *got)
	if len(acc.Rules) != 2 || !acc.Rules[0].Direct || !acc.Rules[1].Direct {
		t.Fatalf("two listed connections should be two direct rules, got %+v", acc.Rules)
	}
	tr := tier{Role: *got, Access: acc}
	for _, host := range []string{"billing", "wiki"} {
		if !a.covers(tr, []grantStep{{Method: "PUT", URL: "https://" + host + ".example.com/x"}}) {
			t.Fatalf("the listed %s connection's host is not covered", host)
		}
	}
	if a.covers(tr, []grantStep{{Method: "PUT", URL: "https://crm.example.com/x"}}) {
		t.Fatal("a connection in the same bundle that was not listed must not be covered")
	}

	// Adding the whole bundle widens it, and a connection listed both ways is one rule.
	if err := st.AttachRoleBundle(ctx, orgID, r.ID, bl.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ApprovalRole(ctx, orgID, r.ID)
	acc = a.roleGrants(ctx, orgID, *got)
	if len(acc.Rules) != 3 {
		t.Fatalf("bundle plus one-offs should be the bundle's three connections once each, got %d", len(acc.Rules))
	}
	if !a.covers(tier{Role: *got, Access: acc}, []grantStep{{Method: "PUT", URL: "https://crm.example.com/x"}}) {
		t.Fatal("the bundle's other connection is not covered once the bundle is on the tier")
	}

	// A disabled connection is not granted, however it is listed.
	if _, err := st.db.Exec(`update connections set status='disabled' where id=?`, wiki); err != nil {
		t.Fatal(err)
	}
	if acc = a.roleGrants(ctx, orgID, *got); len(acc.Rules) != 2 {
		t.Fatalf("a disabled connection should drop out of the tier's reach, got %d rules", len(acc.Rules))
	}

	// Taking the bundle off again leaves the one-offs where they were.
	if err := st.DetachRoleBundle(ctx, orgID, r.ID, bl.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ApprovalRole(ctx, orgID, r.ID)
	if len(got.BundleIDs) != 0 || len(got.ConnectionIDs) != 2 {
		t.Fatalf("after detaching the bundle: bundles %v connections %v", got.BundleIDs, got.ConnectionIDs)
	}
	// And deleting the tier takes its grants with it.
	if err := st.DeleteApprovalRole(ctx, orgID, r.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRow(`select count(*) from approval_role_connections where role_id=?`, r.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d grant rows survived their tier", n)
	}
}

// A tier's grants are this organisation's bundles and connections, attached by this organisation.
// The ids come from a URL, so the write itself has to refuse anything else.
func TestTierGrantsStayInsideTheOrganisation(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	ours := &ApprovalRole{Name: "Approver", Rank: 1}
	if err := st.AddApprovalRole(ctx, orgID, ours); err != nil {
		t.Fatal(err)
	}
	other := orgID + 1
	theirs, err := st.CreateBundle(ctx, other, "their tools", "test")
	if err != nil {
		t.Fatal(err)
	}
	theirConn := grantConn(t, st, other, theirs.ID, "theirs", "theirs.example.com")
	if err := st.AttachRoleBundle(ctx, orgID, ours.ID, theirs.ID); err == nil {
		t.Fatal("another organisation's bundle was attached to our tier")
	}
	if err := st.AttachRoleConnection(ctx, orgID, ours.ID, theirConn); err == nil {
		t.Fatal("another organisation's connection was attached to our tier")
	}
	// Nor can they reach into our tier with their own things.
	if err := st.AttachRoleBundle(ctx, other, ours.ID, theirs.ID); err == nil {
		t.Fatal("another organisation attached its bundle to our tier")
	}
	if err := st.AttachRoleConnection(ctx, other, ours.ID, theirConn); err == nil {
		t.Fatal("another organisation attached its connection to our tier")
	}
	got, _ := st.ApprovalRole(ctx, orgID, ours.ID)
	if len(got.BundleIDs)+len(got.ConnectionIDs) != 0 {
		t.Fatalf("grants leaked across the boundary: bundles %v connections %v", got.BundleIDs, got.ConnectionIDs)
	}
	// Creating a tier that names something foreign fails the same way.
	bad := &ApprovalRole{Name: "Wide", Rank: 9, BundleIDs: []int64{theirs.ID}}
	if err := st.AddApprovalRole(ctx, orgID, bad); err == nil {
		t.Fatal("a tier was created granting from another organisation's bundle")
	}
}

// A tier used to name one bundle in a column. Opening an older database moves that into the
// grants list, once: taking the bundle off the tier afterwards is not undone by the next start.
func TestOldTierBundleColumnBecomesAGrant(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	skipUnlessSQLite(t, st)
	bl, err := st.CreateBundle(ctx, orgID, "tools", "test")
	if err != nil {
		t.Fatal(err)
	}
	r := &ApprovalRole{Name: "Approver", Rank: 1}
	if err := st.AddApprovalRole(ctx, orgID, r); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`update approval_roles set bundle_id=? where id=?`, bl.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(st.db); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ApprovalRole(ctx, orgID, r.ID)
	if len(got.BundleIDs) != 1 || got.BundleIDs[0] != bl.ID {
		t.Fatalf("the bundle_id column did not become a grant: %v", got.BundleIDs)
	}
	if err := st.DetachRoleBundle(ctx, orgID, r.ID, bl.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(st.db); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ApprovalRole(ctx, orgID, r.ID)
	if len(got.BundleIDs) != 0 {
		t.Fatalf("a second start re-attached the bundle the admin had removed: %v", got.BundleIDs)
	}
}
