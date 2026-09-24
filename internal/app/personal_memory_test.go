package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The rule this whole feature rests on: a note reaches exactly one prompt, the one belonging to
// the person who wrote it, on a turn they are present for. Every case below is a way it might
// reach a second one -- and most of them are turns that carry a real Slack user id, which is why
// the id alone was never the question.
func TestOnlyAPersonPresentOnTheTurnOwnsAnyNotes(t *testing.T) {
	sl := &Chat{BotUserID: "UBOT", TeamID: "T1"}
	base := func() *Call {
		return &Call{OrgID: 1, TeamID: "T1", SL: sl, Channel: "C1", UserID: "UALICE", Kind: "channel", HumanTurn: true}
	}

	for _, tc := range []struct {
		name string
		why  string
		fix  func(*Call)
	}{
		{"a routine running as its author", "routines.go runs as r.CreatedBy: a real id, months old, posting unattended",
			func(c *Call) { c.HumanTurn, c.Kind = false, "routine" }},
		{"an investigation that inherited a channel Kind", "investigations.go takes sess.Kind, so it looks like a person asking",
			func(c *Call) { c.HumanTurn = false }},
		{"a console preview", "playground.go runs as whoever opened the console, and returns the answer to them over HTTP",
			func(c *Call) { c.Preview = true }},
		{"a quiet routine", "no asker at all, and nothing it says is read by a person in the moment",
			func(c *Call) { c.Silent = true }},
		{"a turn attributed to the bot", "four fallbacks land here; without it the bot owns a bucket every unowned turn shares",
			func(c *Call) { c.UserID = sl.BotUserID }},
		{"a turn with no requester", "no requester means nobody, not everybody",
			func(c *Call) { c.UserID = "" }},
		{"a turn with no workspace", "Slack ids are only unique inside one, so without it the owner is ambiguous",
			func(c *Call) { c.TeamID = "" }},
		{"a turn with no organisation", "every other per-org row carries it and this one is no different",
			func(c *Call) { c.OrgID = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.fix(c)
			if k, ok := c.personalKey(); ok {
				t.Errorf("%s got the notes of %q: %s", tc.name, k.owner, tc.why)
			}
		})
	}

	// And the two turns that are a person: a message in a channel, and one in a DM.
	for _, kind := range []string{"channel", "dm"} {
		c := base()
		c.Kind = kind
		k, ok := c.personalKey()
		if !ok {
			t.Fatalf("a person typing in a %s turn should own their notes", kind)
		}
		if k.orgID != 1 || k.teamID != "T1" || k.owner != "UALICE" {
			t.Errorf("key is %+v, want all three of org, workspace and person", k)
		}
	}
}

// Slack only promises user ids are unique inside a workspace, so the same U… in two workspaces is
// two people. tenancy_test.go holds the same line for channels and sessions.
func TestTheSameSlackIdInTwoWorkspacesIsTwoPeople(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	one := personalKey{orgID: 1, teamID: "T1", owner: "U1"}
	two := personalKey{orgID: 1, teamID: "T2", owner: "U1"}
	if _, err := st.AddPersonalMemory(ctx, one, "the T1 person's note"); err != nil {
		t.Fatal(err)
	}
	got, err := st.PersonalMemories(ctx, two)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the same id in another workspace read %d note(s); it is a different person", len(got))
	}
}

// The three predicates, one at a time: owner, workspace, organisation.
func TestANoteIsReadableOnlyByItsOwner(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	mine := personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}
	if _, err := st.AddPersonalMemory(ctx, mine, "I owe Priya the Q3 numbers"); err != nil {
		t.Fatal(err)
	}
	for _, other := range []personalKey{
		{orgID: 1, teamID: "T1", owner: "UBOB"},
		{orgID: 1, teamID: "T2", owner: "UALICE"},
		{orgID: 2, teamID: "T1", owner: "UALICE"},
	} {
		got, err := st.PersonalMemories(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("%+v read %d of Alice's notes", other, len(got))
		}
	}
	if got, _ := st.PersonalMemories(ctx, mine); len(got) != 1 {
		t.Fatalf("the owner reads %d of their own notes, want 1", len(got))
	}
}

// A personal note must not appear in any organisation-wide read. With a table of its own this is
// true by construction rather than by a filter somebody has to remember -- which is exactly what
// this test is pinning, so that a later change back to a shared table fails here first.
func TestAPersonalNoteNeverReachesAnOrgWideRead(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	const secret = "I am interviewing at another company on Thursday"
	if _, err := st.AddPersonalMemory(ctx, personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}, secret); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMemory(ctx, 1, "T1", teamMemoryScope("T1"), "standup is at 10:45", "UBOB"); err != nil {
		t.Fatal(err)
	}
	all, err := st.AllMemories(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Text == secret {
			t.Fatal("AllMemories returned a personal note; it feeds GET /api/memories and GET /v1/memories, neither of which checks a permission")
		}
	}
	if len(all) != 1 {
		t.Fatalf("AllMemories returned %d rows, want only the workspace one", len(all))
	}
	mems, err := st.Memories(ctx, 1, channelMemoryScope("T1", "C1"), teamMemoryScope("T1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if m.Text == secret {
			t.Fatal("the prompt's memory read returned a personal note")
		}
	}
	if n := st.countRows(ctx, "memories", 1); n != 1 {
		t.Errorf("the org memory count is %d; personal notes must not spend the organisation's cap or show on its Overview tile", n)
	}
}

