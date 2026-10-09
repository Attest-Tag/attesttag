package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"attesttag/internal/review"
)

// Code review as a team meets it, end to end and on both dialects: the settings made through the
// console's own routes, a signed delivery through the webhook, the dispatcher, the gate, the lane,
// the engine against a scripted model, the poster against a fake GitHub, and the conversation and
// the catch-up after. The other review tests pin each part; these pin that the parts, wired the way
// a deployment wires them, add up to what the console promised — a rule set in Settings is the rule
// a review runs under, a reply moves the score on the pull request, shadow writes nothing, a pull
// request missed while the deployment was down is reviewed once and one turned away is not, and Try
// runs the words being edited.

// newE2ERig is the console's rig before anybody has added the installation to Reviews › Settings.
func newE2ERig(t *testing.T) *reviewAPIRig {
	t.Helper()
	return newReviewAPIRigOn(t, newBareLaneRig(t, totalsFixture()))
}

// liveOnTesting is what an admin does in Reviews › Settings: Add connection, then acme/web live with
// a branch rule — a pull request into testing gets General and Security, anything else General.
func (rig *reviewAPIRig) liveOnTesting() (connID string) {
	rig.t.Helper()
	out := rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "connection", "installation_id": fakeInstallation})
	connID = out["node"].(map[string]any)["id"].(string)
	rig.must(200, "PUT", "/api/review-settings/"+connID+"?repo=acme/web", rig.admin, map[string]any{"settings": map[string]any{
		"mode": "live", "branch_rules": []map[string]any{{"base": "testing", "types": []string{"general", "security"}}, {"types": []string{"general"}}}}})
	return connID
}

// intoTesting is a pull request opened into testing rather than main.
func (rig *reviewAPIRig) intoTesting() {
	rig.gh.mu.Lock()
	rig.gh.baseRef = "testing"
	rig.gh.mu.Unlock()
}

func baseTesting(pr map[string]any) { pr["base"].(map[string]any)["ref"] = "testing" }

// wroteNothing fails the test if anything but a read reached GitHub. Tokens are minted elsewhere
// (oauthHTTPClient), so every request the fake API saw is the review's own.
func (rig *reviewAPIRig) wroteNothing() {
	rig.t.Helper()
	for _, s := range rig.fake.sent() {
		if !strings.HasPrefix(s, "GET ") {
			rig.t.Errorf("wrote to GitHub: %s", s)
		}
	}
}

// reviewedIntoTesting is liveOnTesting, then acme/web#7 opened into testing and reviewed: General
// finds the lock and Security nothing. It returns the run.
func reviewedIntoTesting(t *testing.T) (*reviewAPIRig, *ReviewRun) {
	t.Helper()
	rig := newE2ERig(t)
	rig.liveOnTesting()
	rig.intoTesting()
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(lockFinding()) }
	rig.model.finder["security"] = func(int, reviewModelReq) reviewModelReply { return submitFindings() }
	rig.model.verify = confirmAll(90)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead, baseTesting))
	rig.drain()
	runs := rig.runs(7)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	return rig, runs[0]
}

