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

// routineAPI is the console API over a fresh organisation with one channel the bot is in, and
// a caller holding the role given. Everything a routine needs to exist is here and nowhere
// else: the scope row is what resolves a channel id to a workspace.
func routineAPI(t *testing.T, role string) (*Store, *http.ServeMux, int64, string) {
	t.Helper()
	st := testStore(t)
	org, _, token := seedOrg(t, st, role)
	ctx := context.Background()
	st.UpsertScope(ctx, org, "team", "T1", "T1", "Acme")
	st.UpsertScope(ctx, org, "channel", "T1", "C1", "#ops")
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: testRegistry(&Chat{TeamID: "T1"})}
	mux := http.NewServeMux()
	b.routes(mux, fstest.MapFS{})
	return st, mux, org, token
}

func postRoutine(t *testing.T, mux *http.ServeMux, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/routines", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// A routine made in the console is the same row as one asked for in Slack: it is on, it knows
// which workspace its channel belongs to, it has a next run, and its writes go through without
// a Confirm card nobody would be there to press.
func TestConsoleCreatesRoutine(t *testing.T) {
	st, mux, org, token := routineAPI(t, RoleAdmin)
	ctx := context.Background()
	if err := st.PutSettings(ctx, org, map[string]string{"timezone": "Europe/London"}); err != nil {
		t.Fatal(err)
	}

	w := postRoutine(t, mux, token, `{"channel":"C1","cron":"0 9 * * 1-5","prompt":" post the standup digest "}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == 0 {
		t.Fatalf("create returned no id: %s", w.Body)
	}

	rs, err := st.Routines(ctx, org, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 routine, got %d", len(rs))
	}
	r := rs[0]
	switch {
	case r.ID != out.ID:
		t.Errorf("stored id %d, create said %d", r.ID, out.ID)
	case !r.Enabled:
		t.Error("a new routine is paused; it should be running")
	case r.TeamID != "T1":
		t.Errorf("team resolved to %q, want T1", r.TeamID)
	case r.Prompt != "post the standup digest":
		t.Errorf("prompt = %q, want it trimmed", r.Prompt)
	case r.TZ != "Europe/London":
		t.Errorf("timezone = %q, want the organisation's", r.TZ)
	case r.NextRun == "":
		t.Error("no next run: the scheduler would never fire it")
	case r.Notify != notifyAlways:
		t.Errorf("notify = %q, want %q", r.Notify, notifyAlways)
	case !r.AutoConfirm:
		t.Error("writes were left waiting for a Confirm card nobody will press")
	}

	// An explicit false is still honoured, the way it is from Slack.
	if w := postRoutine(t, mux, token, `{"channel":"C1","cron":"0 9 * * *","prompt":"ask first","autoConfirm":false,"notify":"when_needed","notifyWhen":"anything is on fire"}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	rs, _ = st.Routines(ctx, org, "")
	held := rs[len(rs)-1]
	if held.AutoConfirm {
		t.Error("autoConfirm:false was ignored")
	}
	if held.Notify != notifyWhenNeeded || held.NotifyWhen != "anything is on fire" {
		t.Errorf("quiet mode was not stored: %+v", held)
	}
}

// Everything that would make a routine fail every morning is refused before the row exists.
func TestConsoleRefusesARoutineItCannotRun(t *testing.T) {
	st, mux, org, token := routineAPI(t, RoleAdmin)
	if err := st.PutSettings(context.Background(), org, map[string]string{"channel_models": "m-cheap"}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"no prompt":                       `{"channel":"C1","cron":"0 9 * * *","prompt":"  "}`,
		"no channel":                      `{"channel":"","cron":"0 9 * * *","prompt":"x"}`,
		"a channel the bot is not in":     `{"channel":"C9","cron":"0 9 * * *","prompt":"x"}`,
		"a schedule that will not parse":  `{"channel":"C1","cron":"every morning","prompt":"x"}`,
		"a schedule that fires too often": `{"channel":"C1","cron":"* * * * *","prompt":"x"}`,
		"a model nobody offered":          `{"channel":"C1","cron":"0 9 * * *","prompt":"x","model":"m-expensive"}`,
	} {
		if w := postRoutine(t, mux, token, body); w.Code != 400 {
			t.Errorf("%s: got %d %s, want 400", name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	rs, _ := st.Routines(context.Background(), org, "")
	if len(rs) != 0 {
		t.Errorf("a refused create still wrote %d routine(s)", len(rs))
	}
}

// Creating a routine is a routines.manage decision, like editing one. A viewer may read the
// list and nothing more.
func TestConsoleRoutineCreateNeedsThePermission(t *testing.T) {
	st, mux, org, token := routineAPI(t, RoleViewer)
	if w := postRoutine(t, mux, token, `{"channel":"C1","cron":"0 9 * * *","prompt":"x"}`); w.Code != 403 {
		t.Fatalf("a viewer got %d %s, want 403", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if rs, _ := st.Routines(context.Background(), org, ""); len(rs) != 0 {
		t.Errorf("a viewer created %d routine(s)", len(rs))
	}
}
