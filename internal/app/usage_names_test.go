package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The overview's channel table names every row itself. It used to know only the channels the bot
// is in, so anything else showed its id: a Teams chat's is over a hundred characters and pushed
// the rest of the page off the screen, and the rows filed under no channel at all were called
// "Direct messages", which they are not.
func TestOverviewNamesEveryRow(t *testing.T) {
	st := testStore(t)
	token := seedAdmin(t, st)
	ctx := context.Background()

	slack := &Chat{TeamID: "T1"}
	slack.names.Store("U1", "Alex Kim")
	slack.chans.Store("C9", "general")
	teams := &Chat{TeamID: msteamsTeamPrefix + "tenant"}
	teams.names.Store("oid-1", "Priya Shah")
	reg := testRegistry(slack)
	reg.Put(teams.TeamID, teams)
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: reg}
	mux := http.NewServeMux()
	b.routes(mux, fstest.MapFS{})

	if _, err := st.UpsertChannelScope(ctx, orgID, "T1", "C1", "#ops", false); err != nil {
		t.Fatal(err)
	}
	// A Slack DM can have a scope row of its own. It is still named for the person.
	if _, err := st.UpsertChannelScope(ctx, orgID, "T1", "D1", "#DM", true); err != nil {
		t.Fatal(err)
	}
	personal := "a:1" + strings.Repeat("x", 120)
	want := map[string]string{
		"T1|C1":                            "#ops",
		"T1|C9":                            "#general", // not a scope (the bot left), so named by Slack
		"T2|C1":                            "C1",       // another workspace's C1 is not #ops, and nobody can name it
		"T1|D1":                            "DM with Alex Kim",
		"T1|D2":                            "Direct message", // nobody on record to name it after
		"T1|":                              "Message screening",
		"|" + assistantChannel:             "Console assistant",
		teams.TeamID + "|" + personal:      "DM with Priya Shah",
		teams.TeamID + "|19:abc@thread.v2": "Group chat",
	}
	users := map[string]string{"T1|C1": "U1", "T1|C9": "U1", "T2|C1": "U9", "T1|D1": "U1",
		"|" + assistantChannel: "0e8f0c0c", teams.TeamID + "|" + personal: "oid-1", teams.TeamID + "|19:abc@thread.v2": "oid-1"}
	for key := range want {
		team, channel, _ := strings.Cut(key, "|")
		st.LogUsageBy(ctx, orgID, team, channel, "", users[key], "test", Usage{In: 10, Out: 5})
	}

	r := httptest.NewRequest("GET", "/api/overview", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET /api/overview: %d %s", w.Code, w.Body)
	}
	var o struct {
		TopChannels []struct{ TeamID, Channel, ChannelName string } `json:"top_channels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if len(o.TopChannels) != len(want) {
		t.Errorf("%d rows, want %d: %+v", len(o.TopChannels), len(want), o.TopChannels)
	}
	for _, row := range o.TopChannels {
		key := row.TeamID + "|" + row.Channel
		if row.ChannelName != want[key] {
			t.Errorf("%s is called %q, want %q", key, row.ChannelName, want[key])
		}
	}
	// Who spoke is only for naming a DM. A channel's row must not single out one of its members.
	if strings.Contains(w.Body.String(), "U9") {
		t.Errorf("the overview sends a user id: %s", w.Body)
	}
}

// Activity names conversations the way the overview does, on every tab. Its turns used to call
// the read-every-message check "Direct message" and a DM just "DM", and its tool calls and proxy
// requests showed a Teams chat's whole id.
func TestActivityNamesEveryConversation(t *testing.T) {
	st := testStore(t)
	token := seedAdmin(t, st)
	ctx := context.Background()

	teams := &Chat{TeamID: msteamsTeamPrefix + "tenant"}
	teams.names.Store("oid-1", "Priya Shah")
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: testRegistry(teams)}
	mux := http.NewServeMux()
	b.routes(mux, fstest.MapFS{})

	personal := "a:1" + strings.Repeat("x", 120)
	group := "19:abc@thread.v2"
	st.LogUsageBy(ctx, orgID, teams.TeamID, personal, "", "oid-1", "test", Usage{In: 10, Out: 5})
	st.LogUsageBy(ctx, orgID, teams.TeamID, "", "", "", "test", Usage{In: 1, Out: 1})
	// The DM is named for its person on the tool-call tab too, though a tool call records nobody.
	st.LogToolCall(ctx, orgID, teams.TeamID, personal, "", "about_me", "{}", "ok", true, 5)
	st.LogProxy(ctx, orgID, ProxyAudit{TeamID: teams.TeamID, Channel: group, Requester: "oid-1",
		Method: "GET", Host: "example.com", Path: "/", Status: 200})

	r := httptest.NewRequest("GET", "/api/activity", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET /api/activity: %d %s", w.Code, w.Body)
	}
	type named struct{ Channel, ChannelName string }
	var a struct {
		Turns     []named `json:"turns"`
		ToolCalls []named `json:"tool_calls"`
		Proxy     []struct {
			Channel     string `json:"channel"`
			ChannelName string `json:"channel_name"`
		} `json:"proxy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range a.Turns {
		got["turn "+row.Channel] = row.ChannelName
	}
	for _, row := range a.ToolCalls {
		got["call "+row.Channel] = row.ChannelName
	}
	for _, row := range a.Proxy {
		got["proxy "+row.Channel] = row.ChannelName
	}
	for key, want := range map[string]string{
		"turn " + personal: "DM with Priya Shah",
		"turn ":            "Message screening",
		"call " + personal: "DM with Priya Shah",
		"proxy " + group:   "Group chat",
	} {
		if got[key] != want {
			t.Errorf("%s is called %q, want %q", key, got[key], want)
		}
	}
}
