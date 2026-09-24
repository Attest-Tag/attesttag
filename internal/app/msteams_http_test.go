package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	anaID    = "8f3b1c2d-0000-4000-8000-00000000000a"
	teamsOrg = "tenant-1"
)

// teamsTestBot is a bot with Teams configured against the fake Microsoft, and an agent whose model
// answers every question with answer.
func teamsTestBot(t *testing.T, answer string) (*Bot, *http.ServeMux, *fakeMicrosoft, *Store) {
	t.Helper()
	// Every test links the same fake tenant, and teamsLinks counts attempts per tenant for the whole
	// process: without a fresh one, the tests between them spend the ten an hour and a later test's
	// link is refused for somebody else's attempts.
	teamsLinks = newRateLimiter()
	b, mux, st := identityBot(t)
	f := newFakeMicrosoft(t)
	b.slacks.msteams = f.client(st)
	llm := httptest.NewServer(&textCallLLM{answer: answer, markup: answer})
	t.Cleanup(llm.Close)
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12}
	b.agent = &Agent{cfg: cfg, store: st, tools: map[string]Tool{}, runs: map[int64]*runHandle{}, loc: time.UTC,
		slacks: b.slacks, settings: b.settings, llm: NewLLM(Config{LLMBaseURL: llm.URL, LLMKey: "k", Model: "test"})}
	f.addMember(msMember{ID: "29:ana", Name: "Ana", AADObjectID: anaID, Email: "ana@contoso.com", TenantID: teamsOrg, Role: "user"})
	return b, mux, f, st
}

var activitySeq = 1700000009000

// activity is a Teams message from Ana in her one-to-one chat with the bot, with over applied.
func (f *fakeMicrosoft) activity(text string, over map[string]any) map[string]any {
	activitySeq++
	a := map[string]any{
		"type": "message", "id": strconv.Itoa(activitySeq),
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "serviceUrl": f.srv.URL, "channelId": "msteams",
		"from":         map[string]any{"id": "29:ana", "name": "Ana", "aadObjectId": anaID},
		"recipient":    map[string]any{"id": "28:" + fakeAppID, "name": "attest_tag"},
		"conversation": map[string]any{"id": "a:chat-ana", "conversationType": "personal", "tenantId": teamsOrg},
		"channelData":  map[string]any{"tenant": map[string]any{"id": teamsOrg}},
		"text":         text,
	}
	for k, v := range over {
		a[k] = v
	}
	return a
}

