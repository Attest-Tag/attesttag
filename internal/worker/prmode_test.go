package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"attesttag/internal/app"
)

// seedBranch adds branch to origin, one commit on top of main, as a pull request's head would be,
// and returns that commit.
func seedBranch(t *testing.T, origin, branch string) string {
	t.Helper()
	scratch := t.TempDir()
	gitCmd(t, scratch, "clone", "-q", origin, ".")
	gitCmd(t, scratch, "checkout", "-q", "-b", branch)
	os.WriteFile(filepath.Join(scratch, "feature.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, scratch, "add", "-A")
	gitCmd(t, scratch, "commit", "-q", "-m", "feature")
	gitCmd(t, scratch, "push", "-q", "origin", branch)
	return gitCmd(t, scratch, "rev-parse", "HEAD")
}

// pushToBranch is somebody else pushing to branch: file gets one more line, committed on top of
// what origin holds now. It returns the new head.
func pushToBranch(t *testing.T, origin, branch, file, line string) string {
	t.Helper()
	scratch := t.TempDir()
	gitCmd(t, scratch, "clone", "-q", "--branch", branch, origin, ".")
	f, err := os.OpenFile(filepath.Join(scratch, file), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(line + "\n")
	f.Close()
	gitCmd(t, scratch, "add", "-A")
	gitCmd(t, scratch, "commit", "-q", "-m", "somebody else")
	gitCmd(t, scratch, "push", "-q", "origin", branch)
	return gitCmd(t, scratch, "rev-parse", "HEAD")
}

// prClaim is a job asked for on pull request #7, whose head branch is feature at head.
func prClaim(head string) app.JobClaim {
	c := testClaim("fake")
	s := &c.Job.Spec
	s.Mode, s.BaseBranch, s.Branch, s.HeadSHA = app.JobModePR, "feature", "feature", head
	s.PR = &app.JobPRRef{Number: 7, Base: "main", URL: "https://github.com/local/test/pull/7", InstallationID: 1, AskedBy: "alice"}
	return c
}

// A job asked for on a pull request ends as one more commit on the pull request's own branch, on
// top of the head it was asked for at: nothing else moves, and no pull request is opened.
func TestRunnerPRModePushesOntoThePullRequestsBranch(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	origin := seedRepo(t, "build:\n\t@echo built\ntest:\n\t@echo \"1 passed\"\n")
	head := seedBranch(t, origin, "feature")
	main := gitCmd(t, origin, "rev-parse", "main")
	fb := &fakeBot{claim: prClaim(head)}
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	res := fb.result
	if res == nil || res.Status != app.JobSucceeded || res.Branch != "feature" || res.PR != nil || res.HeadSHA == "" {
		t.Fatalf("result: %+v", res)
	}
	if got := gitCmd(t, origin, "rev-parse", "feature"); got != res.HeadSHA {
		t.Errorf("feature is at %s, the job says it pushed %s", got, res.HeadSHA)
	}
	if parent := gitCmd(t, origin, "rev-parse", res.HeadSHA+"^"); parent != head {
		t.Errorf("the commit's parent is %s, want the pull request's head %s", parent, head)
	}
	if got := gitCmd(t, origin, "rev-parse", "main"); got != main {
		t.Error("main moved")
	}
	if msg := gitCmd(t, origin, "log", "-1", "--format=%B", res.HeadSHA); !strings.Contains(msg, "asked for by @alice") {
		t.Errorf("the commit does not say who asked: %q", msg)
	}
	if len(gh.bodies) != 0 {
		t.Errorf("a pull request was opened: %v", gh.bodies)
	}
	if ph := fb.phases(); ph["push"] != "ok" || ph["pr"] != "skipped" || ph["clone"] != "ok" {
		t.Errorf("phases: %v", ph)
	}
}

// A change that breaks a check that passed before it stays off the branch somebody is working on:
// the job fails saying which, and its diff still reaches the bot for whoever asked to read.
func TestRunnerPRModeKeepsABreakingChangeOffTheBranch(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	// Passes until the fake engine's marker is in README.md.
	origin := seedRepo(t, "test:\n\t@if grep -q 'attest_tag fix job' README.md; then echo \"1 failed\"; exit 1; else echo \"1 passed\"; fi\n")
	head := seedBranch(t, origin, "feature")
	fb := &fakeBot{claim: prClaim(head)}
	r := newTestRunner(t, fb, &fakeGitHub{}, origin)
	r.Run(context.Background())
	res := fb.result
	if res == nil || res.Status != app.JobFailed || res.Error.Code != "checks_broken" || !strings.Contains(res.Error.Message, "tests") {
		t.Fatalf("result: %+v", res)
	}
	if got := gitCmd(t, origin, "rev-parse", "feature"); got != head {
		t.Errorf("feature moved to %s", got)
	}
	if !strings.Contains(fb.diff, "attest_tag fix job") {
		t.Errorf("the diff did not reach the bot: %q", fb.diff)
	}
	if ph := fb.phases(); ph["push"] != "skipped" {
		t.Errorf("phases: %v", ph)
	}
}

// prRepo is a clone of origin's feature branch with one commit of the job's on it — README.md gets
// a line — and the head it started from.
func prRepo(t *testing.T, origin string) (*Repo, string) {
	t.Helper()
	ctx := context.Background()
	jobDir := t.TempDir()
	for _, d := range []string{"home", "tmp", ".cache"} {
		os.MkdirAll(filepath.Join(jobDir, d), 0o700)
	}
	ws := &Workspace{JobDir: jobDir, RepoDir: filepath.Join(jobDir, "repo"), Env: baseEnv(jobDir, jobDir)}
	repo, err := cloneRepo(ctx, ws, app.JobSpec{Repo: "local/test", BaseBranch: "feature"}, "t", "file://"+origin)
	if err != nil {
		t.Fatal(err)
	}
	start := repo.headSHA(ctx)
	f, _ := os.OpenFile(filepath.Join(ws.RepoDir, "README.md"), os.O_WRONLY|os.O_APPEND, 0o644)
	f.WriteString("the job's line\n")
	f.Close()
	if _, _, err := repo.stage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repo.commit(ctx, "the job's change"); err != nil {
		t.Fatal(err)
	}
	return repo, start
}

// Somebody pushed to the branch while the job worked: the push is refused as not a fast-forward,
// the job's commit is replayed once onto what is there now, and pushed — never forced, so their
// commit stays.
func TestPushOntoReplaysOntoWhatWasPushedMeanwhile(t *testing.T) {
	origin := seedRepo(t, "")
	seedBranch(t, origin, "feature")
	repo, start := prRepo(t, origin)
	theirs := pushToBranch(t, origin, "feature", "feature.txt", "their line")
	moved, err := repo.pushOnto(context.Background(), "feature", "main", start)
	if err != nil {
		t.Fatalf("pushOnto: %v", err)
	}
	if moved != theirs {
		t.Errorf("moved = %q, want their commit %s", moved, theirs)
	}
	tip := gitCmd(t, origin, "rev-parse", "feature")
	if parent := gitCmd(t, origin, "rev-parse", tip+"^"); parent != theirs {
		t.Errorf("the job's commit sits on %s, not on their %s", parent, theirs)
	}
	if got := gitCmd(t, origin, "show", "feature:README.md"); !strings.Contains(got, "the job's line") {
		t.Errorf("README at the tip: %q", got)
	}
	if got := gitCmd(t, origin, "show", "feature:feature.txt"); !strings.Contains(got, "their line") {
		t.Errorf("their change was lost: %q", got)
	}
}

// A replay that conflicts is abandoned: nothing is pushed, their commit is the tip, and the error
// says the branch moved.
func TestPushOntoGivesUpOnAConflict(t *testing.T) {
	origin := seedRepo(t, "")
	seedBranch(t, origin, "feature")
	repo, start := prRepo(t, origin)
	theirs := pushToBranch(t, origin, "feature", "README.md", "their different line")
	_, err := repo.pushOnto(context.Background(), "feature", "main", start)
	var se *stepError
	if !errors.As(err, &se) || se.code != "branch_moved" {
		t.Fatalf("err = %v, want branch_moved", err)
	}
	if tip := gitCmd(t, origin, "rev-parse", "feature"); tip != theirs {
		t.Errorf("feature is at %s, want their %s", tip, theirs)
	}
}

// The push goes nowhere but the branch named, and only with HEAD building on the head the job began
// from: not the branch the pull request merges into, and not after the engine moved HEAD elsewhere.
func TestPushOntoRefusesAnyOtherTarget(t *testing.T) {
	origin := seedRepo(t, "")
	seedBranch(t, origin, "feature")
	repo, start := prRepo(t, origin)
	main := gitCmd(t, origin, "rev-parse", "main")
	ctx := context.Background()
	for _, c := range []struct {
		name, branch, base, start string
	}{
		{"the base branch", "main", "main", start},
		{"no branch", "", "main", start},
		{"another branch than HEAD's", "other", "main", start},
		{"HEAD not on the head it began from", "feature", "main", "0123456789012345678901234567890123456789"},
	} {
		if _, err := repo.pushOnto(ctx, c.branch, c.base, c.start); err == nil {
			t.Errorf("%s: pushed", c.name)
		}
	}
	if got := gitCmd(t, origin, "rev-parse", "main"); got != main {
		t.Error("main moved")
	}
}