// A note is the only copy of something nobody else can see, so every write path refuses an
// unowned key outright instead of quietly succeeding or quietly doing nothing.
func TestAnUnownedKeyCannotWriteOrDelete(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	var none personalKey
	if _, err := st.AddPersonalMemory(ctx, none, "belongs to nobody"); err == nil {
		t.Error("an unowned key saved a note")
	}
	if _, err := st.UpdatePersonalMemory(ctx, none, 1, "rewritten"); err == nil {
		t.Error("an unowned key rewrote a note")
	}
	if _, err := st.DeletePersonalMemory(ctx, none, 1); err == nil {
		t.Error("an unowned key deleted a note")
	}
}

// Editing and deleting are keyed by owner as well as id, so another person's id is missing rather
// than forbidden -- and, more to the point, is not theirs to change.
func TestOnlyTheOwnerCanEditOrDeleteTheirNote(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	alice := personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}
	bob := personalKey{orgID: 1, teamID: "T1", owner: "UBOB"}
	id, err := st.AddPersonalMemory(ctx, alice, "dentist on Tuesday")
	if err != nil {
		t.Fatal(err)
	}
	if found, err := st.UpdatePersonalMemory(ctx, bob, id, "call the bank instead"); err != nil || found {
		t.Errorf("Bob rewrote Alice's note (found=%v, err=%v)", found, err)
	}
	if found, err := st.DeletePersonalMemory(ctx, bob, id); err != nil || found {
		t.Errorf("Bob deleted Alice's note (found=%v, err=%v)", found, err)
	}
	if v, _ := st.PersonalMemoryByID(ctx, bob, id); v != nil {
		t.Error("Bob read Alice's note by id")
	}
	v, err := st.PersonalMemoryByID(ctx, alice, id)
	if err != nil || v == nil || v.Text != "dentist on Tuesday" {
		t.Fatalf("Alice's own note came back as %+v (err=%v)", v, err)
	}
	if found, err := st.DeletePersonalMemory(ctx, alice, id); err != nil || !found {
		t.Errorf("the owner could not delete their own note (found=%v, err=%v)", found, err)
	}
}

// One person's list must not be the reason the organisation stops remembering things.
func TestOnePersonCannotSpendTheOrganisationsMemoryBudget(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	k := personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}
	for i := 0; i < maxPersonalNotesPerUser; i++ {
		if _, err := st.AddPersonalMemory(ctx, k, "note"); err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	if _, err := st.AddPersonalMemory(ctx, k, "one too many"); err != errPersonalCap {
		t.Errorf("the personal cap gave %v, want errPersonalCap", err)
	}
	// The organisation's own memory is untouched by all of that.
	if err := st.AddMemory(ctx, 1, "T1", teamMemoryScope("T1"), "standup is at 10:45", "UBOB"); err != nil {
		t.Errorf("a full personal list blocked an organisation memory: %v", err)
	}
	// And another person still has their whole allowance.
	if _, err := st.AddPersonalMemory(ctx, personalKey{orgID: 1, teamID: "T1", owner: "UBOB"}, "mine"); err != nil {
		t.Errorf("one person's full list blocked another's first note: %v", err)
	}
}

