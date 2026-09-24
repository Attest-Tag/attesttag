package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// Scope's JSON advertises team_name, and the store cannot fill it: names live in the teams
// table, not on the scope row. Both listings shipped it empty, so an account with several
// connected workspaces could only tell its scopes apart by raw team id — the one thing a
// person reading the console does not know by heart.
func TestScopeListingsNameTheWorkspace(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	org, userID, token := seedOrg(t, st, RoleAdmin)
	if org != orgID {
		t.Fatalf("seeded org %d, tests assume %d", org, orgID)
	}
	rawKey := mintAPIKey()
	if _, err := st.CreateAPIKey(ctx, orgID, userID, "scopes", apiKeyDisplayPrefix(rawKey), hashAPIKey(rawKey), "", "test"); err != nil {
		t.Fatalf("api key: %v", err)
	}
	if err := st.SaveTeam(ctx, &Team{TeamID: "T1", OrgID: orgID, Name: "Example Co"}, []byte("sealed")); err != nil {
		t.Fatalf("team: %v", err)
	}
	st.UpsertScope(ctx, orgID, "team", "T1", "T1", "Example Co")
	st.UpsertScope(ctx, orgID, "channel", "T1", "C1", "#all-hands")

	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: testRegistry(&Chat{TeamID: "T1"})}
	mux := http.NewServeMux()
	b.routes(mux, fstest.MapFS{})

	get := func(path, bearer string) []byte {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}

	// The console's listing. sync=0 keeps it off Slack, which this test does not stand up.
	var console []map[string]any
	if err := json.Unmarshal(get("/api/scopes?sync=0", token), &console); err != nil {
		t.Fatalf("console json: %v", err)
	}
	if len(console) == 0 {
		t.Fatal("no scopes came back")
	}
	for _, s := range console {
		if s["team_id"] == "T1" && s["team_name"] != "Example Co" {
			t.Errorf("/api/scopes left %v unnamed: team_name=%q", s["slack_id"], s["team_name"])
		}
	}

	// And the developer API, which a script reads without a console to fall back on.
	var v1 struct {
		Scopes []map[string]any `json:"scopes"`
	}
	if err := json.Unmarshal(get("/v1/scopes", rawKey), &v1); err != nil {
		t.Fatalf("v1 json: %v", err)
	}
	for _, s := range v1.Scopes {
		if s["team_id"] == "T1" && s["team_name"] != "Example Co" {
			t.Errorf("/v1/scopes left %v unnamed: team_name=%q", s["slack_id"], s["team_name"])
		}
	}
}
