package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Two organisations on one database, and a check that neither can see the other.
//
// Every entry here is a read that used to have no predicate at all. The point of the table is
// that it is a table: when the next per-organisation thing is added, it gets a row, and a
// missing row is the thing review is looking for.

type twoOrgs struct {
	st       *Store
	a, b     int64
	aUser    int64
	bUser    int64
	aTok     string
	bTok     string
	aBundle  int64
	bBundle  int64
	aConn    int64
	bConn    int64
	aScopeID int64
	bScopeID int64
}

func seedTwoOrgs(t *testing.T) *twoOrgs {
	t.Helper()
	fixedMasterKey(t)
	st := testStore(t)
	ctx := context.Background()

	mk := func(email, org, team string) (int64, int64, string, int64, int64, int64) {
		u, err := st.CreateUser(ctx, email, email, "")
		if err != nil {
			t.Fatal(err)
		}
		o, err := st.CreateOrg(ctx, org, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Email: u.Email, OrgID: o.ID}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		// A connected workspace, a bundle with a sealed credential, and a channel scope holding it.
		if err := st.SaveTeam(ctx, &Team{TeamID: team, OrgID: o.ID, Name: org}, []byte("sealed")); err != nil {
			t.Fatal(err)
		}
		bd, err := st.CreateBundle(ctx, o.ID, org+" tools", email)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := st.InsertConnection(ctx, o.ID, &Connection{BundleID: bd.ID, Name: org + " GitHub",
			Preset: "github", CredType: "bearer", AllowedHosts: []string{"api.github.com"}, Status: "active"},
			[]byte(org+"-secret"))
		if err != nil {
			t.Fatal(err)
		}
		sc, err := st.UpsertScope(ctx, o.ID, "channel", team, "C1", "#eng")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AttachBundle(ctx, o.ID, sc.ID, bd.ID); err != nil {
			t.Fatal(err)
		}
		// A document and its chunks: the retrieval corpus is the worst thing to leak.
		if err := st.UpsertDocument(ctx, o.ID, Document{Path: "handbook.md", Name: "handbook.md", Status: "indexed", Chunks: 1}); err != nil {
			t.Fatal(err)
		}
		if err := st.ReplaceDoc(ctx, o.ID, "local:handbook.md", []DocChunk{{
			Source: "local", DocID: "local:handbook.md", Title: org + " handbook",
			Text: org + " keeps its secrets here", Hash: org, Embedding: []float32{1, 0, 0},
		}}); err != nil {
			t.Fatal(err)
		}
		return o.ID, u.ID, tok, bd.ID, conn, sc.ID
	}

	x := &twoOrgs{st: st}
	x.a, x.aUser, x.aTok, x.aBundle, x.aConn, x.aScopeID = mk("a@example.com", "Acme", "T_A")
	x.b, x.bUser, x.bTok, x.bBundle, x.bConn, x.bScopeID = mk("b@example.com", "Beta", "T_B")
	return x
}

// Lists must return only the caller's rows.
func TestListsAreScopedToOneOrg(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	if bs, err := x.st.Bundles(ctx, x.a); err != nil || len(bs) != 1 || bs[0].ID != x.aBundle {
		t.Errorf("Bundles returned %d rows for Acme: %+v (%v)", len(bs), bs, err)
	}
	if scs, err := x.st.Scopes(ctx, x.a); err != nil {
		t.Fatal(err)
	} else {
		for _, sc := range scs {
			if sc.TeamID != "" && sc.TeamID != "T_A" {
				t.Errorf("Scopes leaked %s from another organisation", sc.TeamID)
			}
		}
	}
	if ds, err := x.st.Documents(ctx, x.a); err != nil || len(ds) != 1 {
		t.Errorf("Documents returned %d rows for Acme (%v)", len(ds), err)
	}
	// The retrieval corpus: the single most important predicate in the schema.
	chunks, err := x.st.AllChunks(ctx, x.a)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("AllChunks returned %d chunks for Acme, want 1", len(chunks))
	}
	if chunks[0].Title != "Acme handbook" {
		t.Errorf("Acme's retrieval returned %q — another organisation's document", chunks[0].Title)
	}
	if d, c, err := x.st.DocStats(ctx, x.a); err != nil || d != 1 || c != 1 {
		t.Errorf("DocStats = %d docs, %d chunks (%v); want 1 and 1", d, c, err)
	}
	if m, err := x.st.MembersOf(ctx, x.a); err != nil || len(m) != 1 || m[0].UserID != x.aUser {
		t.Errorf("MembersOf returned %+v (%v)", m, err)
	}
}

