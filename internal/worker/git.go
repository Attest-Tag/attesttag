package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"attesttag/internal/app"
)

// stepError names which step failed, in the vocabulary of the result's error code.
type stepError struct {
	code string
	msg  string
}

func (e *stepError) Error() string { return e.msg }

func stepErr(code, msg string) error { return &stepError{code: code, msg: msg} }

// Repo is the clone. Git runs with the job's base environment; the repository token is added
// only for clone and push, as an HTTP header via GIT_CONFIG_* (what actions/checkout does): it is
// never in argv, in the remote URL or in .git/config.
type Repo struct {
	dir  string
	env  []string
	auth []string
	name string
	// sandbox runs git as the sandbox user once the repository has been handed over to it. After
	// grantSandbox chowns the clone (its .git/config included) to that user, the repository's own
	// code can write .git/config; if the worker then ran git as root, a planted core.fsmonitor or
	// filter driver would execute as root (a sandbox escape). Running the post-handover git —
	// stage, diff, commit, push — as the same sandbox user keeps any such execution inside the
	// sandbox, where the repository's code already runs. Clone and checkout happen before the
	// handover, on a root-owned clone, so they stay root.
	sandbox bool
}

func gitAuthEnv(token string) []string {
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + b64}
}

// gitPins is on every git command line. The repository's contents are untrusted, and the tests
// and the engine run inside it: a planted hook must never run with the repository token in the
// environment, and no git operation may reach for a file:// or ext:: transport a repository
// could name. HOME is also the job's own, and GIT_CONFIG_GLOBAL points it at nothing, so a
// ~/.gitconfig written by a test script changes nothing either. safe.directory covers the
// worker committing files the sandbox user wrote.
//
// The last three defend the token git carries on a networked command. Once the clone is handed to
// the sandbox user, its own code can write .git/config, and these -c flags win over what it
// writes there: core.fsmonitor= stops a planted monitor program running on `git status`;
// credential.helper= stops a planted helper being executed (and reading the token from its own
// environment) during push; http.sslVerify=true stops a planted http.<url>.proxy from being an
// intercepting proxy that could read the authenticated push. A cross-host url.insteadOf redirect
// cannot steal the token because the auth header is scoped to https://github.com/.
var gitPins = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "protocol.file.allow=user",
	"-c", "protocol.ext.allow=never",
	"-c", "safe.directory=*",
	"-c", "core.fsmonitor=",
	"-c", "credential.helper=",
	"-c", "http.sslVerify=true",
	// gpg.program runs on `git commit` when a planted commit.gpgSign=true is in the
	// sandbox-owned .git/config, so pin the signer off and empty its program the same way the
	// others are pinned: a repository must not choose the program a commit runs.
	"-c", "commit.gpgSign=false",
	"-c", "gpg.program=",
}

func (g *Repo) git(ctx context.Context, timeout time.Duration, network bool, args ...string) (procOut, error) {
	env := g.env
	if network {
		env = append(append([]string{}, env...), g.auth...)
	}
	return runCmd(ctx, cmdSpec{Dir: g.dir, Argv: append(append([]string{"git"}, gitPins...), args...), Env: env, Timeout: timeout, Sandbox: g.sandbox})
}

// cloneRepo clones the base branch shallowly into ws.RepoDir. cloneURL overrides the GitHub
// URL (tests clone from a local bare repository).
//
// Tags come with it, and submodules after it. Both used to be left out, and both broke whole
// classes of repository quietly: a build that derives its version from tags (setuptools_scm,
// hatch-vcs, most JVM release plugins) fails at install time with a message about git, and a
// repository with a submodule presents the engine with an empty directory where its code should
// be. The extra objects are cheap next to being wrong.
func cloneRepo(ctx context.Context, ws *Workspace, spec app.JobSpec, token, cloneURL string) (*Repo, error) {
	g := &Repo{dir: ws.RepoDir, env: ws.Env, auth: gitAuthEnv(token), name: spec.Repo}
	if cloneURL == "" {
		cloneURL = "https://github.com/" + spec.Repo + ".git"
	}
	env := append(append([]string{}, ws.Env...), g.auth...)
	args := append([]string{"clone", "-q", "--depth", "50", "--branch", spec.BaseBranch, "--single-branch"}, cloneURL, filepath.Base(ws.RepoDir))
	out, err := runCmd(ctx, cmdSpec{Dir: filepath.Dir(ws.RepoDir), Env: env, Timeout: 5 * time.Minute,
		Argv: append(append([]string{"git"}, gitPins...), args...)})
	if err != nil || out.Code != 0 {
		msg := lastLines(out.Output, 1500)
		if strings.Contains(msg, "Remote branch") && strings.Contains(msg, "not found") {
			return nil, stepErr("base_branch_missing", fmt.Sprintf("branch %s does not exist on %s", spec.BaseBranch, spec.Repo))
		}
		if err != nil && msg == "" {
			msg = err.Error()
		}
		return nil, stepErr("clone_failed", "git clone failed: "+msg)
	}
	g.git(ctx, 10*time.Second, false, "config", "user.name", "attest_tag")
	g.git(ctx, 10*time.Second, false, "config", "user.email", "attest-tag@users.noreply.github.com")
	// Best-effort from here: a repository without submodules, LFS objects or tags is the common
	// case, and none of these failing is a reason not to do the work.
	if _, err := os.Stat(filepath.Join(ws.RepoDir, ".gitmodules")); err == nil {
		if out, _ := g.gitNet(ctx, 4*time.Minute, "submodule", "update", "--init", "--recursive", "--depth", "1"); out.Code != 0 {
			ws.Reporter.Warn("submodules were not checked out; code under them is missing from this clone")
		}
	}
	if raw, err := os.ReadFile(filepath.Join(ws.RepoDir, ".gitattributes")); err == nil && strings.Contains(string(raw), "filter=lfs") {
		if out, _ := g.gitNet(ctx, 4*time.Minute, "lfs", "pull"); out.Code != 0 {
			ws.Reporter.Warn("git-lfs objects were not fetched; large files in this clone are pointers")
		}
	}
	// Tags, shallow, for the builds that read a version out of them.
	g.gitNet(ctx, 90*time.Second, "fetch", "-q", "--depth", "50", "--tags", "origin")
	return g, nil
}