// A workspace that is gone takes its notes with it: the key is (org, team, owner) and nothing
// will ever read them again. DeleteUserConnectionsFor is the precedent.
func TestDisconnectingAWorkspaceTakesItsNotesWithIt(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	gone := personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}
	kept := personalKey{orgID: 1, teamID: "T2", owner: "UALICE"}
	if _, err := st.AddPersonalMemory(ctx, gone, "in the workspace being removed"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPersonalMemory(ctx, kept, "in the other one"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePersonalMemoriesForTeam(ctx, 1, "T1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PersonalMemories(ctx, gone); len(got) != 0 {
		t.Errorf("%d note(s) outlived their workspace", len(got))
	}
	if got, _ := st.PersonalMemories(ctx, kept); len(got) != 1 {
		t.Errorf("removing one workspace took another's notes")
	}
	if err := st.DeletePersonalMemoriesForTeam(ctx, 1, ""); err == nil {
		t.Error("an empty workspace id was accepted; that statement would take every note in the org")
	}
}

func budgetAgent(t *testing.T, st *Store) *Agent {
	t.Helper()
	return NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
}

// The companion to TestClockIsOutsideTheCacheablePrompt, and for the same reason. The cacheable
// half is shared by everybody in a channel; anything per-person in it gives each of them their own
// copy of the whole prefix, so one cache read for the room becomes one cache write each.
func TestPrivateNotesAreOutsideTheCacheablePrompt(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := budgetAgent(t, st)
	const secret = "I owe Priya the Q3 numbers"
	k := personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}
	if _, err := st.AddPersonalMemory(ctx, k, secret); err != nil {
		t.Fatal(err)
	}
	dm := func(user string) *Call {
		return &Call{OrgID: 1, TeamID: "T1", Channel: "D1", ThreadTS: "1", UserID: user, Kind: "dm", HumanTurn: true}
	}
	for _, kind := range []string{"dm", "channel", "routine"} {
		c := dm("UALICE")
		c.Kind = kind
		if strings.Contains(a.systemPrompt(ctx, c), secret) {
			t.Errorf("a private note reached the cacheable prompt on a %s turn", kind)
		}
	}
	if got := a.volatilePrompt(ctx, dm("UALICE")); !strings.Contains(got, secret) {
		t.Error("the owner's own DM did not carry their notes")
	}
	if got := a.volatilePrompt(ctx, dm("UBOB")); strings.Contains(got, secret) {
		t.Error("somebody else's DM carried Alice's notes")
	}
	// A channel turn gets nothing injected at all: whatever is said there is read by everyone,
	// replayed on later turns, and eventually folded into the thread summary.
	inChannel := dm("UALICE")
	inChannel.Kind, inChannel.Channel = "channel", "C1"
	if got := a.volatilePrompt(ctx, inChannel); strings.Contains(got, secret) {
		t.Error("a shared channel's prompt carried a private note; only recall_personal may fetch one there")
	}
	// And the lane that carries a real Slack id with nobody behind it.
	routine := dm("UALICE")
	routine.Kind, routine.HumanTurn = "routine", false
	if got := a.volatilePrompt(ctx, routine); strings.Contains(got, secret) {
		t.Error("a routine running as its author carried their private notes")
	}
}

// Which tools a turn is offered is the other half of the same rule.
func TestOnlyAPersonIsOfferedTheirOwnNoteTools(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := budgetAgent(t, st)
	k := personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}
	if _, err := st.AddPersonalMemory(ctx, k, "dentist on Tuesday"); err != nil {
		t.Fatal(err)
	}
	has := func(c *Call, name string) bool {
		_, ok := a.toolsFor(ctx, c)[name]
		return ok
	}
	person := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", UserID: "UALICE", Kind: "channel", HumanTurn: true}
	for _, name := range []string{"remember_personal", "recall_personal", "forget_personal"} {
		if !has(person, name) {
			t.Errorf("a person typing was not offered %s", name)
		}
	}
	for _, tc := range []struct {
		name string
		fix  func(*Call)
	}{
		{"a routine", func(c *Call) { c.HumanTurn, c.Kind = false, "routine" }},
		{"an investigation", func(c *Call) { c.HumanTurn = false }},
		{"a preview", func(c *Call) { c.Preview = true }},
		{"a quiet run", func(c *Call) { c.Silent = true }},
	} {
		c := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", UserID: "UALICE", Kind: "channel", HumanTurn: true}
		tc.fix(c)
		for _, name := range []string{"remember_personal", "recall_personal", "forget_personal"} {
			if has(c, name) {
				t.Errorf("%s was offered %s", tc.name, name)
			}
		}
	}
	// Somebody with nothing saved carries one definition, not three: the list is re-sent on
	// every round of every turn, and there is nothing for them to recall or forget yet.
	fresh := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", UserID: "UNEW", Kind: "channel", HumanTurn: true}
	if !has(fresh, "remember_personal") {
		t.Error("somebody with no notes could not save their first one")
	}
	if has(fresh, "recall_personal") || has(fresh, "forget_personal") {
		t.Error("somebody with no notes was charged for tools with nothing to act on")
	}
}

// Even if the model returns a name that was never offered, the handler refuses. toolsFor shapes
// what the model sees; it is not what decides who owns a row.
func TestANoteToolRefusesWhenNobodyOwnsTheTurn(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := budgetAgent(t, st)
	tools := a.personalMemoryTools(personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}, true)
	routine := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", UserID: "UALICE", Kind: "routine"}
	for _, tl := range tools {
		if _, err := tl.Run(ctx, routine, json.RawMessage(`{"note":"x","id":1}`)); err == nil {
			t.Errorf("%s ran on a turn nobody owns", tl.Name)
		}
	}
	// And the refusal says so rather than returning an empty list, which reads as "you have
	// nothing saved" and invites the model to try again.
	if _, err := tools[0].Run(ctx, routine, json.RawMessage(`{"note":"x"}`)); err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Errorf("the refusal was %v, want one that says there is nobody to save it for", err)
	}
}

