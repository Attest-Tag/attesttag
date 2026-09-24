package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/slack-go/slack"
)

// The connection banner used to read "Connected by U01ABC… · bot U02DEF…", which names nobody.
// Both ids are resolved for the console instead, in the workspace they belong to.
func TestTeamsAPINamesTheInstallerAndTheBot(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	token := seedAdmin(t, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch r.Form.Get("user") {
		case "U_INSTALLER": // a display name set: that is what the workspace calls them
			enc.Encode(map[string]any{"ok": true, "user": map[string]any{
				"id": "U_INSTALLER", "name": "alex.kim", "real_name": "Alex Kim",
				"profile": map[string]any{"display_name": "Grace"}}})
		case "U_BOT": // no display name: the real name stands in
			enc.Encode(map[string]any{"ok": true, "user": map[string]any{
				"id": "U_BOT", "name": "attest_tag", "real_name": "attest_tag",
				"profile": map[string]any{"display_name": ""}}})
		default:
			enc.Encode(map[string]any{"ok": false, "error": "user_not_found"})
		}
	}))
	defer srv.Close()

	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: 1, Name: "Acme",
		BotUserID: "U_BOT", InstalledBy: "U_INSTALLER", EmailScope: true, DMScope: true}, enc); err != nil {
		t.Fatal(err)
	}
	// TB is connected but its client cannot be built, which is what a revoked token or a
	// changed master key looks like from here.
	if err := st.SaveTeam(ctx, &Team{TeamID: "TB", OrgID: 1, Name: "Beta",
		BotUserID: "U_GONE", InstalledBy: "U_ALSO_GONE", EmailScope: true, DMScope: true}, enc); err != nil {
		t.Fatal(err)
	}
	b.slacks = testRegistry(&Chat{
		t:         &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))},
		BotUserID: "U_BOT", TeamID: "TA",
	})

	var body struct {
		Teams []struct {
			TeamID          string `json:"team_id"`
			InstalledBy     string `json:"installed_by"`
			InstalledByName string `json:"installed_by_name"`
			BotUserID       string `json:"bot_user_id"`
			BotUserName     string `json:"bot_user_name"`
		} `json:"teams"`
	}
	w := do(t, mux, "GET", "/api/teams", token)
	if w.Code != 200 {
		t.Fatalf("GET /api/teams = %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for _, tm := range body.Teams {
		got[tm.TeamID] = [2]string{tm.InstalledByName, tm.BotUserName}
		if tm.InstalledBy == "" || tm.BotUserID == "" {
			t.Errorf("%s: the raw ids must still ship, the banner only prefers the names", tm.TeamID)
		}
	}
	if got["TA"][0] != "Grace" {
		t.Errorf("installed_by_name = %q, want the display name", got["TA"][0])
	}
	// The regression this guards: UserName answers "assistant" for the bot's own id, which is
	// right in a transcript and useless in a banner.
	if got["TA"][1] != "attest_tag" {
		t.Errorf("bot_user_name = %q, want the bot's own name", got["TA"][1])
	}
	// A workspace nothing can be asked about shows the ids, not another workspace's names.
	if got["TB"] != [2]string{"U_ALSO_GONE", "U_GONE"} {
		t.Errorf("unreachable workspace = %v, want both ids unchanged", got["TB"])
	}
}
