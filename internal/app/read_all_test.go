package app

import (
	"context"
	"testing"
)

// An unset scope reads as "inherit", and the mode survives a round trip. Anything that is not
// on or off is stored as inherit: the value arrives from a form post and an API body.
func TestScopeReadAllRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc, err := st.UpsertChannelScope(ctx, 1, "T1", "C1", "#general", false)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if sc.ReadAll != "inherit" {
		t.Fatalf("a fresh channel scope should inherit, got %q", sc.ReadAll)
	}
	for _, tc := range []struct{ set, want string }{
		{"on", "on"}, {"off", "off"}, {"inherit", "inherit"}, {"nonsense", "inherit"}, {"", "inherit"},
	} {
		if err := st.SetScopeReadAll(ctx, 1, sc.ID, tc.set); err != nil {
			t.Fatalf("set %q: %v", tc.set, err)
		}
		got, err := st.ChannelScope(ctx, 1, "T1", "C1")
		if err != nil || got == nil {
			t.Fatalf("read back %q: %v", tc.set, err)
		}
		if got.ReadAll != tc.want {
			t.Errorf("set %q → %q, want %q", tc.set, got.ReadAll, tc.want)
		}
	}
}

// Reading every message costs a model call per message, so it is off until someone asks for it,
// and a channel may say no after its workspace or the account has said yes.
func TestReadAllInheritance(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	b := &Bot{store: st}
	acct, _ := st.UpsertScope(ctx, 1, "workspace", "", "", "Account")
	team, _ := st.UpsertScope(ctx, 1, "team", "T1", "T1", "Workspace")
	chn, _ := st.UpsertChannelScope(ctx, 1, "T1", "C1", "#general", false)

	if b.readAll(ctx, 1, "T1", "C1") {
		t.Error("off by default, with every link inheriting")
	}
	st.SetScopeReadAll(ctx, 1, acct.ID, "on")
	if !b.readAll(ctx, 1, "T1", "C1") {
		t.Error("the account link should reach a channel that inherits")
	}
	st.SetScopeReadAll(ctx, 1, team.ID, "off")
	if b.readAll(ctx, 1, "T1", "C1") {
		t.Error("a workspace saying off overrides the account")
	}
	st.SetScopeReadAll(ctx, 1, chn.ID, "on")
	if !b.readAll(ctx, 1, "T1", "C1") {
		t.Error("the channel is the narrowest link and wins")
	}
	// An untouched channel in the same workspace still follows the workspace's "off".
	st.UpsertChannelScope(ctx, 1, "T1", "C2", "#other", false)
	if b.readAll(ctx, 1, "T1", "C2") {
		t.Error("a sibling channel that inherits should follow the workspace")
	}
	// And a channel in another organisation is untouched by any of it.
	if b.readAll(ctx, 2, "T1", "C1") {
		t.Error("another organisation must not see these scopes")
	}
}

// The classifier answers in prose more often than it should. Only a clean verdict may act, and
// a reaction is only ever a Slack emoji name — never a sentence spliced into an API call.
func TestParseWatchVerdict(t *testing.T) {
	for _, tc := range []struct{ in, action, emoji string }{
		{"NOTHING", "", ""},
		{"nothing.", "", ""},
		{"", "", ""},
		{"   ", "", ""},
		{"REPLY", "reply", ""},
		{"reply — they asked how to reset a password", "reply", ""},
		{"**REPLY**", "reply", ""},
		{"REACT eyes", "react", "eyes"},
		{"react :white_check_mark:", "react", "white_check_mark"},
		{"REACT +1", "react", "+1"},
		{"React ThumbsUp, since the instructions ask for it", "react", "thumbsup"},
		{"REACT", "", ""},   // no emoji named
		{"REACT 👍", "", ""}, // a literal emoji is not a Slack name
		// Whether a well-formed name exists is Slack's to say: reactions.add answers
		// invalid_name and AddReaction returns it. The parser only rejects what could not
		// be a name at all, so this passes through and fails at the API.
		{"REACT some emoji please", "react", "some"},
		{"I think the assistant should reply", "", ""},
		{"maybe", "", ""},
	} {
		action, emoji := parseWatchVerdict(tc.in)
		if action != tc.action || emoji != tc.emoji {
			t.Errorf("%q → (%q, %q), want (%q, %q)", tc.in, action, emoji, tc.action, tc.emoji)
		}
	}
}