// (a) The console adds the connection and sets the repository live with a branch rule; a pull request
// into testing opens; one review is posted, against the head it read, carrying both types the rule
// names — and the console lists it as that.
func TestReviewE2EConsoleSettingsToAPostedReview(t *testing.T) {
	rig := newE2ERig(t)
	// Before the connection is added, GitHub's deliveries for it are not even written down.
	wantStatus(t, ghPost(rig.b, "pull_request", "e2e-before", prEvent("opened", 7, reviewHead, baseTesting)), 200, "pull_request")
	if n := deliveryCount(t, rig.b); n != 0 {
		t.Fatalf("%d deliveries stored for an installation nobody added to review", n)
	}

	rig.liveOnTesting()
	rig.intoTesting()
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(lockFinding()) }
	rig.model.finder["security"] = func(int, reviewModelReq) reviewModelReply { return submitFindings() }
	rig.model.verify = confirmAll(90)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead, baseTesting))
	rig.drain()

	runs := rig.runs(7)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	run := runs[0]
	var keys []string
	for _, ty := range run.Types {
		keys = append(keys, ty.Key)
	}
	// The rule's two, and Concurrency after them: the change takes a lock away, which brings it in.
	if run.Status != "posted" || run.Trigger != "open" || run.RuleLabel != "any → testing" || !slices.Equal(keys, []string{"general", "security", "concurrency"}) {
		t.Fatalf("run = %s, trigger %s, rule %q, types %v", run.Status, run.Trigger, run.RuleLabel, keys)
	}
	var finders []string
	for _, q := range rig.model.requests("finder") {
		if !slices.Contains(finders, q.Type) {
			finders = append(finders, q.Type)
		}
	}
	slices.Sort(finders)
	if !slices.Equal(finders, []string{"concurrency", "general", "security"}) {
		t.Errorf("the finder ran for %v", finders)
	}
	posts, reviews, comments, _ := rig.gh.snapshot()
	if len(posts) != 1 || len(reviews) != 1 || posts[0]["commit_id"] != reviewHead || posts[0]["event"] != "COMMENT" ||
		len(posts[0]["comments"].([]reviewInlineComment)) != 1 {
		t.Fatalf("GitHub got %d review posts: %v", len(posts), posts)
	}
	if len(comments) != 1 {
		t.Fatalf("%d issue comments; want the one summary", len(comments))
	}
	summary := comments[0].Body
	for _, want := range []string{"Confidence 3/5", "Reviewed as General, Security and Concurrency and state (auto)",
		"open: General 1 · Security 0", "Rule: `any → testing`", "<summary>Code</summary>"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary lacks %q:\n%s", want, summary)
		}
	}

	// The same body again, under another delivery id, is the same review: nothing more is posted.
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead, baseTesting))
	rig.drain()
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 || len(rig.runs(7)) != 1 {
		t.Errorf("a redelivery posted %d reviews over %d runs", len(posts), len(rig.runs(7)))
	}

	listed := rig.must(200, "GET", "/api/reviews?repo=acme/web", rig.viewer, nil)["runs"].([]any)
	if len(listed) != 1 {
		t.Fatalf("History lists %v", listed)
	}
	h := listed[0].(map[string]any)
	if h["id"] != run.PublicID || h["post"] != "live" || h["status"] != "posted" || h["rule"] != "any → testing" ||
		h["findings"] != float64(1) || len(h["types"].([]any)) != 3 {
		t.Errorf("History shows %v", h)
	}
}

// (b) "@attesttag status" is answered from what is stored — the score, what is open, the rule — with
// no model call.
func TestReviewE2EStatusIsAnsweredWithoutTheModel(t *testing.T) {
	rig, _ := reviewedIntoTesting(t)
	rig.serveConversation()
	calls := len(rig.model.requests(""))
	rig.deliver("issue_comment", commentEvent(901, "alice", "MEMBER", "@attesttag status", true))
	a := rig.answers()
	if len(a) != 1 {
		t.Fatalf("answers = %q", a)
	}
	for _, want := range []string{"Confidence **3/5**", "Open findings: 1 P1.", "any → testing"} {
		if !strings.Contains(a[0], want) {
			t.Errorf("status lacks %q:\n%s", want, a[0])
		}
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("status made %d model calls", n-calls)
	}
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 {
		t.Errorf("status posted a review: %d posts", len(posts))
	}
}

