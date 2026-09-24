package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A routine's budget is its own. The channel's "tool rounds per reply" is about replies, and a
// scheduled run that inherited it used to die twelve rounds into work written for sixty.
func TestRoutineBudgetIsNotTheReplyBudget(t *testing.T) {
	rounds, wall := routineBudget(Settings{MaxToolRounds: 12})
	if rounds != defaultRoutineRounds {
		t.Errorf("rounds = %d, want the routine default %d — a deployment that never set this gets the routine's number, not the reply's", rounds, defaultRoutineRounds)
	}
	if wall != defaultRoutineMinutes*time.Minute {
		t.Errorf("wall = %s, want %d minutes", wall, defaultRoutineMinutes)
	}
	// What an organisation sets is used, between the floor and the ceiling.
	if rounds, _ := routineBudget(Settings{RoutineRounds: 25}); rounds != 25 {
		t.Errorf("rounds = %d, want 25", rounds)
	}
	if rounds, _ := routineBudget(Settings{RoutineRounds: 5000}); rounds != maxRoutineRounds {
		t.Errorf("rounds = %d, want the ceiling %d", rounds, maxRoutineRounds)
	}
	if _, wall := routineBudget(Settings{RoutineMinutes: 1}); wall != minRoutineMinutes*time.Minute {
		t.Errorf("wall = %s, want the floor of %d minutes", wall, minRoutineMinutes)
	}
	if _, wall := routineBudget(Settings{RoutineMinutes: 600}); wall != maxRoutineMinutes*time.Minute {
		t.Errorf("wall = %s, want the ceiling of %d minutes", wall, maxRoutineMinutes)
	}
}

// The budget a run carries has to survive the trip into the turn. It did not: roundsFor clamped
// everything to the scope ceiling, which is a reply's number, so a routine given two hundred
// rounds in Settings quietly got eighty and landed on its write-up with the work half done.
func TestRunBudgetSurvivesTheReplyCeiling(t *testing.T) {
	a, ctx := &Agent{}, context.Background()
	rounds, _ := routineBudget(Settings{})
	if got := a.roundsFor(ctx, &Call{Kind: "routine", MaxRounds: rounds}, Settings{}); got != rounds {
		t.Errorf("rounds = %d, want the %d the run was given", got, rounds)
	}
	// It is still bounded, just by the ceiling meant for a run rather than for a reply.
	if got := a.roundsFor(ctx, &Call{Kind: "routine", MaxRounds: 5000}, Settings{}); got != maxRunRounds {
		t.Errorf("rounds = %d, want the run ceiling %d", got, maxRunRounds)
	}
	// A reply brings no budget of its own and keeps the scope ceiling it always had.
	if got := a.roundsFor(ctx, &Call{}, Settings{MaxToolRounds: 5000}); got != maxRoundsCeiling {
		t.Errorf("reply rounds = %d, want the scope ceiling %d", got, maxRoundsCeiling)
	}
}

// HubSpot's CRM is queried by POSTing filter groups — there is no GET that takes them. Holding
// that for a Confirm holds reading itself, which for an unattended run means it cannot even look.
// The same goes for every other service that reads over POST, which is why the shape of the path
// decides it and not a list of hosts: the customer-owned hostnames (a Grafana, a Jira) could
// never be on such a list.
func TestSearchPOSTsAreReads(t *testing.T) {
	read := []string{
		"https://api.hubapi.com/crm/v3/objects/contacts/search",
		"https://api.hubapi.com/crm/v3/objects/p_custom_object/search",
		"https://api.hubapi.com/crm/v3/objects/companies/batch/read",
		"https://www.googleapis.com/calendar/v3/freeBusy",
		"https://logging.googleapis.com/v2/entries:list",
		"https://api.notion.com/v1/databases/9a1b/query",
		"https://logs.example.internal/index-2026/_search",
		"https://monitoring.googleapis.com/v3/projects/p/timeSeries:query",
		// Nothing writes at a path ending in /search, whatever stands in front of it — which is
		// the whole of the claim the shape rule makes. It used to be held because the table
		// pinned HubSpot's own two ends; now it reads, and HubSpot answers 404 to it either way.
		"https://api.hubapi.com/crm/v3/objects/contacts/12345/search",
	}
	for _, u := range read {
		p, _ := url.Parse(u)
		if !readShapedPOST(p) {
			t.Errorf("%s should read as a read", u)
		}
	}
	// The writes, including the ones wearing a read's vocabulary: ClickUp creates a list by
	// POSTing to one, and "read" on its own is how half the world marks a notification.
	write := []string{
		"https://api.hubapi.com/crm/v3/objects/contacts",
		"https://api.hubapi.com/crm/v3/objects/contacts/12345",
		"https://www.googleapis.com/calendar/v3/calendars/primary/events",
		// batch/read is still the pinned pair, so the prefix does not leak down a longer path.
		"https://api.hubapi.com/crm/v3/objects/contacts/12345/batch/read",
		"https://api.clickup.com/api/v2/folder/901/list",
		"https://api.example.com/v1/notifications/read",
		"https://api.example.com/v1/contacts:batchDelete",
		"https://bigquery.googleapis.com/bigquery/v2/projects/p/queries",
		"https://api.linear.app/graphql",
	}
	for _, u := range write {
		p, _ := url.Parse(u)
		if readShapedPOST(p) {
			t.Errorf("%s must still be held as a write", u)
		}
	}
}

