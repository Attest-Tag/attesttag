package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The console's settings API for what came after the tree: the chat channel reviews are announced
// in, a write that names the fields it changes, and adding repositories to a connection or taking one
// out of code review. What these pin is the same as the rest of the API's tests — who may do what,
// that another organisation's ids and channels are nobody's here — and that a page left open cannot
// put back what somebody else changed since.

// withChannel connects workspace team to org and puts the bot in channel there, as an install and a
// channel sync do.
func withChannel(t *testing.T, st *Store, org int64, team, channel string) {
	t.Helper()
	ctx := context.Background()
	if err := st.SaveTeam(ctx, &Team{TeamID: team, OrgID: org, Name: team}, nil); err != nil && err != ErrTeamOwnedElsewhere {
		t.Fatal(err)
	}
	if _, err := st.UpsertChannelScope(ctx, org, team, channel, "#"+channel, false); err != nil {
		t.Fatal(err)
	}
}

func settingsBody(s map[string]any) map[string]any { return map[string]any{"settings": s} }

// The channel is a setting like any single value: inherited, overridden by a branch rule, and only
// ever one of the organisation's own connected channels — named by its workspace too, which the
// server fills in when only one workspace has the channel. Setting it, a rule's, or clearing it is
// connections.manage's: it carries private repositories' findings to whoever reads the channel.
func TestReviewAPINotifyChannel(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	withChannel(t, rig.st, orgID, "T1", "C1")
	withChannel(t, rig.st, orgID, "T1", "C2")
	other, _ := secondOrg(t, rig.st)
	withChannel(t, rig.st, other, "T9", "C9")
	conn := "/api/review-settings/" + rig.connID()

	if code, out := rig.call("PUT", conn, rig.editor, settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"}})); code != 403 ||
		!slices.Contains(fieldsOf(out), "notify") {
		t.Errorf("an editor naming a channel = %d %v", code, out)
	}
	for _, bad := range []map[string]any{{"team": "T9", "channel": "C9"}, {"channel": "C9"}, {"channel": "C404"},
		{"team": "T1", "channel": "C9"}, {"team": "T1"}} {
		if code, out := rig.call("PUT", conn, rig.admin, settingsBody(map[string]any{"notify": bad})); code != 400 ||
			!strings.Contains(out["error"].(string), "notify") {
			t.Errorf("notify %v = %d %v, want a 400 about it", bad, code, out)
		}
	}
	// The channel alone, as an API caller may know it: the workspace is filled in.
	out := rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"notify": map[string]any{"channel": "C1"}}))
	if n, _ := out["own"].(map[string]any)["notify"].(map[string]any); n["team"] != "T1" || n["channel"] != "C1" {
		t.Errorf("stored = %s", reviewJSON(out["own"]))
	}
	repo := rig.must(200, "GET", rig.repoPath()+"&base=main&head=feature", rig.viewer, nil)
	eff := repo["effective"].(map[string]any)
	if n := eff["notify"].(map[string]any); n["channel"] != "C1" || eff["source"].(map[string]any)["notify"] != "connection" {
		t.Errorf("the repository inherits notify %v from %v", n, eff["source"].(map[string]any)["notify"])
	}

	// A form re-saving it unchanged beside the editor's own change is not refused for it; changing or
	// clearing it is, at any level and by any route.
	rig.must(200, "PUT", conn, rig.editor, settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"},
		"strictness": "high"}))
	for what, body := range map[string]map[string]any{
		"clearing it":                settingsBody(map[string]any{"strictness": "high"}),
		"turning it off":             settingsBody(map[string]any{"notify": map[string]any{}, "strictness": "high"}),
		"clearing it by naming it":   {"fields": []string{"notify"}, "settings": map[string]any{}},
		"giving a rule its own":      settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"}, "strictness": "high", "branch_rules": []map[string]any{{"base": "main", "notify": map[string]any{"team": "T1", "channel": "C2"}}, {}}}),
		"giving a rule an empty one": settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"}, "strictness": "high", "branch_rules": []map[string]any{{"base": "main", "notify": map[string]any{}}, {}}}),
	} {
		if code, out := rig.call("PUT", conn, rig.editor, body); code != 403 {
			t.Errorf("an editor %s = %d %v", what, code, out)
		}
	}
	if code, out := rig.call("PUT", rig.repoPath(), rig.editor, settingsBody(map[string]any{"notify": map[string]any{}})); code != 403 ||
		!slices.Contains(fieldsOf(out), "notify") {
		t.Errorf("an editor silencing one repository = %d %v", code, out)
	}
	rule := []map[string]any{{"base": "main", "notify": map[string]any{"channel": "C2"}}, {}}
	if code, out := rig.call("PUT", conn, rig.admin, settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"},
		"branch_rules": []map[string]any{{"base": "main", "notify": map[string]any{"team": "T9", "channel": "C9"}}, {}}})); code != 400 {
		t.Errorf("a rule naming another organisation's channel = %d %v", code, out)
	}
	rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"notify": map[string]any{"team": "T1", "channel": "C1"}, "branch_rules": rule}))
	d := rig.must(200, "GET", rig.repoPath()+"&base=main&head=feature", rig.viewer, nil)
	if n := d["rule"].(map[string]any)["effective"].(map[string]any)["notify"].(map[string]any); n["team"] != "T1" || n["channel"] != "C2" {
		t.Errorf("a pull request into main is announced in %v, want the rule's channel with its workspace filled in", n)
	}
	// Moving a repository under a level that sets no channel of its own changes nothing about it.
	gid := rig.must(200, "POST", "/api/review-settings", rig.editor, map[string]any{"kind": "group", "connection_id": rig.connID(),
		"name": "Web"})["node"].(map[string]any)["id"].(string)
	rig.must(200, "POST", rig.repoPathTo("move"), rig.editor, map[string]any{"parent": gid})
	// A group that silences its repositories is a change of channel for them: not the editor's.
	if code, out := rig.call("PUT", "/api/review-settings/"+gid, rig.editor, settingsBody(map[string]any{"notify": map[string]any{}})); code != 403 {
		t.Errorf("an editor silencing a group = %d %v", code, out)
	}
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.admin, settingsBody(map[string]any{"notify": map[string]any{}}))
	if code, out := rig.call("DELETE", "/api/review-settings/"+gid, rig.editor, nil); code != 403 || !slices.Contains(fieldsOf(out), "notify") {
		t.Errorf("an editor deleting the group that kept its repositories quiet = %d %v", code, out)
	}
	// A Slack Connect channel shared into two of the organisation's workspaces has to be named with one.
	withChannel(t, rig.st, orgID, "T2", "C1")
	if code, out := rig.call("PUT", "/api/review-settings/"+gid, rig.admin, settingsBody(map[string]any{"notify": map[string]any{"channel": "C1"}})); code != 400 ||
		!strings.Contains(out["error"].(string), "say which with team") {
		t.Errorf("an ambiguous channel = %d %v", code, out)
	}
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.admin, settingsBody(map[string]any{"notify": map[string]any{"team": "T2", "channel": "C1"}}))
}

