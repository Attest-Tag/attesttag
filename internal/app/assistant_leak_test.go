package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
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

	turns := func(sess string, q ...string) []map[string]any {
		path := "/api/assistant/turns?limit=10"
		if len(q) > 0 {
			path += "&q=" + url.QueryEscape(q[0])
		}
		r := httptest.NewRequest("GET", path, nil)
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

	// Nor may the viewer search the words they cannot read: a search that narrowed their list would
	// say which replies contain what, a guess at a time. Theirs ignores q.
	f.st.AddAssistantTurn(ctx, f.org, AssistantTurn{Actor: "actor", ActorName: "Admin", Question: "hello", Reply: "hi", Model: "m"})
	all := len(turns(vsess))
	if hit, miss := len(turns(vsess, "10.0.0.9")), len(turns(vsess, "no reply says this")); hit != all || miss != all {
		t.Errorf("a viewer's search narrowed the list (all %d, a phrase in a redacted reply %d, one in none %d): "+
			"the rows that come back say what the replies they cannot read contain", all, hit, miss)
	}
	// The admin's search still searches.
	if hit, miss := len(turns(f.sess, "10.0.0.9")), len(turns(f.sess, "no reply says this")); hit != 1 || miss != 0 {
		t.Errorf("an admin's search found %d and %d, want 1 and 0", hit, miss)
	}
}

