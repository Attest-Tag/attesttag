package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"attesttag/internal/review"
)

// Fixes asked for on the pull request, end to end: a signed delivery — a ticked fix box, `@attesttag
// fix` in a finding's thread or on the conversation — through the real webhook route and the
// dispatcher, to a fix job on the pull request's own branch, and back: the answer on GitHub when the
// job ends and the review of the commit it pushed. What these pin is what lets a team leave the box
// on: only somebody who can push gets a push, nothing is written in shadow, a request delivered
// twice is one job, one job a pull request at a time, and a job that pushed nothing says why.

// fixWriters may push to acme/web; anybody else may only read it.
var fixWriters = []string{"octocat", "alice"}

// newFixRig is a live (or settings) repository with a fix worker: jobs go to a fake dispatcher on
// the shared model key, and GitHub answers who may push. Its pull request has been reviewed, and
// the lock finding posted inline — with the fix box, when the settings allow fixes.
func newFixRig(t *testing.T, settings string) (*laneRig, *convo, *fakeDispatcher) {
	t.Helper()
	rig := newLaneRig(t, totalsFixture(), settings)
	cv := rig.serveConversation()
	cfg := rig.b.cfg
	cfg.WorkerMode, cfg.WorkerLLMKey = "fake", "sk-or-shared-key-0123456789"
	rig.b.cfg = cfg
	fd := &fakeDispatcher{}
	rig.b.jobs = NewJobRunner(cfg, rig.st, nil, rig.b.proxy, rig.b.settings)
	rig.b.jobs.dispatchers["fake"] = fd
	rig.b.jobs.onGitHubEnd = rig.b.reviewFixEnded
	rig.fake.mux.HandleFunc("GET /repos/acme/web/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) {
		if got := rig.fake.permsOf(r); got != readPerms {
			t.Errorf("the permission read went out with a token for %q, want %q", got, readPerms)
		}
		perm := "read"
		if slices.Contains(fixWriters, r.PathValue("login")) {
			perm = "write"
		}
		json.NewEncoder(w).Encode(map[string]any{"permission": perm, "role_name": perm})
	})
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	return rig, cv, fd
}

// findingComment is the finding's inline comment as GitHub holds it now.
func (rig *laneRig) findingComment(f *ReviewFinding) githubComment {
	rig.t.Helper()
	rig.gh.mu.Lock()
	defer rig.gh.mu.Unlock()
	for _, c := range rig.gh.reviewComments {
		if c.ID == f.GitHubCommentID {
			return c
		}
	}
	rig.t.Fatalf("no comment %d on the fake GitHub", f.GitHubCommentID)
	return githubComment{}
}

// tickEvent is the edit GitHub delivers when login ticks the fix box on f's comment; the comment on
// the fake GitHub is ticked too.
func (rig *laneRig) tickEvent(f *ReviewFinding, login string) []byte {
	rig.t.Helper()
	c := rig.findingComment(f)
	before := c.Body
	after := strings.Replace(before, "- [ ] "+review.FixBoxText, "- [x] "+review.FixBoxText, 1)
	if after == before {
		rig.t.Fatalf("the finding's comment has no fix box to tick:\n%s", before)
	}
	rig.gh.mu.Lock()
	for i := range rig.gh.reviewComments {
		if rig.gh.reviewComments[i].ID == c.ID {
			rig.gh.reviewComments[i].Body = after
		}
	}
	rig.gh.mu.Unlock()
	c.Body, c.HTMLURL = after, fmt.Sprintf("https://github.com/acme/web/pull/7#discussion_r%d", c.ID)
	var p map[string]any
	json.Unmarshal(replyEvent(c, githubUser{Login: login, Type: "User"}), &p)
	p["action"], p["changes"] = "edited", map[string]any{"body": map[string]any{"from": before}}
	b, _ := json.Marshal(p)
	return b
}

// fixJobs are the jobs the rig's organisation has, oldest first, each with its spec.
func (rig *laneRig) fixJobs() []*Job {
	rig.t.Helper()
	list, err := rig.st.Jobs(context.Background(), orgID, JobFilter{Limit: 50})
	if err != nil {
		rig.t.Fatal(err)
	}
	var out []*Job
	for i := len(list) - 1; i >= 0; i-- {
		j, err := rig.st.Job(context.Background(), orgID, list[i].ID)
		if err != nil || j == nil {
			rig.t.Fatalf("job %d: %v", list[i].ID, err)
		}
		out = append(out, j)
	}
	return out
}

