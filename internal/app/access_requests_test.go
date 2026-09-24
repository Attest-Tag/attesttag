package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func testRequest(t *testing.T, st *Store, requester string, approvers []string, calls ...string) *AccessRequest {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(calls))
	for _, c := range calls {
		raw = append(raw, json.RawMessage(c))
	}
	r := &AccessRequest{
		OrgID: orgID, TeamID: "T1",
		Channel: "C1", ThreadTS: "111.1", Requester: requester,
		Approver: approvers[0], Approvers: approvers,
		What: "read access to billing", Why: "setting up my environment", Ask: "I need billing access",
		Calls: raw,
	}
	if err := st.AddAccessRequest(context.Background(), r); err != nil {
		t.Fatalf("add: %v", err)
	}
	return r
}

// grantAccess builds the access an approved request runs under: one connection with a real sealed
// credential, so the proxy's injection path is exercised rather than stubbed around.
func grantAccess(t *testing.T, b *Bot, id int64, grants bool, host string) *Access {
	t.Helper()
	c, sec, err := b.buildConnection(&connectionInput{Name: "example", Preset: "github", CredType: "bearer",
		Secret: &Secret{Token: "tok"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := b.sealSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	c.ID, c.secretEnc, c.Status, c.AllowGrants = id, enc, "active", grants
	c.AllowedHosts, c.Methods = []string{host}, []string{"PUT"}
	return &Access{Rules: []Rule{{Conn: c}}, ToolPacks: map[string]bool{}}
}

// The record is the thing an approver's decision rests on, so every field has to survive the
// round trip — and the calls have to come back in the order they were approved in.
func TestAccessRequestRoundTrip(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	r := testRequest(t, st, "U_ASK", []string{"U_APP", "U_TWO"},
		`{"method":"PUT","url":"https://api.example.com/one","conn":7}`,
		`{"method":"POST","url":"https://api.example.com/two","conn":7}`)

	got, err := st.AccessRequest(ctx, orgID, r.ID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Requester != "U_ASK" || got.What != "read access to billing" || got.Status != "pending" {
		t.Fatalf("fields lost: %+v", got)
	}
	if len(got.Approvers) != 2 || got.Approvers[1] != "U_TWO" {
		t.Fatalf("approvers lost: %v", got.Approvers)
	}
	if len(got.Calls) != 2 || !strings.Contains(string(got.Calls[0]), "/one") || !strings.Contains(string(got.Calls[1]), "/two") {
		t.Fatalf("calls out of order or lost: %s", got.Calls)
	}
	if got.ExpiresAt <= got.CreatedAt {
		t.Fatalf("expiry not in the future: created %s expires %s", got.CreatedAt, got.ExpiresAt)
	}
}

// Two approvers can press at the same moment. Only one may win, or the grant runs twice.
func TestTakeAccessRequestOnce(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	r := testRequest(t, st, "U_ASK", []string{"U_APP", "U_TWO"}, `{"method":"PUT","url":"https://x/1"}`)

	first, err := st.TakeAccessRequest(ctx, orgID, r.ID, "U_APP", false)
	if err != nil || first == nil {
		t.Fatalf("first take should win: %v", err)
	}
	second, err := st.TakeAccessRequest(ctx, orgID, r.ID, "U_TWO", false)
	if err != nil {
		t.Fatalf("second take errored: %v", err)
	}
	if second != nil {
		t.Fatal("second take won too: the grant would run twice")
	}
	if first.DecidedBy != "U_APP" {
		t.Fatalf("decided_by not recorded: %q", first.DecidedBy)
	}
}

// Nobody approves their own request. This is enforced in three places; this is the one in SQL,
// which is the one no future call site can forget.
func TestNoSelfApproval(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	r := testRequest(t, st, "U_ASK", []string{"U_ASK", "U_APP"}, `{"method":"PUT","url":"https://x/1"}`)

	got, err := st.TakeAccessRequest(ctx, orgID, r.ID, "U_ASK", false)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if got != nil {
		t.Fatal("the requester approved their own request")
	}
	// And it is still open for somebody else.
	if other, _ := st.TakeAccessRequest(ctx, orgID, r.ID, "U_APP", false); other == nil {
		t.Fatal("refusing self-approval must not close the request")
	}
}

// Self-approval is a deliberate switch for a workspace testing the flow with one person. It has
// to actually work when it is on, be off otherwise, and leave a mark either way.
func TestSelfApprovalWhenAllowed(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	r := testRequest(t, st, "U_ASK", []string{"U_ASK"}, `{"method":"PUT","url":"https://x/1"}`)

	if got, _ := st.TakeAccessRequest(ctx, orgID, r.ID, "U_ASK", false); got != nil {
		t.Fatal("self-approval must be refused unless it is switched on")
	}
	got, err := st.TakeAccessRequest(ctx, orgID, r.ID, "U_ASK", true)
	if err != nil || got == nil {
		t.Fatalf("self-approval should be allowed when the switch is on: %v", err)
	}
	if !got.SelfApproved {
		t.Fatal("a self-approval must be recorded as one, or the audit trail reads like two people agreed")
	}
	// Still exactly once, switch or no switch.
	if again, _ := st.TakeAccessRequest(ctx, orgID, r.ID, "U_ASK", true); again != nil {
		t.Fatal("the claim is no longer idempotent with self-approval on")
	}
}

// An expired request cannot be approved, denied or taken.
func TestExpiredAccessRequest(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	r := testRequest(t, st, "U_ASK", []string{"U_APP"}, `{"method":"PUT","url":"https://x/1"}`)
	// Expired by the process, not by datetime('now','-1 hour'), which only SQLite has — and the
	// error is checked, because ignoring it is what let this pass on Postgres while the update
	// silently did nothing and an expired request was approved.
	if _, err := st.db.ExecContext(ctx, `update access_requests set expires_at=? where id=?`,
		nowMinus(time.Hour), r.ID); err != nil {
		t.Fatalf("expiring the request: %v", err)
	}

	if got, _ := st.TakeAccessRequest(ctx, orgID, r.ID, "U_APP", false); got != nil {
		t.Fatal("an expired request was approved")
	}
	gone, err := st.ExpireAccessRequests(ctx)
	if err != nil || len(gone) != 1 {
		t.Fatalf("sweep should have claimed exactly one row, got %d (%v)", len(gone), err)
	}
	// Claiming is guarded, so a second sweep — another instance mid-deploy — says nothing again.
	again, _ := st.ExpireAccessRequests(ctx)
	if len(again) != 0 {
		t.Fatalf("second sweep claimed %d rows: the requester would be told twice", len(again))
	}
}

// Withdrawal is the requester's own. One person saying "cancel" in a busy thread must not drop
// somebody else's request that happens to share it.
func TestCancelOnlyTakesYourOwn(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	mine := testRequest(t, st, "U_ASK", []string{"U_APP"}, `{"method":"PUT","url":"https://x/1"}`)
	theirs := testRequest(t, st, "U_OTHER", []string{"U_APP"}, `{"method":"PUT","url":"https://x/2"}`)

	gone, err := st.CancelAccessRequests(ctx, "T1", "C1", "111.1", "U_ASK")
	if err != nil || len(gone) != 1 || gone[0].ID != mine.ID {
		t.Fatalf("cancel took the wrong rows: %v (%v)", gone, err)
	}
	if got, _ := st.AccessRequest(ctx, orgID, theirs.ID); got == nil || got.Status != "pending" {
		t.Fatal("somebody else's request was withdrawn")
	}
}

// The in-thread confirm path is a different thing with a different window. If anyone ever
// "unifies" the two, this is the test that says no: a five-minute hold must still expire in five
// minutes, whatever the seven-day path does.
func TestPendingWritesStillExpireAtFiveMinutes(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	id, err := st.AddPendingWrite(ctx, orgID, "", "C1", "111.1", "U_ASK", `{"method":"PUT","url":"https://x/1"}`)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	// Backdated by the process, not by datetime('now','-10 minutes'), which only SQLite has.
	// The error is checked: an ignored one here made this test pass by writing nothing, and the
	// failure it eventually produced pointed at the expiry rather than at the update.
	if _, err := st.db.ExecContext(ctx, `update pending_writes set created_at=? where id=?`,
		nowMinus(10*time.Minute), id); err != nil {
		t.Fatalf("backdating the pending write: %v", err)
	}

	if raw, _ := st.TakePendingWriteByID(ctx, orgID, "", "C1", id); raw != "" {
		t.Fatal("a ten-minute-old pending write was still confirmable")
	}
}

// Who was tagged is deterministic input the model never sees. It has to survive the forms Slack
// actually sends, and it must not change what the model is shown.
func TestMentionedUsers(t *testing.T) {
	in := "<@U1> hi <@U2|alex> and <@U1> again <@W3ABC>"
	got := mentionedUsers(in)
	want := []string{"U1", "U2", "W3ABC"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// A tagged person has to reach the model by name. "@attest_tag please add @alex to my call" used
// to arrive as "please add to my call" — every mention deleted — and the only reply left was to
// ask who was meant. The bot's own mention still goes, and an id Slack will not name stays an id.
func TestNameMentions(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.FormValue("user") {
		case "U1":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U1","name":"alex","profile":{"display_name":"Alex Kim"}}}`)
		case "U2":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U2","name":"jordan","real_name":"Jordan Lee","profile":{}}}`)
		default:
			fmt.Fprint(w, `{"ok":false,"error":"user_not_found"}`)
		}
	}))
	defer srv.Close()
	sl := &Chat{t: &slackTransport{api: slack.New("fake", slack.OptionAPIURL(srv.URL+"/"))}, BotUserID: "UBOT"}

	in := "<@UBOT> please add <@U1> and <@U2|pari> and <@U3> to my call, <@U1>"
	want := "please add Alex Kim (<@U1>) and Jordan Lee (<@U2>) and <@U3> to my call, Alex Kim (<@U1>)"
	if got := strings.TrimSpace(sl.NameMentions(context.Background(), in)); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// U1 twice, U2 once, U3 once: a name is looked up once and then cached.
	if calls != 3 {
		t.Fatalf("users.info called %d times, want 3", calls)
	}
	// Text with nobody in it is handed back untouched.
	if got := sl.NameMentions(context.Background(), "no mentions here"); got != "no mentions here" {
		t.Fatalf("plain text rewritten: %q", got)
	}
}

// A message that names a crowd must not turn one turn into a burst of users.info calls.
func TestNameMentionsLookupBudget(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"user":{"id":%q,"profile":{"display_name":"person"}}}`, r.FormValue("user"))
	}))
	defer srv.Close()
	sl := &Chat{t: &slackTransport{api: slack.New("fake", slack.OptionAPIURL(srv.URL+"/"))}, BotUserID: "UBOT"}

	var b strings.Builder
	for i := range maxNameLookups + 5 {
		fmt.Fprintf(&b, "<@U%04d> ", i)
	}
	out := sl.NameMentions(context.Background(), b.String())
	if calls != maxNameLookups {
		t.Fatalf("users.info called %d times, want %d", calls, maxNameLookups)
	}
	// The ones past the cap keep their id, so they are still people the model can look up.
	if !strings.Contains(out, "<@U0024>") || strings.Contains(out, "person (<@U0024>)") {
		t.Fatalf("mention past the cap was not left alone: %q", out)
	}
}

func TestParseApprovers(t *testing.T) {
	ids, emails, bad := parseApprovers("U01ABCDEF, <@U02GHIJKL|alex>; ops@example.com  U01ABCDEF  nonsense")
	if len(ids) != 2 || ids[0] != "U01ABCDEF" || ids[1] != "U02GHIJKL" {
		t.Fatalf("ids: %v", ids)
	}
	if len(emails) != 1 || emails[0] != "ops@example.com" {
		t.Fatalf("emails: %v", emails)
	}
	if len(bad) != 1 || bad[0] != "nonsense" {
		t.Fatalf("bad should be reported, not swallowed: %v", bad)
	}
}

// The card is the whole security story: what an approver reads has to come from the stored
// payload, and text somebody else wrote must not be able to forge structure in it.
func TestAccessBlocksRenderFromThePayload(t *testing.T) {
	r := &AccessRequest{
		ID: 12, Requester: "U_ASK", Approvers: []string{"U_APP"},
		What: "admin on the billing org",
		Ask:  "I need this *urgently* <https://evil.example|https://api.github.com/safe>",
		Calls: []json.RawMessage{
			json.RawMessage(`{"method":"put","url":"https://api.example.com/orgs/acme/members/bob?token=sekrit","label":"add bob","conn":7}`),
		},
		ExpiresAt: time.Now().Add(time.Hour).Format(time.DateTime),
	}
	raw, err := json.Marshal(slackBlocks(accessCard(r, "Alice", "<#C1>", false)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, want := range []string{actAccessApprove, actAccessDeny, "api.example.com/orgs/acme/members/bob", `"style":"primary"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("card is missing %q", want)
		}
	}
	// A token in a query string must not be printed on a card that lives in a DM forever.
	if strings.Contains(s, "sekrit") {
		t.Fatal("the card printed a query-string value")
	}
	// The requester's words must not be able to open a Slack link or a mrkdwn span.
	if strings.Contains(s, "<https://evil.example|") {
		t.Fatal("requester text was rendered as live mrkdwn: a link can be masked")
	}
}