// (c) The author pushes back in the finding's thread, the verdict withdraws it, and the summary on
// the pull request — and the console — show the score it leaves.
func TestReviewE2EPushbackWithdrawsAndTheSummaryFollows(t *testing.T) {
	rig, run := reviewedIntoTesting(t)
	rig.serveConversation()
	f := rig.finding()
	rig.model.classify = func(reviewModelReq) reviewModelReply { return classifyAs("pushback", "", "") }
	rig.model.reply = func(n int, q reviewModelReq) reviewModelReply {
		if n == 0 {
			return reviewModelReply{Calls: []reviewCall{{"read_file", map[string]any{"path": "src/totals.go"}}}}
		}
		return replyVerdictOf("withdraw", "", "You are right: every caller of Add holds the outer lock.", true)
	}
	rig.reply(f, "octocat", "CONTRIBUTOR", "Add is only ever called under the caller's lock.")
	rig.drain()

	if got := rig.finding(); got.Status != review.FindingWithdrawn {
		t.Fatalf("the finding is %s after the withdrawal", got.Status)
	}
	if pr := rig.pr(7); pr.Score != 5 {
		t.Errorf("the pull request's score = %d, want 5", pr.Score)
	}
	_, _, comments, patches := rig.gh.snapshot()
	if len(patches) == 0 || !strings.Contains(patches[len(patches)-1], "Confidence 5/5") {
		t.Fatalf("the summary was not edited to the new score:\n%s", strings.Join(patches, "\n---\n"))
	}
	summaries := 0
	for _, c := range comments {
		if strings.Contains(c.Body, "Confidence") {
			summaries++
		}
	}
	if summaries != 1 {
		t.Errorf("%d summary comments; the one there is edited in place", summaries)
	}
	if a := rig.botAnswers(f); len(a) != 1 || !strings.HasPrefix(a[0], "**Withdrawn.**") {
		t.Errorf("the answer in the thread = %q", a)
	}
	d := rig.must(200, "GET", "/api/reviews/"+run.PublicID, rig.viewer, nil)
	fs := d["findings"].([]any)
	if len(fs) != 1 || fs[0].(map[string]any)["status"] != string(review.FindingWithdrawn) {
		t.Errorf("the console shows the finding as %v", fs)
	}
}

// (d) Start review in shadow on a deployment with no webhook secret: GitHub's deliveries are refused
// and nothing waits on them, yet the review a person starts runs, writes nothing to GitHub, and is
// in History.
func TestReviewE2EManualShadowStartNeedsNoWebhook(t *testing.T) {
	rig := newE2ERig(t)
	rig.liveOnTesting()
	rig.b.ghHook = newGitHubWebhookFrom("", "", "")
	if !rig.b.reviewLaneOn() || rig.b.reviewCatchupOn() {
		t.Fatalf("without a webhook secret: lane on = %v, catch-up on = %v; want the lane and no catch-up",
			rig.b.reviewLaneOn(), rig.b.reviewCatchupOn())
	}
	wantStatus(t, ghPost(rig.b, "pull_request", "e2e-nohook", prEvent("opened", 7, reviewHead)), http.StatusServiceUnavailable, "pull_request")

	rig.confirmLock()
	out := rig.must(200, "POST", "/api/reviews", rig.editor, map[string]any{"repo": "acme/web", "pr": 7, "post": "shadow"})
	id := out["run"].(map[string]any)["id"].(string)
	rig.drain()
	rig.wroteNothing()
	if posts, _, comments, patches := rig.gh.snapshot(); len(posts)+len(comments)+len(patches) != 0 {
		t.Errorf("a shadow start wrote to GitHub: %d posts, %d comments, %d edits", len(posts), len(comments), len(patches))
	}
	listed := rig.must(200, "GET", "/api/reviews", rig.viewer, nil)["runs"].([]any)
	if len(listed) != 1 {
		t.Fatalf("History lists %v", listed)
	}
	h := listed[0].(map[string]any)
	if h["id"] != id || h["trigger"] != "console" || h["post"] != "shadow" || h["status"] != "shadow" || h["findings"] != float64(1) {
		t.Errorf("History shows %v", h)
	}
	if pr := rig.pr(7); pr.SummaryCommentID != 0 {
		t.Errorf("a shadow review left a summary comment id: %+v", pr)
	}
}

// catchupListing serves GET /repos/acme/web/pulls from fn, under the read token, as the catch-up
// lists a repository's open pull requests.
func (rig *reviewAPIRig) catchupListing(fn func(head string) []map[string]any) {
	rig.fake.mux.HandleFunc("GET /repos/acme/web/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := rig.fake.permsOf(r); got != readPerms {
			rig.t.Errorf("the catch-up listed pull requests with a token for %q", got)
		}
		rig.gh.mu.Lock()
		head := rig.gh.head
		rig.gh.mu.Unlock()
		json.NewEncoder(w).Encode(fn(head))
	})
}