// The channel is judged branch by branch, on the one each branch is announced in. A rule's empty
// channel that stops overriding — the repository moved under a group that names the channel, or its
// own copy of the rules reset so the farther list applies — announces what it held quiet, and is not
// the editor's however the list itself reads. A restore is judged against a level that told nobody,
// so a rule that only silences, or a channel under a level that is off, switches nothing on.
func TestReviewAPIChannelIsJudgedWhereEachBranchIsAnnounced(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	withChannel(t, rig.st, orgID, "T1", "C1")
	conn := "/api/review-settings/" + rig.connID()
	c1 := map[string]any{"team": "T1", "channel": "C1"}
	quietSecurity := []map[string]any{{"head": "security/**", "notify": map[string]any{}}, {}}
	rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"mode": "shadow", "notify": c1, "branch_rules": quietSecurity}))
	gid := rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "group", "connection_id": rig.connID(),
		"name": "Web"})["node"].(map[string]any)["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.admin, settingsBody(map[string]any{"notify": c1}))

	if code, out := rig.call("POST", rig.repoPathTo("move"), rig.editor, map[string]any{"parent": gid}); code != 403 ||
		!slices.Contains(fieldsOf(out), "notify") {
		t.Errorf("an editor moving a repository where the rule's silence no longer reaches = %d %v", code, out)
	}
	repoID := rig.must(200, "POST", rig.repoPathTo("move"), rig.admin, map[string]any{"parent": gid})["node"].(map[string]any)["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+repoID, rig.admin, settingsBody(map[string]any{"branch_rules": quietSecurity}))
	if code, out := rig.call("PUT", "/api/review-settings/"+repoID, rig.editor, map[string]any{"fields": []string{"branch_rules"},
		"settings": map[string]any{}}); code != 403 || !slices.Contains(fieldsOf(out), "notify") {
		t.Errorf("an editor resetting the repository's own copy of the rules = %d %v", code, out)
	}
	// Tuning the list without moving any branch's channel stays the editor's.
	rig.must(200, "PUT", "/api/review-settings/"+repoID, rig.editor, settingsBody(map[string]any{"branch_rules": []map[string]any{
		{"head": "security/**", "notify": map[string]any{}, "types": []string{"security"}}, {}}}))

	// Restores: a list that only silences switches no channel on, a level that is off switches nothing
	// on, and a channel under one that reviews is the connections permission's.
	rig.must(200, "DELETE", "/api/review-settings/"+repoID, rig.admin, nil)
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.admin, settingsBody(map[string]any{}))
	rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"mode": "shadow", "branch_rules": quietSecurity}))
	for _, mode := range []string{"shadow", "off"} {
		rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"mode": mode, "branch_rules": quietSecurity}))
		rig.must(200, "POST", rig.repoPathTo("remove"), rig.editor, nil)
		rig.must(200, "POST", rig.repoPathTo("restore"), rig.editor, nil)
		rig.must(200, "DELETE", conn, rig.editor, nil)
		rig.must(200, "POST", conn+"/restore", rig.editor, nil)
	}
	rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"mode": "off", "notify": c1}))
	rig.must(200, "DELETE", conn, rig.editor, nil)
	rig.must(200, "POST", conn+"/restore", rig.editor, nil)
	rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"mode": "shadow", "notify": c1}))
	rig.must(200, "DELETE", conn, rig.editor, nil)
	if code, out := rig.call("POST", conn+"/restore", rig.editor, nil); code != 403 || !slices.Contains(fieldsOf(out), "notify") {
		t.Errorf("an editor restoring a connection that announces in a channel = %d %v", code, out)
	}
}