// The highest-risk behaviour: an ordered plan stops dead at the first failure, and nothing after
// it is attempted. A half-applied grant in an order nobody chose is worse than no grant at all.
func TestReplayStopsAtFirstFailure(t *testing.T) {
	var seen []string
	b := repoTestBot(t, func(r *http.Request) (int, string) {
		seen = append(seen, r.URL.Path)
		switch r.URL.Path {
		case "/ok":
			return 200, `{"ok":true}`
		case "/bad":
			return 500, `{"error":"nope"}`
		default:
			t.Errorf("a step after the failure was still called: %s", r.URL.Path)
			return 200, ``
		}
	})
	acc := grantAccess(t, b, 1, true, "api.github.com")

	r := &AccessRequest{ID: 1, Channel: "C1", ThreadTS: "1.1", Requester: "U_ASK", Calls: []json.RawMessage{
		json.RawMessage(`{"method":"PUT","url":"https://api.github.com/ok","conn":1}`),
		json.RawMessage(`{"method":"PUT","url":"https://api.github.com/bad","conn":1}`),
		json.RawMessage(`{"method":"PUT","url":"https://api.github.com/never","conn":1}`),
	}}
	notes, err := b.replay(context.Background(), b.slacks.Any(context.Background()), acc, r, "U_APP")
	if err == nil {
		t.Fatal("a 500 in the middle of a grant must stop the plan")
	}
	if !strings.Contains(err.Error(), "step 2 of 3") {
		t.Fatalf("the error should say which step failed: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("the first step ran, so it should be recorded: got %d notes", len(notes))
	}
	if len(seen) != 2 || seen[0] != "/ok" || seen[1] != "/bad" {
		t.Fatalf("calls should run in order and stop: %v", seen)
	}
}

// What an approved write made has to reach the note, because the note is the only thing the reply
// to the thread is written from. Until it did, a ticket filed on somebody's approval was reported
// back as an id and an HTTP status — a result nobody in the channel could open.
func TestReplayNotesCarryTheLinkToWhatWasMade(t *testing.T) {
	made := "https://github.com/o/r/issues/7"
	b := repoTestBot(t, func(r *http.Request) (int, string) {
		return 200, `{"number":7,"url":"https://api.github.com/repos/o/r/issues/7","html_url":"` + made + `"}`
	})
	acc := grantAccess(t, b, 1, true, "api.github.com")

	r := &AccessRequest{ID: 1, Channel: "C1", ThreadTS: "1.1", Requester: "U_ASK", Calls: []json.RawMessage{
		json.RawMessage(`{"method":"PUT","url":"https://api.github.com/repos/o/r/issues","conn":1}`),
	}}
	notes, err := b.replay(context.Background(), b.slacks.Any(context.Background()), acc, r, "U_APP")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	result := strings.Join(notes, "\n")
	if !strings.Contains(result, "view: "+made) {
		t.Fatalf("the note should state the page the write made: %q", result)
	}
	// And the card an approver pressed reads it back out of the same note.
	if got := viewedLink(result); got != made {
		t.Fatalf("viewedLink over the note = %q, want %q", got, made)
	}
}

// A recorded call must run on the connection it was approved against. Over a week a bundle can be
// rearranged so a different credential now matches the same host; spending that one silently is
// exactly the failure this pinning exists to prevent.
func TestReplayRefusesADifferentConnection(t *testing.T) {
	b := repoTestBot(t, func(r *http.Request) (int, string) {
		t.Errorf("nothing should have been sent, but %s was", r.URL.Path)
		return 200, ``
	})
	// Approved against connection 1; only connection 9 matches now.
	acc := grantAccess(t, b, 9, true, "api.github.com")
	r := &AccessRequest{ID: 1, Channel: "C1", ThreadTS: "1.1", Requester: "U_ASK", Calls: []json.RawMessage{
		json.RawMessage(`{"method":"PUT","url":"https://api.github.com/x","conn":1}`),
	}}
	if _, err := b.replay(context.Background(), b.slacks.Any(context.Background()), acc, r, "U_APP"); err == nil ||
		!strings.Contains(err.Error(), "different connection") {
		t.Fatalf("expected a refusal naming the connection swap, got %v", err)
	}
}

// A call whose connection the tier no longer lists must not run, however it was approved: the
// tier's grants are re-read at run time, and a host nothing on the tier reaches is a dead stop.
func TestReplayRefusesWhenGrantsRevoked(t *testing.T) {
	b := repoTestBot(t, func(r *http.Request) (int, string) {
		t.Errorf("nothing should have been sent, but %s was", r.URL.Path)
		return 200, ``
	})
	// The tier still grants something — just not the host that was approved.
	acc := grantAccess(t, b, 1, true, "other.example.com")
	r := &AccessRequest{ID: 1, Channel: "C1", ThreadTS: "1.1", Requester: "U_ASK", Calls: []json.RawMessage{
		json.RawMessage(`{"method":"PUT","url":"https://api.example.com/x","conn":1}`),
	}}
	if _, err := b.replay(context.Background(), b.slacks.Any(context.Background()), acc, r, "U_APP"); err == nil ||
		!strings.Contains(err.Error(), "can no longer run") {
		t.Fatalf("expected a refusal saying the step can no longer run, got %v", err)
	}
}

// A grant-capable connection must not be satisfiable by the in-thread Confirm card, whichever
// path composed the write.
func TestGrantConnectionNeedsApprovalNotConfirmation(t *testing.T) {
	grant := &Connection{ID: 1, Name: "g", Writes: "confirm", AllowGrants: true}
	if !needsApproval(grant, "POST") {
		t.Fatal("a write on a grant-capable connection must need approval")
	}
	// Even "auto", which normally lets writes straight through.
	if auto := (&Connection{ID: 2, Name: "a", Writes: "auto", AllowGrants: true}); !needsApproval(auto, "POST") {
		t.Fatal("writes:auto must not let a grant skip its approver")
	}
	if needsApproval(grant, "GET") {
		t.Fatal("reads do not grant anything and should not need an approver")
	}
	if needsApproval(&Connection{ID: 3, Writes: "confirm"}, "POST") {
		t.Fatal("an ordinary connection must keep the ordinary confirm path")
	}
}

// role builds a tier: a bundle whose connections are grant-capable, and members who hold it.
func role(t *testing.T, st *Store, name string, rank int, host string, members ...string) ApprovalRole {
	t.Helper()
	ctx := context.Background()
	bl, err := st.CreateBundle(ctx, orgID, name+" grants", "test")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	id, err := st.InsertConnection(ctx, orgID, &Connection{
		BundleID: bl.ID, Name: name + " conn", Preset: "custom", CredType: "bearer", Status: "active",
		AllowGrants: true, AllowedHosts: []string{host}, Methods: []string{"PUT"}, PathPrefixes: []string{"/"},
		Writes: "confirm",
	}, []byte("x"))
	if err != nil || id == 0 {
		t.Fatalf("connection: %v", err)
	}
	r := &ApprovalRole{Name: name, Rank: rank, BundleIDs: []int64{bl.ID}}
	if err := st.AddApprovalRole(ctx, orgID, r); err != nil {
		t.Fatalf("role: %v", err)
	}
	for _, m := range members {
		if err := st.AddApprovalMember(ctx, orgID, r.ID, m); err != nil {
			t.Fatalf("member: %v", err)
		}
	}
	got, _ := st.ApprovalRole(ctx, orgID, r.ID)
	return *got
}

// Routing is the heart of the tier model: the least powerful tier that can do the whole job gets
// asked, and a request the tagged person cannot grant escalates to one that can rather than
// dead-ending.
func TestRoutingPicksAndEscalatesTiers(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := &Agent{store: st, proxy: NewProxy(nil, st), settings: newSettingsCache(st, Config{})}

	role(t, st, "Approver", 1, "api.low.example", "U0LOW00001")
	sa := role(t, st, "Super admin", 9, "api.high.example", "U0SA000001")

	low := []grantStep{{Method: "PUT", URL: "https://api.low.example/x"}}
	high := []grantStep{{Method: "PUT", URL: "https://api.high.example/x"}}

	// Tagged the low approver for something they can grant: they get it.
	c := &Call{OrgID: orgID, TeamID: "T1", UserID: "U_ASK", Tagged: []string{"U0LOW00001"}, approvers: []string{"U0LOW00001", "U0SA000001"}}
	got, targets := a.routeTo(ctx, c, low)
	if got == nil || got.Role.Name != "Approver" || len(targets) != 1 || targets[0] != "U0LOW00001" {
		t.Fatalf("should have gone to the tagged approver, got %+v %v", got, targets)
	}

	// Tagged the same person for something only the super admin can grant: escalate.
	c = &Call{OrgID: orgID, TeamID: "T1", UserID: "U_ASK", Tagged: []string{"U0LOW00001"}, approvers: []string{"U0LOW00001", "U0SA000001"}}
	got, targets = a.routeTo(ctx, c, high)
	if got == nil || got.Role.ID != sa.ID || len(targets) != 1 || targets[0] != "U0SA000001" {
		t.Fatalf("should have escalated to the super admin, got %+v %v", got, targets)
	}

	// Nothing covers a host no tier reaches.
	c = &Call{OrgID: orgID, TeamID: "T1", UserID: "U_ASK", approvers: []string{"U0LOW00001", "U0SA000001"}}
	if got, _ = a.routeTo(ctx, c, []grantStep{{Method: "PUT", URL: "https://api.nobody.example/x"}}); got != nil {
		t.Fatalf("no tier should cover an unknown host, got %+v", got)
	}

	// A tier covers everything below it: rank is what mayApprove compares.
	if r := a.rankOf(ctx, orgID, a.slacks.Any(ctx), "U0SA000001"); r != 9 {
		t.Fatalf("super admin rank = %d, want 9", r)
	}
	if r := a.rankOf(ctx, orgID, a.slacks.Any(ctx), "U0NOBODY01"); r != -1 {
		t.Fatalf("a non-approver should hold no rank, got %d", r)
	}
}

// A plan that spans two tiers cannot be satisfied by either, and must not be split.
func TestRoutingRefusesAPlanNoTierCovers(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	a := &Agent{store: st, proxy: NewProxy(nil, st), settings: newSettingsCache(st, Config{})}
	role(t, st, "Approver", 1, "api.low.example", "U0LOW00001")
	role(t, st, "Super admin", 9, "api.high.example", "U0SA000001")

	c := &Call{OrgID: orgID, TeamID: "T1", UserID: "U_ASK", approvers: []string{"U0LOW00001", "U0SA000001"}}
	got, _ := a.routeTo(ctx, c, []grantStep{
		{Method: "PUT", URL: "https://api.low.example/x"},
		{Method: "PUT", URL: "https://api.high.example/y"},
	})
	if got != nil {
		t.Fatalf("a plan spanning two tiers must be refused, not split; got %s", got.Role.Name)
	}
}