// [AR-1] The arguments of the assistant's calls follow the reader as its question and reply do. They are
// the model's, written from the question and from what it read before, so a viewer holding activity.view
// sees that a tool ran and how it went, and not what it was asked: not on Activity, not in one call's
// detail, not in the overview's recent failures, not through the assistant's own tool_calls read. A
// reader holding audit.view and what the tool's arguments are about reads them whole, as before. The
// audit log's own rows are not this log, and nothing here writes one.
func TestAssistantArgsFollowTheReader(t *testing.T) {
	f := newAssist(t, RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ctx := context.Background()
	admin := f.consoleCallFor(PermAuditView, PermActivityView, PermScopesManage)
	byName := map[string]consoleTool{}
	for _, tl := range f.b.consoleTools(admin) {
		byName[tl.Name] = tl
	}
	calls := map[string]string{
		"read_console":             `{"resource":"audit","id":"SECRET-ACTION"}`,
		"propose_channel_settings": `{"channel":"C1","changes":{"instructions":"SECRET-INSTRUCTIONS"}}`,
		"search_guide":             `{"query":"SECRET-QUERY"}`,
	}
	for name, raw := range calls {
		f.b.runConsoleTool(ctx, admin, byName, name, raw)
	}
	// One that fails, which the overview lists to every member.
	f.b.runConsoleTool(ctx, admin, byName, "read_console", `{"resource":"nope","id":"SECRET-FAIL"}`)
	auditRows, _ := f.st.AuditEvents(ctx, f.org, AuditFilter{Limit: 50})

	vsess := f.member("viewer@example.com", RoleViewer)
	get := func(sess, path string) string {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	activity := func(sess string) []ToolCallRow {
		var out struct {
			ToolCalls []ToolCallRow `json:"tool_calls"`
		}
		json.Unmarshal([]byte(get(sess, "/api/activity?limit=50")), &out)
		return out.ToolCalls
	}

	// The viewer: every call is there, by name and outcome, and none of what it was asked.
	rows := activity(vsess)
	if len(rows) != 4 {
		t.Fatalf("the viewer sees %d assistant calls, want 4", len(rows))
	}
	for _, r := range rows {
		if r.Args != "" || len(r.Withheld) == 0 || !slices.Contains(r.Withheld, PermAuditView) {
			t.Errorf("a viewer was shown the arguments of %s: args %q, withheld %v", r.Name, r.Args, r.Withheld)
		}
		if body := get(vsess, "/api/tool-calls/"+strconv.FormatInt(r.ID, 10)); strings.Contains(body, "SECRET") {
			t.Errorf("a viewer read %s's arguments in its detail: %s", r.Name, body)
		}
	}
	if body := get(vsess, "/api/overview"); strings.Contains(body, "SECRET") || !strings.Contains(body, "read_console") {
		t.Errorf("the overview's recent failures = %s", body)
	}
	viewer := f.consoleCallFor(PermActivityView)
	if out, err := f.tool(viewer, "read_console").Run(ctx, viewer, args(map[string]any{"resource": "tool_calls"})); err != nil ||
		strings.Contains(out, "SECRET") || !strings.Contains(out, "withheld") {
		t.Errorf("the assistant read its own calls back to a viewer as %q (%v)", out, err)
	}

	// The admin reads every argument whole, on Activity and through the assistant.
	seen := map[string]string{}
	for _, r := range activity(f.sess) {
		if len(r.Withheld) > 0 {
			t.Errorf("an admin was refused %s's arguments: %v", r.Name, r.Withheld)
		}
		seen[r.Name] += r.Args
	}
	for name, raw := range calls {
		if !strings.Contains(seen[name], raw) {
			t.Errorf("an admin reads %s's arguments as %q, want %s", name, seen[name], raw)
		}
	}
	if out, _ := f.tool(admin, "read_console").Run(ctx, admin, args(map[string]any{"resource": "tool_calls"})); !strings.Contains(out, "SECRET-INSTRUCTIONS") {
		t.Errorf("the assistant reads its own calls back to an admin as %q", out)
	}

	// What each call's arguments need: what the turn's words need, and what they are about.
	auditor := map[string]bool{PermActivityView: true, PermAuditView: true}
	for _, r := range rows {
		full, _ := f.st.ToolCall(ctx, f.org, r.ID)
		full.unmarkPrivateFor(auditor)
		wantHidden := r.Name == "propose_channel_settings" // about a channel's settings, which needs scopes.manage
		if hidden := len(full.Withheld) > 0; hidden != wantHidden {
			t.Errorf("an auditor holding audit.view: %s's arguments hidden = %v (needs %v)", r.Name, hidden, full.Withheld)
		}
	}

	// A row logged before its arguments said what they need is read as the turn's words are, by audit.view.
	old := ToolCallRow{Channel: assistantChannel, Args: privateMark + ` {"resource":"settings"}`, Result: markPrivate(struct{}{})}
	if old.unmarkPrivateFor(map[string]bool{PermActivityView: true}); old.Args != "" || !slices.Equal(old.Withheld, []string{PermAuditView}) {
		t.Errorf("an old row to a viewer = %+v", old)
	}
	// And one logged before the assistant's calls were private at all, its result with its arguments.
	older := ToolCallRow{Channel: assistantChannel, Args: `{"resource":"audit"}`, Result: "- SIGN-IN by ceo@example.com"}
	if older.unmarkPrivateFor(map[string]bool{PermActivityView: true}); older.Args != "" || older.Result != "" || !older.Private {
		t.Errorf("an unmarked row to a viewer = %+v", older)
	}
	older = ToolCallRow{Channel: assistantChannel, Args: `{"resource":"audit"}`, Result: "- SIGN-IN by ceo@example.com"}
	if older.unmarkPrivateFor(map[string]bool{PermActivityView: true, PermAuditView: true}); older.Result == "" || older.Private {
		t.Errorf("an unmarked row to an auditor = %+v", older)
	}
	// Such a row as the assistant reads its own calls back: to the viewer, neither what it was asked nor
	// what it read; to the admin, both.
	f.st.LogToolCall(ctx, f.org, "", assistantChannel, "", "read_console", `{"resource":"audit","id":"SECRET-OLD-ARGS"}`,
		"- SIGN-IN by ceo@example.com SECRET-OLD-RESULT", true, 3)
	if out, err := f.tool(viewer, "read_console").Run(ctx, viewer, args(map[string]any{"resource": "tool_calls"})); err != nil ||
		strings.Contains(out, "SECRET") || strings.Contains(out, "ceo@example.com") {
		t.Errorf("the assistant read an unmarked call back to a viewer as %q (%v)", out, err)
	}
	if out, _ := f.tool(admin, "read_console").Run(ctx, admin, args(map[string]any{"resource": "tool_calls"})); !strings.Contains(out, "SECRET-OLD-ARGS") ||
		!strings.Contains(out, "SECRET-OLD-RESULT") {
		t.Errorf("the assistant reads an unmarked call back to an admin as %q", out)
	}
	if after, _ := f.st.AuditEvents(ctx, f.org, AuditFilter{Limit: 50}); len(after) != len(auditRows) {
		t.Errorf("reading the log wrote %d audit rows", len(after)-len(auditRows))
	}
}