// pullItem is one entry of the listing.
func pullItem(n int, head, login, userType string, created time.Time) map[string]any {
	ref := func(sha, branch string) map[string]any {
		return map[string]any{"sha": sha, "ref": branch, "repo": map[string]any{"full_name": "acme/web", "private": true}}
	}
	return map[string]any{"number": n, "state": "open", "draft": false, "title": "A change",
		"created_at": created.UTC().Format(time.RFC3339), "updated_at": created.UTC().Format(time.RFC3339),
		"user": map[string]any{"login": login, "type": userType}, "head": ref(head, "feature"), "base": ref(reviewBase, "main")}
}

// (e) The deployment was down while acme/web#7 opened, so its delivery never came. The catch-up finds
// it and reviews it, once — a second pass, and the delivery arriving after all, add nothing — while
// a bot's pull request the gate turned away stays turned away and one opened before reviews were on
// stays unreviewed. A push it also missed re-renders the summary's footer, for nothing.
func TestReviewE2ECatchupReviewsWhatWasMissedOnce(t *testing.T) {
	ctx := context.Background()
	rig := newE2ERig(t)
	rig.liveOnTesting()
	rig.confirmLock()
	sha8, sha9 := strings.Repeat("8", 40), strings.Repeat("9", 40)

	// While the deployment is up, a bot opens acme/web#8, and the gate says no.
	rig.deliver("pull_request", prEvent("opened", 8, sha8, func(pr map[string]any) {
		pr["user"] = map[string]any{"login": "renovate[bot]", "type": "Bot"}
	}))
	if pr8 := rig.pr(8); pr8 == nil || pr8.SkipReason != "bot" {
		t.Fatalf("the bot's pull request = %+v", pr8)
	}

	// Down: #7 opens, and its delivery is lost. #9 was opened hours before reviews were turned on.
	opened := time.Now()
	rig.catchupListing(func(head string) []map[string]any {
		return []map[string]any{pullItem(7, head, "octocat", "User", opened), pullItem(8, sha8, "renovate[bot]", "Bot", opened),
			pullItem(9, sha9, "octocat", "User", opened.Add(-3*time.Hour))}
	})
	rig.b.reviewCatchup(ctx, 0)
	runs := rig.runs(7)
	if len(runs) != 1 || runs[0].Kind != "review" || runs[0].Trigger != "catchup" || runs[0].Status != "queued" {
		t.Fatalf("after the catch-up #7 has %+v", runs)
	}
	if pr8 := rig.pr(8); len(rig.runs(8)) != 0 || pr8.SkipReason != "bot" {
		t.Errorf("the catch-up resurrected the bot's pull request: %+v, %d runs", pr8, len(rig.runs(8)))
	}
	if rig.pr(9) != nil {
		t.Errorf("the catch-up took up a pull request opened before reviews were on: %+v", rig.pr(9))
	}

	// Another pass, and the "opened" delivery turning up after all, are the same review.
	rig.b.reviewCatchup(ctx, 1)
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	if n := len(rig.runs(7)); n != 1 {
		t.Fatalf("%d runs for #7; the catch-up and the late delivery are one review", n)
	}
	rig.drain()
	if posts, _, comments, _ := rig.gh.snapshot(); len(posts) != 1 || len(comments) != 1 || rig.pr(7).LastReviewedSHA != reviewHead {
		t.Fatalf("#7 got %d reviews and %d summaries", len(posts), len(comments))
	}

	// Pushed to while down again: the head moves and the footer says so, with no model call.
	calls := len(rig.model.requests(""))
	rig.gh.pushTo(reviewHeadC, totalsFixture().files, totalsFixture().head)
	rig.b.reviewCatchup(ctx, 2)
	rig.drain()
	if pr := rig.pr(7); pr.HeadSHA != reviewHeadC {
		t.Errorf("the stored head = %s", pr.HeadSHA)
	}
	_, _, _, patches := rig.gh.snapshot()
	if len(patches) != 1 || !strings.Contains(patches[0], "head `ccccccc` not reviewed") {
		t.Errorf("the summary's footer did not follow the push:\n%s", strings.Join(patches, "\n---\n"))
	}
	if n := len(rig.model.requests("")); n != calls {
		t.Errorf("the missed push made %d model calls on a repository reviewed on open", n-calls)
	}
	if posts, _, _, _ := rig.gh.snapshot(); len(posts) != 1 {
		t.Errorf("%d reviews posted", len(posts))
	}
}