// repoPathTo is the rig's repository addressed through its connection, on one of the routes under it.
func (rig *reviewAPIRig) repoPathTo(route string) string {
	return "/api/review-settings/" + rig.connID() + "/" + route + "?repo=acme/web"
}

// A write that names its fields changes those and nothing else, so a page open since before somebody
// else's save cannot put that save back by sending the settings it showed. Without fields the body is
// still the level's whole object, as the API always took it.
func TestReviewAPIFieldsKeepWhatAWriteDoesNotName(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	conn := "/api/review-settings/" + rig.connID()
	rules := []map[string]any{{"base": "main", "types": []string{"security"}}, {}}
	rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"mode": "shadow", "branch_rules": rules}))

	// What a page loaded before the rules were saved sends: its stale object, and the field it changed.
	stale := map[string]any{"mode": "shadow", "strictness": "high"}
	out := rig.must(200, "PUT", conn, rig.editor, map[string]any{"fields": []string{"strictness"}, "settings": stale})
	own := string(reviewJSON(out["own"]))
	if !strings.Contains(own, `"strictness":"high"`) || !strings.Contains(own, `"branch_rules":[{"base":"main","types":["security"]},{}]`) {
		t.Errorf("a write naming strictness changed something else: %s", own)
	}
	events, _ := rig.st.AuditEvents(context.Background(), orgID, AuditFilter{Action: "review.settings_updated"})
	var d map[string]any
	json.Unmarshal(events[0].Details, &d)
	if got := reviewJSON(d["changed"]); string(got) != `["strictness"]` {
		t.Errorf("audited as changing %s", got)
	}
	// Named and left out of the settings is reset.
	out = rig.must(200, "PUT", conn, rig.editor, map[string]any{"fields": []string{"strictness"}, "settings": map[string]any{}})
	if own := string(reviewJSON(out["own"])); strings.Contains(own, "strictness") || !strings.Contains(own, "branch_rules") {
		t.Errorf("resetting strictness by name: %s", own)
	}
	for _, bad := range []any{[]string{"strictnes"}, []string{"source"}} {
		if code, out := rig.call("PUT", conn, rig.editor, map[string]any{"fields": bad, "settings": stale}); code != 400 {
			t.Errorf("fields %v = %d %v", bad, code, out)
		}
	}
	// The whole-object write is unchanged: what it leaves out goes.
	out = rig.must(200, "PUT", conn, rig.editor, settingsBody(stale))
	if own := string(reviewJSON(out["own"])); strings.Contains(own, "branch_rules") {
		t.Errorf("a whole-object write kept a field it left out: %s", own)
	}
}

