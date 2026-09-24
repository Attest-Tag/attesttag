package app

import (
	"context"
	"path/filepath"
	"testing"
)

// A channel's console shows what it carries but cannot edit there: the workspace's
// instructions and allow rules, the Settings rules, and the instructions of every bundle
// attached at either link. The workspace itself inherits only Settings and its bundles.
func TestInheritedSummary(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: testRegistry(&Chat{TeamID: "T1"})}
	orgID, _, _ := seedOrg(t, st, RoleAdmin)

	st.PutSetting(ctx, orgID, "allow_rules", `["Filing tickets is approved."]`)
	acct, _ := st.UpsertScope(ctx, orgID, "workspace", "", "", "Workspace")
	ws, _ := st.UpsertScope(ctx, orgID, "team", "T1", "T1", "Team")
	ch, _ := st.UpsertScope(ctx, orgID, "channel", "T1", "C1", "#eng")
	st.UpdateScope(ctx, orgID, acct.ID, "Always answer in English.", "", "inherit")
	st.UpdateScope(ctx, orgID, ws.ID, "Keep answers short.", "", "inherit")
	st.SetScopeAllowRules(ctx, orgID, ws.ID, []string{"Commenting on issues is approved."})

	wsBundle, _ := st.CreateBundle(ctx, orgID, "Docs", "U1")
	st.UpdateBundle(ctx, orgID, wsBundle.ID, "Docs", "Cite the document you used.", nil)
	chBundle, _ := st.CreateBundle(ctx, orgID, "Eng", "U1")
	st.UpdateBundle(ctx, orgID, chBundle.ID, "Eng", "Link the pull request.", nil)
	quiet, _ := st.CreateBundle(ctx, orgID, "Quiet", "U1") // no instructions: must not show up
	st.AttachBundle(ctx, orgID, ws.ID, wsBundle.ID)
	st.AttachBundle(ctx, orgID, ch.ID, chBundle.ID)
	st.AttachBundle(ctx, orgID, ch.ID, quiet.ID)

	sc, _ := st.ScopeByID(ctx, orgID, ch.ID)
	got := b.inheritedSummary(ctx, orgID, sc)
	instr := got["instructions"].([]map[string]any)
	if len(instr) != 4 {
		t.Fatalf("want the account, the team, and two bundles with text, got %+v", instr)
	}
	// Resolve walks widest to narrowest — account, then team, then here — and the readout
	// follows that order.
	if instr[0]["kind"] != "workspace" || instr[0]["text"] != "Always answer in English." {
		t.Errorf("first entry = %+v, want the account's own instructions", instr[0])
	}
	if instr[1]["kind"] != "team" || instr[1]["text"] != "Keep answers short." {
		t.Errorf("second entry = %+v, want the workspace's own instructions", instr[1])
	}
	if instr[2]["source"] != "Docs" || instr[2]["where"] != "team" {
		t.Errorf("third entry = %+v, want the bundle attached on the workspace", instr[2])
	}
	if instr[3]["source"] != "Eng" || instr[3]["where"] != "here" {
		t.Errorf("fourth entry = %+v, want the bundle attached on the channel", instr[3])
	}

	rules := got["allow_rules"].([]map[string]any)
	if len(rules) != 2 || rules[0]["kind"] != "settings" || rules[1]["kind"] != "team" {
		t.Fatalf("want Settings then the workspace's rules, got %+v", rules)
	}
	if r := rules[1]["rules"].([]string); len(r) != 1 || r[0] != "Commenting on issues is approved." {
		t.Errorf("workspace rules = %v", r)
	}

	// A Slack workspace inherits the account above it, but nothing from itself: the account's
	// instructions and its own bundle, no more.
	wsScope, _ := st.ScopeByID(ctx, orgID, ws.ID)
	got = b.inheritedSummary(ctx, orgID, wsScope)
	instr = got["instructions"].([]map[string]any)
	if len(instr) != 2 || instr[0]["kind"] != "workspace" || instr[1]["source"] != "Docs" {
		t.Errorf("workspace instructions = %+v, want the account's own plus its bundle", instr)
	}

	// And the account itself inherits nothing above it.
	acctScope, _ := st.ScopeByID(ctx, orgID, acct.ID)
	got = b.inheritedSummary(ctx, orgID, acctScope)
	if instr = got["instructions"].([]map[string]any); len(instr) != 0 {
		t.Errorf("account instructions = %+v, want none: nothing sits above it", instr)
	}
	if rules = got["allow_rules"].([]map[string]any); len(rules) != 1 || rules[0]["kind"] != "settings" {
		t.Errorf("workspace rules = %+v, want only Settings", rules)
	}
}