// A repository somebody had switched off, and then reset to inherit its connection's shadow, is not
// caught up on the pull requests opened while it was off: those were not missed, they were not
// wanted. One opened after the reset is.
func TestReviewCatchupLeavesWhatOpenedWhileTheRepositoryWasOff(t *testing.T) {
	ctx := context.Background()
	rig := newE2ERig(t)
	out := rig.must(200, "POST", "/api/review-settings", rig.admin, map[string]any{"kind": "connection", "installation_id": fakeInstallation})
	connID := out["node"].(map[string]any)["id"].(string)
	detail := rig.must(200, "PUT", "/api/review-settings/"+connID+"?repo=acme/web", rig.admin,
		map[string]any{"settings": map[string]any{"mode": "off"}})
	repoID := detail["node"].(map[string]any)["id"].(string)
	// The connection was added two hours ago and the repository switched off one hour ago.
	ago := func(d time.Duration) string { return time.Now().UTC().Add(-d).Format(time.DateTime) }
	for id, at := range map[string]string{connID: ago(2 * time.Hour), repoID: ago(time.Hour)} {
		if _, err := rig.st.db.ExecContext(ctx, `update review_settings set updated_at=? where org_id=? and public_id=?`, at, orgID, id); err != nil {
			t.Fatal(err)
		}
	}
	opened := time.Now().Add(-30 * time.Minute)
	rig.catchupListing(func(head string) []map[string]any {
		return []map[string]any{pullItem(7, head, "octocat", "User", opened)}
	})
	rig.must(200, "DELETE", "/api/review-settings/"+repoID, rig.admin, nil)
	rig.b.reviewCatchup(ctx, 0)
	if pr := rig.pr(7); pr != nil {
		t.Fatalf("the catch-up took up a pull request opened while the repository was off: %+v", pr)
	}
	opened = time.Now()
	rig.b.reviewCatchup(ctx, 1)
	if runs := rig.runs(7); len(runs) != 1 || runs[0].Trigger != "catchup" {
		t.Errorf("a pull request opened after the reset: %+v", runs)
	}
}

