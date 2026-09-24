package app

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Routines through the developer API, and through it MCP. The claims: a routine goes where the
// caller says — a channel the bot is in, by id or by name — and nowhere else; it runs as the bot,
// never as a person; its writes ask first, and nothing the API accepts can change that; and the
// console's rule about whose a routine is after an edit holds here too.

func routineRig(t *testing.T) (b *Bot, mux *http.ServeMux, key string, orgID int64, st *Store) {
	t.Helper()
	b, mux, st = identityBot(t)
	ctx := context.Background()
	_, orgID, session := signedUp(t, b, mux, st, "founder@example.com")
	key, _ = mintKey(t, mux, session, "routines")
	// #support once; #eng in two workspaces, so its name alone is not enough.
	for _, c := range []struct{ team, id, name string }{{"T1", "C1", "#support"}, {"T1", "C2", "#eng"}, {"T2", "C3", "#eng"}} {
		if _, err := st.UpsertChannelScope(ctx, orgID, c.team, c.id, c.name, false); err != nil {
			t.Fatal(err)
		}
	}
	return b, mux, key, orgID, st
}

func TestV1RoutineGoesWhereTheCallerSays(t *testing.T) {
	_, mux, key, orgID, st := routineRig(t)
	ctx := context.Background()
	create := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		return authReq(t, mux, "POST", "/v1/routines", body, key)
	}

	code, rt := create(map[string]any{"channel": "#support", "cron": "0 9 * * 1-5", "prompt": "Summarise yesterday's tickets"})
	if code != 201 {
		t.Fatalf("create by name = %d %v", code, rt)
	}
	for k, want := range map[string]any{"channel": "C1", "team_id": "T1", "channel_name": "#support",
		"auto_confirm": false, "created_by": "", "enabled": true} {
		if rt[k] != want {
			t.Errorf("%s = %v, want %v", k, rt[k], want)
		}
	}
	if rt["next_run"] == "" || rt["id"] == nil {
		t.Errorf("created routine = %v", rt)
	}
	if code, rt := create(map[string]any{"channel": "C1", "cron": "0 9 * * *", "prompt": "Daily check"}); code != 201 || rt["channel"] != "C1" {
		t.Errorf("create by id = %d %v", code, rt)
	}

	// A name in two workspaces is refused until the caller says which; then it is exact.
	if code, out := create(map[string]any{"channel": "#eng", "cron": "0 9 * * *", "prompt": "x"}); code != 400 || !strings.Contains(out["error"].(string), "team_id") {
		t.Errorf("an ambiguous name = %d %v", code, out)
	}
	if code, rt := create(map[string]any{"channel": "#eng", "team_id": "T2", "cron": "0 9 * * *", "prompt": "x"}); code != 201 || rt["channel"] != "C3" {
		t.Errorf("a name with its workspace = %d %v", code, rt)
	}

	before, _ := st.Routines(ctx, orgID, "")
	refused := []struct {
		name string
		body map[string]any
		says string
	}{
		{"a channel the bot is not in", map[string]any{"channel": "#nowhere", "cron": "0 9 * * *", "prompt": "x"}, "list_scopes"},
		{"writes that run unasked", map[string]any{"channel": "C1", "cron": "0 9 * * *", "prompt": "x", "auto_confirm": true}, "console"},
		{"a schedule under the floor", map[string]any{"channel": "C1", "cron": "*/5 * * * *", "prompt": "x"}, "at most every"},
		{"no prompt", map[string]any{"channel": "C1", "cron": "0 9 * * *"}, "prompt"},
		{"no channel", map[string]any{"cron": "0 9 * * *", "prompt": "x"}, "channel"},
	}
	for _, c := range refused {
		if code, out := create(c.body); code != 400 || !strings.Contains(out["error"].(string), c.says) {
			t.Errorf("%s = %d %v, want 400 mentioning %q", c.name, code, out, c.says)
		}
	}
	if after, _ := st.Routines(ctx, orgID, ""); len(after) != len(before) {
		t.Errorf("refused requests made %d routines", len(after)-len(before))
	}

	// The list says where each one posts, and whether its writes ask.
	_, list := authReq(t, mux, "GET", "/v1/routines", nil, key)
	for _, r := range list["routines"].([]any) {
		row := r.(map[string]any)
		if row["channel_name"] == "" || row["auto_confirm"] != false {
			t.Errorf("listed routine = %v", row)
		}
	}
}