func deliverTeams(t *testing.T, mux *http.ServeMux, token string, act map[string]any) int {
	t.Helper()
	raw, _ := json.Marshal(act)
	r := httptest.NewRequest("POST", "/msteams/messages", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w.Code
}

// linkTeams joins the fake tenant to a fresh organisation through the bot, the way an admin would:
// a code from the console, sent to the bot in Teams.
func linkTeams(t *testing.T, st *Store, mux *http.ServeMux, f *fakeMicrosoft) *Org {
	t.Helper()
	ctx := context.Background()
	org, err := st.CreateOrg(ctx, "Contoso", 0)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := st.NewLinkCode(ctx, org.ID, platformMSTeams, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.messages())
	if got := deliverTeams(t, mux, f.sign(nil), f.activity("link "+code, nil)); got != 200 {
		t.Fatalf("link delivery answered %d", got)
	}
	posts := f.waitForMessages(before + 1)
	if !strings.Contains(posts[len(posts)-1].Activity.Text, "Connected") {
		t.Fatalf("linking answered %q", posts[len(posts)-1].Activity.Text)
	}
	return org
}

// The whole path a Teams user takes: an unsigned delivery is refused; a tenant nobody has linked is
// told how to link it; a code from the console links it; and from then on a question is answered in
// the conversation it was asked in, through exactly the turn a Slack message would get — with both
// sides kept in the log the next turn reads the thread from.
func TestATeamsMessageIsAnsweredOnceItsTenantIsLinked(t *testing.T) {
	b, mux, f, st := teamsTestBot(t, "Refunds take five working days.")
	ctx := context.Background()

	if got := deliverTeams(t, mux, "", f.activity("hello", nil)); got != http.StatusUnauthorized {
		t.Fatalf("an unsigned delivery answered %d", got)
	}
	if got := deliverTeams(t, mux, f.sign(map[string]any{"aud": "another-bot"}), f.activity("hello", nil)); got != http.StatusUnauthorized {
		t.Fatalf("another bot's delivery answered %d", got)
	}
	if n := len(f.messages()); n != 0 {
		t.Fatalf("refused deliveries still produced %d posts", n)
	}

	deliverTeams(t, mux, f.sign(nil), f.activity("hello", nil))
	posts := f.waitForMessages(1)
	if !strings.Contains(posts[0].Activity.Text, "link ABCD-EFGH") || posts[0].Conversation != "a:chat-ana" {
		t.Fatalf("an unlinked tenant was told %+v", posts[0])
	}

	org := linkTeams(t, st, mux, f)
	team, err := st.Team(ctx, "msteams:"+teamsOrg)
	if err != nil || team == nil || team.OrgID != org.ID || team.Platform != platformMSTeams || team.ServiceURL != f.srv.URL {
		t.Fatalf("linked team row: %+v, %v", team, err)
	}

	before := len(f.messages())
	deliverTeams(t, mux, f.sign(nil), f.activity("how long do refunds take?", nil))
	posts = f.waitForMessages(before + 1)
	answer := posts[len(posts)-1]
	if answer.Conversation != "a:chat-ana" || !strings.Contains(answer.Activity.Text, "five working days") ||
		answer.Activity.TextFormat != "markdown" {
		t.Fatalf("the answer was %+v", answer)
	}
	// The next turn reads the chat back from the log, so both sides of this one must be in it.
	sl, err := b.slacks.For(ctx, "msteams:"+teamsOrg)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := sl.Thread(ctx, "a:chat-ana", msteamsChatThread, 0)
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, m := range thread {
		said = append(said, m.Text)
	}
	joined := strings.Join(said, " | ")
	if !strings.Contains(joined, "how long do refunds take?") || !strings.Contains(joined, "five working days") {
		t.Errorf("the log holds %q", joined)
	}
}

// The cutover freeze reaches Teams as it reaches Slack: a genuine activity is answered 200 and
// dropped, because everything it leads to writes to a database about to be replaced, while an
// unsigned one is refused as ever. The tenant here is not linked, so a "hello" is normally told how
// to link it — during the freeze it is told nothing, and once the freeze lifts it is told again.
func TestMaintenanceDropsATeamsActivity(t *testing.T) {
	b, mux, f, _ := teamsTestBot(t, "unused")
	// Conversations of this run's own: the not-linked reply is said once per conversation every
	// few minutes, process-wide, and another test's "hello" must not have used it up.
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	chat := func(id string) map[string]any {
		return map[string]any{"conversation": map[string]any{"id": "a:" + id + "-" + run, "conversationType": "personal", "tenantId": teamsOrg}}
	}

	b.cfg.Maintenance = true
	if got := deliverTeams(t, mux, "", f.activity("hello", chat("frozen"))); got != http.StatusUnauthorized {
		t.Fatalf("an unsigned delivery during the freeze answered %d", got)
	}
	if got := deliverTeams(t, mux, f.sign(nil), f.activity("hello", chat("frozen"))); got != http.StatusOK {
		t.Fatalf("an activity during the freeze answered %d, want 200", got)
	}

	b.cfg.Maintenance = false
	if got := deliverTeams(t, mux, f.sign(nil), f.activity("hello", chat("thawed"))); got != http.StatusOK {
		t.Fatalf("an activity after the freeze answered %d", got)
	}
	f.waitForMessages(1)
	time.Sleep(200 * time.Millisecond) // whatever the frozen activity started has had its turn too
	for _, p := range f.messages() {
		if p.Conversation == "a:frozen-"+run {
			t.Fatalf("the activity delivered during the freeze was acted on: %+v", p)
		}
	}
	if posts := f.messages(); len(posts) != 1 || posts[0].Conversation != "a:thawed-"+run {
		t.Errorf("after the freeze the bot sent %+v, want one reply in a:thawed-%s", posts, run)
	}
}

// In a channel the bot answers in the reply chain it was mentioned in, and its own mention is the
// address on the envelope, not part of the question.
func TestATeamsChannelMentionIsAnsweredInItsThread(t *testing.T) {
	_, mux, f, st := teamsTestBot(t, "Here is the summary.")
	linkTeams(t, st, mux, f)
	before := len(f.messages())
	deliverTeams(t, mux, f.sign(nil), f.activity("<at>attest_tag</at> summarise this", map[string]any{
		"conversation": map[string]any{"id": "19:eng@thread.tacv2;messageid=1700000009500", "conversationType": "channel", "tenantId": teamsOrg},
		"channelData": map[string]any{"tenant": map[string]any{"id": teamsOrg},
			"team": map[string]any{"id": "19:eng@thread.tacv2", "name": "Engineering"}, "channel": map[string]any{"id": "19:eng@thread.tacv2"}},
		"entities": []map[string]any{{"type": "mention", "text": "<at>attest_tag</at>",
			"mentioned": map[string]any{"id": "28:" + fakeAppID, "name": "attest_tag"}}},
	}))
	posts := f.waitForMessages(before + 1)
	got := posts[len(posts)-1]
	if got.Conversation != "19:eng@thread.tacv2;messageid=1700000009500" || !strings.Contains(got.Activity.Text, "summary") {
		t.Fatalf("the channel answer went to %q: %q", got.Conversation, got.Activity.Text)
	}
	// And the question the log kept is the question, not the bot's name.
	thread, _ := st.TeamsThread(context.Background(), "msteams:"+teamsOrg, "19:eng@thread.tacv2", "1700000009500")
	if len(thread) == 0 || thread[0].Text != "summarise this" {
		t.Errorf("the log kept %+v", thread)
	}
}

// Adding the app to a team puts every channel of the team in the console at once — the General
// channel under its own name, which Microsoft leaves out — and taking the app out of the team takes
// them out of the console again, keeping what was configured on them for when it comes back.
func TestATeamsChannelsReachTheConsoleWhenTheAppIsAddedToIt(t *testing.T) {
	_, mux, f, st := teamsTestBot(t, "unused")
	org := linkTeams(t, st, mux, f)
	ctx := context.Background()
	const general, ops = "19:sales@thread.tacv2", "19:sales-ops@thread.tacv2"
	f.mu.Lock()
	f.teams[general] = fakeTeam{Name: "Sales", Channels: map[string]string{general: "", ops: "Ops"}}
	f.mu.Unlock()
	install := func(action string) {
		deliverTeams(t, mux, f.sign(nil), f.activity("", map[string]any{
			"type": "installationUpdate", "action": action,
			"conversation": map[string]any{"id": general, "conversationType": "channel", "tenantId": teamsOrg},
			"channelData":  map[string]any{"tenant": map[string]any{"id": teamsOrg}, "team": map[string]any{"id": general}},
		}))
	}
	rail := func() map[string]string {
		scopes, _ := st.Scopes(ctx, org.ID)
		out := map[string]string{}
		for _, sc := range scopes {
			if sc.Kind == "channel" {
				out[sc.SlackID] = sc.Name
			}
		}
		return out
	}
	until := func(what string, ok func(map[string]string) bool) map[string]string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if got := rail(); ok(got) {
				return got
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: the rail shows %v", what, rail())
		return nil
	}

	before := len(f.messages())
	install("add")
	got := until("after the install", func(m map[string]string) bool { return len(m) == 2 })
	if got[general] != "#Sales › General" || got[ops] != "#Sales › Ops" {
		t.Fatalf("the rail shows %v", got)
	}
	// The greeting is a post of its own. Found live: Teams refuses a reply chain hung off the
	// install activity's id, which is no message, with "Invalid parent message ID".
	posts := f.waitForMessages(before + 1)
	if greeting := posts[len(posts)-1]; greeting.Conversation != general {
		t.Errorf("the install greeting went to %q, want the channel itself", greeting.Conversation)
	}
	sc, _ := st.ScopeFor(ctx, org.ID, "channel", "msteams:"+teamsOrg, ops)
	if sc == nil || !sc.IsPrivate {
		t.Errorf("a Teams channel should read as private, not open to the whole tenant: %+v", sc)
	}

	install("remove")
	until("after the removal", func(m map[string]string) bool { return len(m) == 0 })
	if sc, _ := st.ScopeFor(ctx, org.ID, "channel", "msteams:"+teamsOrg, ops); sc == nil {
		t.Error("removing the app dropped the channel's row, and what was configured on it with it")
	}
}

// A Teams organisation walks the same setup a Slack workspace does, in Teams' words: a deployment
// with a bot registered offers Teams at the first step, and once a tenant is connected the walk
// names the app by what people type after the @ in Teams rather than asking Slack what it is called.
func TestTheSetupWalkSpeaksTeamsForATeamsOrganisation(t *testing.T) {
	_, mux, f, st := teamsTestBot(t, "unused")
	ctx := context.Background()
	orgID, _, token := seedOrg(t, st, RoleAdmin)
	if got := readOnboarding(t, mux, token); got.Step != stepInstall || !got.MSTeams || got.Platform != "" {
		t.Fatalf("before anything is connected = %+v, want the first step with Teams offered", got)
	}
	code, _, err := st.NewLinkCode(ctx, orgID, platformMSTeams, 0)
	if err != nil {
		t.Fatal(err)
	}
	deliverTeams(t, mux, f.sign(nil), f.activity("link "+code, nil))
	f.waitForMessages(1)
	got := readOnboarding(t, mux, token)
	if got.Step != stepChannel || got.Platform != platformMSTeams || got.BotHandle != msteamsAppName {
		t.Fatalf("with a Teams tenant connected = %+v, want the channel step, said for Teams", got)
	}
}

// A guest, or somebody from another organisation's tenant, is refused the way a Slack guest or a
// Slack Connect visitor is — with the reason, and with nothing run.
func TestTeamsGuestsAndOtherTenantsAreRefused(t *testing.T) {
	_, mux, f, st := teamsTestBot(t, "should never be said")
	linkTeams(t, st, mux, f)
	for _, c := range []struct {
		name   string
		member msMember
		want   string
	}{
		{"guest", msMember{ID: "29:gus", Name: "Gus", AADObjectID: "8f3b1c2d-0000-4000-8000-00000000000b", TenantID: teamsOrg, Role: "guest"}, "guest accounts"},
		{"other tenant", msMember{ID: "29:oz", Name: "Oz", AADObjectID: "8f3b1c2d-0000-4000-8000-00000000000c", TenantID: "fabrikam", Role: "user"}, "other Microsoft 365 organisations"},
	} {
		f.addMember(c.member)
		before := len(f.messages())
		deliverTeams(t, mux, f.sign(nil), f.activity("hi", map[string]any{
			"from":         map[string]any{"id": c.member.ID, "name": c.member.Name, "aadObjectId": c.member.AADObjectID},
			"conversation": map[string]any{"id": "a:chat-" + c.member.Name, "conversationType": "personal", "tenantId": teamsOrg},
		}))
		posts := f.waitForMessages(before + 1)
		got := posts[len(posts)-1].Activity.Text
		if !strings.Contains(got, c.want) || strings.Contains(got, "should never be said") {
			t.Errorf("%s was told %q", c.name, got)
		}
	}
}

// An organisation founded through a Slack sign-up starts with that sign-up's email domain as its
// list, and a Microsoft tenant's addresses are often on another one. Whoever links the tenant is told
// so in the reply to their code, with where it is changed, rather than meeting it as a refusal on
// their first question.
func TestLinkingTeamsSaysWhenTheLinkerMayNotUseTheBot(t *testing.T) {
	b, mux, f, st := teamsTestBot(t, "should never be said")
	ctx := context.Background()
	org, err := st.CreateOrg(ctx, "Fabrikam", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(ctx, org.ID, "allowed_email_domains", "fabrikam.com"); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(org.ID)
	code, _, err := st.NewLinkCode(ctx, org.ID, platformMSTeams, 0)
	if err != nil {
		t.Fatal(err)
	}
	deliverTeams(t, mux, f.sign(nil), f.activity("link "+code, nil))
	posts := f.waitForMessages(1)
	got := posts[len(posts)-1].Activity.Text
	for _, want := range []string{"Connected", "can't answer you yet", "fabrikam.com", "Settings → Security"} {
		if !strings.Contains(got, want) {
			t.Errorf("the link reply %q does not say %q", got, want)
		}
	}
	if team, _ := st.Team(ctx, "msteams:"+teamsOrg); team == nil || team.OrgID != org.ID {
		t.Fatalf("the tenant was not linked: %+v", team)
	}
}

// The console's "open the chat" link addresses the bot by the id it has in every tenant, and carries
// the command with its space as %20, which is how Teams reads the message of a deep link.
func TestTheTeamsChatLinkOpensTheBotWithTheCommandTyped(t *testing.T) {
	got := msteamsChatLink(fakeAppID, "link ABCD-EFGH")
	want := "https://teams.microsoft.com/l/chat/0/0?users=28:" + fakeAppID + "&message=link%20ABCD-EFGH"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := msteamsChatLink(fakeAppID, ""); strings.Contains(got, "message=") {
		t.Errorf("a link with nothing to type still carries a message: %s", got)
	}
}

// A press of one of the bot's own buttons arrives as a message with a value, and is routed by the
// code that routes a Slack press: here an access request that no longer exists, whose card is
// answered in place rather than left with live buttons.
func TestATeamsCardPressIsRoutedLikeASlackOne(t *testing.T) {
	_, mux, f, st := teamsTestBot(t, "unused")
	linkTeams(t, st, mux, f)
	before := len(f.posts)
	deliverTeams(t, mux, f.sign(nil), f.activity("", map[string]any{
		"replyToId": "1700000000042",
		"value":     map[string]any{msCardAction: actAccessApprove, msCardValue: confirmValue(99999, ""), msCardSummary: "*Ana is asking for access*"},
	}))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		var update *fakePost
		for _, p := range f.posts[before:] {
			if p.Method == "PUT" {
				p := p
				update = &p
			}
		}
		f.mu.Unlock()
		if update != nil {
			if update.ID != "1700000000042" || update.Conversation != "a:chat-ana" {
				t.Fatalf("the press rewrote %s in %s", update.ID, update.Conversation)
			}
			raw, _ := json.Marshal(update.Activity.Attachments)
			if !strings.Contains(string(raw), "can't find that request") || strings.Contains(string(raw), "Action.Submit") {
				t.Fatalf("the answered card was %s", raw)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the press never rewrote its card")
}