// (f) Try on a PR runs a type as it is being edited — on top of what was saved — in shadow on a live
// repository, with the edited rules in the finder's prompt, and saves nothing: the type stays at the
// version it was, and the pull request's own review is untouched.
func TestReviewE2ETryRunsTheEditedRulesInShadow(t *testing.T) {
	rig := newE2ERig(t)
	rig.liveOnTesting()
	rig.intoTesting()
	shipped := rig.must(200, "GET", "/api/review-types/security", rig.viewer, nil)["type"].(map[string]any)["rules"].([]any)
	var rules []map[string]any
	for _, r := range shipped {
		rules = append(rules, map[string]any{"text": r.(map[string]any)["text"], "severity_cap": r.(map[string]any)["severity_cap"]})
	}
	saved := rig.must(200, "PUT", "/api/review-types/security", rig.editor, map[string]any{"version": 0,
		"rules": append(slices.Clone(rules), map[string]any{"text": "Every list query names the organisation.", "severity_cap": "P1"})})["type"].(map[string]any)
	if saved["version"] != float64(2) {
		t.Fatalf("saved = %v", saved)
	}
	var tried []map[string]any
	for _, r := range saved["rules"].([]any) {
		r := r.(map[string]any)
		tried = append(tried, map[string]any{"id": r["id"], "text": r["text"], "severity_cap": r["severity_cap"]})
	}
	tried = append(tried, map[string]any{"text": "Totals are only written under the lock.", "severity_cap": "P1"})

	rig.model.finder["security"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(lockFinding()) }
	rig.model.verify = confirmAll(90)
	out := rig.must(200, "POST", "/api/review-types/try", rig.editor, map[string]any{"repo": "acme/web", "pr": 7,
		"type": map[string]any{"key": "security", "rules": tried}})
	q := out["run"].(map[string]any)
	if q["kind"] != "try" || q["post"] != "shadow" {
		t.Fatalf("queued = %v", q)
	}
	rig.drain()

	run, _ := rig.st.ReviewRunByPublicID(context.Background(), orgID, q["id"].(string))
	if run == nil || run.Status != "shadow" || len(run.Types) != 1 || run.Types[0] != (ReviewRunType{Key: "security", Version: 0}) {
		t.Fatalf("the try = %+v", run)
	}
	finders := rig.model.requests("finder")
	if len(finders) == 0 {
		t.Fatal("the finder never ran")
	}
	for _, q := range finders {
		if q.Type != "security" || !strings.Contains(q.System, "Totals are only written under the lock.") ||
			!strings.Contains(q.System, "Every list query names the organisation.") {
			t.Errorf("a %s finder was not given the edited rules:\n%s", q.Type, q.System)
		}
	}
	rig.wroteNothing()
	if vs := rig.must(200, "GET", "/api/review-types/"+saved["id"].(string)+"/versions", rig.viewer, nil)["versions"].([]any); len(vs) != 2 {
		t.Errorf("trying saved a version: %v", vs)
	}
	now := rig.must(200, "GET", "/api/review-types/security", rig.viewer, nil)["type"].(map[string]any)
	if now["version"] != float64(2) || strings.Contains(string(reviewJSON(now["rules"])), "Totals are only written") {
		t.Errorf("trying changed the saved type: %v", now)
	}
	if pr := rig.pr(7); pr.LastReviewedSHA != "" || pr.Score != -1 || pr.ReviewsCount != 0 {
		t.Errorf("a try moved the pull request's review: %+v", pr)
	}
	d := rig.must(200, "GET", "/api/reviews/"+run.PublicID, rig.viewer, nil)
	if fs := d["findings"].([]any); len(fs) != 1 {
		t.Errorf("the try's findings in the console = %v", fs)
	}
}