func TestV1RoutineEditsKeepTheConsolesRules(t *testing.T) {
	b, mux, key, orgID, st := routineRig(t)
	ctx := context.Background()

	// A routine somebody wrote in the console after signing in with Slack runs as them.
	id, err := b.addRoutine(ctx, orgID, routineDraft{Channel: "C1", Cron: "0 9 * * *", Prompt: "Check my inbox", CreatedBy: "U42"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/routines/" + itoa(id)

	// Pausing it is housekeeping: it stays theirs.
	code, rt := authReq(t, mux, "PUT", path, map[string]any{"enabled": false}, key)
	if code != 200 || rt["enabled"] != false || rt["created_by"] != "U42" || rt["was_running_as"] != nil {
		t.Fatalf("pause = %d %v", code, rt)
	}
	// Rewriting what it does takes it off them: it runs as the bot from here, and the answer says
	// whose it was.
	code, rt = authReq(t, mux, "PUT", path, map[string]any{"prompt": "Check the shared inbox", "channel": "#support"}, key)
	if code != 200 || rt["created_by"] != "" || rt["was_running_as"] != "U42" || rt["prompt"] != "Check the shared inbox" {
		t.Fatalf("rewrite = %d %v", code, rt)
	}
	// Moving it by name, and the one setting the API will not turn on.
	if code, rt := authReq(t, mux, "PUT", path, map[string]any{"channel": "#eng", "team_id": "T1"}, key); code != 200 || rt["channel"] != "C2" {
		t.Errorf("move = %d %v", code, rt)
	}
	if code, out := authReq(t, mux, "PUT", path, map[string]any{"auto_confirm": true}, key); code != 400 || !strings.Contains(out["error"].(string), "console") {
		t.Errorf("auto_confirm through the API = %d %v", code, out)
	}
	// A refused edit changes nothing, not even the half of it that was fine.
	if code, _ := authReq(t, mux, "PUT", path, map[string]any{"enabled": true, "model": "not-a-model"}, key); code != 400 {
		t.Errorf("an edit naming a model nobody chose = %d", code)
	}
	if r := b.routineByID(ctx, orgID, id); r == nil || r.Enabled {
		t.Errorf("a refused edit resumed the routine: %+v", r)
	}

	// Another organisation's routine is not there to edit or delete.
	_, otherOrg, _ := signedUp(t, b, mux, st, "other@example.com")
	st.UpsertChannelScope(ctx, otherOrg, "T9", "C9", "#theirs", false)
	theirs, err := b.addRoutine(ctx, otherOrg, routineDraft{Channel: "C9", Cron: "0 9 * * *", Prompt: "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"PUT", "DELETE"} {
		if code, _ := authReq(t, mux, m, "/v1/routines/"+itoa(theirs), map[string]any{"enabled": false}, key); code != 404 {
			t.Errorf("%s on another organisation's routine = %d, want 404", m, code)
		}
	}
	if r := b.routineByID(ctx, otherOrg, theirs); r == nil || !r.Enabled {
		t.Error("another organisation's routine was changed")
	}
	// Nor may somebody create one there by naming its channel.
	if code, _ := authReq(t, mux, "POST", "/v1/routines", map[string]any{"channel": "C9", "cron": "0 9 * * *", "prompt": "x"}, key); code != 400 {
		t.Errorf("a routine in another organisation's channel = %d, want 400", code)
	}

	if code, _ := authReq(t, mux, "DELETE", path, nil, key); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := authReq(t, mux, "DELETE", path, nil, key); code != 404 {
		t.Errorf("deleting it twice = %d, want 404", code)
	}
}

func TestV1RoutineWritesNeedRoutinesManage(t *testing.T) {
	_, mux, _, orgID, st := routineRig(t)
	narrow := narrowMember(t, st, orgID, "reader@example.com", PermAPIKeysManage)
	key, _ := mintKey(t, mux, narrow, "reader")
	if code, _ := authReq(t, mux, "POST", "/v1/routines", map[string]any{"channel": "C1", "cron": "0 9 * * *", "prompt": "x"}, key); code != 403 {
		t.Errorf("create without routines.manage = %d, want 403", code)
	}
	names := mcpToolNames(t, mux, key)
	for _, tool := range []string{"create_routine", "update_routine", "delete_routine"} {
		if slices.Contains(names, tool) {
			t.Errorf("%s was offered to a role without routines.manage", tool)
		}
	}
}

// Over MCP the same routes are three tools; a model can pause one with a real boolean.
func TestMCPMakesChangesAndDeletesARoutine(t *testing.T) {
	b, mux, key, orgID, _ := routineRig(t)

	text, isErr := mcpToolCall(t, mux, key, "create_routine", map[string]any{
		"channel": "#support", "cron": "0 8 * * 1", "prompt": "Post the week's open tickets", "notify": "when_needed",
		"notify_when": "there is at least one ticket older than a week"})
	if isErr {
		t.Fatalf("create_routine = %s", text)
	}
	var made map[string]any
	if err := json.Unmarshal([]byte(text), &made); err != nil || made["channel"] != "C1" || made["notify"] != "when_needed" || made["auto_confirm"] != false {
		t.Fatalf("create_routine answered %s", text)
	}
	id := int64(made["id"].(float64))

	text, isErr = mcpToolCall(t, mux, key, "update_routine", map[string]any{"id": id, "enabled": false})
	if isErr || !strings.Contains(text, `"enabled":false`) {
		t.Fatalf("update_routine enabled=false = %s (error %v)", text, isErr)
	}
	if r := b.routineByID(context.Background(), orgID, id); r == nil || r.Enabled {
		t.Fatalf("the routine was not paused: %+v", r)
	}
	if text, isErr := mcpToolCall(t, mux, key, "update_routine", map[string]any{"id": id, "enabled": "maybe"}); !isErr || !strings.Contains(text, "true or false") {
		t.Errorf("a non-boolean enabled = %s (error %v)", text, isErr)
	}
	if text, isErr := mcpToolCall(t, mux, key, "delete_routine", map[string]any{"id": id}); isErr {
		t.Fatalf("delete_routine = %s", text)
	}
	if r := b.routineByID(context.Background(), orgID, id); r != nil {
		t.Error("the routine survived delete_routine")
	}
}