// The leak a tool-based design creates, and the one that would have undone the whole feature
// quietly. runTool writes every call's arguments and result into tool_calls, which /activity
// renders to anyone holding activity.view — a viewer holds it — and /api/activity.csv downloads
// whole. redact() strips secret-shaped tokens and does nothing for a sentence about a person.
func TestAPrivateNoteIsNotWrittenToTheToolCallLog(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := budgetAgent(t, st)
	const secret = "I am interviewing at another company on Thursday"
	// One note already saved, so this turn is offered all three tools. The count that decides
	// that is taken once per turn, so somebody saving their very first note gets recall_personal
	// on their next turn rather than this one — which costs nothing, since they have just been
	// told what it says.
	if _, err := st.AddPersonalMemory(ctx, personalKey{orgID: 1, teamID: "T1", owner: "UALICE"}, "dentist on Tuesday"); err != nil {
		t.Fatal(err)
	}
	c := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1", UserID: "UALICE", Kind: "dm",
		HumanTurn: true, Session: &Session{}, Streamer: &Streamer{silent: true}}

	out, ok := a.runToolRaw(ctx, c, "remember_personal", `{"note":`+strconv.Quote(secret)+`}`)
	if !ok {
		t.Fatalf("saving failed: %s", out)
	}
	if strings.Contains(out, secret) {
		t.Error("the tool told the model back what it had just saved; there is no reason to send it twice")
	}
	// And reading them back, which is the call whose result is the whole list.
	if _, ok := a.runToolRaw(ctx, c, "recall_personal", `{}`); !ok {
		t.Fatal("recall failed")
	}
	rows := st.ToolCallsAfter(ctx, 1, "1", 0)
	if len(rows) != 2 {
		t.Fatalf("logged %d calls, want 2 — the call itself must still be recorded", len(rows))
	}
	for _, r := range rows {
		if strings.Contains(r.Args, secret) || strings.Contains(r.Result, secret) {
			t.Errorf("%s wrote a private note into tool_calls, which /activity and activity.csv both read", r.Name)
		}
		if r.Name == "" || !r.OK {
			t.Errorf("%+v: the call must still be auditable — name, outcome and duration", r)
		}
	}
}

// The activity card goes into the channel, where everyone reads it. humanTitle's fallthrough
// prints an argument, so these three need arms of their own — and their parameters are
// deliberately not named query, url, repo, list_id or task_id, which is what that arm picks up.
func TestTheActivityCardDoesNotQuoteAPrivateNote(t *testing.T) {
	const secret = "I owe Priya the Q3 numbers"
	for _, tc := range []struct{ name, args string }{
		{"remember_personal", `{"note":"` + secret + `"}`},
		{"recall_personal", `{}`},
		{"forget_personal", `{"id":3}`},
	} {
		got := humanTitle(tc.name, json.RawMessage(tc.args), nil)
		if strings.Contains(got, secret) || strings.Contains(got, "3") {
			t.Errorf("%s card reads %q; it is posted where everyone can read it", tc.name, got)
		}
		if got == "" || strings.Contains(got, "_") {
			t.Errorf("%s card reads %q, want a phrase written for a reader", tc.name, got)
		}
	}
}

// A pre-existing bug this change sits next to, so it is fixed and pinned here. ForgetMemory
// escapes % and _ after forget("%") once emptied a scope, but an empty needle builds "%%", which
// matches every row and is reached by leaving the argument out rather than by choosing it. There
// was no test for ForgetMemory at all.
func TestForgettingNothingForgetsNothing(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	scope := channelMemoryScope("T1", "C1")
	for _, text := range []string{"standup is at 10:45", "the staging cluster rebuilds on Friday"} {
		if err := st.AddMemory(ctx, 1, "T1", scope, text, "U1"); err != nil {
			t.Fatal(err)
		}
	}
	for _, needle := range []string{"", "   ", "\t\n"} {
		if n, err := st.ForgetMemory(ctx, 1, scope, needle); n != 0 || err == nil {
			t.Errorf("forget(%q) removed %d memor(y/ies) and gave err=%v", needle, n, err)
		}
	}
	// The wildcards it was already taught, still refused as wildcards.
	if n, _ := st.ForgetMemory(ctx, 1, scope, "%"); n != 0 {
		t.Errorf(`forget("%%") removed %d`, n)
	}
	if got, _ := st.Memories(ctx, 1, scope); len(got) != 2 {
		t.Fatalf("%d memories survived, want 2", len(got))
	}
	// And a real needle still works.
	if n, err := st.ForgetMemory(ctx, 1, scope, "standup"); n != 1 || err != nil {
		t.Errorf("forgetting by its words removed %d (err=%v), want 1", n, err)
	}
}

