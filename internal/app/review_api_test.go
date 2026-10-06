package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"attesttag/internal/review"
)

// The console's code review API, on the lane rig: the real routes, the real store on both dialects,
// GitHub and the model faked. What these pin is who may do what — a viewer reads, an editor tunes
// the noise, and posting live, every push, forks, context, models and money stay with whoever holds
// connections.manage however the change is reached — that another organisation's ids answer like
// ids that do not exist, and that the settings, types and runs the console shows are what the lane
// actually runs.

type reviewAPIRig struct {
	*laneRig
	mux                   *http.ServeMux
	admin, editor, viewer string
	bundle                int64
}

func newReviewAPIRig(t *testing.T, settings string) *reviewAPIRig {
	t.Helper()
	return newReviewAPIRigOn(t, newLaneRig(t, totalsFixture(), settings))
}

// newReviewAPIRigOn is the console's routes, an admin, an editor and a viewer on a lane rig, with
// acme/web saved as one of the organisation's App connections.
func newReviewAPIRigOn(t *testing.T, lr *laneRig) *reviewAPIRig {
	t.Helper()
	b, st := lr.b, lr.st
	b.slacks, b.mail, b.resolver = NewChatRegistry(st, b.sealer), logMailer{}, NewResolver(st)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	rig := &reviewAPIRig{laneRig: lr, mux: mux}
	org, _, admin := seedOrg(t, st, RoleAdmin)
	if org != orgID {
		t.Fatalf("the rig's organisation is %d, want %d", org, orgID)
	}
	rig.admin = admin
	rig.editor = rig.member("editor@acme.test", RoleEditor)
	rig.viewer = rig.member("viewer@acme.test", RoleViewer)
	bd, err := st.CreateBundle(context.Background(), orgID, repoBundleName, "")
	if err != nil {
		t.Fatal(err)
	}
	rig.bundle = bd.ID
	rig.appRepo("acme/web", fakeInstallation)
	return rig
}

// member adds somebody to the rig's organisation in role and returns a session for them.
func (rig *reviewAPIRig) member(email, role string) string {
	rig.t.Helper()
	ctx := context.Background()
	u, err := rig.st.CreateUser(ctx, email, email, "")
	if err != nil {
		rig.t.Fatal(err)
	}
	if err := rig.st.AddMembership(ctx, u.ID, orgID, role, 0); err != nil {
		rig.t.Fatal(err)
	}
	tok, err := rig.st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Email: email, OrgID: orgID}, time.Hour)
	if err != nil {
		rig.t.Fatal(err)
	}
	return tok
}

// appRepo saves repo as one of the organisation's App connections under installation, the way
// installing the App files the repositories somebody picked.
func (rig *reviewAPIRig) appRepo(repo string, installation int64) {
	rig.t.Helper()
	c, sec, err := rig.b.buildConnection(&connectionInput{BundleID: rig.bundle, Name: strings.ReplaceAll(repo, "/", "-"),
		Preset: "github", CredType: "github_app", Secret: &Secret{InstallationID: installation}}, nil)
	if err != nil {
		rig.t.Fatal(err)
	}
	c.Repo, c.Status, c.GitHubInstallationID = repo, "active", installation
	enc, err := rig.b.sealSecret(sec)
	if err != nil {
		rig.t.Fatal(err)
	}
	if _, err := rig.st.InsertConnection(context.Background(), orgID, c, enc); err != nil {
		rig.t.Fatal(err)
	}
}

// call makes one console request and decodes the answer.
func (rig *reviewAPIRig) call(method, path, token string, body any) (int, map[string]any) {
	rig.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	rig.mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// must is call, failing the test on any status but want.
func (rig *reviewAPIRig) must(want int, method, path, token string, body any) map[string]any {
	rig.t.Helper()
	code, out := rig.call(method, path, token, body)
	if code != want {
		rig.t.Fatalf("%s %s = %d %v, want %d", method, path, code, out, want)
	}
	return out
}

// tree is GET /api/review-settings, and conn the rig's connection in it.
func (rig *reviewAPIRig) tree(token string) map[string]any {
	rig.t.Helper()
	return rig.must(200, "GET", "/api/review-settings", token, nil)
}

func (rig *reviewAPIRig) conn() map[string]any {
	rig.t.Helper()
	for _, c := range rig.tree(rig.admin)["connections"].([]any) {
		if c := c.(map[string]any); c["installation_id"] == float64(fakeInstallation) {
			return c
		}
	}
	rig.t.Fatal("the rig's connection is not in the tree")
	return nil
}

func (rig *reviewAPIRig) connID() string { return rig.conn()["id"].(string) }

// repoPath is the settings route for acme/web, addressed by its connection and name until it has
// a row of its own.
func (rig *reviewAPIRig) repoPath() string {
	return "/api/review-settings/" + rig.connID() + "?repo=acme/web"
}

func fieldsOf(out map[string]any) []string {
	var fs []string
	for _, f := range out["fields"].([]any) {
		fs = append(fs, f.(string))
	}
	return fs
}

// A viewer reads and changes nothing. An editor tunes the noise, and is refused — naming the field —
// everything that posts, spends or reaches further: live, every push, forks, context repositories,
// the model, max_usd, and a branch rule that posts live; and is not refused a form that sends an
// admin's value back unchanged. An admin may do all of it.
func TestReviewAPIPermissionMatrix(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	rig.appRepo("acme/api", fakeInstallation)

	for _, path := range []string{"/api/review-settings", "/api/review-types", "/api/reviews", "/api/review-types/general"} {
		rig.must(200, "GET", path, rig.viewer, nil)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PUT", rig.repoPath(), map[string]any{"settings": map[string]any{"strictness": "high"}}},
		{"POST", "/api/review-settings", map[string]any{"kind": "group", "connection_id": rig.connID(), "name": "Frontend"}},
		{"POST", "/api/review-types", map[string]any{"key": "api-contract", "name": "API contract"}},
		{"PUT", "/api/review-types/general", map[string]any{"purpose": "Anything."}},
		{"POST", "/api/reviews", map[string]any{"repo": "acme/web", "pr": 7}},
		{"POST", "/api/review-types/try", map[string]any{"repo": "acme/web", "pr": 7, "type": map[string]any{"name": "Try"}}},
		{"POST", "/api/review-pulls/resume", map[string]any{"repo": "acme/web", "pr": 7}},
	} {
		if code, out := rig.call(c.method, c.path, rig.viewer, c.body); code != 403 || !strings.Contains(out["error"].(string), "code review") {
			t.Errorf("a viewer's %s %s = %d %v", c.method, c.path, code, out)
		}
	}

	rig.must(200, "PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{"strictness": "high",
		"instructions": []string{"Check the error paths."}, "ignore_paths": []string{"**/*.snap"}}})
	for field, val := range map[string]any{"mode": "live", "trigger": "push", "forks": "off", "model": "heavy", "max_usd": 2,
		"context_repos": []string{"acme/api"}} {
		code, out := rig.call("PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{"strictness": "high", field: val}})
		if code != 403 || !slices.Contains(fieldsOf(out), field) {
			t.Errorf("an editor setting %s = %d %v", field, code, out)
		}
	}
	liveRule := []map[string]any{{"base": "main", "post": "live"}, {}}
	if code, out := rig.call("PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{"branch_rules": liveRule}}); code != 403 ||
		!slices.Contains(fieldsOf(out), "branch_rules") {
		t.Errorf("an editor's branch rule that posts live = %d %v", code, out)
	}
	// A rule that only picks types is the editor's.
	quiet := []map[string]any{{"base": "main", "types": []string{"general", "security"}}, {}}
	rig.must(200, "PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{"strictness": "high", "branch_rules": quiet}})

	// The admin sets all of it.
	rig.must(200, "PUT", rig.repoPath(), rig.admin, map[string]any{"settings": map[string]any{"mode": "live", "trigger": "push",
		"forks": "off", "model": "heavy", "max_usd": 2, "context_repos": []string{"acme/api"}, "branch_rules": liveRule}})
	// An editor saving the form back with one noise field changed is not refused for the admin's.
	rig.must(200, "PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{"mode": "live", "trigger": "push",
		"forks": "off", "model": "heavy", "max_usd": 2, "context_repos": []string{"acme/api"}, "branch_rules": liveRule,
		"strictness": "low"}})
	// But clearing the admin's model, or their live, is a change too.
	if code, out := rig.call("PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{"strictness": "low"}}); code != 403 {
		t.Errorf("an editor clearing the admin's fields = %d %v", code, out)
	}
}