// installationRepos makes the fake GitHub list repos as what the rig's installation reaches.
func (rig *reviewAPIRig) installationRepos(repos map[string]bool) {
	rig.fake.mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		list := []map[string]any{}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for k := range repos {
				if !yield(k) {
					return
				}
			}
		}) {
			list = append(list, map[string]any{"full_name": name, "private": repos[name]})
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": len(list), "repositories": list})
	})
}

func repoNamesOf(list any) []string {
	var out []string
	for _, v := range list.([]any) {
		switch v := v.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			out = append(out, anyString(v["repo"]))
		}
	}
	slices.Sort(out)
	return out
}

func anyString(v any) string {
	s, _ := v.(string)
	return s
}

// Add repositories offers what the installation reaches at GitHub and the tree does not hold, and
// saves each one asked for as the organisation's App connection, once it is on GitHub's list; one
// taken out of code review comes back the same way. Remove from reviews takes a repository out of
// the tree and out of review — its deliveries are not kept, the gate refuses it — and leaves its
// connection for the bot's tools and fix jobs. Adding saves connections, so it is connections.manage's;
// removing stops reviews, which reviews.manage may do; restoring is judged like restoring a stopped
// connection. Another organisation's connection is not found.
func TestReviewAPIAddAndRemoveRepositories(t *testing.T) {
	ctx := context.Background()
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	rig.installationRepos(map[string]bool{"Acme/Web": true, "acme/api": true, "acme/docs": false})
	conn := "/api/review-settings/" + rig.connID()

	if code, _ := rig.call("GET", conn+"/available", rig.editor, nil); code != 403 {
		t.Errorf("an editor reading Add repositories' list = %d", code)
	}
	avail := rig.must(200, "GET", conn+"/available", rig.admin, nil)
	if got := repoNamesOf(avail["repos"]); !slices.Equal(got, []string{"acme/api", "acme/docs"}) {
		t.Errorf("available = %v, want what the tree does not hold", got)
	}
	for _, r := range avail["repos"].([]any) {
		r := r.(map[string]any)
		if r["repo"] == "acme/docs" && (r["private"] != false || r["visibility"] != "public") ||
			r["repo"] == "acme/api" && (r["private"] != true || r["visibility"] != "private" || r["removed"] != false) {
			t.Errorf("available entry = %v", r)
		}
	}

	add := map[string]any{"repos": []string{"acme/api", "acme/elsewhere", "https://github.com/acme/web", "acme/api"}}
	if code, _ := rig.call("POST", conn+"/repos", rig.editor, add); code != 403 {
		t.Errorf("an editor adding repositories = %d", code)
	}
	out := rig.must(200, "POST", conn+"/repos", rig.admin, add)
	if !slices.Equal(repoNamesOf(out["added"]), []string{"acme/api"}) || !slices.Equal(repoNamesOf(out["already"]), []string{"acme/web"}) ||
		len(out["failed"].([]any)) != 1 || !strings.Contains(string(reviewJSON(out["failed"])), "acme/elsewhere") {
		t.Errorf("adding = %v", out)
	}
	if code, out := rig.call("POST", conn+"/repos", rig.admin, map[string]any{"repos": []string{"acme/elsewhere"}}); code != 400 {
		t.Errorf("adding only a repository the installation does not reach = %d %v", code, out)
	}
	saved := func(repo string) bool {
		conns, _ := rig.st.AllConnections(ctx, orgID)
		return slices.ContainsFunc(conns, func(c *Connection) bool {
			return strings.EqualFold(c.Repo, repo) && c.CredType == "github_app" && c.GitHubInstallationID == fakeInstallation
		})
	}
	if !saved("acme/api") || !slices.Contains(repoNamesOf(rig.conn()["repos"]), "acme/api") {
		t.Fatalf("acme/api is not saved and in the tree: %v", rig.conn()["repos"])
	}

	// Removed by an editor: out of the tree, out of review, still connected.
	api := "/api/review-settings/" + rig.connID() + "/remove?repo=acme/api"
	node := rig.must(200, "POST", api, rig.editor, nil)["node"].(map[string]any)
	c := rig.conn()
	if slices.Contains(repoNamesOf(c["repos"]), "acme/api") || !slices.Equal(repoNamesOf(c["removed"]), []string{"acme/api"}) ||
		node["mode"] != "off" || node["removed_at"] == "" {
		t.Errorf("after removing: repos %v, removed %v, node %v", repoNamesOf(c["repos"]), repoNamesOf(c["removed"]), node)
	}
	if mode, err := rig.st.ReviewModeAt(ctx, orgID, fakeInstallation, "acme/api"); err != nil || mode != "off" {
		t.Errorf("a removed repository's deliveries are kept: mode %q (%v)", mode, err)
	}
	if _, s, err := rig.b.reviewEffective(ctx, orgID, fakeInstallation, "acme/api"); err != nil || s == nil || s.Reason != "removed" {
		t.Errorf("the gate on a removed repository: %+v (%v)", s, err)
	}
	if !saved("acme/api") {
		t.Error("removing a repository from code review deleted its connection")
	}
	if code, _ := rig.call("DELETE", "/api/review-settings/"+node["id"].(string), rig.admin, nil); code != 400 {
		t.Errorf("resetting a removed repository's settings, which would put it back = %d", code)
	}
	avail = rig.must(200, "GET", conn+"/available", rig.admin, nil)
	if got := repoNamesOf(avail["repos"]); !slices.Equal(got, []string{"acme/api", "acme/docs"}) ||
		!strings.Contains(string(reviewJSON(avail["repos"])), `"removed":true,"repo":"acme/api"`) {
		t.Errorf("available after removing = %s", reviewJSON(avail["repos"]))
	}

	// Back through Add repositories.
	out = rig.must(200, "POST", conn+"/repos", rig.admin, map[string]any{"repos": []string{"acme/api"}})
	if !slices.Equal(repoNamesOf(out["restored"]), []string{"acme/api"}) || len(out["added"].([]any)) != 0 {
		t.Errorf("adding a removed repository = %v", out)
	}
	if mode, _ := rig.st.ReviewModeAt(ctx, orgID, fakeInstallation, "acme/api"); mode != "shadow" {
		t.Errorf("restored acme/api resolves to %q", mode)
	}
	// And through restore, by an editor while nothing it wakes posts live; not once it would.
	rig.must(200, "POST", api, rig.editor, nil)
	rig.must(200, "POST", "/api/review-settings/"+rig.connID()+"/restore?repo=acme/api", rig.editor, nil)
	rig.must(200, "POST", api, rig.editor, nil)
	rig.must(200, "PUT", conn, rig.admin, settingsBody(map[string]any{"mode": "live"}))
	if code, out := rig.call("POST", "/api/review-settings/"+rig.connID()+"/restore?repo=acme/api", rig.editor, nil); code != 403 ||
		!slices.Contains(fieldsOf(out), "mode") {
		t.Errorf("an editor restoring a repository that would post live = %d %v", code, out)
	}
	rig.must(200, "POST", "/api/review-settings/"+rig.connID()+"/restore?repo=acme/api", rig.admin, nil)
	// A repository nobody set anything on is removed too, its row made for it.
	rig.must(200, "POST", "/api/review-settings/"+rig.connID()+"/remove?repo=acme/web", rig.editor, nil)
	if mode, _ := rig.st.ReviewModeAt(ctx, orgID, fakeInstallation, "acme/web"); mode != "off" {
		t.Errorf("a removed repository with no settings resolves to %q", mode)
	}
	if code, _ := rig.call("POST", conn+"/remove", rig.editor, nil); code != 400 {
		t.Errorf("removing a connection as if it were a repository = %d", code)
	}

	events, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review."})
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	for _, want := range []string{"review.repos_added", "review.repo_removed", "review.repo_restored"} {
		if !slices.Contains(actions, want) {
			t.Errorf("audit lacks %s: %v", want, actions)
		}
	}

	// Another organisation: the connection is not found, however it is asked.
	_, other := secondOrg(t, rig.st)
	for _, c := range []struct{ method, path string }{{"GET", conn + "/available"}, {"POST", conn + "/repos"},
		{"POST", api}, {"POST", "/api/review-settings/" + rig.connID() + "/restore?repo=acme/api"}} {
		if code, out := rig.call(c.method, c.path, other, map[string]any{"repos": []string{"acme/api"}}); code != 404 {
			t.Errorf("another organisation's %s %s = %d %v", c.method, c.path, code, out)
		}
	}
}