// gitNet is a git command that reaches the remote, and so carries the repository token.
func (g *Repo) gitNet(ctx context.Context, timeout time.Duration, args ...string) (procOut, error) {
	return g.git(ctx, timeout, true, args...)
}

func (g *Repo) checkoutBranch(ctx context.Context, branch string) error {
	out, err := g.git(ctx, 30*time.Second, false, "checkout", "-q", "-b", branch)
	if err != nil || out.Code != 0 {
		return stepErr("clone_failed", "git checkout -b failed: "+lastLines(out.Output, 500))
	}
	return nil
}

func (g *Repo) headSHA(ctx context.Context) string {
	out, _ := g.git(ctx, 10*time.Second, false, "rev-parse", "HEAD")
	return strings.TrimSpace(out.Output)
}

func (g *Repo) currentBranch(ctx context.Context) string {
	out, _ := g.git(ctx, 10*time.Second, false, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out.Output)
}

// Files the worker never commits, whatever the engine did.
var excludedPath = regexp.MustCompile(`(?i)(^|/)(\.env[^/]*|[^/]*\.pem|[^/]*\.key|[^/]*\.p12|id_rsa[^/]*|id_ed25519[^/]*)$|^\.git/|^\.github/`)

// buildOutput is where compilers and package managers put things, and it is never a change
// somebody asked for. This only applies to files git does not already track: the job runs the
// repository's build twice, so on a repository without a .gitignore for its own artifacts —
// which is most repositories in some ecosystems, and any repository whose ignore file predates
// the tool that made these — `git add` would otherwise sweep the whole of target/ or
// node_modules/ into the pull request. A tracked file is the repository's business and is
// committed as normal, including one inside these directories.
var buildOutput = regexp.MustCompile(`(?i)(^|/)(target|build|dist|out|bin|obj|node_modules|__pycache__|\.venv|venv|\.tox|\.gradle|\.next|\.nuxt|\.pytest_cache|\.mypy_cache|\.ruff_cache|vendor|Pods|DerivedData|\.dart_tool|_build|deps|\.terraform|coverage|htmlcov)/|\.(pyc|pyo|class|o|obj|so|dylib|dll|exe|jar|war|nupkg|beam)$`)

// generatedLock is a lockfile the job's own dependency install wrote. Same tracked/untracked
// rule as buildOutput and the same reason: `uv sync` and `npm install` write one whether or not
// the repository keeps it, and a repository that does not keep it did not ask for one in a pull
// request about something else. A lockfile the repository does track is a tracked file, so a
// dependency the change genuinely needed still lands in the commit.
var generatedLock = regexp.MustCompile(`(?i)^(.*/)?(\.?mise\.toml|\.tool-versions\.bak|uv\.lock|poetry\.lock|Pipfile\.lock|package-lock\.json|npm-shrinkwrap\.json|pnpm-lock\.yaml|yarn\.lock|bun\.lockb?|Cargo\.lock|Gemfile\.lock|composer\.lock|packages\.lock\.json|mix\.lock|pubspec\.lock|gradle\.lockfile)$`)

const maxFileBytes = 5 << 20

