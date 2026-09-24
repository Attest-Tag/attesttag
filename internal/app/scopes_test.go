package app

import (
	"context"
	"path/filepath"
	"testing"
)

// A connection attached on its own grants just that connection (and its preset's tool pack
// when the bundle enables it); a bundle grants everything; the two never duplicate a rule.
func TestResolveDirectConnections(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	orgID, _, _ := seedOrg(t, st, RoleAdmin)
	ws, _ := st.UpsertScope(ctx, orgID, "team", "T1", "T1", "Team")
	ch, _ := st.UpsertScope(ctx, orgID, "channel", "T1", "C1", "#eng")
	bd, _ := st.CreateBundle(ctx, orgID, "Eng", "U1")
	st.UpdateBundle(ctx, orgID, bd.ID, "Eng", "bundle instructions", []string{"github", "sentry"})
	gh, _ := st.InsertConnection(ctx, orgID, &Connection{BundleID: bd.ID, Name: "GitHub", Preset: "github", CredType: "bearer",
		AllowedHosts: []string{"api.github.com"}, Status: "active"}, []byte("sealed"))
	st.InsertConnection(ctx, orgID, &Connection{BundleID: bd.ID, Name: "Sentry", Preset: "sentry", CredType: "bearer",
		AllowedHosts: []string{"sentry.io"}, Status: "active"}, []byte("sealed"))
	st.AddDomain(ctx, orgID, bd.ID, "docs.example.com", "")

	// One-off: Eng/GitHub on the channel, nothing else.
	if err := st.AttachConnection(ctx, orgID, ch.ID, gh); err != nil {
		t.Fatal(err)
	}
	rs := NewResolver(st)
	acc, err := rs.Resolve(ctx, orgID, "T1", "C1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(acc.Rules) != 1 || acc.Rules[0].Conn.ID != gh || !acc.Rules[0].Direct || acc.Rules[0].Rank != 2 {
		t.Fatalf("want one direct GitHub rule at rank 2, got %+v", acc.Rules)
	}
	if !acc.ToolPacks["github"] || acc.ToolPacks["sentry"] {
		t.Errorf("a one-off brings only its own pack: %v", acc.ToolPacks)
	}
	if len(acc.Domains) != 0 || acc.Instructions != "" {
		t.Errorf("a one-off must not bring the bundle's domains or instructions: %+v %q", acc.Domains, acc.Instructions)
	}
	if sc, _ := st.ScopeByID(ctx, orgID, ch.ID); len(sc.ConnectionIDs) != 1 || sc.ConnectionIDs[0] != gh {
		t.Errorf("scope.connection_ids = %v", sc.ConnectionIDs)
	}
	if c, _ := st.Connection(ctx, orgID, gh); len(c.ScopeIDs) != 1 || c.ScopeIDs[0] != ch.ID {
		t.Errorf("connection.scope_ids = %v", c.ScopeIDs)
	}

	// The bundle on the workspace as well: the channel inherits both connections and the
	// domain; its own one-off stays a separate, narrower rule listed first.
	st.AttachBundle(ctx, orgID, ws.ID, bd.ID)
	rs.Invalidate(orgID)
	acc, _ = rs.Resolve(ctx, orgID, "T1", "C1", 2)
	if len(acc.Rules) != 3 || len(acc.Domains) != 1 || !acc.ToolPacks["sentry"] {
		t.Fatalf("want 3 rules, 1 domain, sentry pack: rules=%d domains=%d packs=%v", len(acc.Rules), len(acc.Domains), acc.ToolPacks)
	}
	if acc.Rules[0].Conn.ID != gh || !acc.Rules[0].Direct {
		t.Errorf("narrowest rule first: %+v", acc.Rules[0])
	}

	// Bundle and one of its connections both on the channel: the bundle covers it, no duplicate.
	st.AttachBundle(ctx, orgID, ch.ID, bd.ID)
	rs.Invalidate(orgID)
	acc, _ = rs.Resolve(ctx, orgID, "T1", "C1", 3)
	n := 0
	for _, r := range acc.Rules {
		if r.Rank == 2 {
			n++
		}
	}
	if n != 2 {
		t.Errorf("want 2 channel-rank rules (bundle's two, one-off deduped), got %d", n)
	}

	// The workspace resolved for itself lists each grant once.
	if acc, _ = rs.Resolve(ctx, orgID, "T1", "T1", 3); len(acc.Rules) != 2 {
		t.Errorf("workspace resolve should have 2 rules, got %d", len(acc.Rules))
	}

	// Deleting the connection drops its one-off grants.
	st.DeleteConnection(ctx, orgID, gh)
	if sc, _ := st.ScopeByID(ctx, orgID, ch.ID); len(sc.ConnectionIDs) != 0 {
		t.Errorf("stale scope_connections row after delete: %v", sc.ConnectionIDs)
	}

	// So does deleting the bundle.
	b2, _ := st.CreateBundle(ctx, orgID, "Ops", "U1")
	pd, _ := st.InsertConnection(ctx, orgID, &Connection{BundleID: b2.ID, Name: "PagerDuty", Preset: "pagerduty", CredType: "bearer",
		AllowedHosts: []string{"api.pagerduty.com"}, Status: "active"}, []byte("sealed"))
	st.AttachConnection(ctx, orgID, ws.ID, pd)
	st.DeleteBundle(ctx, orgID, b2.ID)
	if sc, _ := st.ScopeByID(ctx, orgID, ws.ID); len(sc.ConnectionIDs) != 0 {
		t.Errorf("stale scope_connections row after bundle delete: %v", sc.ConnectionIDs)
	}
}