// A DM has no channel link to make: <#D…> renders as a dead reference in the very message that
// is telling somebody their routine broke.
func TestRoutineWhereNamesADM(t *testing.T) {
	if got := routineWhere("C123"); got != "<#C123>" {
		t.Errorf("channel = %q, want a link", got)
	}
	if got := routineWhere("D123"); strings.Contains(got, "<#") {
		t.Errorf("dm = %q, want words rather than a dead link", got)
	}
}

// A routine that researches eleven things has eleven things to say. Before send_dm it could only
// finish once, so the eleventh was a paragraph in a post nobody read to the end.
func TestRoutineCanDMPerItem(t *testing.T) {
	a, rec, llm, st, r := quietFixture(t, "done",
		Routine{Prompt: "brief me on each new lead", Notify: notifyWhenNeeded, NotifyWhen: "a lead was found"})
	llm.toolCall, llm.toolArgs = "send_dm", `{"user":"UALICE","text":"*New lead:* Acme"}`
	ctx := context.Background()

	a.runRoutineNow(ctx, r, r.NextRun)

	var dm string
	for _, p := range rec.posts() {
		if strings.HasPrefix(p, "chat.postMessage UALICE:") {
			dm = p
		}
	}
	if dm == "" {
		t.Fatalf("no DM reached Slack; posts = %v", rec.posts())
	}
	if !strings.Contains(dm, "New lead") {
		t.Errorf("DM = %q, want the text the run wrote", dm)
	}
	// The tool is a scheduled run's alone: every definition is re-sent on every round of every
	// turn that carries it, and a person in a thread can tell somebody themselves.
	offered := false
	for _, name := range llm.tools() {
		if name == "send_dm" {
			offered = true
		}
	}
	if !offered {
		t.Errorf("send_dm was not offered to the run: %v", llm.tools())
	}
	if runs, _ := st.RoutineRuns(ctx, 1, r.ID, 0); len(runs) != 1 {
		t.Errorf("runs = %d, want the run recorded", len(runs))
	}
}

// A channel id here would put a per-person message somewhere everyone can read it. That is the
// one mistake this tool must not make quietly.
func TestSendDMRefusesAChannelID(t *testing.T) {
	a, rec, _, _, _ := quietFixture(t, "done", Routine{Prompt: "x"})
	sl, err := a.slacks.For(context.Background(), "T1")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", UserID: "UME", Kind: "routine"}
	tool := a.routineDMTool()

	if _, err := tool.Run(context.Background(), c, []byte(`{"user":"C0123","text":"hi"}`)); err == nil {
		t.Error("a channel id was accepted; a DM tool that posts to a channel is a leak")
	}
	if _, err := tool.Run(context.Background(), c, []byte(`{"user":"U1","text":"  "}`)); err == nil {
		t.Error("an empty message was accepted")
	}
	// "me" is whoever the routine belongs to, so a prompt need not carry an id at all.
	if _, err := tool.Run(context.Background(), c, []byte(`{"user":"me","text":"hi"}`)); err != nil {
		t.Fatalf(`"me" should resolve to the run's own user: %v`, err)
	}
	if posts := rec.posts(); len(posts) != 1 || !strings.HasPrefix(posts[0], "chat.postMessage UME:") {
		t.Errorf("posts = %v, want one DM to UME", posts)
	}

	// The cap is what stops a prompt that loops from turning one schedule into a mailing list.
	c.dmsSent = maxRoutineDMs
	if _, err := tool.Run(context.Background(), c, []byte(`{"user":"U1","text":"hi"}`)); err == nil {
		t.Error("the per-run cap did not hold")
	}
}

