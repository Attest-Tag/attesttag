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

// leaveHarness is one organisation with a connected workspace and one channel in it, plus a
// stand-in Slack that records what conversations.leave was asked to do and can be told to
// refuse the way a real workspace does.
type leaveHarness struct {
	b        *Bot
	mux      *http.ServeMux
	st       *Store
	orgID    int64
	token    string
	channel  *Scope
	left     []string // channels Slack was asked to leave, in order
	refuse   string   // when set, the Slack error conversations.leave answers with
	channels string   // the channels users.conversations reports the bot is in
}

func newLeaveHarness(t *testing.T) *leaveHarness {
	t.Helper()
	h := &leaveHarness{channels: `{"id":"C1","name":"eng","is_private":false}`}
	var st *Store
	h.b, h.mux, st = installTestBot(t)
	h.st = st
	ctx := context.Background()
	h.orgID, _, h.token = seedOrg(t, st, RoleAdmin)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/users.conversations") {
			// Slack can still name the bot as a member for a moment after it leaves, which is
			// the whole point of the sweep below: a sync in that window must not undo a removal.
			fmt.Fprintf(w, `{"ok":true,"channels":[%s],"response_metadata":{"next_cursor":""}}`, h.channels)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations.leave") {
			r.ParseForm()
			h.left = append(h.left, r.FormValue("channel"))
			if h.refuse != "" {
				fmt.Fprintf(w, `{"ok":false,"error":%q}`, h.refuse)
				return
			}
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	enc, err := h.b.sealer.Seal([]byte("xoxb-acme"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: h.orgID, Name: "Acme", EmailScope: true, DMScope: true}, enc); err != nil {
		t.Fatal(err)
	}
	h.b.slacks = testRegistry(&Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))},
		TeamID: "TA", OrgID: h.orgID, BotUserID: "UBOT"})

	h.b.ensureAccountScope(ctx, h.orgID)
	if _, err := st.UpsertScope(ctx, h.orgID, "team", "TA", "TA", "Acme"); err != nil {
		t.Fatal(err)
	}
	h.channel, err = st.UpsertChannelScope(ctx, h.orgID, "TA", "C1", "#eng", false)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// listed is the channel ids the rail would draw for this organisation.