// What the catch-up does about one open pull request, by what is stored of it.
func TestReviewCatchupAction(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	after, before := since.Add(time.Minute).Format(time.RFC3339), since.Add(-time.Minute).Format(time.RFC3339)
	head, other := strings.Repeat("a", 40), strings.Repeat("c", 40)
	item := func(created string, draft bool, sha string) githubPullItem {
		return githubPullItem{Number: 7, CreatedAt: created, Draft: draft, Head: githubPullRef{SHA: sha}}
	}
	row := func(skip, sha string) *ReviewPR { return &ReviewPR{ID: 1, HeadSHA: sha, SkipReason: skip} }
	for _, c := range []struct {
		name     string
		row      *ReviewPR
		reviewed bool
		p        githubPullItem
		want     string
	}{
		{"never seen, opened after", nil, false, item(after, false, head), "open"},
		{"never seen, opened the same second", nil, false, item(since.Format(time.RFC3339), false, head), "open"},
		{"never seen, opened before", nil, false, item(before, false, head), ""},
		{"opened at a time that does not parse", nil, false, item("yesterday", false, head), ""},
		{"seen, never reviewed, no reason", row("", head), false, item(after, false, head), "open"},
		{"turned away, same head", row("excluded_author", head), false, item(after, false, head), ""},
		{"turned away for the money, same head", row("budget", head), false, item(after, false, head), ""},
		{"turned away, pushed to since", row("bot", head), false, item(after, false, other), "push"},
		{"a draft since made ready", row("draft", head), false, item(after, false, head), "open"},
		{"still a draft", row("draft", head), false, item(after, true, head), ""},
		{"closed, and open again", row("closed", head), false, item(after, false, head), "open"},
		{"reviewed, same head", row("", head), true, item(before, false, head), ""},
		{"reviewed, pushed to", row("", head), true, item(before, false, other), "push"},
		{"reviewed, head unknown", row("", ""), true, item(before, false, other), ""},
		{"reviewed, GitHub's head not a commit", row("", head), true, item(before, false, "main"), ""},
	} {
		if got := reviewCatchupAction(c.row, c.reviewed, c.p, since); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The line is the newest change on the chain, and a chain whose times do not read has none.
func TestReviewChainSince(t *testing.T) {
	at := func(s string) *ReviewSetting { return &ReviewSetting{UpdatedAt: s} }
	got, ok := reviewChainSince([]*ReviewSetting{at("2026-10-01 10:00:00"), at("2026-10-01 12:30:00"), at("2026-10-01 11:00:00")})
	if !ok || !got.Equal(time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("since = %v %v", got, ok)
	}
	if _, ok := reviewChainSince([]*ReviewSetting{at("2026-10-01 10:00:00"), at("soon")}); ok {
		t.Error("an unreadable time gave a line")
	}
	if _, ok := reviewChainSince(nil); ok {
		t.Error("no chain gave a line")
	}
}

// The catch-up's share of a pass is per organisation: forty repositories listed and fifty pull
// requests acted on, the rest deferred to the next pass; the repositories it did not list are listed
// first next time, so none is left out of every pass; GitHub asking for a wait stops that
// organisation's pass and not the next organisation's; and one organisation's backlog takes nothing
// from another's share.
func TestReviewCatchupHoldsEachOrganisationToItsShare(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"shadow"}`)
	if org, _, _ := seedOrg(t, rig.st, RoleAdmin); org != orgID {
		t.Fatalf("the rig's organisation is %d, want %d", org, orgID)
	}
	chain, err := rig.st.ReviewSettingsChain(ctx, orgID, fakeInstallation, "acme/web")
	if err != nil || len(chain) == 0 {
		t.Fatal(err)
	}
	const repos = reviewCatchupReposPerOrg + 5
	for i := range repos {
		if _, err := rig.st.EnsureReviewRepo(ctx, orgID, chain[0].ID, fmt.Sprintf("acme/r%02d", i), "admin@acme.test"); err != nil {
			t.Fatal(err)
		}
	}
	// A second organisation, with one repository of its own through its own installation.
	other, _ := secondOrg(t, rig.st)
	seedInstall(t, rig.st, other, 6161, "octo-org")
	conn2, _, err := rig.st.AddReviewConnection(ctx, other, 6161, json.RawMessage(`{"mode":"shadow"}`), "admin@octo-org.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.EnsureReviewRepo(ctx, other, conn2.ID, "octo-org/api", "admin@octo-org.test"); err != nil {
		t.Fatal(err)
	}
	// Tokens for either installation.
	saved := oauthHTTPClient
	t.Cleanup(func() { oauthHTTPClient = saved })
	oauthHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		var m mintRequest
		json.NewDecoder(r.Body).Decode(&m)
		rig.fake.mu.Lock()
		rig.fake.mints = append(rig.fake.mints, m)
		tok := fmt.Sprintf("ghs_minted%d", len(rig.fake.mints))
		rig.fake.perms[tok] = permissionList(m.Permissions)
		rig.fake.mu.Unlock()
		rec.WriteHeader(201)
		fmt.Fprintf(rec, `{"token":%q,"expires_at":%q}`, tok, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return rec.Result(), nil
	})}

	// Every acme repository has two pull requests nobody heard about; octo-org/api has one. limit
	// answers the next acme listing with GitHub's request to wait.
	var listed []string
	limit := false
	rig.fake.mux.HandleFunc("GET /repos/{owner}/{name}/pulls", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("owner") + "/" + r.PathValue("name")
		rig.gh.mu.Lock()
		listed = append(listed, repo)
		wait := limit && r.PathValue("owner") == "acme"
		limit = limit && !wait
		rig.gh.mu.Unlock()
		if wait {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
			return
		}
		opened := time.Now().Add(time.Minute)
		n := 2
		if r.PathValue("owner") == "octo-org" {
			n = 1
		}
		var out []map[string]any
		for i := 1; i <= n; i++ {
			out = append(out, pullItem(i, strings.Repeat(fmt.Sprint(i), 40), "octocat", "User", opened))
		}
		json.NewEncoder(w).Encode(out)
	})
	pass := func(p int) (acme []string, octo int) {
		t.Helper()
		rig.gh.mu.Lock()
		listed = nil
		rig.gh.mu.Unlock()
		rig.b.reviewCatchup(ctx, p)
		rig.gh.mu.Lock()
		defer rig.gh.mu.Unlock()
		for _, r := range listed {
			if strings.HasPrefix(r, "acme/") {
				acme = append(acme, r)
			} else {
				octo++
			}
		}
		return acme, octo
	}
	acted := func(org int64) int {
		t.Helper()
		var n int
		if err := rig.st.db.QueryRowContext(ctx, `select count(*) from review_prs where org_id=?`, org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	acme, octo := pass(0)
	if len(acme) != reviewCatchupReposPerOrg || slices.Contains(acme, "acme/r40") || octo != 1 {
		t.Fatalf("pass 0 listed %d of acme's repositories (%v) and %d of octo-org's", len(acme), acme, octo)
	}
	if n := acted(orgID); n != reviewCatchupPRsPerOrg {
		t.Errorf("pass 0 acted on %d of acme's pull requests, want its share of %d", n, reviewCatchupPRsPerOrg)
	}
	if n := acted(other); n != 1 {
		t.Errorf("acme's backlog took octo-org's share: %d acted on", n)
	}

	// The repositories pass 0 left out are listed first; what it deferred is queued now.
	acme, _ = pass(1)
	if len(acme) == 0 || acme[0] != "acme/r40" || !slices.Contains(acme, "acme/r44") {
		t.Errorf("pass 1 listed %v; want the five pass 0 left out first", acme)
	}
	if n := acted(orgID); n != reviewCatchupPRsPerOrg+30 {
		t.Errorf("after pass 1 %d of acme's pull requests acted on, want %d", n, reviewCatchupPRsPerOrg+30)
	}

	// GitHub asks acme's installation to wait: acme's pass stops at that listing, octo-org's does not.
	rig.gh.mu.Lock()
	limit = true
	rig.gh.mu.Unlock()
	acme, octo = pass(2)
	if len(acme) != 1 || octo != 1 {
		t.Errorf("a pass GitHub asked to wait listed %d of acme's repositories and %d of octo-org's", len(acme), octo)
	}
	if _, _ = pass(3); acted(orgID) != 2*repos {
		t.Errorf("after pass 3 %d of acme's %d missed pull requests acted on", acted(orgID), 2*repos)
	}
}

// A bot's pull request is turned away unless the settings name the bot: renovate, listed at the
// connection without its "[bot]", is reviewed like anybody's, and dependabot, not listed, is not.
func TestReviewE2EReviewsOnlyTheBotsListed(t *testing.T) {
	rig := newE2ERig(t)
	connID := rig.liveOnTesting()
	rig.confirmLock()
	rig.must(200, "PUT", "/api/review-settings/"+connID, rig.admin, map[string]any{"settings": map[string]any{"review_bots": []string{"renovate"}}})
	skip := func(n int) string {
		if p := rig.pr(n); p != nil {
			return p.SkipReason
		}
		return "(no pull request)"
	}
	sha8, sha9 := strings.Repeat("8", 40), strings.Repeat("9", 40)

	rig.deliver("pull_request", prEvent("opened", 8, sha8, func(pr map[string]any) {
		pr["user"] = map[string]any{"login": "renovate[bot]", "type": "Bot"}
	}))
	if runs := rig.runs(8); len(runs) != 1 || runs[0].Kind != "review" || runs[0].Status != "queued" {
		t.Fatalf("renovate, listed, got %+v, skipped as %q", runs, skip(8))
	}
	rig.deliver("pull_request", prEvent("opened", 9, sha9, func(pr map[string]any) {
		pr["user"] = map[string]any{"login": "dependabot[bot]", "type": "Bot"}
	}))
	if got := skip(9); got != "bot" || len(rig.runs(9)) != 0 {
		t.Fatalf("dependabot, not listed: skipped as %q with %d runs", got, len(rig.runs(9)))
	}
}