// The side doors: a field an editor may not set must not be reachable by making the level inherit
// it. Resetting a repository's shadow under a live connection, deleting the group that held it to
// shadow, and moving it out of that group each make it post live, and each is refused an editor.
func TestReviewAPITiersHoldThroughInheritance(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.must(200, "PUT", rig.repoPath(), rig.admin, map[string]any{"settings": map[string]any{"mode": "shadow"}})
	if code, out := rig.call("PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{}}); code != 403 ||
		!slices.Contains(fieldsOf(out), "mode") {
		t.Errorf("an editor resetting shadow under a live connection = %d %v", code, out)
	}
	repoID := rig.must(200, "GET", rig.repoPath(), rig.admin, nil)["node"].(map[string]any)["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+repoID, rig.admin, map[string]any{"settings": map[string]any{}})
	g := rig.must(200, "POST", "/api/review-settings", rig.editor, map[string]any{"kind": "group", "connection_id": rig.connID(),
		"name": "Frontend"})["node"].(map[string]any)
	gid := g["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.admin, map[string]any{"settings": map[string]any{"mode": "shadow"}})
	// Into the group is shadow: no reach needed.
	rig.must(200, "POST", "/api/review-settings/"+repoID+"/move", rig.editor, map[string]any{"parent": gid})
	if code, out := rig.call("POST", "/api/review-settings/"+repoID+"/move", rig.editor, map[string]any{"parent": rig.connID()}); code != 403 {
		t.Errorf("an editor moving a repository out of a shadow group under a live connection = %d %v", code, out)
	}
	if code, out := rig.call("DELETE", "/api/review-settings/"+gid, rig.editor, nil); code != 403 {
		t.Errorf("an editor deleting the shadow group = %d %v", code, out)
	}
	rig.must(200, "DELETE", "/api/review-settings/"+gid, rig.admin, nil)
	// Stopping a live connection is anybody's who manages reviews; starting it again turns its
	// posting back on, and is not — whether by restore or by adding it again.
	rig.must(200, "DELETE", "/api/review-settings/"+rig.connID(), rig.editor, nil)
	if code, out := rig.call("POST", "/api/review-settings/"+rig.connID()+"/restore", rig.editor, nil); code != 403 ||
		!slices.Contains(fieldsOf(out), "mode") {
		t.Errorf("an editor restoring a live connection = %d %v", code, out)
	}
	if code, out := rig.call("POST", "/api/review-settings", rig.editor, map[string]any{"kind": "connection",
		"installation_id": fakeInstallation}); code != 403 {
		t.Errorf("an editor adding a stopped live connection again = %d %v", code, out)
	}
	rig.must(200, "POST", "/api/review-settings/"+rig.connID()+"/restore", rig.admin, nil)
}

// The tier is judged branch by branch and at every repository a change reaches. A level switched on
// from off is judged against nothing at all, so whatever it kept dormant is what switching it on
// does; a rule list by where each branch ends up, so deleting the rule that held some branches in
// shadow is going live for them; and a write to a group, or deleting it, by what it wakes in the
// repositories under it. Each is refused an editor; the same change with nothing live or every push
// to wake is theirs.
func TestReviewAPITiersFollowBranchesAndDormantSettings(t *testing.T) {
	settings := func(s map[string]any) map[string]any { return map[string]any{"settings": s} }
	refused := func(rig *reviewAPIRig, what, method, path string, body any, field string) {
		t.Helper()
		code, out := rig.call(method, path, rig.editor, body)
		if code != 403 || !slices.Contains(fieldsOf(out), field) {
			t.Errorf("an editor %s = %d %v, want 403 naming %s", what, code, out, field)
		}
	}
	liveRule := []map[string]any{{"post": "live"}}

	// A live rule, and every push, kept dormant under off.
	rig := newReviewAPIRig(t, `{"mode":"off","branch_rules":[{"post":"live"}]}`)
	conn := "/api/review-settings/" + rig.connID()
	refused(rig, "switching on a connection whose rule posts live", "PUT", conn,
		settings(map[string]any{"mode": "shadow", "branch_rules": liveRule}), "mode")
	rig = newReviewAPIRig(t, `{"mode":"off","trigger":"push"}`)
	conn = "/api/review-settings/" + rig.connID()
	refused(rig, "switching on a connection that reviews every push", "PUT", conn,
		settings(map[string]any{"mode": "shadow", "trigger": "push"}), "trigger")
	rig.must(200, "PUT", conn, rig.editor, settings(map[string]any{"mode": "shadow"}))

	// The rule that kept main in shadow, above a live fallback, deleted.
	rig = newReviewAPIRig(t, `{"mode":"shadow","branch_rules":[{"base":"main","post":"shadow"},{"post":"live"}]}`)
	conn = "/api/review-settings/" + rig.connID()
	refused(rig, "deleting the rule that kept main in shadow", "PUT", conn,
		settings(map[string]any{"mode": "shadow", "branch_rules": liveRule}), "branch_rules")
	// Tuning beside it, and changing what the shadow rule runs, moves no branch: the editor's.
	rig.must(200, "PUT", conn, rig.editor, settings(map[string]any{"mode": "shadow", "strictness": "high",
		"branch_rules": []map[string]any{{"base": "main", "post": "shadow", "types": []string{"security"}}, {"post": "live"}}}))

	// A live repository's one exception, deleted.
	rig = newReviewAPIRig(t, `{"mode":"live","branch_rules":[{"base":"release/*","post":"shadow"},{}]}`)
	refused(rig, "deleting the rule that kept release branches in shadow", "PUT", "/api/review-settings/"+rig.connID(),
		settings(map[string]any{"mode": "live", "branch_rules": []map[string]any{{}}}), "branch_rules")
	// Where every branch already posts live, an exception only narrows it.
	rig = newReviewAPIRig(t, `{"mode":"live"}`)
	rig.must(200, "PUT", "/api/review-settings/"+rig.connID(), rig.editor, settings(map[string]any{"mode": "live",
		"branch_rules": []map[string]any{{"base": "release/*", "post": "shadow"}, {}}}))

	// A group switched off, holding a repository whose own rule posts live: switching the group on,
	// and deleting it, each wake that rule.
	rig = newReviewAPIRig(t, `{"mode":"shadow"}`)
	gid := rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "group", "connection_id": rig.connID(),
		"name": "Frontend"})["node"].(map[string]any)["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.admin, settings(map[string]any{"mode": "off"}))
	repoID := rig.must(200, "PUT", rig.repoPath(), rig.admin, settings(map[string]any{"branch_rules": liveRule}))["node"].(map[string]any)["id"].(string)
	rig.must(200, "POST", "/api/review-settings/"+repoID+"/move", rig.admin, map[string]any{"parent": gid})
	refused(rig, "switching on a group over a repository whose own rule posts live", "PUT", "/api/review-settings/"+gid,
		settings(map[string]any{"mode": "shadow"}), "mode")
	refused(rig, "deleting a group that held such a repository off", "DELETE", "/api/review-settings/"+gid, nil, "mode")
	// With nothing live under it, both are the editor's.
	rig.must(200, "PUT", "/api/review-settings/"+repoID, rig.admin, settings(map[string]any{}))
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.editor, settings(map[string]any{"mode": "shadow"}))
	rig.must(200, "DELETE", "/api/review-settings/"+gid, rig.editor, nil)
}

// A connection added as a copy of another is added in shadow, branch rules and all: a rule that
// posted live there records in shadow here, until somebody makes this connection live, while one
// that recorded in shadow stays so. Nothing in it posts, so an editor may add it.
func TestReviewAPICopiedConnectionPostsNothing(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live","branch_rules":[{"base":"main","post":"live"},{"base":"release/*","post":"shadow"},{}]}`)
	seedInstall(t, rig.st, orgID, 5151, "octo-org")
	n := rig.must(200, "POST", "/api/review-settings", rig.editor, map[string]any{"kind": "connection", "installation_id": 5151,
		"copy_from": rig.connID()})["node"].(map[string]any)
	for _, base := range []string{"main", "release/1.2", "testing"} {
		d := rig.must(200, "GET", "/api/review-settings/"+n["id"].(string)+"?base="+base+"&head=feature", rig.viewer, nil)
		if mode := d["rule"].(map[string]any)["effective"].(map[string]any)["mode"]; mode != "shadow" {
			t.Errorf("a pull request into %s on the copied connection is %v, want shadow", base, mode)
		}
	}
	got := string(reviewJSON(n["settings"]))
	if !strings.Contains(got, `{"base":"release/*","post":"shadow"}`) || strings.Contains(got, `"post":"live"`) {
		t.Errorf("copied settings = %s", got)
	}
}

// The tree: connections at the top with how their installation stands — account, status, when GitHub
// last delivered, what review still needs accepted — then groups, then repositories, including every
// repository the installation reaches through the organisation's App connections that nobody has
// set anything on yet, marked as inheriting; and the installations not in the tree yet, for Add
// connection.
func TestReviewAPITreeListsConnectionsGroupsReposAndAvailableInstalls(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live","strictness":"high"}`)
	ctx := context.Background()
	rig.appRepo("acme/docs", fakeInstallation)
	if err := rig.st.SetGitHubInstallPermissions(ctx, orgID, fakeInstallation, `{"contents":"read","pull_requests":"read","metadata":"read"}`); err != nil {
		t.Fatal(err)
	}
	if err := rig.st.SaveGitHubInstall(ctx, &GitHubInstall{ID: 5151, OrgID: orgID, AccountLogin: "octo-org", AccountType: "Organization",
		RepoSelection: "all", Permissions: `{"contents":"write","pull_requests":"write"}`}); err != nil {
		t.Fatal(err)
	}
	rig.appRepo("octo-org/site", 5151)
	if _, err := rig.st.enqueueGitHubDelivery(ctx, "d-1", orgID, fakeInstallation, "ping", "", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	g := rig.must(200, "POST", "/api/review-settings", rig.editor, map[string]any{"kind": "group", "connection_id": rig.connID(),
		"name": "Frontend"})["node"].(map[string]any)
	rig.must(200, "POST", "/api/review-settings/"+rig.connID()+"/move?repo=acme/web", rig.editor, map[string]any{"parent": g["id"]})

	c := rig.conn()
	if c["status"] != "active" || c["account_login"] != "acme" || c["last_delivery_at"] == "" || c["mode"] != "live" ||
		!slices.Equal(reviewStrings(c["missing_permissions"]), []string{"pull_requests:write"}) || c["permissions_known"] != true {
		t.Errorf("connection = %v", c)
	}
	if c["installed_by"] != "admin@example.com" || c["installed_at"] == "" || c["suspended_at"] != "" {
		t.Errorf("the connection's installation: by %v at %v, suspended %v", c["installed_by"], c["installed_at"], c["suspended_at"])
	}
	groups := c["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	inGroup := groups[0].(map[string]any)["repos"].([]any)
	if len(inGroup) != 1 || inGroup[0].(map[string]any)["repo"] != "acme/web" || inGroup[0].(map[string]any)["inherits"] != false {
		t.Errorf("the group's repositories = %v", inGroup)
	}
	repos := c["repos"].([]any)
	if len(repos) != 1 || repos[0].(map[string]any)["repo"] != "acme/docs" || repos[0].(map[string]any)["inherits"] != true ||
		repos[0].(map[string]any)["id"] != "" || repos[0].(map[string]any)["mode"] != "live" {
		t.Errorf("the connection's own repositories = %v", repos)
	}
	avail := rig.tree(rig.viewer)["available"].([]any)
	if len(avail) != 1 {
		t.Fatalf("available = %v", avail)
	}
	a := avail[0].(map[string]any)
	if a["installation_id"] != float64(5151) || a["account_login"] != "octo-org" || !slices.Equal(reviewStrings(a["repos"]), []string{"octo-org/site"}) ||
		len(reviewStrings(a["missing_permissions"])) != 0 {
		t.Errorf("available = %v", a)
	}
}

func reviewStrings(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, s := range list {
		out = append(out, s.(string))
	}
	return out
}

// A level's detail says what it sets, what runs, and where each value comes from — connection,
// group, the repository itself, or the built-in default — and what it would inherit with nothing
// set here. A branch preview says which rule a pull request between two branches falls under.
func TestReviewAPIDetailSaysWhereEachValueComesFrom(t *testing.T) {
	rig := newReviewAPIRig(t, `{"strictness":"high","instructions":["Mind the tenant."],"branch_rules":[{"base":"main","types":["security"]},{}]}`)
	g := rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "group", "connection_id": rig.connID(),
		"name": "Backend"})["node"].(map[string]any)
	gid := g["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+gid, rig.admin, map[string]any{"settings": map[string]any{"max_comments": 3}})
	rig.must(200, "POST", "/api/review-settings/"+rig.connID()+"/move?repo=acme/web", rig.admin, map[string]any{"parent": gid})
	repoID := rig.must(200, "GET", rig.repoPath(), rig.admin, nil)["node"].(map[string]any)["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+repoID, rig.editor, map[string]any{"settings": map[string]any{"drafts": true,
		"instructions": []string{"Check the migrations."}}})

	d := rig.must(200, "GET", "/api/review-settings/"+repoID+"?base=main&head=feature", rig.viewer, nil)
	eff := d["effective"].(map[string]any)
	src := eff["source"].(map[string]any)
	for field, want := range map[string]string{"strictness": "connection", "max_comments": "group", "drafts": "repo", "mode": "default",
		"instructions": "repo", "branch_rules": "connection"} {
		if src[field] != want {
			t.Errorf("source of %s = %v, want %s", field, src[field], want)
		}
	}
	if !slices.Equal(reviewStrings(eff["instructions"]), []string{"Mind the tenant.", "Check the migrations."}) {
		t.Errorf("instructions add up down the chain: %v", eff["instructions"])
	}
	inh := d["inherited"].(map[string]any)
	if inh["drafts"] != false || inh["max_comments"] != float64(3) {
		t.Errorf("inherited = %v", inh)
	}
	chain := d["chain"].([]any)
	if len(chain) != 3 || chain[1].(map[string]any)["name"] != "Backend" || chain[2].(map[string]any)["name"] != "acme/web" {
		t.Errorf("chain = %v", chain)
	}
	rule := d["rule"].(map[string]any)
	if rule["index"] != float64(0) || rule["label"] != "any → main" || !slices.Equal(reviewStrings(rule["types"]), []string{"security"}) {
		t.Errorf("rule = %v", rule)
	}
	if own := d["own"].(map[string]any); own["drafts"] != true || own["strictness"] != nil {
		t.Errorf("own = %v", own)
	}
	// A key review does not know is refused rather than saved to be ignored; so is a branch rule
	// naming a type that does not exist, and context outside the organisation's App connections.
	for _, bad := range []map[string]any{{"strictnes": "high"}, {"branch_rules": []map[string]any{{"types": []string{"nope"}}}},
		{"context_repos": []string{"someone/else"}}} {
		if code, out := rig.call("PUT", "/api/review-settings/"+repoID, rig.admin, map[string]any{"settings": bad}); code != 400 {
			t.Errorf("PUT %v = %d %v", bad, code, out)
		}
	}
}

// Add connection takes an installation the organisation holds, in shadow even when it starts from a
// copy of a live one; refuses one it does not hold, alike whether it is nobody's or another
// organisation's; stopping keeps everything and restoring brings it back; deleting a group moves its
// repositories up to the connection.
func TestReviewAPIAddStopRestoreConnectionAndDeleteGroup(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live","strictness":"low"}`)
	ctx := context.Background()
	seedInstall(t, rig.st, orgID, 5151, "octo-org")
	other, _ := secondOrg(t, rig.st)
	seedInstall(t, rig.st, other, 6161, "elsewhere")

	add := map[string]any{"kind": "connection", "installation_id": 5151, "copy_from": rig.connID()}
	n := rig.must(200, "POST", "/api/review-settings", rig.editor, add)["node"].(map[string]any)
	if n["mode"] != "shadow" || !strings.Contains(string(reviewJSON(n["settings"])), `"strictness":"low"`) {
		t.Errorf("added = %v", n)
	}
	if code, _ := rig.call("POST", "/api/review-settings", rig.admin, add); code != 409 {
		t.Errorf("adding it twice = %d", code)
	}
	_, nobody := rig.call("POST", "/api/review-settings", rig.admin, map[string]any{"kind": "connection", "installation_id": 999})
	code, elsewhere := rig.call("POST", "/api/review-settings", rig.admin, map[string]any{"kind": "connection", "installation_id": 6161})
	if code != 404 || elsewhere["error"] != nobody["error"] {
		t.Errorf("another organisation's installation = %d %v; nobody's = %v", code, elsewhere, nobody)
	}

	id := n["id"].(string)
	rig.must(200, "DELETE", "/api/review-settings/"+id, rig.editor, nil)
	if ok, _ := rig.st.ReviewedInstallation(ctx, orgID, 5151); ok {
		t.Error("a stopped connection is still reviewed")
	}
	stopped := rig.must(200, "GET", "/api/review-settings/"+id, rig.viewer, nil)["node"].(map[string]any)
	if stopped["removed_at"] == "" {
		t.Errorf("stopped = %v", stopped)
	}
	rig.must(200, "POST", "/api/review-settings/"+id+"/restore", rig.editor, nil)
	if ok, _ := rig.st.ReviewedInstallation(ctx, orgID, 5151); !ok {
		t.Error("a restored connection is not reviewed")
	}

	g := rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "group", "connection_id": rig.connID(),
		"name": "Frontend"})["node"].(map[string]any)
	rig.must(200, "POST", "/api/review-settings/"+rig.connID()+"/move?repo=acme/web", rig.admin, map[string]any{"parent": g["id"]})
	rig.must(200, "DELETE", "/api/review-settings/"+g["id"].(string), rig.admin, nil)
	c := rig.conn()
	repos := c["repos"].([]any)
	if len(c["groups"].([]any)) != 0 || len(repos) != 1 || repos[0].(map[string]any)["repo"] != "acme/web" ||
		repos[0].(map[string]any)["parent_id"] != rig.connID() {
		t.Errorf("after deleting the group: groups %v, repos %v", c["groups"], repos)
	}
	// A repository moves only within its connection.
	if code, _ := rig.call("POST", "/api/review-settings/"+rig.connID()+"/move?repo=acme/web", rig.admin, map[string]any{"parent": id}); code != 400 {
		t.Errorf("a move to another connection = %d", code)
	}
}

// Types: the built-ins listed as shipped, version 0; editing one copies it and saves the edit as the
// next version, keeping the built-in rules that were not changed as built-in; versions, revert and
// reset all add a version rather than rewriting one; a model change is connections.manage's; a new
// type can start from a copy; turning one off keeps it; and a type is counted by the branch rules
// naming it.
func TestReviewAPITypesCopyOnWriteVersionsRevertReset(t *testing.T) {
	rig := newReviewAPIRig(t, `{"branch_rules":[{"base":"main","types":["security","general"]},{"types":["security"]}]}`)
	list := rig.must(200, "GET", "/api/review-types", rig.viewer, nil)["types"].([]any)
	byKey := map[string]map[string]any{}
	for _, v := range list {
		byKey[v.(map[string]any)["key"].(string)] = v.(map[string]any)
	}
	gen := byKey["general"]
	if gen == nil || gen["builtin"] != true || gen["edited"] != false || gen["version"] != float64(0) || gen["used_by"] != float64(1) ||
		byKey["security"]["used_by"] != float64(2) {
		t.Fatalf("types = %v", list)
	}
	shipped := gen["rules"].([]any)
	if len(shipped) < 2 {
		t.Fatalf("the general type ships %d rules", len(shipped))
	}
	var edit []map[string]any
	for i, r := range shipped {
		rule := map[string]any{"text": r.(map[string]any)["text"], "severity_cap": r.(map[string]any)["severity_cap"]}
		if i == 0 {
			rule["enabled"] = false
		}
		edit = append(edit, rule)
	}
	edit = append(edit, map[string]any{"text": "Every handler checks the organisation.", "severity_cap": "P1"})
	if code, out := rig.call("PUT", "/api/review-types/general", rig.editor, map[string]any{"model": "heavy", "rules": edit}); code != 403 {
		t.Errorf("an editor setting a type's model = %d %v", code, out)
	}
	saved := rig.must(200, "PUT", "/api/review-types/general", rig.editor, map[string]any{"version": 0, "rules": edit})["type"].(map[string]any)
	rules := saved["rules"].([]any)
	if saved["version"] != float64(2) || saved["edited"] != true || saved["id"] == "" || len(rules) != len(shipped)+1 ||
		rules[0].(map[string]any)["enabled"] != false || rules[1].(map[string]any)["source"] != "builtin" ||
		rules[len(rules)-1].(map[string]any)["source"] != "team" {
		t.Fatalf("saved = %v", saved)
	}
	id := saved["id"].(string)
	// A save against the version somebody else replaced is refused.
	if code, _ := rig.call("PUT", "/api/review-types/"+id, rig.editor, map[string]any{"version": 1, "purpose": "Stale."}); code != 409 {
		t.Errorf("a stale save = %d", code)
	}
	vs := rig.must(200, "GET", "/api/review-types/"+id+"/versions", rig.viewer, nil)["versions"].([]any)
	if len(vs) != 2 || vs[0].(map[string]any)["version"] != float64(2) {
		t.Errorf("versions = %v", vs)
	}
	v1 := rig.must(200, "GET", "/api/review-types/general/versions/1", rig.viewer, nil)["type"].(map[string]any)
	if len(v1["rules"].([]any)) != len(shipped) {
		t.Errorf("version 1 = %v", v1)
	}
	reverted := rig.must(200, "POST", "/api/review-types/"+id+"/revert", rig.editor, map[string]any{"version": 1})["type"].(map[string]any)
	if reverted["version"] != float64(3) || len(reverted["rules"].([]any)) != len(shipped) || reverted["edited"] != false {
		t.Errorf("reverted = %v", reverted)
	}
	rig.must(200, "PUT", "/api/review-types/general", rig.admin, map[string]any{"version": 3, "model": "heavy", "purpose": "Ours."})
	if code, _ := rig.call("POST", "/api/review-types/general/reset", rig.editor, nil); code != 403 {
		t.Errorf("an editor's reset that clears an admin's model = %d", code)
	}
	reset := rig.must(200, "POST", "/api/review-types/general/reset", rig.admin, nil)["type"].(map[string]any)
	if reset["edited"] != false || reset["version"] != float64(5) || reset["model"] != "" {
		t.Errorf("reset = %v", reset)
	}

	created := rig.must(200, "POST", "/api/review-types", rig.editor, map[string]any{"key": "api-contract", "name": "API contract",
		"copy_from": "security"})["type"].(map[string]any)
	if created["custom"] != true || created["version"] != float64(1) || len(created["rules"].([]any)) == 0 {
		t.Errorf("created = %v", created)
	}
	if code, _ := rig.call("POST", "/api/review-types", rig.editor, map[string]any{"key": "security", "name": "Mine"}); code != 409 {
		t.Errorf("a type under a built-in's key = %d", code)
	}
	off := rig.must(200, "POST", "/api/review-types/api-contract/disable", rig.editor, nil)["type"].(map[string]any)
	if off["enabled"] != false || off["version"] != float64(2) {
		t.Errorf("disabled = %v", off)
	}
	keys, _ := rig.b.reviewTypeKeys(context.Background(), orgID)
	if slices.Contains(keys, "api-contract") {
		t.Error("a type turned off is still offered")
	}
	rig.must(200, "POST", "/api/review-types/api-contract/enable", rig.editor, nil)
	// Turning off a built-in nobody edited copies it.
	if sec := rig.must(200, "POST", "/api/review-types/security/disable", rig.editor, nil)["type"].(map[string]any); sec["enabled"] != false ||
		sec["builtin"] != true || sec["version"] != float64(2) {
		t.Errorf("security disabled = %v", sec)
	}
}

// A copy of a built-in keeps getting what later releases add to it. A rule the built-in ships that
// the copy lacks runs, as shipped, and the console lists it as new; switching it off saves it into
// the copy, off, and it stays off; a built-in rule deleted from the copy by hand is kept, off, rather
// than coming back as new. Made here as a copy from before the release that added the last rule.
func TestReviewAPICopiesGetNewBuiltinRules(t *testing.T) {
	ctx := context.Background()
	rig := newReviewAPIRig(t, `{}`)
	bt, _ := review.BuiltinType("security")
	older := reviewTypeRowOfBuiltin(bt)
	added := older.Rules[len(older.Rules)-1].Text
	older.Rules = older.Rules[:len(older.Rules)-1]
	if _, err := rig.st.CopyBuiltinReviewType(ctx, orgID, older, "admin@acme.test"); err != nil {
		t.Fatal(err)
	}
	runs := func(text string) (on, found bool) {
		t.Helper()
		specs, _, err := resolveReviewTypes(ctx, rig.st, orgID, []string{"security"})
		if err != nil || len(specs) != 1 {
			t.Fatalf("security resolves to %+v (%v)", specs, err)
		}
		for _, r := range specs[0].Rules {
			if r.Text == text {
				return !r.Off, true
			}
		}
		return false, false
	}
	if on, found := runs(added); !on || !found {
		t.Fatalf("a rule the built-in added since the copy was made: running %v, there %v", on, found)
	}
	view := rig.must(200, "GET", "/api/review-types/security", rig.viewer, nil)["type"].(map[string]any)
	if nb, _ := view["new_builtin_rules"].([]any); len(nb) != 1 || nb[0].(map[string]any)["text"] != added {
		t.Errorf("new built-in rules = %v", view["new_builtin_rules"])
	}

	// Switched off: the console sends the rule list with it, off.
	rules := view["rules"].([]any)
	rules = append(rules, map[string]any{"text": added, "enabled": false})
	saved := rig.must(200, "PUT", "/api/review-types/security", rig.editor, map[string]any{"version": view["version"], "rules": rules})["type"].(map[string]any)
	if on, found := runs(added); on || !found {
		t.Errorf("the new rule switched off: running %v, there %v", on, found)
	}
	if nb, _ := saved["new_builtin_rules"].([]any); len(nb) != 0 {
		t.Errorf("a rule switched off is still offered as new: %v", nb)
	}
	// The first built-in rule deleted by hand: kept, off.
	first := saved["rules"].([]any)[0].(map[string]any)["text"].(string)
	saved = rig.must(200, "PUT", "/api/review-types/security", rig.editor, map[string]any{"version": saved["version"],
		"rules": saved["rules"].([]any)[1:]})["type"].(map[string]any)
	if on, found := runs(first); on || !found {
		t.Errorf("a built-in rule deleted from the copy: running %v, there %v", on, found)
	}
	// Reworded: the team's words run, and the shipped ones are kept, off.
	rules = saved["rules"].([]any)
	second := rules[0].(map[string]any)["text"].(string)
	rules[0].(map[string]any)["text"] = "Our own wording of it."
	rig.must(200, "PUT", "/api/review-types/security", rig.editor, map[string]any{"version": saved["version"], "rules": rules})
	if on, found := runs("Our own wording of it."); !on || !found {
		t.Errorf("the reworded rule: running %v, there %v", on, found)
	}
	if on, found := runs(second); on || !found {
		t.Errorf("the shipped wording of a reworded rule: running %v, there %v", on, found)
	}
}

// Turning a type on or off is its own switch: reverting to a version saved while it was off leaves it
// on, and a model taken off the organisation's list since the type was saved does not stop the type
// being switched off, or a rule beside it being saved.
func TestReviewAPITypeSwitchSurvivesRevertAndAStaleModel(t *testing.T) {
	ctx := context.Background()
	rig := newReviewAPIRig(t, `{}`)
	created := rig.must(200, "POST", "/api/review-types", rig.admin, map[string]any{"key": "api-contract", "name": "API contract",
		"rules": []map[string]any{{"text": "Every handler checks the organisation."}}})["type"].(map[string]any)
	rig.must(200, "POST", "/api/review-types/api-contract/disable", rig.editor, nil) // version 2, off
	rig.must(200, "POST", "/api/review-types/api-contract/enable", rig.editor, nil)  // version 3, on
	reverted := rig.must(200, "POST", "/api/review-types/api-contract/revert", rig.editor, map[string]any{"version": 2})["type"].(map[string]any)
	if reverted["enabled"] != true {
		t.Errorf("reverting to a version saved while the type was off turned it off: %v", reverted)
	}

	// A model the organisation no longer offers, as if taken off its list after it was saved.
	if _, err := rig.st.db.ExecContext(ctx, `update review_types set model='retired/model-1' where org_id=? and public_id=?`,
		orgID, created["id"]); err != nil {
		t.Fatal(err)
	}
	rig.must(200, "POST", "/api/review-types/api-contract/disable", rig.editor, nil)
	cur := rig.must(200, "GET", "/api/review-types/api-contract", rig.viewer, nil)["type"].(map[string]any)
	rig.must(200, "PUT", "/api/review-types/api-contract", rig.editor, map[string]any{"version": cur["version"],
		"purpose": "Checks the API contract."})
	if code, _ := rig.call("PUT", "/api/review-types/api-contract", rig.admin, map[string]any{"model": "another/retired"}); code != 400 {
		t.Errorf("setting a model the organisation does not offer = %d", code)
	}
}

// A new type tried before it has a key is tried from nothing — not from a type of the organisation's
// that happens to be called "draft", whose paths, model and rules it would otherwise inherit.
func TestReviewAPITryOfANewTypeStartsFromNothing(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.model.verify = confirmAll(90)
	rig.must(200, "POST", "/api/review-types", rig.admin, map[string]any{"key": "draft", "name": "Draft notes",
		"path_globs": []string{"docs/**"}, "rules": []map[string]any{{"text": "Drafts are spelled right."}}})
	out := rig.must(200, "POST", "/api/review-types/try", rig.editor, map[string]any{"repo": "acme/web", "pr": 7,
		"type": map[string]any{"name": "Totals", "purpose": "Whether the totals hold.",
			"rules": []map[string]any{{"text": "Totals are updated under the lock."}}}})
	d := rig.must(200, "GET", "/api/reviews/"+out["run"].(map[string]any)["id"].(string), rig.viewer, nil)
	inline := d["request"].(map[string]any)["inline"].(map[string]any)
	if globs, _ := inline["path_globs"].([]any); len(globs) != 0 || inline["name"] != "Totals" {
		t.Errorf("the new type was tried as %v", inline)
	}
}

// Try on a PR runs a type as it is being edited, on the pull request picked, in shadow — even on a
// live repository, and even for an admin — and leaves the pull request's own review untouched: no
// post, no summary, no head marked reviewed, no score.
func TestReviewAPITryRunsAnInlineTypeAndNeverPosts(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.model.finder["api-contract"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(lockFinding()) }
	rig.model.verify = confirmAll(90)
	body := map[string]any{"repo": "acme/web", "pr": 7, "type": map[string]any{"key": "api-contract", "name": "API contract",
		"purpose": "Whether the totals keep their contract.", "rules": []map[string]any{{"text": "Totals are updated under the lock.", "severity_cap": "P1"}}}}
	out := rig.must(200, "POST", "/api/review-types/try", rig.admin, body)
	runID := out["run"].(map[string]any)["id"].(string)
	if out["run"].(map[string]any)["kind"] != "try" || out["run"].(map[string]any)["post"] != "shadow" {
		t.Errorf("queued = %v", out)
	}
	rig.drain()
	run, _ := rig.st.ReviewRunByPublicID(context.Background(), orgID, runID)
	if run == nil || run.Status != "shadow" || run.Kind != "try" || len(run.Types) != 1 || run.Types[0] != (ReviewRunType{Key: "api-contract", Version: 0}) {
		t.Fatalf("run = %+v", run)
	}
	posts, reviews, comments, _ := rig.gh.snapshot()
	if len(posts)+len(reviews)+len(comments) != 0 {
		t.Errorf("a try wrote to GitHub: %d posts, %d comments", len(posts), len(comments))
	}
	pr := rig.pr(7)
	if pr.LastReviewedSHA != "" || pr.ReviewsCount != 0 || pr.Score != -1 || pr.HeadSHA != "" {
		t.Errorf("a try moved the pull request's review: %+v", pr)
	}
	finders := rig.model.requests("finder")
	if len(finders) == 0 || !strings.Contains(finders[0].System, "Totals are updated under the lock.") {
		t.Error("the finder was not given the tried type's rule")
	}
	d := rig.must(200, "GET", "/api/reviews/"+runID, rig.viewer, nil)
	if fs := d["findings"].([]any); len(fs) != 1 || d["request"].(map[string]any)["inline"] == nil {
		t.Errorf("the try's detail = %v", d)
	}
	// The pull request's own review, when it comes, is not held back by what the try found.
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(lockFinding()) }
	rig.drain()
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 || len(posts[0]["comments"].([]reviewInlineComment)) != 1 {
		t.Errorf("the real review after a try posted %v", posts)
	}
	// An editor may not try a model.
	body["type"].(map[string]any)["model"] = "heavy"
	if code, _ := rig.call("POST", "/api/review-types/try", rig.editor, body); code != 403 {
		t.Errorf("an editor trying a model = %d", code)
	}
}

// Start review: past the "when" setting and the author list, which are for reviews nobody asked for;
// posting live on a shadow repository is connections.manage's; the same head with the same types is
// answered from the review that exists, and Run anyway runs it again.
func TestReviewAPIManualStart(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow","trigger":"command","exclude_authors":["octocat"]}`)
	rig.confirmLock()
	if code, out := rig.call("POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7, "post": "live"}); code != 403 {
		t.Errorf("an editor posting live on a shadow repository = %d %v", code, out)
	}
	if code, out := rig.call("POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7, "types": []string{"nope"}}); code != 400 {
		t.Errorf("an unknown type = %d %v", code, out)
	}
	if code, out := rig.call("POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/other", "pr": 7}); code != 404 {
		t.Errorf("a repository the organisation does not reach = %d %v", code, out)
	}
	out := rig.must(200, "POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7})
	first := out["run"].(map[string]any)
	if first["trigger"] != "console" || out["answered_from_state"] != false || !strings.HasPrefix(first["requested_by"].(string), "console:") {
		t.Errorf("started = %v", out)
	}
	rig.drain()
	if runs := rig.runs(7); len(runs) != 1 || runs[0].Status != "shadow" {
		t.Fatalf("runs = %+v", runs)
	}
	again := rig.must(200, "POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7})
	if again["answered_from_state"] != true || again["run"].(map[string]any)["id"] != first["id"] {
		t.Errorf("the same review again = %v", again)
	}
	if n := len(rig.runs(7)); n != 1 {
		t.Errorf("answering from state queued a run: %d runs", n)
	}
	forced := rig.must(200, "POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7, "force": true})
	if forced["answered_from_state"] != false || forced["run"].(map[string]any)["id"] == first["id"] {
		t.Errorf("run anyway = %v", forced)
	}
	rig.drain()
	// Another type is another review.
	other := rig.must(200, "POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7, "types": []string{"security"}})
	if other["answered_from_state"] != false {
		t.Errorf("another type was answered from state: %v", other)
	}
	rig.drain()
	// The admin posts live on the shadow repository, and Run again on that keeps it live.
	live := rig.must(200, "POST", "/api/reviews", rig.admin, map[string]any{"repo": "acme/web", "pr": 7, "post": "live"})
	rig.drain()
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 {
		t.Errorf("a live start posted %d reviews", len(posts))
	}
	if code, out := rig.call("POST", "/api/reviews/"+live["run"].(map[string]any)["id"].(string)+"/rerun", rig.editor,
		map[string]any{"force": true}); code != 403 {
		t.Errorf("an editor running a live review again on a shadow repository = %d %v", code, out)
	}
	rerun := rig.must(200, "POST", "/api/reviews/"+first["id"].(string)+"/rerun", rig.editor, map[string]any{"force": true})
	if rerun["run"].(map[string]any)["post"] != "shadow" || rerun["left_out"] != nil {
		t.Errorf("rerun = %v", rerun)
	}
	rig.drain()

	// Run again on a review whose type was turned off since runs the types still on and says which
	// it left out, rather than refusing the whole run for one of them; a review of nothing but types
	// that are off has nothing left to run again.
	both := rig.must(200, "POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7,
		"types": []string{"general", "security"}, "force": true})["run"].(map[string]any)
	rig.drain()
	rig.must(200, "POST", "/api/review-types/security/disable", rig.editor, nil)
	without := rig.must(200, "POST", "/api/reviews/"+both["id"].(string)+"/rerun", rig.editor, map[string]any{"force": true})
	if !slices.Equal(reviewStrings(without["left_out"]), []string{"security"}) {
		t.Errorf("run again without the type turned off = %v", without)
	}
	rig.drain()
	var ran []string
	for _, ty := range rig.must(200, "GET", "/api/reviews/"+without["run"].(map[string]any)["id"].(string), rig.viewer, nil)["run"].(map[string]any)["types"].([]any) {
		ran = append(ran, ty.(map[string]any)["key"].(string))
	}
	if !slices.Equal(ran, []string{"general"}) {
		t.Errorf("run again ran %v, want general alone", ran)
	}
	if code, out := rig.call("POST", "/api/reviews/"+other["run"].(map[string]any)["id"].(string)+"/rerun", rig.editor, nil); code != 400 ||
		!strings.Contains(fmt.Sprint(out["error"]), "security") {
		t.Errorf("run again of a review whose only type is off = %d %v", code, out)
	}
}

// History: newest first, a page at a time by an opaque cursor, filtered; and a run's detail with its
// findings and how each stands, what it dropped, its cost and its links.
func TestReviewAPIRunsListAndDetail(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	rig.must(200, "POST", "/api/reviews", rig.admin, map[string]any{"repo": "acme/web", "pr": 7, "types": []string{"security"}})
	page := rig.must(200, "GET", "/api/reviews?limit=1", rig.viewer, nil)
	runs := page["runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["trigger"] != "console" || page["next_cursor"] == "" {
		t.Fatalf("page 1 = %v", page)
	}
	page2 := rig.must(200, "GET", "/api/reviews?limit=1&cursor="+page["next_cursor"].(string), rig.viewer, nil)
	runs2 := page2["runs"].([]any)
	if len(runs2) != 1 || runs2[0].(map[string]any)["trigger"] != "open" {
		t.Fatalf("page 2 = %v", page2)
	}
	if empty := rig.must(200, "GET", "/api/reviews?status=failed", rig.viewer, nil)["runs"].([]any); len(empty) != 0 {
		t.Errorf("status filter = %v", empty)
	}
	id := runs2[0].(map[string]any)["id"].(string)
	d := rig.must(200, "GET", "/api/reviews/"+id, rig.viewer, nil)
	if d["run"].(map[string]any)["status"] != "posted" || d["run"].(map[string]any)["score"] != float64(3) {
		t.Errorf("run = %v", d["run"])
	}
	fs := d["findings"].([]any)
	if len(fs) != 1 {
		t.Fatalf("findings = %v", fs)
	}
	f := fs[0].(map[string]any)
	if f["status"] != "open" || f["placement"] != "inline" || f["severity"] != "P1" || !strings.Contains(f["comment_url"].(string), "#discussion_r") ||
		len(f["history"].([]any)) != 1 || !slices.Equal(reviewStrings(f["types"]), []string{"general"}) {
		t.Errorf("finding = %v", f)
	}
	links := d["links"].(map[string]any)
	if !strings.Contains(links["review"].(string), "#pullrequestreview-") || !strings.Contains(links["summary"].(string), "#issuecomment-") {
		t.Errorf("links = %v", links)
	}
	if d["usage"].(map[string]any)["tokens_in"].(float64) == 0 || d["types"].([]any)[0].(map[string]any)["key"] != "general" {
		t.Errorf("usage %v, types %v", d["usage"], d["types"])
	}
	if _, ok := d["dropped"].([]any); !ok {
		t.Errorf("dropped = %v", d["dropped"])
	}
}

// The list says, beside what each review kept, how much of it is still open: a finding fixed since
// leaves the kept count where it was and takes one off the open one, which is the detail's Open rows.
func TestReviewAPIRunsListCountsOpenFindings(t *testing.T) {
	ctx := context.Background()
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	row := func() map[string]any {
		t.Helper()
		runs := rig.must(200, "GET", "/api/reviews", rig.viewer, nil)["runs"].([]any)
		if len(runs) != 1 {
			t.Fatalf("runs = %v", runs)
		}
		return runs[0].(map[string]any)
	}
	if r := row(); r["findings"] != float64(1) || r["open"] != float64(1) {
		t.Fatalf("a review whose one finding is open lists %v kept, %v open", r["findings"], r["open"])
	}
	pr, err := rig.st.ReviewPRByNumber(ctx, orgID, "acme/web", 7)
	if err != nil || pr == nil {
		t.Fatalf("the pull request: %v %v", pr, err)
	}
	fs, err := rig.st.ReviewFindings(ctx, orgID, pr.ID)
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings: %v %v", fs, err)
	}
	if err := rig.st.SetReviewFindingStatus(ctx, orgID, fs[0].ID, review.FindingFixed, "fixed by a push", "code review"); err != nil {
		t.Fatal(err)
	}
	if r := row(); r["findings"] != float64(1) || r["open"] != float64(0) {
		t.Errorf("once its finding is fixed the review lists %v kept, %v open", r["findings"], r["open"])
	}
}

// The repository's open pull requests come from GitHub under the read-only token, each with how its
// last review went; the estimate prices a review of one from its size and the types it would run.
func TestReviewAPIOpenPullsAndEstimate(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.confirmLock()
	var listed int
	rig.fake.mux.HandleFunc("GET /repos/acme/web/pulls", func(w http.ResponseWriter, r *http.Request) {
		listed++
		if got := rig.fake.permsOf(r); got != readPerms {
			t.Errorf("open pull requests listed with a token for %q", got)
		}
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("listed %q pull requests", r.URL.Query().Get("state"))
		}
		ref := func(sha, branch string) map[string]any { return map[string]any{"sha": sha, "ref": branch} }
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 7, "title": "Make Add faster", "draft": false, "updated_at": "2026-10-01T10:00:00Z", "html_url": "https://github.com/acme/web/pull/7",
				"user": map[string]any{"login": "octocat"}, "head": ref(reviewHead, "feature"), "base": ref(reviewBase, "main")},
			{"number": 8, "title": "Docs", "draft": true, "user": map[string]any{"login": "hubot"}, "head": ref(reviewHeadC, "docs"),
				"base": ref(reviewBase, "main")}})
	})
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	out := rig.must(200, "GET", "/api/review-pulls?repo=acme/web", rig.viewer, nil)
	pulls := out["pulls"].([]any)
	if listed != 1 || len(pulls) != 2 || out["more"] != false {
		t.Fatalf("pulls = %v", out)
	}
	p7, p8 := pulls[0].(map[string]any), pulls[1].(map[string]any)
	rv, _ := p7["review"].(map[string]any)
	if p7["base"] != "main" || p7["author"] != "octocat" || rv == nil || rv["status"] != "posted" || rv["score"] != float64(3) ||
		rv["reviewed_head"] != true {
		t.Errorf("#7 = %v", p7)
	}
	if p8["review"] != nil || p8["draft"] != true {
		t.Errorf("#8 = %v", p8)
	}

	est := rig.must(200, "GET", "/api/reviews/estimate?repo=acme/web&pr=7&types=general,security", rig.viewer, nil)
	if est["priced"] != false || est["usd"] != nil || !slices.Equal(reviewStrings(est["types"]), []string{"general", "security"}) {
		t.Errorf("unpriced estimate = %v", est)
	}
	rig.b.agent.llm.pricer = func(context.Context, string) (modelPrice, bool) {
		return modelPrice{In: 1.40 / 1e6, CachedIn: 0.14 / 1e6, Out: 4.40 / 1e6}, true
	}
	est = rig.must(200, "GET", "/api/reviews/estimate?repo=acme/web&pr=7", rig.viewer, nil)
	usd, _ := est["usd"].(map[string]any)
	if est["priced"] != true || usd == nil || !(usd["low"].(float64) > 0 && usd["low"].(float64) < usd["high"].(float64)) ||
		usd["high"].(float64) > est["max_usd"].(float64) || est["rule"] != "any → any" {
		t.Errorf("priced estimate = %v", est)
	}
	// "default" is priced as the deployment's default model, which is what the engine runs for it,
	// not as a model whose id is "default".
	rig.must(200, "PUT", "/api/review-settings/"+rig.connID(), rig.admin, map[string]any{"settings": map[string]any{"mode": "live", "model": "default"}})
	est = rig.must(200, "GET", "/api/reviews/estimate?repo=acme/web&pr=7", rig.viewer, nil)
	if want := rig.b.agent.llm.Model; want == "" || est["model"] != want {
		t.Errorf("estimate on the default model names %v, want %q", est["model"], want)
	}
}

// Resume in the console is `@… resume` from the pull request: for reviews.manage only, it starts a
// paused pull request's automatic reviews again with the count back at nothing, audits it once as the
// person who pressed it, and the summary's footer stops saying paused. The open pull requests list
// and the run detail say where it stands; one not paused is answered so and left alone, and a pull
// request nobody reviewed is not there.
func TestReviewAPIResumesAPausedPullRequest(t *testing.T) {
	ctx := context.Background()
	api := newReviewAPIRig(t, `{"mode":"live","trigger":"push"}`)
	rig := api.laneRig
	rig.serveConversation()
	rig.confirmLock()
	rig.fake.mux.HandleFunc("GET /repos/acme/web/pulls", func(w http.ResponseWriter, r *http.Request) {
		ref := func(sha, branch string) map[string]any { return map[string]any{"sha": sha, "ref": branch} }
		json.NewEncoder(w).Encode([]map[string]any{{"number": 7, "title": "Make Add faster", "user": map[string]any{"login": "octocat"},
			"head": ref(reviewHead, "feature"), "base": ref(reviewBase, "main")}})
	})
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	rig.deliver("issue_comment", commentEvent(1301, "alice", "MEMBER", "@attesttag pause", true))
	rig.drain()
	if pr := rig.pr(7); !pr.Paused || pr.AutoReviews != 1 || !strings.Contains(rig.lastSummary(), "Automatic reviews paused") {
		t.Fatalf("after a member's pause: %+v\n%s", pr, rig.lastSummary())
	}
	var listed map[string]any
	for _, p := range api.must(200, "GET", "/api/review-pulls?repo=acme/web", api.viewer, nil)["pulls"].([]any) {
		if p := p.(map[string]any); p["number"] == float64(7) {
			listed, _ = p["review"].(map[string]any)
		}
	}
	if listed == nil || listed["paused"] != true || listed["paused_by"] != "member" || listed["auto_reviews"] != float64(1) {
		t.Errorf("the open pull requests list #7 as %v", listed)
	}

	body := map[string]any{"repo": "acme/web", "pr": 7}
	if code, out := api.call("POST", "/api/review-pulls/resume", api.viewer, body); code != 403 {
		t.Errorf("a viewer's resume = %d %v", code, out)
	}
	out := api.must(200, "POST", "/api/review-pulls/resume", api.editor, body)
	if p := out["pr"].(map[string]any); out["resumed"] != true || p["paused"] != false || p["auto_reviews"] != float64(0) {
		t.Errorf("resume = %v", out)
	}
	rig.drain()
	if pr := rig.pr(7); pr.Paused || pr.AutoReviews != 0 {
		t.Errorf("after resume: %+v", pr)
	}
	if summary := rig.lastSummary(); strings.Contains(summary, "paused") {
		t.Errorf("the footer still says paused after a resume from the console:\n%s", summary)
	}
	ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.resumed"})
	if len(ev) != 1 || ev[0].ActorEmail != "editor@acme.test" || ev[0].TargetID != "acme/web#7" {
		t.Errorf("resume audits = %+v", ev)
	}
	run := rig.runs(7)[len(rig.runs(7))-1]
	if p := api.must(200, "GET", "/api/reviews/"+run.PublicID, api.viewer, nil)["pr"].(map[string]any); p["paused"] != false || p["paused_by"] != "" {
		t.Errorf("the run detail after resume = %v", p)
	}

	if again := api.must(200, "POST", "/api/review-pulls/resume", api.editor, body); again["resumed"] != false {
		t.Errorf("resuming what is not paused = %v", again)
	}
	if ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.resumed"}); len(ev) != 1 {
		t.Errorf("a resume that changed nothing was audited: %d", len(ev))
	}
	api.must(404, "POST", "/api/review-pulls/resume", api.editor, map[string]any{"repo": "acme/web", "pr": 99})
	api.must(400, "POST", "/api/review-pulls/resume", api.editor, map[string]any{"repo": "acme/web"})
}

// Reviews › Types lists the five built-ins in the order they are offered, Tests and Performance among
// them, on and unedited; and a label rule is one of the branch rules a type counts as named by, since
// it is the rule that runs it.
func TestReviewAPIListsEveryBuiltinType(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	rules := []map[string]any{{"labels": []string{"perf"}, "types": []string{"performance"}}, {"types": []string{"general", "tests"}}}
	rig.must(200, "PUT", rig.repoPath(), rig.admin, map[string]any{"settings": map[string]any{"branch_rules": rules}})
	var keys []string
	used := map[string]float64{}
	for _, ty := range rig.must(200, "GET", "/api/review-types", rig.viewer, nil)["types"].([]any) {
		ty := ty.(map[string]any)
		if ty["builtin"] != true || ty["enabled"] != true || ty["edited"] != false {
			t.Errorf("built-in %v", ty)
		}
		keys = append(keys, ty["key"].(string))
		used[ty["key"].(string)] = ty["used_by"].(float64)
	}
	if want := []string{"general", "security", "tests", "performance", "release"}; !slices.Equal(keys, want) {
		t.Errorf("types listed %v, want %v", keys, want)
	}
	if used["performance"] != 1 || used["tests"] != 1 || used["security"] != 0 {
		t.Errorf("used by = %v", used)
	}
}

// The estimate's arithmetic, on the plan's typical pull request: ten files and four hundred lines.
func TestReviewEstimateTokens(t *testing.T) {
	fl, fh, vl, vh := reviewEstimateTokens(10, 300, 100, 1)
	diff := 400*12 + 10*150
	if fl.In != 4*(reviewEstimateBase+diff) || fh.In != 8*(reviewEstimateBase+2*diff) || vh.In != 10*2*12_000 || vl.In != 36_000 {
		t.Errorf("finder %v–%v, verifier %v–%v", fl, fh, vl, vh)
	}
	if two, _, _, _ := reviewEstimateTokens(10, 300, 100, 2); two.In != 2*fl.In {
		t.Error("a second type is a second finder pass")
	}
}

// Another organisation's ids answer exactly like ids that do not exist, on every route that takes
// one, and its repositories are not this organisation's to review or list.
func TestReviewAPIIsolatesOrganisations(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	connID := rig.connID()
	g := rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "group", "connection_id": connID, "name": "Web"})["node"].(map[string]any)
	saved := rig.must(200, "PUT", "/api/review-types/general", rig.admin, map[string]any{"purpose": "Ours."})["type"].(map[string]any)
	runID := rig.runs(7)[0].PublicID

	_, other := secondOrg(t, rig.st)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/review-settings/" + connID, nil},
		{"GET", "/api/review-settings/" + connID + "?repo=acme/web", nil},
		{"PUT", "/api/review-settings/" + connID, map[string]any{"settings": map[string]any{"mode": "off"}}},
		{"DELETE", "/api/review-settings/" + g["id"].(string), nil},
		{"POST", "/api/review-settings/" + connID + "/restore", nil},
		{"POST", "/api/review-settings/" + connID + "/move?repo=acme/web", map[string]any{"parent": g["id"]}},
		{"POST", "/api/review-settings", map[string]any{"kind": "group", "connection_id": connID, "name": "Mine"}},
		{"POST", "/api/review-settings", map[string]any{"kind": "connection", "installation_id": fakeInstallation}},
		{"GET", "/api/review-types/" + saved["id"].(string), nil},
		{"GET", "/api/review-types/" + saved["id"].(string) + "/versions", nil},
		{"PUT", "/api/review-types/" + saved["id"].(string), map[string]any{"purpose": "Theirs."}},
		{"GET", "/api/reviews/" + runID, nil},
		{"POST", "/api/reviews/" + runID + "/rerun", nil},
		{"POST", "/api/reviews", map[string]any{"repo": "acme/web", "pr": 7}},
		{"GET", "/api/review-pulls?repo=acme/web", nil},
		{"POST", "/api/review-pulls/resume", map[string]any{"repo": "acme/web", "pr": 7}},
		{"GET", "/api/reviews/estimate?repo=acme/web&pr=7", nil},
		{"POST", "/api/review-types/try", map[string]any{"repo": "acme/web", "pr": 7, "type": map[string]any{"key": "x-try", "name": "Try"}}},
	} {
		if code, out := rig.call(c.method, c.path, other, c.body); code != 404 {
			t.Errorf("another organisation's %s %s = %d %v", c.method, c.path, code, out)
		}
	}
	// What it does see is its own: an empty tree, the built-ins as shipped, no runs.
	if tree := rig.tree(other); len(tree["connections"].([]any)) != 0 || len(tree["available"].([]any)) != 0 {
		t.Errorf("another organisation's tree = %v", tree)
	}
	if gen := rig.must(200, "GET", "/api/review-types/general", other, nil)["type"].(map[string]any); gen["edited"] != false || gen["purpose"] == "Ours." {
		t.Errorf("another organisation's general type = %v", gen)
	}
	if runs := rig.must(200, "GET", "/api/reviews", other, nil)["runs"].([]any); len(runs) != 0 {
		t.Errorf("another organisation's runs = %v", runs)
	}
	if cur := rig.must(200, "GET", "/api/reviews?cursor="+runID, rig.admin, nil)["runs"].([]any); len(cur) != 0 {
		t.Errorf("a cursor past the only run = %v", cur)
	}
}

// What the console reads about the deployment: whether pull requests can be reviewed here at all,
// what code review is missing apart from what installing is, what each installation still has to
// accept, and — on a single-tenant deployment only — where GitHub should send deliveries.
func TestReviewAPIDeploymentFacts(t *testing.T) {
	rig := newReviewAPIRig(t, `{}`)
	ctx := context.Background()
	rig.b.ghApp = rig.b.proxy.ghApp
	if err := rig.st.SetGitHubInstallPermissions(ctx, orgID, fakeInstallation, `{"contents":"write","pull_requests":"read"}`); err != nil {
		t.Fatal(err)
	}
	me := rig.must(200, "GET", "/api/me", rig.viewer, nil)
	if me["github_review"] != true {
		t.Errorf("/api/me github_review = %v with the App and the secret", me["github_review"])
	}
	ins := rig.must(200, "GET", "/api/github/installations", rig.viewer, nil)
	if hook, _ := ins["webhook_url"].(string); len(reviewStrings(ins["review_missing"])) != 0 || !strings.HasSuffix(hook, "/github/webhook") ||
		ins["webhook_secret_set"] != true {
		t.Errorf("installations = %v", ins)
	}
	list := ins["installations"].([]any)
	if len(list) != 1 || !slices.Equal(reviewStrings(list[0].(map[string]any)["missing_permissions"]), []string{"pull_requests:write"}) ||
		list[0].(map[string]any)["installation_id"] != float64(fakeInstallation) {
		t.Errorf("installation = %v", list)
	}

	rig.b.ghHook = newGitHubWebhookFrom("", "", "")
	if me := rig.must(200, "GET", "/api/me", rig.viewer, nil); me["github_review"] != false {
		t.Errorf("/api/me github_review = %v with no webhook secret", me["github_review"])
	}
	ins = rig.must(200, "GET", "/api/github/installations", rig.viewer, nil)
	if !slices.Equal(reviewStrings(ins["review_missing"]), []string{"GITHUB_APP_WEBHOOK_SECRET"}) || ins["webhook_secret_set"] != false {
		t.Errorf("installations with no secret = %v", ins)
	}
	// A hosted deployment, open to anybody's signup, tells no tenant where its webhook is.
	rig.b.cfg.SignupMode = SignupOpen
	ins = rig.must(200, "GET", "/api/github/installations", rig.viewer, nil)
	if _, ok := ins["webhook_url"]; ok {
		t.Errorf("an open-signup deployment showed its webhook: %v", ins)
	}
	if _, ok := ins["webhook_secret_set"]; ok {
		t.Errorf("an open-signup deployment said whether its secret is set: %v", ins)
	}
	// Nor, by the list of what is missing or by /api/me, whether the secret is set at all.
	if got := reviewStrings(ins["review_missing"]); len(got) != 0 {
		t.Errorf("review_missing on a hosted deployment = %v", got)
	}
	if me := rig.must(200, "GET", "/api/me", rig.viewer, nil); me["github_review"] != true {
		t.Errorf("/api/me github_review on a hosted deployment = %v; it tells the secret's state", me["github_review"])
	}
	rig.b.cfg.SignupMode = ""
	rig.b.ghApp, rig.b.proxy.ghApp = nil, nil
	ins = rig.must(200, "GET", "/api/github/installations", rig.viewer, nil)
	if got := reviewStrings(ins["review_missing"]); !slices.Contains(got, "GITHUB_APP_ID") || !slices.Contains(got, "GITHUB_APP_WEBHOOK_SECRET") {
		t.Errorf("review_missing with no App = %v", got)
	}
}

// Code review's two budgets ride on /api/settings, validated, and only somebody who may decide what a
// review spends changes them: settings.manage alone — a custom role — is refused, and a form sending
// them back unchanged is not.
func TestReviewBudgetSettingsNeedTheConnectionsPermission(t *testing.T) {
	rig := newReviewAPIRig(t, `{}`)
	ctx := context.Background()
	if err := rig.st.UpsertConsoleRole(ctx, orgID, &ConsoleRole{Key: "settings_only", Label: "Settings only",
		Permissions: []string{PermSettingsManage}}); err != nil {
		t.Fatal(err)
	}
	settingsOnly := rig.member("settings@acme.test", "settings_only")
	rig.must(200, "PUT", "/api/settings", rig.admin, map[string]string{"review_daily_usd": "2.5", "review_monthly_budget_usd": "40"})
	if code, out := rig.call("PUT", "/api/settings", rig.admin, map[string]string{"review_daily_usd": "-1"}); code != 400 {
		t.Errorf("a negative cap = %d %v", code, out)
	}
	if code, out := rig.call("PUT", "/api/settings", settingsOnly, map[string]string{"review_daily_usd": "50"}); code != 403 {
		t.Errorf("settings.manage alone raising the review cap = %d %v", code, out)
	}
	rig.must(200, "PUT", "/api/settings", settingsOnly, map[string]string{"review_daily_usd": "2.5", "bot_name": "Reviewer"})
	if code, _ := rig.call("PUT", "/api/settings", rig.editor, map[string]string{"review_daily_usd": "3"}); code != 403 {
		t.Errorf("an editor changed a review budget")
	}
	got := rig.must(200, "GET", "/api/settings", rig.viewer, nil)
	eff := got["effective"].(map[string]any)
	if eff["ReviewDailyUSD"] != 2.5 || eff["ReviewMonthlyBudgetUSD"] != float64(40) || got["stored"].(map[string]any)["review_daily_usd"] != "2.5" {
		t.Errorf("GET /api/settings = effective %v / %v, stored %v", eff["ReviewDailyUSD"], eff["ReviewMonthlyBudgetUSD"], got["stored"])
	}
}

// The permissions themselves: a viewer reads reviews, an editor manages them, and neither holds what
// reaches further, which stays the admin's.
func TestReviewPermissionsByRole(t *testing.T) {
	for role, want := range map[string][2]bool{RoleAdmin: {true, true}, RoleEditor: {true, true}, RoleViewer: {true, false}} {
		p := permissionsForRole(role, nil)
		if p[PermReviewsView] != want[0] || p[PermReviewsManage] != want[1] {
			t.Errorf("%s: reviews.view %v, reviews.manage %v", role, p[PermReviewsView], p[PermReviewsManage])
		}
		if role != RoleAdmin && p[PermConnManage] {
			t.Errorf("%s holds connections.manage", role)
		}
	}
	for _, p := range []Permission{PermReviewsView, PermReviewsManage} {
		if denialCopy(p) == denialCopy("nothing.at.all") {
			t.Errorf("%s has no denial copy of its own", p)
		}
	}
	if fmt.Sprint(reviewMissingPermissionsOf(`{"contents":"read","pull_requests":"write"}`)) != "[]" ||
		fmt.Sprint(reviewMissingPermissionsOf(`{"pull_requests":"admin"}`)) != "[contents:read]" {
		t.Errorf("missing permissions misread")
	}
	if _, known := reviewMissingPermissions(""); known {
		t.Error("an installation with nothing recorded read as known")
	}
}

func reviewMissingPermissionsOf(s string) []string { m, _ := reviewMissingPermissions(s); return m }

func reviewJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// On a repository in shadow, where a review's findings count as said once it is recorded, a try's
// are still nobody's: the review after it raises the same problem rather than dropping it as one
// already open, and scores it.
func TestReviewAPITryDoesNotSilenceAShadowReview(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	rig.confirmLock()
	rig.must(200, "POST", "/api/review-types/try", rig.editor, map[string]any{"repo": "acme/web", "pr": 7,
		"type": map[string]any{"key": "general", "purpose": "As edited."}})
	rig.drain()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	runs := rig.runs(7)
	if len(runs) != 2 || runs[0].Kind != "review" || runs[0].Status != "shadow" || runs[0].Kept != 1 || runs[0].Score != 3 {
		t.Fatalf("the review after a try: %+v", runs[0])
	}
	if pr := rig.pr(7); pr.Score != 3 || pr.LastReviewedSHA != reviewHead || pr.ReviewsCount != 1 {
		t.Errorf("pull request = %+v", pr)
	}
}

// A rule learned from a reply waits as proposed: a save of its type that says nothing about it keeps
// it waiting, Approve is saving it active and Reject saving it rejected — and only an active one
// reaches a prompt.
func TestReviewAPIApprovesAndRejectsProposedRules(t *testing.T) {
	rig := newReviewAPIRig(t, `{}`)
	ctx := context.Background()
	for _, text := range []string{"Prefer errors.Join for several errors.", "Never log a request body."} {
		if _, _, err := rig.st.proposeLearnedReviewRule(ctx, orgID, "general", text, "", "github:octocat"); err != nil {
			t.Fatal(err)
		}
	}
	gen := rig.must(200, "GET", "/api/review-types/general", rig.viewer, nil)["type"].(map[string]any)
	rules := gen["rules"].([]any)
	n := len(rules)
	if rules[n-1].(map[string]any)["status"] != "proposed" || rules[n-1].(map[string]any)["source"] != "learned" {
		t.Fatalf("the learned rules = %v", rules[n-2:])
	}
	edit := func(statuses map[int]string) []map[string]any {
		var out []map[string]any
		for i, r := range rules {
			r := r.(map[string]any)
			e := map[string]any{"id": r["id"], "text": r["text"], "severity_cap": r["severity_cap"]}
			if s, ok := statuses[i]; ok {
				e["status"] = s
			}
			out = append(out, e)
		}
		return out
	}
	version := gen["version"]
	saved := rig.must(200, "PUT", "/api/review-types/general", rig.editor, map[string]any{"version": version, "purpose": "Ours.",
		"rules": edit(nil)})["type"].(map[string]any)
	if got := saved["rules"].([]any)[n-1].(map[string]any)["status"]; got != "proposed" {
		t.Errorf("a save that said nothing about the proposed rule made it %v", got)
	}
	saved = rig.must(200, "PUT", "/api/review-types/general", rig.editor, map[string]any{"version": saved["version"],
		"rules": edit(map[int]string{n - 2: "active", n - 1: "rejected"})})["type"].(map[string]any)
	got := saved["rules"].([]any)
	if got[n-2].(map[string]any)["status"] != "active" || got[n-1].(map[string]any)["status"] != "rejected" ||
		got[n-2].(map[string]any)["source"] != "learned" {
		t.Errorf("after approve and reject: %v", got[n-2:])
	}
	row, _ := rig.st.ReviewTypeByKey(ctx, orgID, "general")
	spec := reviewTypeOfRow(row)
	prompt := spec.Type.Rules
	if prompt[n-2].Off || !prompt[n-1].Off {
		t.Errorf("the approved rule is off or the rejected one on: %+v", prompt[n-2:])
	}
}

// Starting a review by hand bypasses the filters, not the tree: a repository whose installation
// nobody added to code review is not reviewed, and the console is told why.
func TestReviewAPIManualStartNeedsTheConnectionAdded(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	rig.must(200, "DELETE", "/api/review-settings/"+rig.connID(), rig.admin, nil)
	code, out := rig.call("POST", "/api/reviews", rig.admin, map[string]any{"repo": "acme/web", "pr": 7})
	if code != 409 || out["reason"] != "not_reviewed" {
		t.Errorf("a start on a stopped connection = %d %v", code, out)
	}
	if runs := rig.runs(7); len(runs) != 0 {
		t.Errorf("runs = %+v", runs)
	}
}

// Review settings can name the deployment's default model: "default", since an empty model is
// what leaving the key out means. Before, a review could run on Advanced or a channel model but
// never on the default.
func TestReviewSettingsAcceptTheDefaultModel(t *testing.T) {
	if !(reviewDefaultModel == "default") {
		t.Fatal("the default model's name changed")
	}
	r := &reviewRun{st: Settings{}}
	r.ep = &LLM{}
	if _, err := r.resolveModel(context.Background(), reviewDefaultModel); err != nil {
		t.Errorf("resolveModel(%q): %v", reviewDefaultModel, err)
	}
	if _, err := r.resolveModel(context.Background(), "not-offered/model"); err == nil {
		t.Error("a model nobody offered was accepted")
	}
}

// A save of the branch rules says what it read them as (expect, the digest every detail carries), and
// is refused — 409, nothing written — where they read otherwise now: changed at the level itself, or
// above it where the level inherits its list. A change above a level with a list of its own is not
// the level's, and does not refuse it. An expect naming a field the write does not change is a
// mistake in the request, not a stale read.
func TestReviewSettingsExpect(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow","branch_rules":[{"base":"main","types":["security"]},{}]}`)
	conn := "/api/review-settings/" + rig.connID()
	digest := func(out map[string]any) string {
		t.Helper()
		d, _ := out["digests"].(map[string]any)["branch_rules"].(string)
		if !strings.HasPrefix(d, "sha256:") {
			t.Fatalf("digests = %v", out["digests"])
		}
		return d
	}
	read := func(path string) string { t.Helper(); return digest(rig.must(200, "GET", path, rig.viewer, nil)) }
	rules := func(base string) []map[string]any {
		return []map[string]any{{"base": base, "types": []string{"general"}}, {}}
	}
	save := func(expect string, rules any) map[string]any {
		return map[string]any{"fields": []string{"branch_rules"}, "settings": map[string]any{"branch_rules": rules},
			"expect": map[string]any{"branch_rules": expect}}
	}
	saves := func() int { t.Helper(); return len(rig.audited("review.settings_updated")) }

	// The repository has no row and inherits the connection's list: it reads as the connection does.
	repoRead := read(rig.repoPath())
	if connRead := read(conn); repoRead != connRead {
		t.Errorf("an inheriting repository reads %s, its connection %s", repoRead, connRead)
	}
	for what, body := range map[string]map[string]any{
		"a field the write leaves alone": {"fields": []string{"strictness"}, "settings": map[string]any{"strictness": "high"},
			"expect": map[string]any{"branch_rules": repoRead}},
		"a field that keeps no digest": {"fields": []string{"strictness"}, "settings": map[string]any{"strictness": "high"},
			"expect": map[string]any{"strictness": "sha256:00"}},
		"a whole-object write": {"settings": map[string]any{"branch_rules": rules("main")},
			"expect": map[string]any{"branch_rules": repoRead}},
	} {
		if code, out := rig.call("PUT", rig.repoPath(), rig.editor, body); code != 400 || !strings.Contains(out["error"].(string), "expect") {
			t.Errorf("expect naming %s = %d %v", what, code, out)
		}
	}

	// Somebody changes the list above the repository; the page that read it before saves.
	connOut := rig.must(200, "PUT", conn, rig.admin, save(read(conn), rules("release/*")))
	if digest(connOut) == repoRead {
		t.Error("a write answered with the digest of the list it replaced")
	}
	before := saves()
	if code, out := rig.call("PUT", rig.repoPath(), rig.editor, save(repoRead, rules("hotfix/*"))); code != 409 ||
		!strings.Contains(out["error"].(string), "changed since") {
		t.Errorf("a save over a list changed above it = %d %v", code, out)
	}
	if d := rig.must(200, "GET", rig.repoPath(), rig.viewer, nil); d["node"].(map[string]any)["id"] != "" ||
		!strings.Contains(string(reviewJSON(d["effective"])), `"base":"release/*"`) || saves() != before {
		t.Errorf("a refused save wrote something: node %v, effective %s, %d audited", d["node"], reviewJSON(d["effective"]), saves()-before)
	}

	// Read again, it saves — and the answer, the level's detail, carries the digest of what it saved.
	out := rig.must(200, "PUT", rig.repoPath(), rig.editor, save(read(rig.repoPath()), rules("hotfix/*")))
	repo := "/api/review-settings/" + out["node"].(map[string]any)["id"].(string)
	mine := digest(out)
	if mine != read(repo) {
		t.Errorf("the write answered %s, a read says %s", mine, read(repo))
	}

	// Somebody saves the repository's own list; the page that read it before saves.
	rig.must(200, "PUT", repo, rig.admin, save(mine, rules("feature/*")))
	before = saves()
	if code, out := rig.call("PUT", repo, rig.editor, save(mine, rules("hotfix/*"))); code != 409 {
		t.Errorf("a save over a list changed at the level = %d %v", code, out)
	}
	// Resetting it to inherit is a change of the list as much as a new one is.
	if code, out := rig.call("PUT", repo, rig.editor, map[string]any{"fields": []string{"branch_rules"}, "settings": map[string]any{},
		"expect": map[string]any{"branch_rules": mine}}); code != 409 {
		t.Errorf("a reset over a list changed at the level = %d %v", code, out)
	}
	own := rig.must(200, "GET", repo, rig.viewer, nil)["own"]
	if !strings.Contains(string(reviewJSON(own)), `"base":"feature/*"`) || saves() != before {
		t.Errorf("a refused save wrote something: own %s, %d audited", reviewJSON(own), saves()-before)
	}

	// The connection's list changing is nothing to a repository with a list of its own.
	current := read(repo)
	rig.must(200, "PUT", conn, rig.admin, save(read(conn), rules("main")))
	if read(repo) != current {
		t.Error("a repository's digest followed a list it does not use")
	}
	rig.must(200, "PUT", repo, rig.editor, save(current, rules("hotfix/*")))

	// Without expect a write is what it always was.
	rig.must(200, "PUT", repo, rig.editor, map[string]any{"fields": []string{"branch_rules"}, "settings": map[string]any{}})
	if d := rig.must(200, "GET", repo, rig.viewer, nil); d["effective"].(map[string]any)["source"].(map[string]any)["branch_rules"] != "connection" {
		t.Errorf("reset without expect: %v", d["effective"])
	}
}

// conversationHookChat is a chat platform whose channel lookup lets something else happen first:
// another save landing while a write waits on Slack.
type conversationHookChat struct {
	notifyChat
	during func()
}

func (c *conversationHookChat) conversation(ctx context.Context, id string) (string, convInfo, error) {
	if f := c.during; f != nil {
		c.during = nil
		f()
	}
	return c.notifyChat.conversation(ctx, id)
}

// The digest is checked again just before the write, not only where the request starts: a rule that
// names a channel is checked against Slack in between, which takes long enough for somebody else's
// save to land. One that does is not overwritten.
func TestReviewSettingsExpectIsCheckedAgainBeforeTheWrite(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	ctx := context.Background()
	withChannel(t, rig.st, orgID, "T1", "C1")
	chat := &conversationHookChat{}
	rig.b.slacks = testRegistry(&Chat{t: chat, Platform: platformSlack, TeamID: "T1", OrgID: orgID})
	conn := "/api/review-settings/" + rig.connID()
	idx, err := rig.b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	row := idx.byPub[rig.connID()]
	theirs := `{"mode":"shadow","branch_rules":[{"base":"release/*","types":["security"]},{}]}`
	chat.during = func() {
		if err := rig.st.UpdateReviewSettings(ctx, orgID, row.ID, json.RawMessage(theirs), "someone@acme.test"); err != nil {
			t.Error(err)
		}
	}
	announced := []map[string]any{{"base": "main", "notify": map[string]any{"team": "T1", "channel": "C1"}}, {}}
	read := rig.must(200, "GET", conn, rig.viewer, nil)["digests"].(map[string]any)["branch_rules"]
	body := map[string]any{"fields": []string{"branch_rules"}, "settings": map[string]any{"branch_rules": announced},
		"expect": map[string]any{"branch_rules": read}}
	if code, out := rig.call("PUT", conn, rig.admin, body); code != 409 {
		t.Fatalf("a save another landed under while Slack was asked = %d %v", code, out)
	}
	if chat.during != nil {
		t.Fatal("the channel was never looked up, so nothing landed in between")
	}
	if own := rig.must(200, "GET", conn, rig.viewer, nil)["own"]; !strings.Contains(string(reviewJSON(own)), `"base":"release/*"`) {
		t.Errorf("the save that landed in between was overwritten: %s", reviewJSON(own))
	}
	// Read again, the same save goes through.
	body["expect"] = map[string]any{"branch_rules": rig.must(200, "GET", conn, rig.viewer, nil)["digests"].(map[string]any)["branch_rules"]}
	rig.must(200, "PUT", conn, rig.admin, body)
}

// Nor is a save of another field: the request's settings were made from the level as it read before
// that save landed, so writing them would put that save back — a mode somebody moved to shadow back
// to live, by a request that never named mode and was never judged for it. With expect or without,
// that is refused and the save in between kept. A repository given a row of its own in that time is
// refused the same way, rather than having the row the other save made overwritten.
func TestReviewSettingsSaveNeverPutsBackAnotherField(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	ctx := context.Background()
	withChannel(t, rig.st, orgID, "T1", "C1")
	chat := &conversationHookChat{}
	rig.b.slacks = testRegistry(&Chat{t: chat, Platform: platformSlack, TeamID: "T1", OrgID: orgID})
	conn := "/api/review-settings/" + rig.connID()
	idx, err := rig.b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	row := idx.byPub[rig.connID()]
	chat.during = func() {
		if err := rig.st.UpdateReviewSettings(ctx, orgID, row.ID, json.RawMessage(`{"mode":"shadow"}`), "someone@acme.test"); err != nil {
			t.Error(err)
		}
	}
	announced := []map[string]any{{"base": "main", "notify": map[string]any{"team": "T1", "channel": "C1"}}, {}}
	body := map[string]any{"fields": []string{"branch_rules"}, "settings": map[string]any{"branch_rules": announced}}
	if code, out := rig.call("PUT", conn, rig.admin, body); code != 409 {
		t.Fatalf("a save of the mode landed under a save of the branch rules = %d %v", code, out)
	}
	if chat.during != nil {
		t.Fatal("the channel was never looked up, so nothing landed in between")
	}
	if own := string(reviewJSON(rig.must(200, "GET", conn, rig.viewer, nil)["own"])); own != `{"mode":"shadow"}` {
		t.Errorf("the mode saved in between was put back: own settings now %s", own)
	}

	chat.during = func() {
		web, err := rig.st.EnsureReviewRepo(ctx, orgID, row.ID, "acme/web", "someone@acme.test")
		if err == nil {
			err = rig.st.UpdateReviewSettings(ctx, orgID, web.ID, json.RawMessage(`{"mode":"off"}`), "someone@acme.test")
		}
		if err != nil {
			t.Error(err)
		}
	}
	// Another channel, since the one above is now known and would not be asked about again.
	withChannel(t, rig.st, orgID, "T1", "C2")
	body["settings"] = map[string]any{"branch_rules": []map[string]any{
		{"base": "main", "notify": map[string]any{"team": "T1", "channel": "C2"}}, {}}}
	if code, out := rig.call("PUT", rig.repoPath(), rig.admin, body); code != 409 {
		t.Fatalf("a save at a repository that got a row of its own meanwhile = %d %v", code, out)
	}
	if chat.during != nil {
		t.Fatal("the second channel was never looked up, so nothing landed in between")
	}
	if own := string(reviewJSON(rig.must(200, "GET", rig.repoPath(), rig.viewer, nil)["own"])); own != `{"mode":"off"}` {
		t.Errorf("the repository's row made in between was overwritten: own settings now %s", own)
	}
	// Read again, the same save goes through.
	rig.must(200, "PUT", rig.repoPath(), rig.admin, body)
}

// A create that says which version of the type it copies is refused once that type has been saved
// again, and makes nothing: its body holds what it read of the copy. One that names no version — the
// Types tab's own dialog — copies the type as it reads when it lands.
func TestReviewTypeCreateIsRefusedOverACopySavedSince(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	rig.must(200, "POST", "/api/review-types", rig.editor, map[string]any{"key": "sec-a", "copy_from": "security", "copy_from_version": 0})
	rig.must(200, "PUT", "/api/review-types/security", rig.editor, map[string]any{"version": 0, "strictness": "high"})
	if code, out := rig.call("POST", "/api/review-types", rig.editor, map[string]any{"key": "sec-b", "copy_from": "security",
		"copy_from_version": 0}); code != 409 {
		t.Fatalf("a create over a copy saved since = %d %v, want 409", code, out)
	}
	if code, _ := rig.call("GET", "/api/review-types/sec-b", rig.viewer, nil); code != 404 {
		t.Errorf("the refused create made sec-b anyway (%d)", code)
	}
	rig.must(200, "POST", "/api/review-types", rig.editor, map[string]any{"key": "sec-b", "copy_from": "security", "copy_from_version": 2})
	rig.must(200, "POST", "/api/review-types", rig.editor, map[string]any{"key": "sec-c", "copy_from": "security"})
}

// A write the console assistant's card confirms says so on its audit row — the settings, a type's
// save and a type's create — so it pairs with the assistant.proposed row the card left. A write
// nobody proposed carries no proposal id at all.
func TestReviewAuditsCarryTheProposalID(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	conn := "/api/review-settings/" + rig.connID()
	proposalOf := func(action string) []any {
		t.Helper()
		var out []any
		for _, ev := range rig.audited(action) {
			var d map[string]any
			json.Unmarshal(ev.Details, &d)
			out = append(out, d["proposal_id"])
		}
		return out
	}

	rig.must(200, "PUT", conn, rig.editor, map[string]any{"fields": []string{"strictness"}, "settings": map[string]any{"strictness": "high"},
		"proposal_id": "prop_settings"})
	rig.must(200, "PUT", conn, rig.editor, map[string]any{"fields": []string{"strictness"}, "settings": map[string]any{"strictness": "low"}})
	rig.must(200, "PUT", "/api/review-types/general", rig.editor, map[string]any{"version": 0, "purpose": "Anything.",
		"proposal_id": "prop_type"})
	rig.must(200, "PUT", "/api/review-types/general", rig.editor, map[string]any{"version": 2, "purpose": "Anything else."})
	rig.must(200, "POST", "/api/review-types", rig.editor, map[string]any{"key": "api-contract", "name": "API contract",
		"copy_from": "security", "proposal_id": "prop_create"})
	rig.must(200, "POST", "/api/review-types", rig.editor, map[string]any{"key": "migrations", "name": "Migrations",
		"purpose": "Schema changes that lock or lose data."})
	// A body is the browser's: an id longer than any the assistant makes is cut, not stored whole.
	rig.must(200, "PUT", conn, rig.editor, map[string]any{"fields": []string{"strictness"}, "settings": map[string]any{"strictness": "high"},
		"proposal_id": strings.Repeat("p", 500)})

	// Newest first.
	for action, want := range map[string][]any{
		"review.settings_updated": {strings.Repeat("p", 64), nil, "prop_settings"},
		"review.type_saved":       {nil, "prop_type"},
		"review.type_created":     {nil, "prop_create"},
	} {
		if got := proposalOf(action); !slices.Equal(got, want) {
			t.Errorf("%s audited proposal ids %v, want %v", action, got, want)
		}
	}
}

// A repository is found by name as Reviews › Settings finds one from a link that knows only the name
// (resolveSelection, over this tree): through the first connection in the tree that lists it, unless
// conn names one — two installations can reach the same repository — and, once it has a row, through
// the connection that row is under and no other. What it finds is the node the settings route writes.
func TestReviewNodeByNameMatchesTheConsole(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"shadow"}`)
	ctx := context.Background()
	seedInstall(t, rig.st, orgID, 5151, "octo-org")
	rig.appRepo("acme/web", 5151)
	rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "connection", "installation_id": 5151})
	listing := func() []string {
		t.Helper()
		var out []string
		for _, c := range rig.tree(rig.viewer)["connections"].([]any) {
			c := c.(map[string]any)
			for _, r := range c["repos"].([]any) {
				if r.(map[string]any)["repo"] == "acme/web" {
					out = append(out, c["id"].(string))
				}
			}
		}
		return out
	}
	index := func() *reviewTreeIndex {
		t.Helper()
		idx, err := rig.b.reviewTreeIndex(ctx, orgID)
		if err != nil {
			t.Fatal(err)
		}
		return idx
	}

	both := listing()
	if len(both) != 2 {
		t.Fatalf("the console lists acme/web under %v, want both connections", both)
	}
	idx := index()
	n, c, err := reviewNodeByName(idx, "Acme/Web", "")
	if err != nil || c.PublicID != both[0] || n.PublicID != "" || n.Repo != "acme/web" || n.ParentID != c.ID {
		t.Fatalf("by name alone = %+v under %+v, %v; want acme/web, inheriting, under %s", n, c, err, both[0])
	}
	if _, c, err := reviewNodeByName(idx, "acme/web", both[1]); err != nil || c.PublicID != both[1] {
		t.Errorf("by name under the second connection = %+v, %v", c, err)
	}

	// Saved through the second connection, it is that connection's repository, and only its.
	out := rig.must(200, "PUT", "/api/review-settings/"+both[1]+"?repo=acme/web", rig.admin,
		map[string]any{"fields": []string{"strictness"}, "settings": map[string]any{"strictness": "high"}})
	rowID := out["node"].(map[string]any)["id"].(string)
	if now := listing(); !slices.Equal(now, both[1:]) {
		t.Fatalf("after its save the console lists acme/web under %v, want %v", now, both[1:])
	}
	idx = index()
	if n, c, err := reviewNodeByName(idx, "acme/web", ""); err != nil || n.PublicID != rowID || c.PublicID != both[1] {
		t.Errorf("by name once it has a row = %+v under %+v, %v; want %s", n, c, err, rowID)
	}
	if _, _, err := reviewNodeByName(idx, "acme/web", both[0]); err != ErrReviewSettingNotFound {
		t.Errorf("by name under the connection it is not under: %v", err)
	}
	if _, _, err := reviewNodeByName(idx, "acme/nope", ""); err != ErrReviewSettingNotFound {
		t.Errorf("a repository nothing reaches: %v", err)
	}
	if _, _, err := reviewNodeByName(idx, "web", ""); !errors.Is(err, ErrReviewName) {
		t.Errorf("a name that is not owner/name: %v", err)
	}
	// By id, as the route addresses it.
	if n, err := reviewTargetIn(idx, rowID, ""); err != nil || n.PublicID != rowID {
		t.Errorf("by its id = %+v, %v", n, err)
	}
	if _, err := reviewTargetIn(idx, rowID, "acme/web"); err != ErrReviewSettingNotFound {
		t.Errorf("a repository under a repository's id: %v", err)
	}
}

// A create is built by the two functions the console assistant builds its card with, so what they
// make is exactly what the create saves: the same name, purpose and rules, from a copy or from
// nothing.
func TestReviewTypeFromCreateIsWhatTheCreateSaves(t *testing.T) {
	rig := newReviewAPIRig(t, `{}`)
	ctx := context.Background()
	for _, body := range []map[string]any{
		{"key": "api-contract", "name": "API contract", "copy_from": "security"},
		{"key": "migrations", "name": "Migrations", "purpose": "Schema changes that lock or lose data.", "strictness": "high",
			"rules": []map[string]any{{"text": "A migration that rewrites a large table runs in batches.", "severity_cap": "P1"}}},
	} {
		var in reviewTypeInput
		if err := json.Unmarshal(reviewJSON(body), &in); err != nil {
			t.Fatal(err)
		}
		base, err := rig.b.reviewTypeCreateBase(ctx, orgID, in.CopyFrom)
		if err != nil {
			t.Fatal(err)
		}
		built := reviewTypeFromCreate(in, base, in.Key)
		if err := checkReviewType(built, in.Key); err != nil {
			t.Errorf("%s: built a type the create would refuse: %v", in.Key, err)
		}
		rig.must(200, "POST", "/api/review-types", rig.editor, body)
		saved, err := rig.st.ReviewTypeByKey(ctx, orgID, in.Key)
		if err != nil || saved == nil {
			t.Fatalf("%s was not saved: %v", in.Key, err)
		}
		if got, want := reviewTypeContent(built), reviewTypeContent(saved); got != want {
			t.Errorf("%s: built\n%s\nsaved\n%s", in.Key, got, want)
		}
	}
	if _, err := rig.b.reviewTypeCreateBase(ctx, orgID, "nope"); err != ErrReviewTypeNotFound {
		t.Errorf("a copy of a type that does not exist: %v", err)
	}
}

// A level's settings refused for what they say read as that — a branch rule naming a type that does
// not exist, a field that is none, a key review does not know — and not after the sentence for a body
// that is no JSON object at all, which only such a body is answered with. Each is still
// ErrReviewSettingsInvalid, and a 400.
func TestReviewSettingsRefusalsReadOnTheirOwn(t *testing.T) {
	rig := newReviewAPIRig(t, `{}`)
	conn := "/api/review-settings/" + rig.connID()
	const object = "review settings must be a JSON object"
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"a branch rule naming a type that does not exist", map[string]any{"fields": []string{"branch_rules"},
			"settings": map[string]any{"branch_rules": []map[string]any{{"base": "main", "types": []string{"nope"}}, {}}}},
			`branch_rules: rule 1 names review type "nope", which does not exist here`},
		{"a field that is none", map[string]any{"fields": []string{"strictnes"}, "settings": map[string]any{}},
			`fields: "strictnes" is not a setting`},
		{"a key review does not know", map[string]any{"settings": map[string]any{"strictnes": "high"}},
			`settings: unknown field "strictnes"`},
		{"a body that is no object", map[string]any{"settings": []int{1}}, object},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := rig.call("PUT", conn, rig.admin, tc.body)
			msg, _ := out["error"].(string)
			if code != 400 || msg != tc.want {
				t.Errorf("= %d %q, want 400 %q", code, msg, tc.want)
			}
		})
	}
	_, unknown := decodeReviewSettings(json.RawMessage(`{"strictnes":"high"}`))
	_, cut := decodeReviewSettings(json.RawMessage(`{"mode":`))
	_, field := mergeReviewFields(review.Settings{}, review.Settings{}, []string{"nope"})
	for _, err := range []error{unknown, cut, field, invalidReviewSettings(errors.Join(errors.New("one"), errors.New("two")))} {
		if !errors.Is(err, ErrReviewSettingsInvalid) {
			t.Errorf("%v is not ErrReviewSettingsInvalid", err)
		}
	}
	if cut == nil || !strings.HasPrefix(cut.Error(), object) {
		t.Errorf("a body that is not JSON at all: %v", cut)
	}
}

// A type of the organisation's own can be deleted once no branch rule names it; a built-in cannot.
// The deleted type is gone from every list and lookup, but its history stays and its key stays
// taken, so the runs that name {key, version} still mean what they ran.
func TestReviewAPIDeletesACustomType(t *testing.T) {
	ctx := context.Background()
	rig := newReviewAPIRig(t, `{}`)
	created := rig.must(200, "POST", "/api/review-types", rig.editor, map[string]any{"key": "api-contract", "name": "API contract",
		"rules": []map[string]any{{"text": "Every handler checks the organisation."}}})["type"].(map[string]any)
	id := created["id"].(string)

	if code, _ := rig.call("DELETE", "/api/review-types/general?version=1", rig.editor, nil); code != 400 {
		t.Errorf("deleting a built-in = %d", code)
	}
	if code, _ := rig.call("DELETE", "/api/review-types/api-contract?version=1", rig.viewer, nil); code != 403 {
		t.Errorf("a viewer's delete = %d", code)
	}
	if code, _ := rig.call("DELETE", "/api/review-types/api-contract", rig.editor, nil); code != 400 {
		t.Errorf("a delete without the version = %d", code)
	}
	rig.must(200, "PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{
		"branch_rules": []map[string]any{{"types": []string{"api-contract"}}}}})
	if code, out := rig.call("DELETE", "/api/review-types/api-contract?version=1", rig.editor, nil); code != 409 ||
		!strings.Contains(fmt.Sprint(out["error"]), "1 branch rule names api-contract") {
		t.Errorf("deleting a type a branch rule names = %d %v", code, out)
	}
	rig.must(200, "PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{"branch_rules": []map[string]any{}}})
	rig.must(200, "POST", "/api/review-types/api-contract/disable", rig.editor, nil) // version 2
	if code, _ := rig.call("DELETE", "/api/review-types/api-contract?version=1", rig.editor, nil); code != 409 {
		t.Errorf("a delete against a version saved over = %d", code)
	}
	rig.must(200, "DELETE", "/api/review-types/"+id+"?version=2", rig.editor, nil)

	for _, v := range rig.must(200, "GET", "/api/review-types", rig.viewer, nil)["types"].([]any) {
		if v.(map[string]any)["key"] == "api-contract" {
			t.Error("a deleted type is still listed")
		}
	}
	if code, _ := rig.call("GET", "/api/review-types/api-contract", rig.viewer, nil); code != 404 {
		t.Errorf("reading a deleted type = %d", code)
	}
	if code, _ := rig.call("PUT", "/api/review-types/"+id, rig.editor, map[string]any{"version": 2, "purpose": "Back."}); code != 404 {
		t.Errorf("saving a deleted type = %d", code)
	}
	if code, _ := rig.call("DELETE", "/api/review-types/"+id+"?version=2", rig.editor, nil); code != 404 {
		t.Errorf("deleting it twice = %d", code)
	}
	if code, out := rig.call("POST", "/api/review-types", rig.editor, map[string]any{"key": "api-contract", "name": "Again"}); code != 409 ||
		!strings.Contains(fmt.Sprint(out["error"]), "deleted type") {
		t.Errorf("a new type under a deleted type's key = %d %v", code, out)
	}
	if code, _ := rig.call("PUT", rig.repoPath(), rig.editor, map[string]any{"settings": map[string]any{
		"branch_rules": []map[string]any{{"types": []string{"api-contract"}}}}}); code != 400 {
		t.Errorf("a branch rule naming a deleted type = %d", code)
	}
	if run, skipped, err := resolveReviewTypes(ctx, rig.st, orgID, []string{"api-contract"}); err != nil || len(run) != 0 ||
		len(skipped) != 1 || skipped[0].Skipped != "no such review type" {
		t.Errorf("resolving a deleted type = %v %v %v", run, skipped, err)
	}
	var versions int
	if err := rig.st.db.QueryRowContext(ctx, `select count(*) from review_type_versions v join review_types t on t.id=v.type_id
		where t.org_id=? and t.public_id=?`, orgID, id).Scan(&versions); err != nil || versions != 2 {
		t.Errorf("a deleted type's history = %d versions, %v", versions, err)
	}
}
