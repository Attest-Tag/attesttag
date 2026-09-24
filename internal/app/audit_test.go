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
)

// The audit log. What these hold up: a row is written for every write, named or not; it says
// who and from where; it is one organisation's and nobody else's; it is read behind its own
// permission; and it is kept by its own policy rather than the activity tables'.

func auditRows(t *testing.T, st *Store, orgID int64, f AuditFilter) []AuditEvent {
	t.Helper()
	if f.Limit == 0 {
		f.Limit = 100
	}
	rows, err := st.AuditEvents(context.Background(), orgID, f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func actionsOf(rows []AuditEvent) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}

func TestAuditLogIsScopedToOneOrg(t *testing.T) {
	x := seedTwoOrgs(t)
	ctx := context.Background()
	for _, o := range []int64{x.a, x.b} {
		if _, err := x.st.LogAudit(ctx, AuditEvent{OrgID: o, Action: "connection.created", Via: viaConsole,
			ActorEmail: fmt.Sprintf("o%d@example.com", o)}); err != nil {
			t.Fatal(err)
		}
	}
	x.st.LogAudit(ctx, AuditEvent{OrgID: x.b, Action: "auth.sign_in", Via: viaConsole, ActorEmail: "b@example.com"})

	got := auditRows(t, x.st, x.a, AuditFilter{})
	if len(got) != 1 || got[0].ActorEmail != fmt.Sprintf("o%d@example.com", x.a) {
		t.Fatalf("Acme's log: %+v", got)
	}
	actions, err := x.st.AuditActions(ctx, x.a)
	if err != nil || len(actions) != 1 || actions[0] != "connection.created" {
		t.Errorf("Acme's actions = %v (%v): Beta's sign-in leaked into the menu", actions, err)
	}
}

func TestAuditFiltersAndCursors(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seed := []AuditEvent{
		{Action: "auth.sign_in", ActorEmail: "dana@example.com", ActorPublic: "dana"},
		{Action: "auth.sign_in_failed", ActorEmail: "dana@example.com", ActorPublic: "dana", Outcome: auditDenied},
		{Action: "connection.created", ActorEmail: "sam@example.com", TargetName: "GitHub"},
		{Action: "connection.deleted", ActorEmail: "sam@example.com", TargetName: "GitHub"},
		{Action: "console.request", ActorEmail: "dana@example.com", ActorPublic: "dana", TargetID: "PUT /api/account", IP: "203.0.113.9"},
	}
	for _, e := range seed {
		e.OrgID, e.Via = 1, viaConsole
		if _, err := st.LogAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(rows []AuditEvent) string {
		var s []string
		for _, r := range rows {
			s = append(s, fmt.Sprint(r.ID))
		}
		return strings.Join(s, ",")
	}

	cases := []struct {
		name string
		f    AuditFilter
		want string
	}{
		{"newest first", AuditFilter{}, "5,4,3,2,1"},
		{"a family by its prefix", AuditFilter{Action: "auth."}, "2,1"},
		{"one action exactly", AuditFilter{Action: "auth.sign_in"}, "1"},
		{"by the actor's public id", AuditFilter{Actor: "dana"}, "5,2,1"},
		{"by the actor's address", AuditFilter{Actor: "sam@example.com"}, "4,3"},
		{"only what was refused", AuditFilter{Outcome: auditDenied}, "2"},
		{"a word in the target, any case", AuditFilter{Query: "github"}, "4,3"},
		{"an address", AuditFilter{Query: "203.0.113"}, "5"},
		{"older than a row: the console's next page", AuditFilter{Before: 3}, "2,1"},
		{"newer than a row: a collector's pull, oldest first", AuditFilter{After: 3}, "4,5"},
		{"a page", AuditFilter{Limit: 2}, "5,4"},
	}
	for _, c := range cases {
		if got := ids(auditRows(t, st, 1, c.f)); got != c.want {
			t.Errorf("%s: got ids %s, want %s", c.name, got, c.want)
		}
	}
}

// The activity tables' policy must not reach the audit log, and the audit log's must stop at
// its own organisation.
func TestAuditRetentionIsItsOwnPolicy(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	old, recent := nowMinus(90*24*time.Hour), nowMinus(time.Hour)
	put := func(orgID int64, at string) {
		t.Helper()
		id, err := st.LogAudit(ctx, AuditEvent{OrgID: orgID, Action: "auth.sign_in", Via: viaConsole})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `update audit_log set created_at=? where org_id=? and id=?`, at, orgID, id); err != nil {
			t.Fatal(err)
		}
	}
	put(1, old)
	put(1, recent)
	put(2, old)
	count := func(orgID int64) int {
		var n int
		st.db.QueryRowContext(ctx, `select count(*) from audit_log where org_id=?`, orgID).Scan(&n)
		return n
	}

	if _, err := st.PurgeOrgData(ctx, 1, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if count(1) != 2 {
		t.Fatalf("data_retention_days reached the audit log: %d rows left of 2", count(1))
	}
	if n, err := st.PurgeAuditLog(ctx, 1, 0); err != nil || n != 0 {
		t.Errorf("a zero window deleted %d rows (%v); it means keep everything", n, err)
	}
	n, err := st.PurgeAuditLog(ctx, 1, 30*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("PurgeAuditLog deleted %d rows (%v), want the one old row", n, err)
	}
	if count(1) != 1 || count(2) != 1 {
		t.Errorf("after the sweep org 1 has %d rows and org 2 has %d; want 1 and 1", count(1), count(2))
	}

	for _, v := range []string{"1", "7", "29", "4000", "abc"} {
		if err := validateSecuritySetting("audit_retention_days", v); err == nil {
			t.Errorf("audit_retention_days=%q was accepted", v)
		}
	}
	for _, v := range []string{"", "0", "30", "365", "3650"} {
		if err := validateSecuritySetting("audit_retention_days", v); err != nil {
			t.Errorf("audit_retention_days=%q was refused: %v", v, err)
		}
	}
}

// Every write through the console leaves a row: the named event when the handler wrote one,
// the plain route otherwise, and a refusal as a refusal. Sign-ins, failed sign-ins and sign-outs
// are the lines around them.
func TestConsoleWritesLeaveARow(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	founder, orgID, token := signedUp(t, b, mux, st, "founder@example.com")

	// The first two lines of a new organisation's log.
	rows := auditRows(t, st, orgID, AuditFilter{})
	if got := actionsOf(rows); len(got) != 2 || got[0] != "auth.sign_in" || got[1] != "org.created" {
		t.Fatalf("after signup the log reads %v; want auth.sign_in then org.created", got)
	}
	if rows[0].ActorEmail != "founder@example.com" || rows[0].IP == "" || rows[0].Via != viaConsole {
		t.Errorf("the sign-in row does not say who or from where: %+v", rows[0])
	}
	var signIn map[string]any
	json.Unmarshal(rows[0].Details, &signIn)
	if signIn["via"] != "signup" {
		t.Errorf("the sign-in row should say it was a signup: %v", signIn)
	}

	// A write no handler names: the floor.
	if code, body := authReq(t, mux, "PUT", "/api/account", map[string]string{"name": "Dana"}, token); code != 200 {
		t.Fatalf("rename = %d: %v", code, body)
	}
	rows = auditRows(t, st, orgID, AuditFilter{Limit: 1})
	floor := rows[0]
	if floor.Action != "console.request" || floor.TargetID != "PUT /api/account" || floor.Outcome != auditOK {
		t.Fatalf("the plain row is wrong: %+v", floor)
	}
	if floor.ActorEmail != "founder@example.com" || floor.ActorPublic != founder.PublicID || floor.IP == "" {
		t.Errorf("the plain row does not say who or from where: %+v", floor)
	}
	var d map[string]any
	json.Unmarshal(floor.Details, &d)
	if d["pattern"] != "/api/account" || d["status"] != float64(200) {
		t.Errorf("details = %v", d)
	}

	// A named event, and no plain row beside it.
	before := len(auditRows(t, st, orgID, AuditFilter{}))
	raw, keyID := mintKey(t, mux, token, "ci")
	rows = auditRows(t, st, orgID, AuditFilter{})
	if len(rows) != before+1 || rows[0].Action != "api_key.created" || rows[0].TargetName != "ci" {
		t.Fatalf("minting a key wrote %v; want exactly one api_key.created", actionsOf(rows[:min(len(rows), 3)]))
	}
	if strings.Contains(string(rows[0].Details), raw) {
		t.Fatal("the raw key is in the audit log")
	}

	// A refusal is recorded as one, by the person who was refused.
	viewer, err := st.CreateUser(ctx, "viewer@example.com", "Vic", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMembership(ctx, viewer.ID, orgID, RoleViewer, 0); err != nil {
		t.Fatal(err)
	}
	viewerTok, err := st.CreateAdminSession(ctx, AdminUser{ID: viewer.ID, Email: viewer.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := authReq(t, mux, "DELETE", fmt.Sprintf("/api/api-keys/%d", keyID), nil, viewerTok); code != 403 {
		t.Fatalf("a viewer revoked a key: %d", code)
	}
	rows = auditRows(t, st, orgID, AuditFilter{Limit: 1})
	if rows[0].Action != "console.request" || rows[0].Outcome != auditDenied || rows[0].ActorEmail != "viewer@example.com" {
		t.Errorf("the refusal was recorded as %+v", rows[0])
	}

	// A wrong password is recorded against the account's organisation; a wrong address nowhere.
	before = len(auditRows(t, st, orgID, AuditFilter{}))
	if code, _ := post(t, mux, "/api/auth/login", map[string]string{"email": "founder@example.com", "password": "not it"}); code != 401 {
		t.Fatalf("wrong password = %d", code)
	}
	rows = auditRows(t, st, orgID, AuditFilter{Limit: 1})
	if rows[0].Action != "auth.sign_in_failed" || rows[0].Outcome != auditDenied || rows[0].ActorEmail != "founder@example.com" {
		t.Errorf("the failed sign-in was recorded as %+v", rows[0])
	}
	post(t, mux, "/api/auth/login", map[string]string{"email": "nobody@example.com", "password": "not it"})
	if n := len(auditRows(t, st, orgID, AuditFilter{})); n != before+1 {
		t.Errorf("a sign-in for an unknown address wrote %d rows", n-before-1)
	}

	// Signing out is the last line of the session.
	post(t, mux, "/api/auth/logout", nil, &http.Cookie{Name: sessionCookie, Value: viewerTok})
	rows = auditRows(t, st, orgID, AuditFilter{Limit: 1})
	if rows[0].Action != "auth.sign_out" || rows[0].ActorEmail != "viewer@example.com" {
		t.Errorf("the sign-out was recorded as %+v", rows[0])
	}
}

// Reading the log is its own permission, held by admin and by nobody else among the built-ins,
// and the same filters serve the page, the export and the API.
func TestAuditLogIsReadBehindItsOwnPermission(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, token := signedUp(t, b, mux, st, "founder@example.com")
	raw, _ := mintKey(t, mux, token, "siem")

	editor, _ := st.CreateUser(ctx, "ed@example.com", "Ed", "")
	st.AddMembership(ctx, editor.ID, orgID, RoleEditor, 0)
	editorTok, _ := st.CreateAdminSession(ctx, AdminUser{ID: editor.ID, Email: editor.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if code, _ := authReq(t, mux, "GET", "/api/audit", nil, editorTok); code != 403 {
		t.Errorf("an editor read the audit log: %d", code)
	}

	code, body := authReq(t, mux, "GET", "/api/audit?action=auth.", nil, token)
	if code != 200 {
		t.Fatalf("GET /api/audit = %d: %v", code, body)
	}
	events, _ := body["events"].([]any)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for _, e := range events {
		if a := e.(map[string]any)["action"].(string); !strings.HasPrefix(a, "auth.") {
			t.Errorf("?action=auth. returned %s", a)
		}
	}
	if actions, _ := body["actions"].([]any); len(actions) < 2 {
		t.Errorf("the action menu is %v", actions)
	}

	// The export carries the same rows and is itself recorded.
	r := httptest.NewRequest("GET", "/api/audit.csv?action=auth.", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("export = %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != len(events)+1 || !strings.HasPrefix(lines[0], "id,time,action,outcome") {
		t.Errorf("the export has %d lines for %d events; first line %q", len(lines), len(events), lines[0])
	}
	if rows := auditRows(t, st, orgID, AuditFilter{Limit: 1}); rows[0].Action != "export.audit" {
		t.Errorf("the export was not recorded: latest row is %s", rows[0].Action)
	}

	// The API: a key with the permission reads it, walking forward from a cursor.
	r = httptest.NewRequest("GET", "/v1/audit?after=1", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out struct {
		Events []AuditEvent `json:"events"`
		LastID int64        `json:"last_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("GET /v1/audit = %d: %s", w.Code, w.Body.String())
	}
	if len(out.Events) < 2 || out.Events[0].ID != 2 || out.Events[0].ID > out.Events[1].ID || out.LastID != out.Events[len(out.Events)-1].ID {
		t.Errorf("?after=1 should walk forward from id 2 with last_id at the end: %+v last=%d", actionsOf(out.Events), out.LastID)
	}
	// A key without the permission does not.
	st.SetMemberRole(ctx, editor.ID, orgID, RoleAdmin)
	editorKey, _ := mintKey(t, mux, editorTok, "ed's")
	st.SetMemberRole(ctx, editor.ID, orgID, RoleEditor)
	r = httptest.NewRequest("GET", "/v1/audit", nil)
	r.Header.Set("Authorization", "Bearer "+editorKey)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("an editor's key read the audit log: %d", w.Code)
	}
}

// A script's writes are recorded like a person's, with the key they were made with.
func TestAPIKeyWritesNameTheKey(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, token := signedUp(t, b, mux, st, "founder@example.com")
	if err := st.SaveTeam(ctx, &Team{TeamID: "T1", OrgID: orgID, Name: "Acme", Status: "active"}, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	raw, keyID := mintKey(t, mux, token, "cron")

	body, _ := json.Marshal(map[string]string{"scope": "team:T1", "text": "the deploy window is Tuesday"})
	r := httptest.NewRequest("POST", "/v1/memories", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+raw)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("POST /v1/memories = %d: %s", w.Code, w.Body.String())
	}
	rows := auditRows(t, st, orgID, AuditFilter{Limit: 1})
	row := rows[0]
	if row.Action != "console.request" || row.Via != viaAPIKey || row.TargetID != "POST /v1/memories" || row.ActorEmail != "founder@example.com" {
		t.Fatalf("the API write was recorded as %+v", row)
	}
	var d map[string]any
	json.Unmarshal(row.Details, &d)
	if d["api_key_id"] != float64(keyID) {
		t.Errorf("the row does not name the key: %v", d)
	}
}

// The floor itself: a write that finished is recorded, a read is not, a failed validation is
// not, and a person's own notes are nobody else's business.
func TestAuditedWritesFloor(t *testing.T) {
	st := testStore(t)
	b := &Bot{store: st}
	me := &AdminUser{ID: 1, OrgID: 1, PublicID: "p1", Email: "dana@example.com", Via: "password"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/things", b.auditedWrites(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 201, map[string]any{"ok": true}) }))
	mux.HandleFunc("GET /api/things", b.auditedWrites(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) }))
	mux.HandleFunc("PUT /api/things/{id}", b.auditedWrites(func(w http.ResponseWriter, r *http.Request) { bad(w, fmt.Errorf("no")) }))
	mux.HandleFunc("DELETE /api/setup-links/{token}", b.auditedWrites(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) }))
	mux.HandleFunc("POST /api/personal-memories", b.auditedWrites(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 201, map[string]any{"ok": true}) }))
	do := func(method, path string) {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r = r.WithContext(context.WithValue(r.Context(), userKey, me))
		mux.ServeHTTP(httptest.NewRecorder(), r)
	}
	do("POST", "/api/things")
	do("GET", "/api/things")
	do("PUT", "/api/things/7")
	do("DELETE", "/api/setup-links/sl_secret_token")
	do("POST", "/api/personal-memories")

	rows := auditRows(t, st, 1, AuditFilter{})
	if len(rows) != 2 {
		t.Fatalf("%d rows for one create, one read, one failed update, one token revoke and one private note; want 2: %v", len(rows), actionsOf(rows))
	}
	if rows[1].TargetID != "POST /api/things" || rows[0].TargetID != "DELETE /api/setup-links/{token}" {
		t.Errorf("targets = %q, %q", rows[1].TargetID, rows[0].TargetID)
	}
	if strings.Contains(string(rows[0].Details)+rows[0].TargetID, "sl_secret_token") {
		t.Error("a setup-link token was written into the audit log")
	}
}