// A Confirm card posted at 6am is nobody's decision — it expires unpressed — so a routine that
// was set up to act has to be able to act. The default is still to ask.
func TestRoutineAutoConfirmRunsWritesWithoutAsking(t *testing.T) {
	a, c, calls := clickupAgent(t, func(r *http.Request) (int, string) {
		return 200, `{"id":"abc","url":"https://app.clickup.com/t/abc"}`
	})
	// The held path asks the allow-rule checker, which reads settings.
	a.settings = newSettingsCache(a.store, Config{})
	c.Kind, c.OrgID, c.TeamID = "routine", orgID, "T1"
	ctx := context.Background()
	write := ProxyRequest{Method: "POST", URL: "https://api.clickup.com/api/v2/list/1/task", Body: jsonBody(map[string]any{"name": "x"})}

	// Off by default: a schedule does not get to write just because it is a schedule.
	out, err := a.proxied(ctx, c, write)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "human OK") {
		t.Errorf("out = %q, want the write held for a person", out)
	}
	if *calls != 0 || c.writesRun != 0 {
		t.Errorf("upstream calls = %d, writesRun = %d — nothing should have been sent", *calls, c.writesRun)
	}

	c.autoConfirm = true
	if out, err = a.proxied(ctx, c, write); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "without asking") {
		t.Errorf("out = %q, want the write to have run", out)
	}
	if *calls != 1 {
		t.Errorf("upstream calls = %d, want 1", *calls)
	}
	// Counted, because a routine that changes things unattended should not read as "posted"
	// and nothing else.
	if c.writesRun != 1 {
		t.Errorf("writesRun = %d, want 1", c.writesRun)
	}
}

// The one thing a routine's author may not pre-approve. Handing out access is not their
// decision whenever it happens, which is why that gate is checked before this one.
func TestAutoConfirmNeverCoversAccessGrants(t *testing.T) {
	a, c, calls := clickupAgent(t, func(r *http.Request) (int, string) { return 200, `{}` })
	a.settings = newSettingsCache(a.store, Config{})
	c.Kind, c.OrgID, c.TeamID, c.autoConfirm = "routine", orgID, "T1", true
	c.Access.Rules[0].Conn.AllowGrants = true

	out, err := a.proxied(context.Background(), c, ProxyRequest{Method: "POST", URL: "https://api.clickup.com/api/v2/list/1/task", Body: jsonBody(map[string]any{"name": "x"})})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "approval") {
		t.Errorf("out = %q, want it held for a named approver", out)
	}
	if *calls != 0 || c.writesRun != 0 {
		t.Errorf("upstream calls = %d, writesRun = %d — a grant must never run unasked", *calls, c.writesRun)
	}
}

// The flag has to survive the round trip, including through an edit that does not mention it.
func TestAutoConfirmRoundTrips(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	id, err := st.AddRoutine(ctx, Routine{OrgID: 1, TeamID: "T1", Channel: "C1", Cron: "0 9 * * *", TZ: "UTC",
		Prompt: "brief me", NextRun: "2030-01-01 09:00:00", AutoConfirm: true})
	if err != nil {
		t.Fatal(err)
	}
	read := func() Routine {
		rs, err := st.Routines(ctx, 1, "C1")
		if err != nil || len(rs) != 1 {
			t.Fatalf("routines = %d (%v)", len(rs), err)
		}
		return rs[0]
	}
	if !read().AutoConfirm {
		t.Fatal("a routine created to act came back asking")
	}
	prompt := "brief me twice"
	if err := st.UpdateRoutine(ctx, 1, id, RoutinePatch{Prompt: &prompt}); err != nil {
		t.Fatal(err)
	}
	if !read().AutoConfirm {
		t.Error("editing the prompt silently turned the routine back into one that asks")
	}
	off := false
	if err := st.UpdateRoutine(ctx, 1, id, RoutinePatch{AutoConfirm: &off}); err != nil {
		t.Fatal(err)
	}
	if read().AutoConfirm {
		t.Error("it could not be turned off")
	}
}
