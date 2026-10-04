package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"attesttag/internal/review"
)

// Resolving a finding's thread on GitHub once the finding is closed — fixed by a push, or withdrawn
// on a reply — through GitHub's GraphQL API, faked here. What these pin: the thread is found from the
// one stored or by listing the pull request's threads page by page, resolved with the post token
// after the answer, audited, and never unresolved; a refusal for want of a permission costs the run
// nothing, is remembered against the installation for the console, and is forgotten once a thread
// resolves; and the client sends exactly the two documents, about this pull request, and nothing
// else. They run on both dialects.

// fakeThreads is GitHub's GraphQL for review threads: the pull request's threads are its inline
// comments that reply to nothing, after `before` threads people started, which a review's comments
// come after.
type fakeThreads struct {
	page      int  // threads to a page; 0 is GitHub's hundred
	before    int  // threads before the review's own
	refuse    bool // answer resolving FORBIDDEN, as GitHub answers a token without the permission
	resolved  map[string]bool
	pages     int      // pages of threads listed
	cursors   []string // the after of each page asked for
	mutations []string // threads asked to be resolved, refused ones included
}

func (rig *laneRig) serveGraphQL() *fakeThreads {
	g, f, t := rig.gh, rig.fake, rig.t
	ft := &fakeThreads{resolved: map[string]bool{}}
	answer := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	f.mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		perms := f.permsOf(r)
		g.mu.Lock()
		defer g.mu.Unlock()
		switch in.Query {
		case reviewThreadsQuery:
			if perms != readPerms {
				t.Errorf("the threads were listed with a token for %q, want %q", perms, readPerms)
			}
			if in.Variables["owner"] != "acme" || in.Variables["name"] != "web" || in.Variables["number"] != float64(7) {
				t.Errorf("the threads of another pull request were asked for: %v", in.Variables)
			}
			type node struct {
				ID       string `json:"id"`
				Resolved bool   `json:"isResolved"`
				Root     int64
			}
			var all []node
			for i := range ft.before {
				all = append(all, node{ID: fmt.Sprintf("PRRT_human%d", i), Root: int64(9000 + i)})
			}
			for _, c := range g.reviewComments {
				if c.InReplyToID == 0 {
					id := fmt.Sprintf("PRRT_kw%d", c.ID)
					all = append(all, node{ID: id, Resolved: ft.resolved[id], Root: c.ID})
				}
			}
			start := 0
			after, _ := in.Variables["after"].(string)
			if after != "" {
				start, _ = strconv.Atoi(strings.TrimPrefix(after, "Y3Vyc29y"))
			}
			ft.pages++
			ft.cursors = append(ft.cursors, after)
			size := ft.page
			if size == 0 {
				size = 100
			}
			end := min(start+size, len(all))
			nodes := []map[string]any{}
			for _, n := range all[start:end] {
				nodes = append(nodes, map[string]any{"id": n.ID, "isResolved": n.Resolved,
					"comments": map[string]any{"nodes": []map[string]any{{"fullDatabaseId": strconv.FormatInt(n.Root, 10)}}}})
			}
			answer(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
				"reviewThreads": map[string]any{"nodes": nodes,
					"pageInfo": map[string]any{"hasNextPage": end < len(all), "endCursor": fmt.Sprintf("Y3Vyc29y%d", end)}}}}}})
		case reviewResolveThreadMutation:
			if perms != postPerms {
				t.Errorf("a thread was resolved with a token for %q, want %q", perms, postPerms)
			}
			id, _ := in.Variables["threadId"].(string)
			ft.mutations = append(ft.mutations, id)
			if ft.refuse {
				answer(w, map[string]any{"data": map[string]any{"resolveReviewThread": nil}, "errors": []map[string]any{{
					"type": "FORBIDDEN", "path": []string{"resolveReviewThread"}, "message": "Resource not accessible by integration"}}})
				return
			}
			ft.resolved[id] = true
			answer(w, map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{
				"thread": map[string]any{"id": id, "isResolved": true}}}})
		default:
			t.Errorf("a GraphQL document code review does not send:\n%s", in.Query)
			w.WriteHeader(400)
		}
	})
	return ft
}

func (ft *fakeThreads) threadOf(f *ReviewFinding) string {
	return fmt.Sprintf("PRRT_kw%d", f.GitHubCommentID)
}