func jobSpecOf(t *testing.T, j *Job) JobSpec {
	t.Helper()
	var s JobSpec
	if err := json.Unmarshal([]byte(j.Spec), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// fixAudits are the review.fix audit rows' outcomes, oldest first.
func (rig *laneRig) fixAudits() []string {
	rig.t.Helper()
	events, err := rig.st.AuditEvents(context.Background(), orgID, AuditFilter{Action: "review.fix"})
	if err != nil {
		rig.t.Fatal(err)
	}
	var out []string
	for i := len(events) - 1; i >= 0; i-- {
		var d map[string]any
		json.Unmarshal(events[i].Details, &d)
		out = append(out, fmt.Sprint(d["outcome"]))
	}
	return out
}

// Ticking the box is the request: a fix job on the pull request's own branch, for that finding,
// pushing as the App through the installation the review came through, picked up with eyes and a
// line in the thread saying where the commit will go. Delivered again, it is the same request.
func TestReviewFixBoxTickStartsAJobOnThePullRequestsBranch(t *testing.T) {
	rig, cv, fd := newFixRig(t, `{"mode":"live"}`)
	f := rig.finding()
	if body := rig.findingComment(f).Body; !strings.Contains(body, "- [ ] "+review.FixBoxText) || !strings.Contains(body, "<code>@attesttag fix</code>") {
		t.Fatalf("the finding's comment offers no fix:\n%s", body)
	}
	tick := rig.tickEvent(f, "octocat")
	wantStatus(t, ghPost(rig.b, "pull_request_review_comment", "fix-tick-1", tick), 200, "tick")
	dispatchAll(t, rig.b)

	jobs := rig.fixJobs()
	if len(jobs) != 1 || len(fd.launches) != 1 {
		t.Fatalf("jobs = %d, launches = %d; want one of each", len(jobs), len(fd.launches))
	}
	j := jobs[0]
	if j.TeamID != "" || j.Channel != "github:acme/web" || j.ThreadTS != "pr:7" || j.Approval != "github" ||
		j.Requester != "github:octocat" || j.Branch != "feature" || j.PRURL != "https://github.com/acme/web/pull/7" || !j.onGitHub() {
		t.Errorf("job = %+v", j)
	}
	s := jobSpecOf(t, j)
	if s.Mode != JobModePR || s.BaseBranch != "feature" || s.Branch != "feature" || s.HeadSHA != reviewHead || s.ConnectionID != 0 ||
		s.PR == nil || s.PR.InstallationID != fakeInstallation || s.PR.Thread != f.GitHubCommentID || s.PR.Base != "main" ||
		s.PR.AskedBy != "octocat" || !slices.Equal(s.PR.Findings, []string{f.PublicID}) {
		t.Errorf("spec = %+v, pr = %+v", s, s.PR)
	}
	if !strings.Contains(s.Requirement, f.Title) || !strings.Contains(s.Title, f.Title) || !slices.Contains(s.FilesHint, f.Path) {
		t.Errorf("the brief does not carry the finding: title %q, files %v\n%s", s.Title, s.FilesHint, s.Requirement)
	}
	// The push goes out as the App, through the installation, whatever the spec names.
	conn, err := rig.b.jobs.jobPushConnection(context.Background(), orgID, s)
	if err != nil || conn.CredType != "github_app" || conn.GitHubInstallationID != fakeInstallation || conn.Repo != "acme/web" {
		t.Errorf("push connection = %+v, %v", conn, err)
	}
	if !cv.has(rig.gh, fmt.Sprintf("inline:%d:eyes", f.GitHubCommentID)) {
		t.Errorf("the tick was not picked up visibly: %v", cv.reactions)
	}
	if a := rig.botAnswers(f); len(a) != 1 || !strings.Contains(a[0], "fix job #1") || !strings.Contains(a[0], "`feature`") {
		t.Errorf("answers in the thread = %q", a)
	}
	if got := rig.fixAudits(); !slices.Equal(got, []string{"started"}) {
		t.Errorf("audits = %q", got)
	}

	// The same delivery again, and the same tick under a new delivery id while the job runs.
	wantStatus(t, ghPost(rig.b, "pull_request_review_comment", "fix-tick-1", tick), 200, "tick again")
	dispatchAll(t, rig.b)
	if n := len(rig.fixJobs()); n != 1 {
		t.Errorf("a redelivered tick started %d jobs", n)
	}
	rig.deliver("pull_request_review_comment", tick)
	if n := len(rig.fixJobs()); n != 1 {
		t.Errorf("a second tick while the job runs started %d jobs", n)
	}
	if a := rig.botAnswers(f); len(a) != 2 || !strings.Contains(a[1], "Fix job #1 is still working") {
		t.Errorf("answers after a second request = %q", a)
	}
}

// `@attesttag fix` in a finding's thread fixes that finding, with the asker's own words carried to
// the job; it is not a reply for the verdict model.
func TestReviewFixThreadCommandFixesThatFinding(t *testing.T) {
	rig, cv, _ := newFixRig(t, `{"mode":"live"}`)
	f := rig.finding()
	c := rig.reply(f, "alice", "MEMBER", "@attesttag fix, and keep Add's signature as it is")
	rig.drain()
	jobs := rig.fixJobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	s := jobSpecOf(t, jobs[0])
	if !strings.Contains(s.Requirement, "@alice, who asked for the fix, added: and keep Add's signature as it is") {
		t.Errorf("the asker's words are not in the brief:\n%s", s.Requirement)
	}
	if s.PR.AskedURL != c.HTMLURL || s.PR.Thread != f.GitHubCommentID {
		t.Errorf("spec.PR = %+v", s.PR)
	}
	if !cv.has(rig.gh, fmt.Sprintf("inline:%d:eyes", c.ID)) {
		t.Errorf("the command was not picked up visibly: %v", cv.reactions)
	}
	if runs := rig.replyRuns(); len(runs) != 0 {
		t.Errorf("the command was also queued as a reply: %+v", runs)
	}
	if n := len(rig.model.requests("reply")); n != 0 {
		t.Errorf("the command reached the verdict model %d times", n)
	}
}

// `@attesttag fix` on the conversation fixes the open findings of the severities named, and says so
// on the conversation; a severity with none open is told that, and nothing starts.
func TestReviewFixCommandOnTheConversation(t *testing.T) {
	rig, cv, _ := newFixRig(t, `{"mode":"live"}`)
	f := rig.finding()
	rig.deliver("issue_comment", commentEvent(901, "alice", "MEMBER", "@attesttag fix p2", true))
	if n := len(rig.fixJobs()); n != 0 {
		t.Fatalf("a fix of P2s started %d jobs with only a P1 open", n)
	}
	if a := rig.answers(); len(a) != 1 || !strings.Contains(a[0], "no open finding") {
		t.Errorf("answers = %q", a)
	}
	rig.deliver("issue_comment", commentEvent(902, "alice", "MEMBER", "@attesttag fix p1", true))
	jobs := rig.fixJobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	s := jobSpecOf(t, jobs[0])
	if s.PR.Thread != 0 || !slices.Equal(s.PR.Findings, []string{f.PublicID}) {
		t.Errorf("spec.PR = %+v", s.PR)
	}
	if !cv.has(rig.gh, "issue:902:eyes") {
		t.Errorf("the command was not picked up visibly: %v", cv.reactions)
	}
	if a := rig.answers(); len(a) != 2 || !strings.Contains(a[1], "fixing 1 finding") {
		t.Errorf("answers = %q", a)
	}
}

// Who and where: somebody who may only read is told once a day and nothing starts; a pull request
// from a fork, or a repository with fixes off, is refused in words; in shadow nothing is written at
// all, a box least.
func TestReviewFixRefusals(t *testing.T) {
	t.Run("no write access", func(t *testing.T) {
		rig, _, fd := newFixRig(t, `{"mode":"live"}`)
		f := rig.finding()
		rig.reply(f, "mallory", "MEMBER", "@attesttag fix")
		rig.reply(f, "mallory", "MEMBER", "@attesttag fix please")
		if len(fd.launches) != 0 || len(rig.fixJobs()) != 0 {
			t.Fatal("a fix started for somebody who may only read")
		}
		if a := rig.botAnswers(f); len(a) != 1 || !strings.Contains(a[0], "Only people who can push") {
			t.Errorf("answers = %q; want one refusal", a)
		}
	})
	t.Run("a fork", func(t *testing.T) {
		rig, _, _ := newFixRig(t, `{"mode":"live"}`)
		f := rig.finding()
		rig.gh.mu.Lock()
		rig.gh.headRepo = "mallory/web"
		rig.gh.mu.Unlock()
		rig.reply(f, "octocat", "OWNER", "@attesttag fix")
		if len(rig.fixJobs()) != 0 {
			t.Fatal("a fix started on a fork's branch")
		}
		if a := rig.botAnswers(f); len(a) != 1 || !strings.Contains(a[0], "comes from a fork") {
			t.Errorf("answers = %q", a)
		}
	})
	t.Run("fixes off", func(t *testing.T) {
		rig, _, _ := newFixRig(t, `{"mode":"live","fixes":false}`)
		f := rig.finding()
		if body := rig.findingComment(f).Body; strings.Contains(body, review.FixBoxText) {
			t.Errorf("a comment offers a fix the settings do not allow:\n%s", body)
		}
		rig.reply(f, "octocat", "OWNER", "@attesttag fix")
		if len(rig.fixJobs()) != 0 {
			t.Fatal("a fix started with fixes off")
		}
		if a := rig.botAnswers(f); len(a) != 1 || !strings.Contains(a[0], "turned off") {
			t.Errorf("answers = %q", a)
		}
	})
	t.Run("shadow", func(t *testing.T) {
		rig, cv, _ := newFixRig(t, `{"mode":"shadow"}`)
		rig.deliver("issue_comment", commentEvent(903, "octocat", "OWNER", "@attesttag fix", true))
		if len(rig.fixJobs()) != 0 {
			t.Fatal("a fix started on a repository in shadow")
		}
		if a := rig.answers(); len(a) != 0 || len(cv.reactions) != 0 {
			t.Errorf("shadow wrote to GitHub: answers %q, reactions %v", a, cv.reactions)
		}
	})
	t.Run("a finding no longer open", func(t *testing.T) {
		rig, _, _ := newFixRig(t, `{"mode":"live"}`)
		f := rig.finding()
		if err := rig.st.SetReviewFindingStatus(context.Background(), orgID, f.ID, review.FindingFixed, "test", "github:octocat"); err != nil {
			t.Fatal(err)
		}
		rig.reply(f, "octocat", "OWNER", "@attesttag fix")
		if len(rig.fixJobs()) != 0 {
			t.Fatal("a fix started for a finding already fixed")
		}
		if a := rig.botAnswers(f); len(a) != 1 || !strings.Contains(a[0], "already fixed") {
			t.Errorf("answers = %q", a)
		}
	})
}

// When the job ends it answers where it was asked. A push says which commit, what the checks said,
// and queues the review of that head that closes what it fixed; a change kept off the branch for
// breaking a check says so and shows its diff; either way the thread says how to try again.
func TestReviewFixEndAnswersOnThePullRequest(t *testing.T) {
	ctx := context.Background()
	t.Run("pushed", func(t *testing.T) {
		rig, _, _ := newFixRig(t, `{"mode":"live"}`)
		f := rig.finding()
		rig.deliver("pull_request_review_comment", rig.tickEvent(f, "octocat"))
		j := rig.fixJobs()[0]
		rig.gh.mu.Lock()
		rig.gh.head = reviewHeadC // the commit the job pushed
		rig.gh.mu.Unlock()
		res := &JobResult{Status: JobSucceeded, HeadSHA: reviewHeadC, Branch: "feature", Summary: "Took the lock in Add again.",
			Tests: JobTests{Command: "go test ./...", Before: JobTestRun{Ran: true, OK: true, Passed: 3}, After: JobTestRun{Ran: true, OK: true, Passed: 4}}}
		rig.b.jobs.finish(ctx, orgID, j.ID, JobSucceeded, res)
		a := rig.botAnswers(f)
		if len(a) != 2 {
			t.Fatalf("answers = %q", a)
		}
		for _, want := range []string{"Pushed [`ccccccc`](https://github.com/acme/web/commit/" + reviewHeadC + ")", "to `feature`",
			"Took the lock in Add again.", "tests passed (4)", "fix job #1", "octocat"} {
			if !strings.Contains(a[1], want) {
				t.Errorf("the answer lacks %q:\n%s", want, a[1])
			}
		}
		var fix *ReviewRun
		for _, r := range rig.runs(7) {
			if r.Trigger == "fix" {
				fix = r
			}
		}
		if fix == nil || fix.HeadSHA != reviewHeadC || fix.TriggerRef != fmt.Sprintf("job:%d", j.ID) || fix.RequestedBy != "github:octocat" {
			t.Errorf("the review of the pushed head = %+v", fix)
		}
		if got := rig.fixAudits(); !slices.Equal(got, []string{"started", "job succeeded"}) {
			t.Errorf("audits = %q", got)
		}
	})
	t.Run("kept off the branch", func(t *testing.T) {
		rig, _, _ := newFixRig(t, `{"mode":"live"}`)
		f := rig.finding()
		rig.deliver("pull_request_review_comment", rig.tickEvent(f, "octocat"))
		j := rig.fixJobs()[0]
		diff := "--- a/totals.go\n+++ b/totals.go\n@@ -1 +1 @@\n-x\n+y\n"
		if err := rig.st.PutJobFile(ctx, orgID, j.ID, "diff", diff); err != nil {
			t.Fatal(err)
		}
		msg := "the change broke the tests, which passed before it, so it was not pushed to feature"
		rig.b.jobs.finish(ctx, orgID, j.ID, JobFailed, &JobResult{Error: JobError{Code: "checks_broken", Message: msg}})
		a := rig.botAnswers(f)
		if len(a) != 2 {
			t.Fatalf("answers = %q", a)
		}
		for _, want := range []string{"could not fix this finding: " + msg, "<details>", "+y", "Tick the box again", "`@attesttag fix`"} {
			if !strings.Contains(a[1], want) {
				t.Errorf("the answer lacks %q:\n%s", want, a[1])
			}
		}
		for _, r := range rig.runs(7) {
			if r.Trigger == "fix" {
				t.Errorf("a review was queued after nothing was pushed: %+v", r)
			}
		}
	})
}