func (h *leaveHarness) listed(t *testing.T) []string {
	t.Helper()
	w := do(t, h.mux, "GET", "/api/scopes?sync=0", h.token)
	if w.Code != 200 {
		t.Fatalf("GET /api/scopes = %d: %s", w.Code, w.Body.String())
	}
	var rows []struct {
		Kind    string `json:"kind"`
		SlackID string `json:"slack_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, row := range rows {
		if row.Kind == "channel" {
			out = append(out, row.SlackID)
		}
	}
	return out
}

// Removing a channel is the bot leaving it in Slack. What the console must not do is throw the
// channel's settings away with it: somebody wrote those instructions, and inviting the bot back
// is meant to return them rather than ask for them again.
func TestRemoveChannelLeavesSlackAndKeepsSettings(t *testing.T) {
	h := newLeaveHarness(t)
	ctx := context.Background()
	if err := h.st.UpdateScope(ctx, h.orgID, h.channel.ID, "answer in threads", "", "inherit"); err != nil {
		t.Fatal(err)
	}

	code, out := authReq(t, h.mux, "POST", fmt.Sprintf("/api/scopes/%d/leave", h.channel.ID), nil, h.token)
	if code != 200 {
		t.Fatalf("removing the channel = %d: %v", code, out)
	}
	if len(h.left) != 1 || h.left[0] != "C1" {
		t.Fatalf("Slack was asked to leave %v, want exactly C1", h.left)
	}
	if got := h.listed(t); len(got) != 0 {
		t.Errorf("rail still lists %v; a channel the bot is not in is not somewhere it can reach", got)
	}

	sc, err := h.st.ChannelScope(ctx, h.orgID, "TA", "C1")
	if err != nil || sc == nil {
		t.Fatalf("the channel row was deleted (%v); removing must keep what was configured", err)
	}
	if sc.Instructions != "answer in threads" {
		t.Errorf("instructions = %q, want them kept for the day the bot is invited back", sc.Instructions)
	}

	// A sweep right behind the removal, with Slack still reporting the bot as a member, must
	// leave the removal standing rather than reading stale membership as a re-invitation.
	h.b.syncScopesFor(ctx, h.orgID, "TA")
	if got := h.listed(t); len(got) != 0 {
		t.Errorf("a scope sync put %v straight back; a removal has to outlive Slack's own lag", got)
	}

	// Invited back: the next scope sync sees the bot in the channel again, which is the same
	// upsert, and the channel returns with everything that was set on it.
	if _, err := h.st.UpsertChannelScope(ctx, h.orgID, "TA", "C1", "#eng", false); err != nil {
		t.Fatal(err)
	}
	if got := h.listed(t); len(got) != 1 || got[0] != "C1" {
		t.Fatalf("after the bot is invited back the rail lists %v, want C1", got)
	}
	sc, _ = h.st.ChannelScope(ctx, h.orgID, "TA", "C1")
	if sc == nil || sc.Instructions != "answer in threads" {
		t.Errorf("instructions after re-invite = %+v, want the ones set before", sc)
	}
}

// Slack grants leaving separately from reading, so a workspace connected before the app asked
// for channels:leave cannot leave. The channel has to stay listed in that case: the bot is
// still in it, and a rail that hid it would be lying about where the bot can be reached.
func TestRemoveChannelMissingScopeKeepsChannel(t *testing.T) {
	h := newLeaveHarness(t)
	h.refuse = "missing_scope"

	code, out := authReq(t, h.mux, "POST", fmt.Sprintf("/api/scopes/%d/leave", h.channel.ID), nil, h.token)
	if code != 409 {
		t.Fatalf("a workspace that cannot leave = %d, want 409: %v", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "channels:leave") {
		t.Errorf("error = %q, want it to name the scope a reconnect grants", msg)
	}
	if got := h.listed(t); len(got) != 1 {
		t.Errorf("rail lists %v, want the channel kept: the bot is still in it", got)
	}
}

// The bot already being out — removed in Slack, or the channel archived — is the state the
// console asked for. It reports success and stops listing the channel rather than making
// somebody clear a row by hand.
func TestRemoveChannelAlreadyOut(t *testing.T) {
	h := newLeaveHarness(t)
	h.refuse = "not_in_channel"

	code, out := authReq(t, h.mux, "POST", fmt.Sprintf("/api/scopes/%d/leave", h.channel.ID), nil, h.token)
	if code != 200 {
		t.Fatalf("leaving a channel the bot is already out of = %d, want 200: %v", code, out)
	}
	if got := h.listed(t); len(got) != 0 {
		t.Errorf("rail still lists %v", got)
	}
}

// Who may remove a channel, and what may be removed. A scope id is not an authorisation: the
// account and a workspace are not channels, and another tenant's channel is not this tenant's
// to touch — both answer 404, so the endpoint cannot be used to find out what exists.
func TestRemoveChannelRefusals(t *testing.T) {
	h := newLeaveHarness(t)
	ctx := context.Background()

	team, err := h.st.ScopeFor(ctx, h.orgID, "team", "TA", "TA")
	if err != nil || team == nil {
		t.Fatal("no workspace scope to test with")
	}
	if code, _ := authReq(t, h.mux, "POST", fmt.Sprintf("/api/scopes/%d/leave", team.ID), nil, h.token); code != 404 {
		t.Errorf("leaving a workspace scope = %d, want 404", code)
	}

	// Another organisation's channel, reached by its real id with a valid session of our own.
	otherOrg, _, _ := seedOrgAs(t, h.st, "other@example.com", RoleAdmin)
	theirs, err := h.st.UpsertChannelScope(ctx, otherOrg, "TB", "C9", "#theirs", false)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := authReq(t, h.mux, "POST", fmt.Sprintf("/api/scopes/%d/leave", theirs.ID), nil, h.token); code != 404 {
		t.Errorf("leaving another organisation's channel = %d, want 404", code)
	}
	if len(h.left) != 0 {
		t.Errorf("Slack was called for %v; nothing refused should reach Slack at all", h.left)
	}
	sc, _ := h.st.ChannelScope(ctx, otherOrg, "TB", "C9")
	if sc == nil {
		t.Error("the other organisation's channel was marked from outside it")
	}

	// A viewer holds no scopes.manage, so the button is not theirs to press.
	_, _, viewer := seedOrgAs(t, h.st, "viewer@example.com", RoleViewer)
	if code, _ := authReq(t, h.mux, "POST", fmt.Sprintf("/api/scopes/%d/leave", h.channel.ID), nil, viewer); code != 403 {
		t.Errorf("a viewer removing a channel = %d, want 403", code)
	}
}

// Scope sync keeps its lease for as long as it has one. A sweep that returned was work finished as
// far as leaderLoop could tell: it handed the lease back, took it again five seconds later and
// logged "holding leader lease", all day. It sweeps, waits, and returns when the lease is gone.
func TestScopeSyncHoldsItsLeaseUntilItIsTakenAway(t *testing.T) {
	b := &Bot{slacks: testRegistry(nil)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.scopeSyncLoop(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("scope sync returned after one sweep, so its lease would be given up and taken again")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scope sync went on after its lease was taken away")
	}
}
