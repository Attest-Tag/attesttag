package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The console assistant runs as the admin who asked, so its reads reach the audit log, the settings
// and the connections — none of which a plain activity.view holder may read directly. Neither the
// tool-call result it logged nor the reply it gave may reach such a viewer through /activity.
func TestAssistantReadsDoNotReachViewers(t *testing.T) {
	f := newAssist(t, RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ctx := context.Background()

	// An assistant tool that returns something only an admin should see. runConsoleTool is the path
	// that logs it to Activity.
	const secret = "SIGN-IN from 10.0.0.9 by ceo@example.com"
	admin := f.consoleCallFor(PermAuditView, PermActivityView)
	byName := map[string]consoleTool{"probe": {Name: "probe", Perm: PermAuditView,
		Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) { return secret, nil }}}
	f.b.runConsoleTool(ctx, admin, byName, "probe", "{}")

	rows, _ := f.st.RecentToolCalls(ctx, f.org, "", 20, false, "")
	if len(rows) == 0 {
		t.Fatal("the assistant tool call was not logged")
	}
	for _, r := range rows {
		if strings.Contains(r.Result, secret) {
			t.Fatalf("the assistant's result was logged in full to Activity: %q", r.Result)
		}
		if !strings.HasPrefix(r.Result, privateMark) {
			t.Errorf("the assistant result was not kept as private metadata: %q", r.Result)
		}
	}

	// A recorded turn whose question and reply quote such data.
	f.st.AddAssistantTurn(ctx, f.org, AssistantTurn{Actor: "actor", ActorName: "Admin",
		Question: "who signed in? " + secret, Reply: "here it is: " + secret, Model: "m"})

	vu, err := f.st.CreateUser(ctx, "viewer@example.com", "Vic", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.AddMembership(ctx, vu.ID, f.org, RoleViewer, 0); err != nil {
		t.Fatal(err)
	}
	vsess, err := f.st.CreateAdminSession(ctx, AdminUser{ID: vu.ID, OrgID: f.org, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	turns := func(sess string) []map[string]any {
		r := httptest.NewRequest("GET", "/api/assistant/turns?limit=10", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("assistant/turns = %d", w.Code)
		}
		var out []map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}

	// The viewer (activity.view, no audit.view) gets the metadata but not the words.
	for _, tr := range turns(vsess) {
		if tr["redacted"] != true {
			t.Errorf("a viewer saw an un-redacted assistant turn: %v", tr)
		}
		if strings.Contains(tr["question"].(string)+tr["reply"].(string), secret) {
			t.Fatalf("a viewer read the assistant's question or reply: %v", tr)
		}
		if tr["actor_name"] != "Admin" {
			t.Errorf("the viewer lost the metadata too: %v", tr)
		}
	}
	// The admin (audit.view) still reads them.
	var sawReply bool
	for _, tr := range turns(f.sess) {
		if strings.Contains(tr["reply"].(string), secret) {
			sawReply = true
		}
	}
	if !sawReply {
		t.Error("an admin with audit.view could no longer read the assistant's reply")
	}
}