// The other half of the same read: when an organisation is over the prompt's memory cap, the
// channel's own memories are the ones to keep. Memories returns channel-scoped rows first, so
// taking the last N kept the workspace's and dropped the room's.
func TestTheChannelsOwnMemoriesSurviveTheCap(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := budgetAgent(t, st)
	for i := 0; i < promptMemoryMax; i++ {
		if err := st.AddMemory(ctx, 1, "T1", teamMemoryScope("T1"), fmt.Sprintf("workspace fact %d", i), "U2"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddMemory(ctx, 1, "T1", channelMemoryScope("T1", "C1"), "this room deploys on Tuesdays", "U1"); err != nil {
		t.Fatal(err)
	}
	got := a.systemPrompt(ctx, &Call{OrgID: 1, TeamID: "T1", Channel: "C1", UserID: "U1", Kind: "channel", HumanTurn: true})
	if !strings.Contains(got, "this room deploys on Tuesdays") {
		t.Error("the channel's own memory was evicted in favour of workspace-wide ones")
	}
}

// The console and developer surfaces. The role matters here: GET /api/memories is wrapped in
// requireAdmin, which despite its name is any authenticated session, and GET /v1/memories is any
// valid key with no permission check. A test that only exercised an admin would miss the whole
// exposure, because the exposure is that these two are not admin-only.
func TestPrivateNotesAreNotOnTheOrganisationsMemorySurfaces(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	u, orgID, session := signedUp(t, b, mux, st, "alice@example.com")
	if err := st.SaveTeam(ctx, &Team{TeamID: "T1", OrgID: orgID, Name: "Acme"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddIdentity(ctx, u.ID, ProviderSlack, slackSubject("T1", "UALICE")); err != nil {
		t.Fatal(err)
	}
	const secret = "I am interviewing at another company on Thursday"
	if _, err := st.AddPersonalMemory(ctx, personalKey{orgID: orgID, teamID: "T1", owner: "UALICE"}, secret); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMemory(ctx, orgID, "T1", teamMemoryScope("T1"), "standup moved to 10:45", "UBOB"); err != nil {
		t.Fatal(err)
	}

	code, body := authReq(t, mux, "GET", "/api/memories", nil, session)
	if code != 200 {
		t.Fatalf("GET /api/memories = %d: %v", code, body)
	}
	if s := fmt.Sprint(body); strings.Contains(s, secret) {
		t.Error("a private note is on GET /api/memories, which any signed-in member of any role can read")
	}
	key, _ := mintKey(t, mux, session, "a script")
	req := httptest.NewRequest("GET", "/v1/memories", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /v1/memories = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Error("a private note is on GET /v1/memories, which any valid key can read")
	}
	// And the organisation's own edit routes cannot reach one by id. Note what makes that true:
	// the two tables number their rows separately, so the id of a private note is an ordinary id
	// in `memories` naming something else entirely. There is no addressing between them to guard,
	// which is the separate table earning its place rather than a check somebody has to maintain.
	mine := personalKey{orgID: orgID, teamID: "T1", owner: "UALICE"}
	notes, _ := st.PersonalMemories(ctx, mine)
	id := notes[0].ID
	authReq(t, mux, "PUT", "/api/memories/"+itoa(id), map[string]string{"text": "planted"}, session)
	authReq(t, mux, "DELETE", "/api/memories/"+itoa(id), nil, session)
	left, _ := st.PersonalMemories(ctx, mine)
	if len(left) != 1 || left[0].Text != secret {
		t.Errorf("the organisation's memory routes changed a private note: %+v", left)
	}
}

// The owner's own page: it reaches their notes through a proved Slack identity, and somebody with
// no Slack account linked gets an empty list and an explanation rather than an error or a guess.
func TestTheNotesPageReachesOnlyYourOwnSlackAccounts(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	alice, orgID, aliceSession := signedUp(t, b, mux, st, "alice@example.com")
	if err := st.SaveTeam(ctx, &Team{TeamID: "T1", OrgID: orgID, Name: "Acme"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddIdentity(ctx, alice.ID, ProviderSlack, slackSubject("T1", "UALICE")); err != nil {
		t.Fatal(err)
	}

	// Saving, listing and deleting her own.
	if code, body := authReq(t, mux, "POST", "/api/personal-memories",
		map[string]string{"team_id": "T1", "text": "I owe Priya the Q3 numbers"}, aliceSession); code != 201 {
		t.Fatalf("saving her own note = %d: %v", code, body)
	}
	code, body := authReq(t, mux, "GET", "/api/personal-memories", nil, aliceSession)
	if code != 200 || !strings.Contains(fmt.Sprint(body), "Priya") {
		t.Fatalf("listing her own notes = %d: %v", code, body)
	}
	notes, _ := st.PersonalMemories(ctx, personalKey{orgID: orgID, teamID: "T1", owner: "UALICE"})
	if len(notes) != 1 {
		t.Fatalf("stored %d notes, want 1", len(notes))
	}

	// A second member of the same organisation sees none of it. Same org on purpose: sharing an
	// organisation is what makes people colleagues, and it is the boundary that has to hold.
	bob, _, bobSession := signedUp(t, b, mux, st, "bob@example.com")
	if err := st.AddMembership(ctx, bob.ID, orgID, "admin", alice.ID); err != nil {
		t.Fatal(err)
	}
	bobSession, err := st.CreateAdminSession(ctx, AdminUser{ID: bob.ID, Name: bob.Name, Email: bob.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := authReq(t, mux, "GET", "/api/personal-memories", nil, bobSession); code != 200 {
		t.Fatalf("Bob's list = %d: %v", code, body)
	} else if strings.Contains(fmt.Sprint(body), "Priya") {
		t.Error("Bob read Alice's private notes from the console")
	}
	// Including by id, which is the only handle he could guess.
	if code, _ := authReq(t, mux, "PUT", "/api/personal-memories/"+itoa(notes[0].ID),
		map[string]string{"text": "planted"}, bobSession); code != 404 {
		t.Errorf("Bob rewriting Alice's note = %d, want 404", code)
	}
	if code, _ := authReq(t, mux, "DELETE", "/api/personal-memories/"+itoa(notes[0].ID), nil, bobSession); code != 404 {
		t.Errorf("Bob deleting Alice's note = %d, want 404", code)
	}
	if left, _ := st.PersonalMemories(ctx, personalKey{orgID: orgID, teamID: "T1", owner: "UALICE"}); len(left) != 1 {
		t.Fatal("Alice's note did not survive Bob")
	}
	// Bob has no Slack identity at all: an empty list and no identities, not a 500 and not a guess.
	if _, body := authReq(t, mux, "GET", "/api/personal-memories", nil, bobSession); body["identities"] != nil {
		if ids, ok := body["identities"].([]any); ok && len(ids) != 0 {
			t.Errorf("an account with no linked Slack reported %d identities", len(ids))
		}
	}
	// And she can delete her own.
	if code, _ := authReq(t, mux, "DELETE", "/api/personal-memories/"+itoa(notes[0].ID), nil, aliceSession); code != 200 {
		t.Errorf("Alice could not delete her own note = %d", code)
	}
}

// The link that edits a note is a bearer capability: whoever holds it can read and rewrite that
// person's notes. So it goes by DM and never into a channel, and never into a tool result, which
// is prompt text and travels to whoever serves the model.
func TestANoteLinkGoesByDMAndNeverThroughTheModel(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := budgetAgent(t, st)
	c := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1", UserID: "UALICE", Kind: "channel",
		HumanTurn: true, Session: &Session{}, Streamer: &Streamer{silent: true}}
	out, ok := a.runToolRaw(ctx, c, "remember_personal", `{"note":"I owe Priya the Q3 numbers"}`)
	if !ok {
		t.Fatalf("saving failed: %s", out)
	}
	for _, bad := range []string{"http://", "https://", "tab=memory", "?t="} {
		if strings.Contains(out, bad) {
			t.Errorf("the tool result carries %q; a link in a tool result travels to the model host", bad)
		}
	}
	if !c.personalMemoryTouched {
		t.Error("saving a note did not ask for the link that edits it")
	}
	// The shared tools flag the other link, and the two are never confused.
	if c.memoryTouched {
		t.Error("a private note asked for the channel's shared link, which everyone can read")
	}
	shared := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1", UserID: "UALICE", Kind: "channel",
		HumanTurn: true, Session: &Session{}, Streamer: &Streamer{silent: true}}
	if out, ok := a.runToolRaw(ctx, shared, "remember", `{"text":"standup moved to 10:45","scope":"channel"}`); !ok {
		t.Fatalf("saving a channel memory failed: %s", out)
	}
	if !shared.memoryTouched || shared.personalMemoryTouched {
		t.Errorf("a channel memory asked for the wrong link (shared=%v personal=%v)",
			shared.memoryTouched, shared.personalMemoryTouched)
	}
	// And a turn nobody owns asks for neither, so postMemoryLink has nothing to send.
	routine := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1", UserID: "UALICE", Kind: "routine",
		Session: &Session{}, Streamer: &Streamer{silent: true}}
	a.runToolRaw(ctx, routine, "remember_personal", `{"note":"x"}`)
	if routine.personalMemoryTouched {
		t.Error("a routine asked for a personal link")
	}
}

// The Memory tab draws two lists, and which one a visitor sees depends on what their link proves.
// The shared Configure link is printed under every reply and read by everyone who can see the
// channel, so it must not be able to render anybody's private notes.
func TestTheMemoryTabShowsYourNotesOnlyToAPersonalLink(t *testing.T) {
	render := func(asUser string, memMine bool) string {
		var b strings.Builder
		err := configurePage.Execute(&b, map[string]any{
			"Bot": "Attest", "Scope": &Scope{Name: "#ops", SlackID: "C1"}, "Token": "tok", "CSRF": "csrf",
			"Tab": "memory", "Saved": false, "ReadOnly": false, "AsUser": asUser, "HasPersonal": false,
			// SubMine is only which side was asked for; what it may show is gated on AsUser
			// inside the template, which is the thing worth testing.
			"SubMine":  memMine,
			"Memories": []memoryRow{{ID: 1, Text: "standup moved to 10:45", Where: "this channel", At: "2026-09-11"}},
			"MyNotes":  []memoryRow{{ID: 7, Text: "I owe Priya the Q3 numbers", Where: "only you", At: "2026-09-11"}},
			"Access":   nil, "Packs": nil, "Routines": nil, "AllowRules": nil, "Personal": nil,
			"DefaultModel": "m", "HeavyModel": "", "ChannelModels": nil, "OtherModel": "",
			"MaxRounds": 80, "RoutineRounds": 4, "RoutineMinutes": 5,
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return b.String()
	}

	shared := render("", false)
	if strings.Contains(shared, "I owe Priya") {
		t.Error("the shared Configure link rendered somebody's private notes; everyone in the channel holds that link")
	}
	if !strings.Contains(shared, "standup moved to 10:45") {
		t.Error("the shared link could not see the channel's own memories, which is what it is for")
	}
	// The Yours sub-tab is offered on every link, so the tab is discoverable...
	if !strings.Contains(shared, "tab=memory&sub=mine") {
		t.Error("no Yours sub-tab offered; nobody would discover personal notes exist")
	}

	// The sub-tab is a query parameter, so it is worth proving it cannot be typed into a shared
	// link. Without the AsUser gate this would render a personal section on everybody's link.
	// ...and following it on a shared link explains how to get a link of your own rather than
	// rendering somebody's notes, which is the whole point of the AsUser gate.
	forged := render("", true)
	if strings.Contains(forged, "I owe Priya") {
		t.Error("&sub=mine on a shared link rendered somebody's private notes")
	}
	if !strings.Contains(forged, "!notes") {
		t.Error("the Yours tab on a shared link does not say how to get a link of your own")
	}

	personal := render("UALICE", true)
	if !strings.Contains(personal, "I owe Priya") {
		t.Error("a personal link could not see the notes it exists to edit")
	}
	if !strings.Contains(personal, "Private to you") {
		t.Error("the private list is not labelled as private")
	}
	// The channel's own memories are one click away on the other sub-tab, not gone.
	if channelSide := render("UALICE", false); !strings.Contains(channelSide, "standup moved to 10:45") {
		t.Error("a personal link lost the channel's memories on the channel sub-tab")
	}
	// And deleting is a button now, not a blank box nobody could guess at.
	if !strings.Contains(personal, `value="delete"`) {
		t.Error("no delete control on a note")
	}
}

// The two links are different things and are never interchangeable: one names a channel and is
// posted in it, the other names a person and is only ever DMed.
func TestAPersonalMemoryLinkNamesThePersonAndTheSharedOneDoesNot(t *testing.T) {
	t.Setenv("ADMIN_BASE_URL", "https://console.example.com")
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	st := testStore(t)
	a := budgetAgent(t, st)
	ctx := context.Background()

	shared := a.configureURL(ctx, 1, "T1", "C1")
	personal := a.personalMemoryURL(ctx, 1, "T1", "C1", "UALICE")
	if shared == "" || personal == "" {
		t.Fatalf("no links minted (shared=%q personal=%q)", shared, personal)
	}
	if !strings.Contains(personal, "tab=memory") {
		t.Errorf("the personal link does not open the Memory tab: %s", personal)
	}
	if strings.Contains(personal, "tab=personal") {
		t.Errorf("the personal link still opens the connections tab: %s", personal)
	}
	if shared == personal || strings.TrimSuffix(personal, "&tab=memory") == strings.TrimSuffix(shared, "&tab=memory") {
		t.Error("the shared and personal links carry the same token; one of them names a person and must not be postable in a channel")
	}
	// And a link for nobody is no link at all, rather than one that opens everybody's.
	if got := a.personalMemoryURL(ctx, 1, "T1", "C1", ""); got != "" {
		t.Errorf("a personal link was minted with no person on it: %s", got)
	}
}

// The Memory tab takes edits for two kinds of row, and the link decides which. A shared link is
// printed under every reply and held by everyone who can see the channel, so a form posted with
// it must not be able to reach one person's notes however the fields are filled in.
func TestASharedLinkCannotEditAPrivateNote(t *testing.T) {
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, _ := signedUp(t, b, mux, st, "alice@example.com")
	if err := st.SaveTeam(ctx, &Team{TeamID: "T1", OrgID: orgID, Name: "Acme"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	mine := personalKey{orgID: orgID, teamID: "T1", owner: "UALICE"}
	id, err := st.AddPersonalMemory(ctx, mine, "I owe Priya the Q3 numbers")
	if err != nil {
		t.Fatal(err)
	}

	// A real POST carries the page's CSRF cookie, so each one does the GET that mints it first.
	// Without this the test would pass by being rejected at the door, which proves nothing about
	// what the page does with a form it accepts.
	post := func(tok, kind string, id int64, text string) int {
		t.Helper()
		url := "/configure/T1/C1?t=" + tok + "&tab=memory"
		get := httptest.NewRecorder()
		mux.ServeHTTP(get, httptest.NewRequest("GET", url, nil))
		var jar []*http.Cookie
		csrf := ""
		for _, c := range get.Result().Cookies() {
			jar = append(jar, c)
			if c.Name == configureCSRFCookie {
				csrf = c.Value
			}
		}
		if csrf == "" {
			t.Fatalf("no CSRF cookie from %s", url)
		}
		form := "csrf=" + csrf + "&tab=memory&kind=" + kind + "&memory=" + itoa(id) + "&text=" + text
		r := httptest.NewRequest("POST", url, strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, c := range jar {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}

	shared := mintConfigureToken("T1", "C1", "", 0, time.Now())
	post(shared, "personal", id, "PLANTED")
	got, _ := st.PersonalMemoryByID(ctx, mine, id)
	if got == nil || got.Text != "I owe Priya the Q3 numbers" {
		t.Fatalf("a shared link changed a private note: %+v", got)
	}
	// Deleting is the same request with an empty box, and equally must not work.
	post(shared, "personal", id, "")
	if got, _ := st.PersonalMemoryByID(ctx, mine, id); got == nil {
		t.Fatal("a shared link deleted a private note")
	}

	// The person's own link does edit it -- otherwise this test proves only that the page is
	// broken. The id in it comes from the MAC over the link, never from the form.
	personal := mintConfigureToken("T1", "C1", "UALICE", 0, time.Now())
	post(personal, "personal", id, "rewritten+by+its+owner")
	got, _ = st.PersonalMemoryByID(ctx, mine, id)
	if got == nil || got.Text != "rewritten by its owner" {
		t.Fatalf("the owner's own link could not edit their note: %+v", got)
	}
	// And somebody else's link, which names a real person, reaches only their own nothing.
	bobs := mintConfigureToken("T1", "C1", "UBOB", 0, time.Now())
	post(bobs, "personal", id, "PLANTED+BY+BOB")
	got, _ = st.PersonalMemoryByID(ctx, mine, id)
	if got == nil || got.Text != "rewritten by its owner" {
		t.Fatalf("another person's link edited Alice's note: %+v", got)
	}
}

// "Your accounts" moved from a top-level tab into Tools and access, so each tab now has a
// channel side and a yours side. The old links keep working: every one !personal_instructions
// ever DMed carries tab=personal, and those outlive the rename.
func TestYourAccountsLivesUnderToolsAndOldLinksStillLand(t *testing.T) {
	render := func(tab string, asUser string, subMine bool, hasPersonal bool) string {
		var b strings.Builder
		err := configurePage.Execute(&b, map[string]any{
			"Bot": "Attest", "Scope": &Scope{Name: "#ops", SlackID: "C1"}, "Token": "tok", "CSRF": "csrf",
			"Tab": tab, "Saved": false, "ReadOnly": false, "AsUser": asUser, "HasPersonal": hasPersonal,
			"SubMine": subMine, "Memories": nil, "MyNotes": nil,
			"Personal": []personalRow{{ConnID: 3, Name: "Google Workspace", Hint: "how to use it"}},
			"Access":   nil, "Packs": nil, "Routines": nil, "AllowRules": nil,
			"DefaultModel": "m", "HeavyModel": "", "ChannelModels": nil, "OtherModel": "",
			"MaxRounds": 80, "RoutineRounds": 4, "RoutineMinutes": 5,
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return b.String()
	}

	// It is no longer one of the tabs across the top.
	top := render("general", "UALICE", false, true)
	if strings.Contains(top, "tab=personal") {
		t.Error("the top bar still offers a Your accounts tab")
	}
	// It is a sub-tab of Tools and access, offered only where the channel has per-person connections.
	tools := render("tools", "UALICE", false, true)
	if !strings.Contains(tools, "tab=tools&sub=mine") {
		t.Error("Tools and access does not offer the Your accounts sub-tab")
	}
	if none := render("tools", "UALICE", false, false); strings.Contains(none, "sub=mine") {
		t.Error("a channel with no per-person connection still offered Your accounts")
	}
	// And the sub-tab shows the per-person connection to the person it belongs to.
	mine := render("tools", "UALICE", true, true)
	if !strings.Contains(mine, "Google Workspace") {
		t.Error("the Your accounts sub-tab does not show the person's connections")
	}
	// A shared link following the same sub-tab gets the explanation, not somebody's settings.
	if shared := render("tools", "", true, true); !strings.Contains(shared, "!personal_instructions") {
		t.Error("a shared link on Your accounts does not say how to get a link of your own")
	}
}
