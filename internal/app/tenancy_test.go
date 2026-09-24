package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
)

func tenancyStore(t *testing.T) *Store {
	t.Helper()
	// A fixed key: without one NewSealer mints a throwaway and appends it to a dotenv file,
	// which a test must never do to the working tree.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The whole point of the change: two connected Slack workspaces on one account see the
// account's access and their own, and never each other's.
func TestResolveIsolatesWorkspaces(t *testing.T) {
	ctx := context.Background()
	st := tenancyStore(t)
	orgID, _, _ := seedOrg(t, st, RoleAdmin)

	acct, _ := st.UpsertScope(ctx, orgID, "workspace", "", "", "Workspace")
	teamA, _ := st.UpsertScope(ctx, orgID, "team", "TA", "TA", "Acme")
	teamB, _ := st.UpsertScope(ctx, orgID, "team", "TB", "TB", "Beta")
	chanA, _ := st.UpsertScope(ctx, orgID, "channel", "TA", "C1", "#eng")
	chanB, _ := st.UpsertScope(ctx, orgID, "channel", "TB", "C2", "#support")

	mkBundle := func(name, host string) (int64, int64) {
		bd, _ := st.CreateBundle(ctx, orgID, name, "U1")
		id, err := st.InsertConnection(ctx, orgID, &Connection{BundleID: bd.ID, Name: name, Preset: "custom", CredType: "bearer",
			AllowedHosts: []string{host}, Status: "active"}, []byte("sealed"))
		if err != nil {
			t.Fatal(err)
		}
		return bd.ID, id
	}
	shared, sharedConn := mkBundle("Shared", "shared.example.com")
	onlyA, connA := mkBundle("OnlyA", "a.example.com")
	onlyB, connB := mkBundle("OnlyB", "b.example.com")

	st.AttachBundle(ctx, orgID, acct.ID, shared)
	st.AttachBundle(ctx, orgID, teamA.ID, onlyA)
	st.AttachBundle(ctx, orgID, teamB.ID, onlyB)

	rs := NewResolver(st)
	version := int64(0)
	conns := func(teamID, channel string) map[int64]int {
		t.Helper()
		version++ // a config change bumps the version, which is what drops the 60s cache
		acc, err := rs.Resolve(ctx, orgID, teamID, channel, version)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]int{}
		for _, r := range acc.Rules {
			out[r.Conn.ID] = r.Rank
		}
		return out
	}

	a := conns("TA", "C1")
	if _, ok := a[connB]; ok {
		t.Error("a channel in workspace A must not reach a bundle attached to workspace B")
	}
	if rank, ok := a[connA]; !ok || rank != 1 {
		_ = rank
		t.Errorf("workspace A's own bundle should arrive at rank 1 (the team link), got %v", a)
	}
	if rank, ok := a[sharedConn]; !ok || rank != 0 {
		t.Errorf("the account's bundle should arrive at rank 0 (the widest link), got %v", a)
	}

	b := conns("TB", "C2")
	if _, ok := b[connA]; ok {
		t.Error("a channel in workspace B must not reach a bundle attached to workspace A")
	}
	if _, ok := b[sharedConn]; !ok {
		t.Error("the account's bundle should reach every connected workspace")
	}

	// Channel-level attachments stay in their own workspace too.
	st.AttachConnection(ctx, orgID, chanA.ID, connB)
	if conns("TB", "C2")[connB] == 2 {
		t.Error("attaching a connection to a channel in A must not change what B's channel sees")
	}
	if conns("TA", "C1")[connB] != 2 {
		t.Error("a connection attached to this channel should arrive at rank 2")
	}
	_ = chanB
}