// "Respond automatically" was retired into "read every message". A database written before that
// still holds both the per-scope column and the global default, and neither may quietly go off:
// a channel someone had switched on has to keep answering under the setting that is left.
func TestFoldAutoRespondIntoReadAll(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	skipUnlessSQLite(t, st)
	// A fresh database has no such column, so give it the old shape back.
	if _, err := st.db.Exec(`alter table scopes add column auto_respond text default 'inherit'`); err != nil {
		t.Fatalf("old shape: %v", err)
	}
	set := func(sc *Scope, auto string) {
		if _, err := st.db.Exec(`update scopes set auto_respond=? where id=?`, auto, sc.ID); err != nil {
			t.Fatalf("seed %q: %v", auto, err)
		}
	}
	autoOf := func(id int64) string {
		var v string
		if err := st.db.QueryRow(`select coalesce(auto_respond,'') from scopes where id=?`, id).Scan(&v); err != nil {
			t.Fatalf("read back: %v", err)
		}
		return v
	}
	readAllOf := func(orgID int64, teamID, channel string) string {
		sc, err := st.ChannelScope(ctx, orgID, teamID, channel)
		if err != nil || sc == nil {
			t.Fatalf("scope %d/%s: %v", orgID, channel, err)
		}
		return sc.ReadAll
	}

	on, _ := st.UpsertChannelScope(ctx, 1, "T1", "C_ON", "#on", false)
	set(on, "on")
	said, _ := st.UpsertChannelScope(ctx, 1, "T1", "C_SAID_NO", "#said-no", false)
	set(said, "on")
	st.SetScopeReadAll(ctx, 1, said.ID, "off")
	off, _ := st.UpsertChannelScope(ctx, 1, "T1", "C_OFF", "#off", false)
	set(off, "off")
	// Organisation 2 had only the global default on, and no account scope to hang it from.
	st.UpsertChannelScope(ctx, 2, "T2", "C_INHERITS", "#inherits", false)
	if _, err := st.db.Exec(`insert into settings (org_id, key, value) values (2, 'unprompted_replies', '1')`); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	if err := foldAutoRespondIntoReadAll(st.db); err != nil {
		t.Fatalf("fold: %v", err)
	}
	if got := readAllOf(1, "T1", "C_ON"); got != "on" {
		t.Errorf("a channel that answered automatically should keep answering, got %q", got)
	}
	if got := readAllOf(1, "T1", "C_SAID_NO"); got != "off" {
		t.Errorf("an explicit off is an answer already given, got %q", got)
	}
	if got := readAllOf(1, "T1", "C_OFF"); got != "inherit" {
		t.Errorf("a channel that was off has nothing to carry over, got %q", got)
	}
	b := &Bot{store: st}
	if !b.readAll(ctx, 2, "T2", "C_INHERITS") {
		t.Error("the global default should survive as the account scope every channel inherits")
	}
	var settings int
	st.db.QueryRow(`select count(*) from settings where key='unprompted_replies'`).Scan(&settings)
	if settings != 0 {
		t.Errorf("the retired setting should be gone, %d row(s) left", settings)
	}

	// Running twice must not undo what an admin decided in between: the fold blanks what it
	// read, so the second pass has nothing to carry over.
	st.SetScopeReadAll(ctx, 1, on.ID, "off")
	if err := foldAutoRespondIntoReadAll(st.db); err != nil {
		t.Fatalf("second fold: %v", err)
	}
	if got := readAllOf(1, "T1", "C_ON"); got != "off" {
		t.Errorf("a later off must stand, got %q", got)
	}
	if got := autoOf(on.ID); got != "" {
		t.Errorf("the old column should read as blank once folded, got %q", got)
	}
}