// A by-id read is where an id from a URL meets a row. Every one of these used to answer.
func TestByIDReadsRefuseAnotherOrg(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	if got, _ := x.st.Bundle(ctx, x.a, x.bBundle); got != nil {
		t.Error("Acme read Beta's bundle by id")
	}
	if got, _ := x.st.Connection(ctx, x.a, x.bConn); got != nil {
		t.Error("Acme read Beta's connection by id — that row holds a sealed credential")
	}
	if got, _ := x.st.ScopeByID(ctx, x.a, x.bScopeID); got != nil {
		t.Error("Acme read Beta's scope by id")
	}
	// And each still reads its own.
	if got, _ := x.st.Bundle(ctx, x.a, x.aBundle); got == nil {
		t.Error("Acme cannot read its own bundle")
	}
	if got, _ := x.st.Connection(ctx, x.b, x.bConn); got == nil {
		t.Error("Beta cannot read its own connection")
	}
}

// Writes are the other half: a by-id update or delete must not reach across either.
func TestWritesRefuseAnotherOrg(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	if err := x.st.DeleteBundle(ctx, x.a, x.bBundle); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.st.Bundle(ctx, x.b, x.bBundle); got == nil {
		t.Fatal("Acme deleted Beta's bundle")
	}
	if err := x.st.DeleteConnection(ctx, x.a, x.bConn); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.st.Connection(ctx, x.b, x.bConn); got == nil {
		t.Fatal("Acme deleted Beta's connection")
	}
	if err := x.st.UpdateBundle(ctx, x.a, x.bBundle, "hijacked", "", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.st.Bundle(ctx, x.b, x.bBundle); got == nil || got.Name == "hijacked" {
		t.Fatal("Acme renamed Beta's bundle")
	}
	// Re-indexing a same-named document must not wipe the other organisation's chunks.
	if err := x.st.ReplaceDoc(ctx, x.a, "local:handbook.md", []DocChunk{{
		Source: "local", DocID: "local:handbook.md", Title: "Acme handbook v2",
		Text: "new", Hash: "x", Embedding: []float32{1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	if chunks, _ := x.st.AllChunks(ctx, x.b); len(chunks) != 1 || chunks[0].Title != "Beta handbook" {
		t.Fatalf("Acme's re-index destroyed Beta's corpus: %+v", chunks)
	}
	// Same for the sweep that deletes documents no longer on disk.
	if _, err := x.st.DeleteDocsNotIn(ctx, x.a, "local", map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if chunks, _ := x.st.AllChunks(ctx, x.b); len(chunks) != 1 {
		t.Fatalf("Acme's ingest sweep deleted Beta's chunks: %+v", chunks)
	}
}

// The join tables are how a scope reaches a credential, so they are how one organisation would
// reach another's if the predicate were missing.
func TestAttachRefusesAcrossOrgs(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	if err := x.st.AttachBundle(ctx, x.a, x.aScopeID, x.bBundle); err == nil {
		t.Error("Acme attached Beta's bundle to its own scope")
	}
	if err := x.st.AttachConnection(ctx, x.a, x.aScopeID, x.bConn); err == nil {
		t.Error("Acme attached Beta's connection to its own scope")
	}
	// A scope belonging to Beta cannot be granted Acme's bundle either, even by Acme's session.
	if err := x.st.AttachBundle(ctx, x.a, x.bScopeID, x.aBundle); err == nil {
		t.Error("Acme attached its bundle to Beta's scope")
	}
	// And the legitimate case still works, twice, without reporting the second as a failure.
	if err := x.st.AttachBundle(ctx, x.a, x.aScopeID, x.aBundle); err != nil {
		t.Errorf("Acme cannot attach its own bundle: %v", err)
	}
}

// Resolution is what the bot actually runs on: the access a channel has. Beta's credential must
// not appear in Acme's channel however the two are arranged.
func TestResolveDoesNotCrossOrgs(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()
	rs := NewResolver(x.st)

	accA, err := rs.Resolve(ctx, x.a, "T_A", "C1", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range accA.Rules {
		if r.Conn.ID == x.bConn {
			t.Fatal("Acme's channel resolved Beta's connection")
		}
	}
	if len(accA.Rules) != 1 || accA.Rules[0].Conn.ID != x.aConn {
		t.Fatalf("Acme's channel should reach exactly its own connection: %+v", accA.Rules)
	}
	// Both organisations use the channel id C1. The cache must not answer one with the other.
	accB, err := rs.Resolve(ctx, x.b, "T_B", "C1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(accB.Rules) != 1 || accB.Rules[0].Conn.ID != x.bConn {
		t.Fatalf("Beta's channel got Acme's cached answer: %+v", accB.Rules)
	}
}

// Sessions carry the organisation, and permissions come from the membership joining the two, so
// a session cannot be pointed at somebody else's organisation to inherit its role.
func TestSessionCannotBorrowAnotherOrg(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()
	b := &Bot{store: x.st, settings: newSettingsCache(x.st, Config{})}

	me := &AdminUser{ID: x.aUser, OrgID: x.b} // Acme's user, Beta's organisation
	b.permissionsFor(ctx, me)
	if me.Role != "" || len(me.Permissions) != 0 {
		t.Fatalf("a session pointed at another organisation held %q with %d permissions", me.Role, len(me.Permissions))
	}
	if m, _ := x.st.Membership(ctx, x.aUser, x.b); m != nil {
		t.Fatal("Membership reported a membership that does not exist")
	}
}

// Alerts are deduplicated by key, and the key is a bare channel id. Two organisations sharing
// one must not silence each other.
func TestAlertsDoNotSilenceAnotherOrg(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	if !x.st.AlertOnce(ctx, x.a, "budget:C1", time.Hour) {
		t.Fatal("Acme's first alert should fire")
	}
	if x.st.AlertOnce(ctx, x.a, "budget:C1", time.Hour) {
		t.Error("Acme's second alert on the same key should be suppressed")
	}
	if !x.st.AlertOnce(ctx, x.b, "budget:C1", time.Hour) {
		t.Fatal("Acme's alert silenced Beta's alert on the same channel id")
	}
}

// Spend is per organisation as well as per workspace, or one customer's bill would stop another
// customer working.
func TestSpendIsPerOrg(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()
	x.st.LogUsage(ctx, x.a, "T_A", "C1", "1.1", "m", Usage{In: 100, Out: 10, CostUSD: 1.50})
	x.st.LogUsage(ctx, x.b, "T_B", "C1", "1.1", "m", Usage{In: 100, Out: 10, CostUSD: 0.25})

	if spend, err := x.st.MonthSpend(ctx, x.a, "T_A", ""); err != nil || spend != 1.50 {
		t.Errorf("Acme's workspace spend = %v (%v), want 1.50", spend, err)
	}
	if spend, err := x.st.MonthSpend(ctx, x.b, "T_B", ""); err != nil || spend != 0.25 {
		t.Errorf("Beta's workspace spend = %v (%v), want 0.25", spend, err)
	}
	// And the account total for one organisation never includes the other's.
	if spend, err := x.st.MonthSpend(ctx, x.a, "", ""); err != nil || spend != 1.50 {
		t.Errorf("Acme's account spend = %v (%v), want 1.50 — Beta's spend leaked in", spend, err)
	}
}

// The rows the bot writes while it is answering — memories, tool calls, routines — belong to an
// organisation as much as anything the console creates. Each of these was a live leak: the
// writes named no org_id and took the column's `default 1`, and the reads carried no predicate
// at all, so one tenant's audit trail and recall were another's to read.
func TestRuntimeWritesCarryTheirOrganisation(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	// Memories. The scope string embeds the workspace, but the scope is not the predicate.
	scopeA, scopeB := teamMemoryScope("T_A"), teamMemoryScope("T_B")
	if err := x.st.AddMemory(ctx, x.b, "T_B", scopeB, "Beta's private fact", "U_B"); err != nil {
		t.Fatal(err)
	}
	if ms, _ := x.st.AllMemories(ctx, x.a); len(ms) != 0 {
		t.Errorf("Acme reads Beta's memories: %+v", ms)
	}
	if ms, _ := x.st.AllMemories(ctx, x.b); len(ms) != 1 {
		t.Errorf("Beta cannot see its own memory, got %d", len(ms))
	}
	// And the agent's own recall path, which reads by scope.
	if ms, _ := x.st.Memories(ctx, x.a, scopeB, scopeA); len(ms) != 0 {
		t.Errorf("recall crossed the boundary even asking for the other org's scope: %+v", ms)
	}

	// Tool calls: the feed, and the by-id read behind "show full output".
	x.st.LogToolCall(ctx, x.b, "T_B", "C1", "1.1", "http_request",
		`{"url":"https://beta.internal/customers"}`, "BETA-ONLY-OUTPUT", true, 3)
	if cs, _ := x.st.RecentToolCalls(ctx, x.a, "", 50, false, ""); len(cs) != 0 {
		t.Errorf("the activity feed pooled another org's tool calls: %+v", cs)
	}
	own, _ := x.st.RecentToolCalls(ctx, x.b, "", 50, false, "")
	if len(own) != 1 {
		t.Fatalf("Beta cannot see its own tool call, got %d", len(own))
	}
	if _, err := x.st.ToolCall(ctx, x.a, own[0].ID); err == nil {
		t.Errorf("ToolCall by id returned another organisation's row")
	}
	if got, err := x.st.ToolCall(ctx, x.b, own[0].ID); err != nil || got.Result != "BETA-ONLY-OUTPUT" {
		t.Errorf("Beta cannot read its own tool call: %+v (%v)", got, err)
	}

	// A routine created from Slack has to carry the org and the workspace, or it lands at org 0
	// and the person who asked for it can never list, edit or stop it again.
	if _, err := x.st.AddRoutine(ctx, Routine{OrgID: x.b, TeamID: "T_B", Channel: "C1",
		Cron: "0 8 * * *", TZ: "UTC", Prompt: "from slack", CreatedBy: "U_B"}); err != nil {
		t.Fatal(err)
	}
	if rs, _ := x.st.Routines(ctx, x.b, "C1"); len(rs) != 1 {
		t.Errorf("a routine created in Beta's channel is not listed for Beta, got %d", len(rs))
	}

	// Routines: listing them, and the id the run-now handler matches against.
	id, err := x.st.AddRoutine(ctx, Routine{OrgID: x.b, TeamID: "T_B", Channel: "C1",
		Cron: "0 9 * * *", TZ: "UTC", Prompt: "post the digest", CreatedBy: "U_B"})
	if err != nil {
		t.Fatal(err)
	}
	if rs, _ := x.st.Routines(ctx, x.a, ""); len(rs) != 0 {
		t.Errorf("Acme can see Beta's routines: %+v", rs)
	}
	if rs, _ := x.st.Routines(ctx, x.b, ""); len(rs) != 2 {
		t.Errorf("Beta cannot see its own routines, got %d of 2", len(rs))
	}
	// Enabling and editing are the two that silently errored on every call: the org_id
	// predicate was in the SQL but its argument was never bound.
	if n, err := x.st.SetRoutineEnabled(ctx, x.b, id, false); err != nil || n != 1 {
		t.Errorf("Beta cannot disable its own routine: n=%d err=%v", n, err)
	}
	if n, _ := x.st.SetRoutineEnabled(ctx, x.a, id, true); n != 0 {
		t.Errorf("Acme toggled Beta's routine, %d row(s) changed", n)
	}
	cron := "0 10 * * *"
	if err := x.st.UpdateRoutine(ctx, x.b, id, RoutinePatch{Cron: &cron}); err != nil {
		t.Errorf("Beta cannot edit its own routine: %v", err)
	}
	if err := x.st.UpdateRoutine(ctx, x.a, id, RoutinePatch{Cron: &cron}); err == nil {
		t.Errorf("Acme edited Beta's routine")
	}
}

// A routine can be toggled only from the channel it lives in. !routine on|off and delete_routine
// take an id straight from a member (or the model), and ids are guessable, so the chat path is
// channel-scoped — the console's org-wide toggle is behind a permission.
func TestRoutineToggleIsScopedToItsChannel(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()
	id, err := x.st.AddRoutine(ctx, Routine{OrgID: x.b, TeamID: "T_B", Channel: "C1",
		Cron: "0 8 * * *", TZ: "UTC", Prompt: "digest", CreatedBy: "U_B"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := x.st.SetRoutineEnabledInChannel(ctx, x.b, "C2", id, false); n != 0 {
		t.Errorf("a routine that lives in C1 was toggled from C2: %d row(s) changed", n)
	}
	if n, err := x.st.SetRoutineEnabledInChannel(ctx, x.b, "C1", id, false); err != nil || n != 1 {
		t.Errorf("the routine could not be toggled from its own channel: n=%d err=%v", n, err)
	}
}

// A Slack workspace belongs to the organisation that installed it. The whole team_id-is-enough
// argument in store_surface_test.go rests on this, and SaveTeam used to write a literal 1.
func TestAnInstallBelongsToTheOrganisationThatMadeIt(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	for _, c := range []struct {
		team string
		want int64
	}{{"T_A", x.a}, {"T_B", x.b}} {
		got, err := x.st.Team(ctx, c.team)
		if err != nil || got == nil {
			t.Fatalf("%s: %v", c.team, err)
		}
		if got.OrgID != c.want {
			t.Errorf("%s belongs to org %d, want %d", c.team, got.OrgID, c.want)
		}
	}

	// A second organisation cannot adopt a workspace someone else holds — team_id is the primary
	// key, so an unguarded upsert would overwrite the first organisation's bot token.
	err := x.st.SaveTeam(ctx, &Team{TeamID: "T_A", OrgID: x.b, Name: "Beta steals Acme"}, []byte("beta-token"))
	if !errors.Is(err, ErrTeamOwnedElsewhere) {
		t.Errorf("Beta was allowed to take Acme's workspace: %v", err)
	}
	if got, _ := x.st.Team(ctx, "T_A"); got == nil || got.OrgID != x.a {
		t.Errorf("Acme's install changed hands: %+v", got)
	}

	// An install with no organisation is a bug, not a default.
	if err := x.st.SaveTeam(ctx, &Team{TeamID: "T_NEW", Name: "no org"}, []byte("x")); err == nil {
		t.Errorf("SaveTeam accepted an install with no organisation")
	}
}

// The organisation has to survive Slack's redirect. handleInstall mints a state token, the
// browser goes to Slack and comes back to handleInstallCallback, and by then the only thing
// linking the install to a tenant is that token — the org used to be a literal 1 in both
// NewOAuthState and SaveTeam, so every workspace anyone connected became the first tenant's.
func TestTheInstallStateCarriesTheOrganisation(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()

	state, err := x.st.NewOAuthState(ctx, x.b, "U_BETA_ADMIN", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	who, org, err := x.st.TakeOAuthState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if org != x.b || who != "U_BETA_ADMIN" {
		t.Fatalf("state came back as org %d by %q, want org %d by U_BETA_ADMIN", org, who, x.b)
	}

	// What the callback then does with it, which is the step that files the workspace.
	if err := x.st.SaveTeam(ctx, &Team{TeamID: "T_NEW", OrgID: org, Name: "Beta's second workspace"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	got, err := x.st.Team(ctx, "T_NEW")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.OrgID != x.b {
		t.Errorf("the new workspace was filed under org %d, want %d", got.OrgID, x.b)
	}
	// And it shows up for its owner, not for the other tenant.
	if ts, _ := x.st.Teams(ctx, x.a); len(ts) != 1 {
		t.Errorf("Acme sees %d workspaces, want only its own", len(ts))
	}
	if ts, _ := x.st.Teams(ctx, x.b); len(ts) != 2 {
		t.Errorf("Beta sees %d workspaces, want its original plus the new one", len(ts))
	}
}