// stage adds every change except the excluded paths and files over 5 MB, and reports both lists.
func (g *Repo) stage(ctx context.Context) (files, skipped []string, err error) {
	artifacts := 0
	out, err := g.git(ctx, 60*time.Second, false, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || out.Code != 0 {
		return nil, nil, stepErr("engine_error", "git status failed: "+lastLines(out.Output, 300))
	}
	entries := strings.Split(out.Output, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		path := e[3:]
		untracked := e[0] == '?' && e[1] == '?'
		if e[0] == 'R' || e[0] == 'C' { // "R  new" followed by the old path as its own entry
			i++
		}
		if excludedPath.MatchString(path) {
			skipped = append(skipped, path)
			continue
		}
		if untracked && (buildOutput.MatchString(path) || generatedLock.MatchString(path)) {
			artifacts++
			continue
		}
		if st, err := os.Stat(filepath.Join(g.dir, path)); err == nil && st.Size() > maxFileBytes {
			skipped = append(skipped, path)
			continue
		}
		files = append(files, path)
	}
	for i := 0; i < len(files); i += 100 {
		end := i + 100
		if end > len(files) {
			end = len(files)
		}
		if out, err := g.git(ctx, 60*time.Second, false, append([]string{"add", "--"}, files[i:end]...)...); err != nil || out.Code != 0 {
			return nil, nil, stepErr("engine_error", "git add failed: "+lastLines(out.Output, 300))
		}
	}
	if artifacts > 0 {
		slog.Info("build output and generated lockfiles left out of the commit", "files", artifacts)
	}
	return files, skipped, nil
}

// diffStat reads the staged change as files / insertions / deletions.
func (g *Repo) diffStat(ctx context.Context) app.JobDiffStat {
	out, _ := g.git(ctx, 60*time.Second, false, "diff", "--cached", "--numstat")
	var st app.JobDiffStat
	for _, line := range strings.Split(out.Output, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		st.Files++
		if n, err := strconv.Atoi(f[0]); err == nil {
			st.Insertions += n
		}
		if n, err := strconv.Atoi(f[1]); err == nil {
			st.Deletions += n
		}
	}
	return st
}

// commit runs after the clone is handed to the sandbox user, so it runs as that user
// (Sandbox: g.sandbox) — a config key the repository's own code planted in .git/config then
// executes inside the sandbox rather than as root, the invariant g.git keeps for every other
// post-handover call. This one does not go through g.git only because it feeds the message on
// stdin, so it carries the same gitPins by hand.
func (g *Repo) commit(ctx context.Context, msg string) error {
	out, err := runCmd(ctx, cmdSpec{Dir: g.dir, Env: g.env, Timeout: 60 * time.Second, Stdin: msg, Sandbox: g.sandbox,
		Argv: append(append([]string{"git"}, gitPins...), "commit", "-q", "--no-verify", "-F", "-")})
	if err != nil || out.Code != 0 {
		return stepErr("engine_error", "git commit failed: "+lastLines(out.Output, 500))
	}
	return nil
}

// diff is the change since base, or "" when it is over the size the bot accepts. Like commit it
// runs as the sandbox user with the pins (it does not use g.git only because of the head/tail
// caps); --no-ext-diff and --no-textconv keep a repository-planted diff driver from being run at
// all, on top of the sandboxing.
func (g *Repo) diff(ctx context.Context, base string, max int) string {
	out, _ := runCmd(ctx, cmdSpec{Dir: g.dir, Env: g.env, Timeout: 60 * time.Second, Head: max + 1, Tail: 1, Sandbox: g.sandbox,
		Argv: append(append([]string{"git"}, gitPins...), "diff", "--no-color", "--no-ext-diff", "--no-textconv", base+"..HEAD")})
	if len(out.Output) > max {
		return ""
	}
	return out.Output
}

// push sends HEAD to the one branch the job may write. It refuses anything else, never forces,
// and retries once on a network error.
func (g *Repo) push(ctx context.Context, branch, base, prefix, suffix string) error {
	switch {
	case branch == "" || branch == base:
		return stepErr("push_failed", "refusing to push to the base branch")
	case prefix != "" && !strings.HasPrefix(branch, prefix):
		return stepErr("push_failed", "refusing to push a branch outside "+prefix)
	case suffix != "" && !strings.HasSuffix(branch, suffix):
		return stepErr("push_failed", "refusing to push a branch that does not end in "+suffix)
	case g.currentBranch(ctx) != branch:
		return stepErr("push_failed", "HEAD is not on "+branch)
	}
	var out procOut
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		out, err = g.git(ctx, 3*time.Minute, true, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
		if err == nil && out.Code == 0 {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		msg := strings.ToLower(out.Output)
		if !strings.Contains(msg, "could not resolve") && !strings.Contains(msg, "timed out") && !strings.Contains(msg, "connection") {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil && out.Output == "" {
		return stepErr("push_failed", "git push failed: "+err.Error())
	}
	return stepErr("push_failed", "git push failed: "+lastLines(out.Output, 800))
}