// The push fixes the query: its thread gets "Fixed in", then is resolved — found by listing the pull
// request's threads, past two pages of other people's, and stored on the finding — and audited. The
// findings still open keep their threads as they are.
func TestReviewThreadResolvedWhenAPushFixesIt(t *testing.T) {
	ctx := context.Background()
	rig, _ := reviewedThenFixed(t, `{"mode":"live"}`, nil)
	ft := rig.serveGraphQL()
	ft.page, ft.before = 1, 2

	rig.deliver("issue_comment", commentEvent(1301, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	fs := rig.resolveFindings()
	sql, scan := fs[sqlTitle], fs[scanTitle]
	if sql.Status != review.FindingFixed || len(rig.botAnswers(sql)) != 1 {
		t.Fatalf("the fixed query: %s, answers %q", sql.Status, rig.botAnswers(sql))
	}
	if len(ft.mutations) != 1 || ft.mutations[0] != ft.threadOf(sql) || !ft.resolved[ft.threadOf(sql)] {
		t.Fatalf("threads resolved = %v; want the fixed query's alone", ft.mutations)
	}
	if ft.pages < 3 || ft.cursors[0] != "" || ft.cursors[1] == "" {
		t.Errorf("the threads were listed in %d pages with cursors %q; the query's is past two of other people's", ft.pages, ft.cursors)
	}
	if sql.ThreadNodeID != ft.threadOf(sql) || ft.resolved[ft.threadOf(scan)] {
		t.Errorf("thread stored %q; the open finding's resolved %v", sql.ThreadNodeID, ft.resolved[ft.threadOf(scan)])
	}
	events, err := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.thread_resolved"})
	if err != nil || len(events) != 1 {
		t.Fatalf("thread resolutions audited: %d (%v)", len(events), err)
	}
	var d map[string]any
	json.Unmarshal(events[0].Details, &d)
	if d["finding"] != sql.PublicID || d["why"] != "fixed" || d["thread"] != ft.threadOf(sql) || events[0].Via != viaSystem {
		t.Errorf("the audit row = %v via %s", d, events[0].Via)
	}
	if runs := rig.runs(7); runs[0].Status != "posted" {
		t.Errorf("the re-review = %s", runs[0].Status)
	}
}

// GitHub's review comment ids are past 2^31, which GraphQL's databaseId — a 32-bit Int — cannot hold:
// a review whose inline comments have such ids still finds a fixed finding's thread, by the BigInt
// fullDatabaseId, and resolves it.
func TestReviewThreadResolvedPastInt32(t *testing.T) {
	rig, fx := reviewedAtAWith(t, `{"mode":"live"}`, nil, func(rig *laneRig) { rig.gh.nextID = 2_500_000_000 })
	rig.pushAdded(fx, reviewHeadC, map[string]string{"src/store.go": storeAtB})
	ft := rig.serveGraphQL()
	rig.deliver("issue_comment", commentEvent(1311, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	sql := rig.resolveFindings()[sqlTitle]
	if sql.GitHubCommentID <= 1<<31 || sql.Status != review.FindingFixed {
		t.Fatalf("the fixed query: %s, comment %d; want one past 2^31", sql.Status, sql.GitHubCommentID)
	}
	if len(ft.mutations) != 1 || !ft.resolved[ft.threadOf(sql)] || sql.ThreadNodeID != ft.threadOf(sql) {
		t.Errorf("threads resolved = %v, stored %q; want the fixed query's", ft.mutations, sql.ThreadNodeID)
	}
}

// GitHub refusing the mutation for want of a permission costs the run nothing: the answer is in the
// thread, the review is posted, the thread stays open. It is remembered against the installation, and
// the connection's card says when; a thread of it resolved later clears it.
func TestReviewThreadRefusedForAPermission(t *testing.T) {
	ctx := context.Background()
	rig, _ := reviewedThenFixed(t, `{"mode":"live"}`, nil)
	ft := rig.serveGraphQL()
	ft.refuse = true
	rig.deliver("issue_comment", commentEvent(1401, "alice", "MEMBER", "@attesttag review", true))
	rig.drain()
	sql := rig.resolveFindings()[sqlTitle]
	if sql.Status != review.FindingFixed || len(rig.botAnswers(sql)) != 1 || len(ft.mutations) != 1 || ft.resolved[ft.threadOf(sql)] {
		t.Fatalf("refused: %s, answers %q, mutations %v", sql.Status, rig.botAnswers(sql), ft.mutations)
	}
	if runs := rig.runs(7); runs[0].Status != "posted" {
		t.Fatalf("the re-review = %s %q", runs[0].Status, runs[0].Error)
	}
	if ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.thread_resolved"}); len(ev) != 0 {
		t.Errorf("a refused resolution was audited as done")
	}
	key := reviewThreadsRefusedKey(fakeInstallation)
	if _, ok := rig.st.AlertSentAt(ctx, orgID, key); !ok {
		t.Fatal("the refusal was not remembered against the installation")
	}
	api := newReviewAPIRigOn(t, rig)
	conn := api.must(200, "GET", "/api/review-settings", api.viewer, nil)["connections"].([]any)[0].(map[string]any)
	if conn["threads_refused_at"] == "" {
		t.Errorf("the connection's card does not say resolving threads was refused: %v", conn["threads_refused_at"])
	}
	// Refused again hours into the day's window: not logged again, and the card says when it last was,
	// not when the window began.
	last := reviewThreadsRefusedLastKey(fakeInstallation)
	if _, err := rig.st.db.ExecContext(ctx, `update alerts_sent set created_at=? where org_id=? and key in (?, ?)`,
		time.Now().Add(-3*time.Hour).UTC().Format(time.DateTime), orgID, key, last); err != nil {
		t.Fatal(err)
	}
	rig.b.reviewThreadNotResolved(ctx, rig.runs(7)[0], sql, &githubGraphQLError{Type: "FORBIDDEN", Message: "Resource not accessible by integration"})
	if at, _ := rig.st.AlertSentAt(ctx, orgID, key); time.Since(at) < 2*time.Hour {
		t.Errorf("a second refusal inside the day was logged again, at %v", at)
	}
	conn = api.must(200, "GET", "/api/review-settings", api.viewer, nil)["connections"].([]any)[0].(map[string]any)
	if at, err := time.Parse(time.RFC3339, fmt.Sprint(conn["threads_refused_at"])); err != nil || time.Since(at) > time.Minute {
		t.Errorf("the card says resolving was last refused at %v (%v); it was just now", conn["threads_refused_at"], err)
	}

	// The owner accepts what was missing: the next thread resolves, and the card stops saying so.
	ft.refuse = false
	scan := rig.resolveFindings()[scanTitle]
	gh, err := rig.b.reviewClient(orgID, fakeInstallation, "acme/web", 7)
	if err != nil {
		t.Fatal(err)
	}
	rig.b.reviewResolveThread(ctx, rig.runs(7)[0], gh, rig.pr(7), scan, "fixed")
	if !ft.resolved[ft.threadOf(scan)] {
		t.Fatalf("the thread was not resolved once the permission was there: %v", ft.mutations)
	}
	if _, ok := rig.st.AlertSentAt(ctx, orgID, key); ok {
		t.Error("the refusal is still remembered after a thread resolved")
	}
	if _, ok := rig.st.AlertSentAt(ctx, orgID, last); ok {
		t.Error("the last refusal is still remembered after a thread resolved")
	}
	conn = api.must(200, "GET", "/api/review-settings", api.viewer, nil)["connections"].([]any)[0].(map[string]any)
	if conn["threads_refused_at"] != "" {
		t.Errorf("the card still says resolving was refused: %v", conn["threads_refused_at"])
	}
}

// A withdrawal on a reply resolves the thread after its answer, by the thread id a delivery about it
// stored, with no listing.
func TestReviewThreadResolvedWhenWithdrawn(t *testing.T) {
	ctx := context.Background()
	rig, _, f := newReplyRig(t)
	ft := rig.serveGraphQL()
	if err := rig.st.SetReviewFindingThread(ctx, orgID, f.ID, ft.threadOf(f)); err != nil {
		t.Fatal(err)
	}
	rig.model.classify = func(reviewModelReq) reviewModelReply { return classifyAs("pushback", "", "") }
	rig.model.reply = func(n int, q reviewModelReq) reviewModelReply {
		if n == 0 {
			return reviewModelReply{Calls: []reviewCall{{"read_file", map[string]any{"path": "src/totals.go"}}}}
		}
		return replyVerdictOf("withdraw", "", "You are right: every caller of Add holds the outer lock.", true)
	}
	rig.reply(f, "octocat", "CONTRIBUTOR", "Add is only ever called under the caller's lock.")
	rig.drain()
	if got := rig.finding(); got.Status != review.FindingWithdrawn || len(rig.botAnswers(f)) != 1 {
		t.Fatalf("the finding = %s, answers %q", got.Status, rig.botAnswers(f))
	}
	if len(ft.mutations) != 1 || ft.mutations[0] != ft.threadOf(f) || ft.pages != 0 {
		t.Errorf("resolved %v after %d listings; want the stored thread, unlisted", ft.mutations, ft.pages)
	}
	if ev, _ := rig.st.AuditEvents(ctx, orgID, AuditFilter{Action: "review.thread_resolved"}); len(ev) != 1 ||
		!strings.Contains(string(ev[0].Details), `"why":"withdrawn"`) {
		t.Errorf("the withdrawal's resolution audited: %+v", ev)
	}
}

// The client sends POST /graphql with exactly the two documents, about this pull request and threads
// known to be on it, and refuses anything else before a request is made. It reads every page of the
// threads, and lists them once however many findings it looks up.
func TestReviewGitHubGraphQLIsAllowlisted(t *testing.T) {
	ctx := context.Background()
	c, f := reviewGitHubFixture(t)
	threadPages := 0
	f.mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Query == reviewResolveThreadMutation {
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{
				"thread": map[string]any{"id": in.Variables["threadId"], "isResolved": true}}}})
			return
		}
		threadPages++
		page := map[string]any{"nodes": []map[string]any{{"id": "PRRT_a", "isResolved": false,
			"comments": map[string]any{"nodes": []map[string]any{{"fullDatabaseId": "11"}}}}},
			"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "Y3Vyc29yOnYyOpHOAQ=="}}
		if in.Variables["after"] != nil {
			page = map[string]any{"nodes": []map[string]any{{"id": "PRRT_b", "isResolved": true,
				"comments": map[string]any{"nodes": []map[string]any{{"fullDatabaseId": "2500000000"}}}}},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequest": map[string]any{"reviewThreads": page}}}})
	})
	var out any
	for name, c2 := range map[string]struct {
		query string
		vars  map[string]any
		want  string
	}{
		"another document":     {`query { viewer { login } }`, nil, "not an operation code review makes"},
		"another repository":   {reviewThreadsQuery, map[string]any{"owner": "evil", "name": "web", "number": 7}, "repository"},
		"another pull request": {reviewThreadsQuery, map[string]any{"owner": "acme", "name": "web", "number": 8}, "pull request"},
		"a variable more":      {reviewThreadsQuery, map[string]any{"owner": "acme", "name": "web", "number": 7, "first": 100}, "takes owner"},
		"a cursor that is not": {reviewThreadsQuery, map[string]any{"owner": "acme", "name": "web", "number": 7, "after": `x") { id }`}, "cursor"},
		"an unknown thread":    {reviewResolveThreadMutation, map[string]any{"threadId": "PRRT_elsewhere"}, "not known to be on this pull request"},
		"two threads at once":  {reviewResolveThreadMutation, map[string]any{"threadId": "PRRT_a", "x": 1}, "one thread id"},
	} {
		if err := c.graphql(ctx, c2.query, c2.vars, &out); err == nil || !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, c2.want)
		}
	}
	if sent := f.sent(); len(sent) != 0 {
		t.Fatalf("refused requests reached GitHub: %v", sent)
	}

	// Comment 2500000000 is past what GraphQL's 32-bit databaseId holds, as GitHub's review comment ids
	// are now: it is found by fullDatabaseId, a BigInt written as a string.
	th, ok, err := c.ThreadOf(ctx, 2_500_000_000)
	if err != nil || !ok || th.ID != "PRRT_b" || !th.Resolved || threadPages != 2 {
		t.Fatalf("the thread of comment 2500000000 = %+v, %v (%v), after %d pages", th, ok, err, threadPages)
	}
	if th, ok, _ := c.ThreadOf(ctx, 11); !ok || th.ID != "PRRT_a" || threadPages != 2 {
		t.Errorf("a second lookup listed the threads again (%d pages) or missed: %+v", threadPages, th)
	}
	if _, ok, _ := c.ThreadOf(ctx, 13); ok {
		t.Error("a comment that opened no thread was given one")
	}
	if err := c.ResolveThread(ctx, "PRRT_a"); err != nil {
		t.Errorf("resolving a thread listed from the pull request: %v", err)
	}
	c.KnowThread("PRRT_stored")
	if err := c.ResolveThread(ctx, "PRRT_stored"); err != nil {
		t.Errorf("resolving a thread a delivery stored: %v", err)
	}
	for _, s := range f.sent() {
		if s != "POST /graphql" {
			t.Errorf("sent %s", s)
		}
	}
	if !githubPermissionDenied(&githubGraphQLError{Type: "FORBIDDEN"}) ||
		!githubPermissionDenied(&githubGraphQLError{Message: "Resource not accessible by integration"}) ||
		!githubPermissionDenied(&githubAPIError{Status: 403}) || githubPermissionDenied(&githubGraphQLError{Type: "NOT_FOUND"}) {
		t.Error("githubPermissionDenied reads GitHub's refusals wrongly")
	}
}
