package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The console's side of connecting a Slack workspace, over the real routes: who may start an
// install, what Slack is asked for, what the rail is told, and what happens when a workspace is
// disconnected. Nothing here talks to Slack — the flow's own HTTP surface is the thing under test.
func installTestBot(t *testing.T) (*Bot, *http.ServeMux, *Store) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("ADMIN_BASE_URL", "https://console.example.com")
	t.Setenv("SLACK_CLIENT_ID", "cid")
	t.Setenv("SLACK_CLIENT_SECRET", "csecret")

	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, Config{}), resolver: NewResolver(st)}
	b.slacks = NewChatRegistry(st, sealer)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	return b, mux, st
}

func do(t *testing.T, mux *http.ServeMux, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestInstallFlowRoutes(t *testing.T) {
	_, mux, st := installTestBot(t)
	ctx := context.Background()
	token := seedAdmin(t, st)

	// Signed out, /slack/install is a browser navigation, so it redirects to the sign-in page
	// rather than answering with JSON nobody will see — and parks the intent, so that sign-in
	// finishes with the install rather than on the Overview.
	if w := do(t, mux, "GET", "/slack/install", ""); w.Code != http.StatusFound ||
		w.Header().Get("Location") != "/admin/login/" || !setsCookie(w, installIntentCookie) {
		t.Errorf("signed out = %d %q, want a redirect to sign in with the install parked", w.Code, w.Header().Get("Location"))
	}

	// Signed in, it sends the browser to Slack with the scopes the app actually needs.
	w := do(t, mux, "GET", "/slack/install", token)
	if w.Code != http.StatusFound {
		t.Fatalf("install = %d, want a redirect to Slack", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != "slack.com" || loc.Path != "/oauth/v2/authorize" {
		t.Fatalf("install redirect = %s", loc)
	}
	q := loc.Query()
	if q.Get("client_id") != "cid" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if got := q.Get("redirect_uri"); got != "https://console.example.com/slack/oauth/callback" {
		t.Errorf("redirect_uri = %q — it must match what is registered on the Slack app", got)
	}
	for _, want := range []string{"app_mentions:read", "chat:write", "users:read.email", "im:write"} {
		if !strings.Contains(q.Get("scope"), want) {
			t.Errorf("the consent screen must ask for %s, got %q", want, q.Get("scope"))
		}
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("no state: the callback would have nothing to check")
	}

	// A callback with the wrong state is refused, and refusing it must not burn the real one.
	w = do(t, mux, "GET", "/slack/oauth/callback?code=x&state=forged", token)
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "install_error=") {
		t.Errorf("forged state = %d %q, want the console page with an error", w.Code, w.Header().Get("Location"))
	}
	gotBy, gotOrg, err := st.TakeOAuthState(ctx, state)
	if err != nil {
		t.Errorf("a forged callback must not consume the real state: %v", err)
	}
	// The state carries the organisation across Slack's redirect: without it the callback has
	// no way to know whose install it is finishing.
	if gotOrg == 0 {
		t.Errorf("the install state lost its organisation (installer %q)", gotBy)
	}

	// Cancelling on Slack's consent screen says so rather than failing silently. The callback
	// is finished by the browser that started it, so it carries the install cookie and the
	// session.
	state2, _ := st.NewOAuthState(ctx, 1, "admin-token", installStateTTL)
	r := httptest.NewRequest("GET", "/slack/oauth/callback?error=access_denied&state="+state2, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.AddCookie(&http.Cookie{Name: installStateCookie, Value: state2})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if got := w.Header().Get("Location"); !strings.Contains(got, "install_error=") ||
		!strings.Contains(got, "cancelled") {
		t.Errorf("cancel = %q, want a cancelled message", got)
	}
}

// What the rail reads. The shape matters: it drives the empty state, the Add to Slack button
// and the per-workspace status chips.
func TestTeamsAPI(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	token := seedAdmin(t, st)

	var body struct {
		Teams []struct {
			TeamID         string `json:"team_id"`
			Name           string `json:"name"`
			Status         string `json:"status"`
			NeedsReinstall bool   `json:"needs_reinstall"`
		} `json:"teams"`
		InstallConfigured bool   `json:"install_configured"`
		InstallURL        string `json:"install_url"`
	}
	read := func() {
		t.Helper()
		w := do(t, mux, "GET", "/api/teams", token)
		if w.Code != 200 {
			t.Fatalf("GET /api/teams = %d: %s", w.Code, w.Body.String())
		}
		body.Teams = nil
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}

	read()
	if len(body.Teams) != 0 || !body.InstallConfigured || body.InstallURL != "/slack/install" {
		t.Fatalf("with nothing connected = %+v", body)
	}

	// A workspace that granted everything is simply connected; one that granted less still
	// works, and has to say so rather than going quiet.
	enc, _ := b.sealer.Seal([]byte("xoxb-a"))
	st.SaveTeam(ctx, &Team{TeamID: "TA", OrgID: 1, Name: "Acme", EmailScope: true, DMScope: true}, enc)
	st.SaveTeam(ctx, &Team{TeamID: "TB", OrgID: 1, Name: "Beta", EmailScope: false, DMScope: true}, enc)
	read()
	if len(body.Teams) != 2 {
		t.Fatalf("want two workspaces, got %+v", body.Teams)
	}
	byID := map[string]bool{}
	for _, tm := range body.Teams {
		byID[tm.TeamID] = tm.NeedsReinstall
	}
	if byID["TA"] {
		t.Error("a workspace that granted every scope must not be flagged for reinstall")
	}
	if !byID["TB"] {
		t.Error("a workspace missing a scope must be flagged, or it just goes quiet")
	}

	// Disconnecting keeps the row so the console can still explain the silence.
	if w := do(t, mux, "POST", "/api/teams/TB/disconnect", token); w.Code != 200 {
		t.Fatalf("disconnect = %d: %s", w.Code, w.Body.String())
	}
	read()
	status := map[string]string{}
	for _, tm := range body.Teams {
		status[tm.TeamID] = tm.Status
	}
	if status["TB"] != "revoked" {
		t.Errorf("TB status = %q, want revoked", status["TB"])
	}
	if _, err := b.slacks.For(ctx, "TB"); err == nil {
		t.Error("a disconnected workspace must stop resolving to a client")
	}
	if w := do(t, mux, "POST", "/api/teams/TNOPE/disconnect", token); w.Code != 404 {
		t.Errorf("disconnecting an unknown workspace = %d, want 404", w.Code)
	}
}

// The scope list is what the three-level rail renders, so it has to carry the kind and the
// workspace each row belongs to.
func TestScopesAPIShape(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	orgID, _, token := seedOrg(t, st, RoleAdmin)
	b.ensureAccountScope(ctx, orgID)
	st.UpsertScope(ctx, orgID, "team", "TA", "TA", "Acme")
	st.UpsertScope(ctx, orgID, "channel", "TA", "C1", "#eng")

	w := do(t, mux, "GET", "/api/scopes?sync=0", token)
	if w.Code != 200 {
		t.Fatalf("GET /api/scopes = %d: %s", w.Code, w.Body.String())
	}
	var rows []struct {
		Kind    string `json:"kind"`
		TeamID  string `json:"team_id"`
		SlackID string `json:"slack_id"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want the account, one workspace and one channel, got %+v", rows)
	}
	// The rail renders them top-down, so the order is part of the contract.
	if rows[0].Kind != "workspace" || rows[0].TeamID != "" || rows[0].SlackID != "" {
		t.Errorf("first row = %+v, want the account with no Slack id of its own", rows[0])
	}
	if rows[1].Kind != "team" || rows[1].TeamID != "TA" {
		t.Errorf("second row = %+v, want the Slack workspace", rows[1])
	}
	if rows[2].Kind != "channel" || rows[2].TeamID != "TA" || rows[2].Name != "#eng" {
		t.Errorf("third row = %+v, want the channel under its own workspace", rows[2])
	}
}