// An installation that reaches more repositories than one listing reads can still have any of them
// added: one past the listing is asked about by name, and refused when the installation does not
// reach it. A removed repository whose connection went too is saved again and restored, and said once.
func TestReviewAPIAddRepositoriesPastTheListing(t *testing.T) {
	ctx := context.Background()
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	rig.fake.mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		list := []map[string]any{}
		for i := range 100 {
			list = append(list, map[string]any{"full_name": fmt.Sprintf("acme/p%s-%03d", r.URL.Query().Get("page"), i), "private": true})
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1000, "repositories": list})
	})
	rig.fake.mux.HandleFunc("GET /repos/acme/far", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/far", "private": true})
	})
	conn := "/api/review-settings/" + rig.connID()
	if avail := rig.must(200, "GET", conn+"/available", rig.admin, nil); avail["truncated"] != true {
		t.Fatalf("the listing should say it was cut short: %v", avail["truncated"])
	}
	out := rig.must(200, "POST", conn+"/repos", rig.admin, map[string]any{"repos": []string{"acme/far", "acme/nowhere"}})
	if !slices.Equal(repoNamesOf(out["added"]), []string{"acme/far"}) || len(out["failed"].([]any)) != 1 ||
		!strings.Contains(string(reviewJSON(out["failed"])), "acme/nowhere") {
		t.Errorf("adding past the listing = %v", out)
	}
	if !slices.Contains(repoNamesOf(rig.conn()["repos"]), "acme/far") {
		t.Errorf("acme/far is not in the tree: %v", rig.conn()["repos"])
	}

	// Removed, and its connection deleted under Access bundles: one repository back, restored.
	rig.must(200, "POST", rig.repoPathTo("remove"), rig.admin, nil)
	conns, _ := rig.st.AllConnections(ctx, orgID)
	for _, c := range conns {
		if strings.EqualFold(c.Repo, "acme/web") {
			if err := rig.st.DeleteConnection(ctx, orgID, c.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	rig.fake.mux.HandleFunc("GET /repos/acme/web", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/web", "private": true})
	})
	out = rig.must(200, "POST", conn+"/repos", rig.admin, map[string]any{"repos": []string{"acme/web"}})
	if len(out["added"].([]any)) != 0 || !slices.Equal(repoNamesOf(out["restored"]), []string{"acme/web"}) {
		t.Errorf("a removed repository with no connection left, added again = %v", out)
	}
}

// A connection whose installation the organisation no longer holds lists nothing from GitHub, and
// adds nothing: the listing is never a way to read an installation that is not this organisation's.
func TestReviewAPIAddRepositoriesNeedsTheInstallation(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	rig.installationRepos(map[string]bool{"acme/api": true})
	conn := "/api/review-settings/" + rig.connID()
	if _, err := rig.st.db.ExecContext(context.Background(), `update github_installs set status='revoked' where org_id=? and installation_id=?`,
		orgID, fakeInstallation); err != nil {
		t.Fatal(err)
	}
	if code, out := rig.call("GET", conn+"/available", rig.admin, nil); code != 404 {
		t.Errorf("listing an installation no longer held = %d %v", code, out)
	}
	if code, out := rig.call("POST", conn+"/repos", rig.admin, map[string]any{"repos": []string{"acme/api"}}); code != 404 {
		t.Errorf("adding through an installation no longer held = %d %v", code, out)
	}
	for _, s := range rig.fake.sent() {
		if strings.Contains(s, "/installation/repositories") {
			t.Errorf("GitHub was asked for a revoked installation's repositories: %s", s)
		}
	}
}