// Slack only guarantees a channel id is unique within a workspace, and a Slack Connect channel
// really does carry the same C-id in both. Two scope rows must therefore be able to exist for
// one channel id, each with its own settings.
func TestSameChannelIDInTwoWorkspaces(t *testing.T) {
	ctx := context.Background()
	st := tenancyStore(t)
	orgID, _, _ := seedOrg(t, st, RoleAdmin)

	a, err := st.UpsertScope(ctx, orgID, "channel", "TA", "C_SHARED", "#shared")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.UpsertScope(ctx, orgID, "channel", "TB", "C_SHARED", "#shared")
	if err != nil {
		t.Fatalf("a second workspace must be able to hold the same channel id: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("the same channel id in two workspaces must be two scope rows")
	}
	st.UpdateScope(ctx, orgID, a.ID, "Answer in English.", "", "inherit")

	got, _ := st.ChannelScope(ctx, orgID, "TB", "C_SHARED")
	if got == nil || got.Instructions != "" {
		t.Errorf("workspace B's copy must not carry workspace A's instructions: %+v", got)
	}
	if got.ID != b.ID {
		t.Errorf("ChannelScope returned the wrong workspace's row: %d want %d", got.ID, b.ID)
	}
}

// Sessions, turns and memories are keyed by workspace as well as channel, so a shared channel
// id cannot merge two workspaces' conversations.
func TestSessionsAreKeyedByWorkspace(t *testing.T) {
	ctx := context.Background()
	st := tenancyStore(t)

	if _, err := st.EnsureSession(ctx, "TA", "C_SHARED", "1.1", "channel", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSession(ctx, "TB", "C_SHARED", "1.1", "channel", ""); err != nil {
		t.Fatal(err)
	}
	st.AddTurn(ctx, "TA", "C_SHARED", "1.1", "note", "U1", "only A saw this", "", 0, 0)

	notesA, _ := st.Notes(ctx, "TA", "C_SHARED", "1.1")
	notesB, _ := st.Notes(ctx, "TB", "C_SHARED", "1.1")
	if len(notesA) != 1 {
		t.Errorf("workspace A should see its own note, got %d", len(notesA))
	}
	if len(notesB) != 0 {
		t.Errorf("workspace B must not see workspace A's thread, got %d notes", len(notesB))
	}

	st.AddMemory(ctx, 1, "TA", channelMemoryScope("TA", "C_SHARED"), "A's fact", "U1")
	if m, _ := st.Memories(ctx, 1, channelMemoryScope("TB", "C_SHARED")); len(m) != 0 {
		t.Errorf("workspace B must not recall workspace A's memories, got %+v", m)
	}
	if m, _ := st.Memories(ctx, 1, channelMemoryScope("TA", "C_SHARED")); len(m) != 1 {
		t.Errorf("workspace A should recall its own memory, got %+v", m)
	}
}

// Which workspace an envelope belongs to. Slack's own SDKs prefer authorizations[0].team_id
// because for a Slack Connect channel the top-level team_id can be the other workspace — and
// slack-go does not parse authorizations at all, so teamOf reads the raw payload.
func TestTeamOfPrefersAuthorizations(t *testing.T) {
	env := func(payload string) json.RawMessage { return json.RawMessage(payload) }

	// Ordinary install: both agree.
	if got := teamOf(env(`{"team_id":"T1","authorizations":[{"team_id":"T1"}]}`),
		slackevents.EventsAPIEvent{TeamID: "T1"}); got != "T1" {
		t.Errorf("teamOf = %q, want T1", got)
	}

	// Slack Connect: the top-level id is the *other* workspace; the authorization is ours.
	if got := teamOf(env(`{"team_id":"T_OTHER","is_ext_shared_channel":true,"authorizations":[{"team_id":"T_MINE"}]}`),
		slackevents.EventsAPIEvent{TeamID: "T_OTHER"}); got != "T_MINE" {
		t.Errorf("teamOf = %q, want the installed workspace T_MINE", got)
	}

	// No authorizations at all: fall back to the parsed team.
	if got := teamOf(env(`{"team_id":"T2"}`), slackevents.EventsAPIEvent{TeamID: "T2"}); got != "T2" {
		t.Errorf("teamOf = %q, want the fallback T2", got)
	}

	// An empty authorization entry must not win over a real team id.
	if got := teamOf(env(`{"team_id":"T3","authorizations":[{"team_id":""}]}`),
		slackevents.EventsAPIEvent{TeamID: "T3"}); got != "T3" {
		t.Errorf("teamOf = %q, want T3", got)
	}

	// Nothing to go on: empty, so the caller drops the turn rather than guessing.
	if got := teamOf(nil, slackevents.EventsAPIEvent{}); got != "" {
		t.Errorf("teamOf = %q, want empty so the event is dropped", got)
	}
}

// The registry must never answer with another workspace's client, and a revoked install must
// stop resolving the moment it is revoked.
func TestRegistryRefusesUnknownAndRevoked(t *testing.T) {
	ctx := context.Background()
	st := tenancyStore(t)
	orgID, _, _ := seedOrg(t, st, RoleAdmin)
	_ = orgID
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := sealer.Seal([]byte("xoxb-real"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: 1, Name: "Acme", BotUserID: "UA"}, enc); err != nil {
		t.Fatal(err)
	}
	r := NewChatRegistry(st, sealer)

	if _, err := r.For(ctx, "TA"); err != nil {
		t.Fatalf("a connected workspace should resolve: %v", err)
	}
	if _, err := r.For(ctx, "TB"); err == nil {
		t.Error("an unconnected workspace must not resolve to some other workspace's client")
	}
	if _, err := r.For(ctx, ""); err == nil {
		t.Error("an event with no workspace must be refused, never defaulted")
	}

	if err := st.RevokeTeam(ctx, "TA", "uninstalled"); err != nil {
		t.Fatal(err)
	}
	r.Evict("TA")
	if _, err := r.For(ctx, "TA"); err == nil {
		t.Error("a revoked workspace must stop resolving once evicted")
	}
	if got := r.Active(ctx); len(got) != 0 {
		t.Errorf("Active should skip revoked workspaces, got %d", len(got))
	}
}

// The install-flow state is single-use and expires, so a replayed callback cannot install twice.
func TestOAuthStateIsSingleUse(t *testing.T) {
	ctx := context.Background()
	st := tenancyStore(t)

	tok, err := st.NewOAuthState(ctx, 7, "U_ADMIN", installStateTTL)
	if err != nil {
		t.Fatal(err)
	}
	who, org, err := st.TakeOAuthState(ctx, tok)
	if err != nil || who != "U_ADMIN" {
		t.Fatalf("first use = %q, %v; want the admin who started it", who, err)
	}
	// And the organisation it was started for, which is what the callback files the install under.
	if org != 7 {
		t.Errorf("install state came back for org %d, want 7", org)
	}
	if _, _, err := st.TakeOAuthState(ctx, tok); err == nil {
		t.Error("a state token must not be usable twice")
	}
	if _, _, err := st.TakeOAuthState(ctx, "never-issued"); err == nil {
		t.Error("an unknown state must be refused")
	}
	if _, _, err := st.TakeOAuthState(ctx, ""); err == nil {
		t.Error("an empty state must be refused")
	}

	// Expired states are refused and consumed rather than left behind.
	old, _ := st.NewOAuthState(ctx, 7, "U_ADMIN", -time.Minute)
	if _, _, err := st.TakeOAuthState(ctx, old); err == nil {
		t.Error("an expired state must be refused")
	}
}

// A workspace's bot token round-trips through the sealer and is never stored in the clear.
func TestBotTokenIsSealedAtRest(t *testing.T) {
	ctx := context.Background()
	st := tenancyStore(t)
	orgID, _, _ := seedOrg(t, st, RoleAdmin)
	_ = orgID
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sealer.Seal([]byte("xoxb-secret-token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: 1, Name: "Acme"}, enc); err != nil {
		t.Fatal(err)
	}

	var raw []byte
	if err := st.db.QueryRowContext(ctx, `select bot_token_enc from teams where team_id='TA'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "xoxb-") {
		t.Fatal("the bot token must not be readable in the database")
	}
	got, err := NewChatRegistry(st, sealer).Token(ctx, "TA")
	if err != nil || got != "xoxb-secret-token" {
		t.Fatalf("token round-trip = %q, %v", got, err)
	}
}
