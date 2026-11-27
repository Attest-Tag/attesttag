package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	dir       string
	pushDir   string // root-owned clean bare copy of the initial .git, never handed to the sandbox
	remoteURL string // canonical clone/push target, never read back from sandbox-writable .git/config
	baseSHA   string
	env       []string
	auth      []string
	name      string
	// sandbox runs git as the sandbox user once the repository has been handed over to it. After
	// grantSandbox chowns the clone (its .git/config included) to that user, the repository's own
	// code can write .git/config; if the worker then ran git as root, a planted core.fsmonitor or
	// filter driver would execute as root (a sandbox escape). Running the post-handover git —
	// stage, diff, commit — as the same sandbox user keeps any such execution inside the sandbox,
	// where the repository's code already runs. Clone, checkout and the final push from the
	// root-owned pushDir never read the sandbox's .git/config, so the repository token never
	// enters a sandbox-owned process or config.
	sandbox bool
}

func gitAuthB64(token string) string {
	if token == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// gitAuthEnv scopes the token's header to https://github.com/, so a url.insteadOf that sends a
// fetch to another host sends it without the token. It is not narrowed to the job's repository:
// a private submodule under the same owner needs it too, and github.com shows no repository's
// owner the headers a clone arrived with.
func gitAuthEnv(token string) []string {
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + gitAuthB64(token)}
}

// gitPins is on every git command line. The repository's contents are untrusted, and the tests
// and the engine run inside it: a planted hook must never run with the repository token in the
// environment, and no git operation may reach for a file://, ssh:// or ext:: transport a
// repository could name. HOME is also the job's own, and GIT_CONFIG_GLOBAL points it at nothing,
// so a ~/.gitconfig written by a test script changes nothing either. safe.directory covers the
// worker committing files the sandbox user wrote.
//
// The token never meets the working clone's config: the clone and its fetches happen before the
// sandbox user owns it, and every push, and every read of a branch that moved, runs in the clean
// copy (pushViaCleanRepo). The http pins are on regardless. Proxy and CA settings are not pinned:
// an empty http.sslCAInfo or http.sslCAPath makes every https remote fail certificate checks, and
// an empty http.proxy turns off the HTTPS_PROXY a self-host behind an egress proxy depends on.
var gitPins = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "protocol.file.allow=user",
	"-c", "protocol.ext.allow=never",
	"-c", "protocol.ssh.allow=never",
	"-c", "core.sshCommand=",
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
	if cloneURL == "" {
		cloneURL = "https://github.com/" + spec.Repo + ".git"
	}
	g := &Repo{dir: ws.RepoDir, remoteURL: cloneURL, env: ws.Env, auth: gitAuthEnv(token), name: spec.Repo}
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
	if _, err := os.Lstat(filepath.Join(ws.RepoDir, ".gitmodules")); err == nil {
		// Disallow file:// and ssh:// inside .gitmodules so a repo cannot read local files
		// or execute ssh commands while fetching submodules.
		if out, _ := g.gitNet(ctx, 4*time.Minute, "-c", "protocol.file.allow=never", "submodule", "update", "--init", "--recursive", "--depth", "1"); out.Code != 0 {
			ws.Reporter.Warn("submodules were not checked out; code under them is missing from this clone")
		}
	}
	if raw, err := readRepoFile(ws.RepoDir, ".gitattributes", 256<<10); err == nil && strings.Contains(string(raw), "filter=lfs") {
		if out, _ := g.gitNet(ctx, 4*time.Minute, "-c", "protocol.file.allow=never", "lfs", "pull"); out.Code != 0 {
			ws.Reporter.Warn("git-lfs objects were not fetched; large files in this clone are pointers")
		}
	}
	// Tags, shallow, for the builds that read a version out of them.
	g.gitNet(ctx, 90*time.Second, "fetch", "-q", "--depth", "50", "--tags", "origin")
	return g, nil
}

// snapshotPushRepo copies the clean root-owned .git directory into a separate root-owned bare
// directory before grantSandbox hands the working tree to the sandbox user. push() imports the
// final commit via a delta bundle into this clean repo and pushes from here, so the GitHub token
// never runs inside a sandbox-owned process or against a sandbox-writable .git/config.
func (g *Repo) snapshotPushRepo(ctx context.Context, pushDir, baseSHA string) error {
	os.RemoveAll(pushDir)
	// LFS content and the submodules' own repositories stay behind: pushing the job's commit needs
	// neither, and on Cloud Run the copy counts against the job's memory.
	if err := copyDirNoSymlinks(filepath.Join(g.dir, ".git"), pushDir, "lfs", "modules"); err != nil {
		return err
	}
	out, err := runCmd(ctx, cmdSpec{Dir: pushDir, Env: g.env, Timeout: 15 * time.Second,
		Argv: append(append([]string{"git"}, gitPins...), "config", "--bool", "core.bare", "true")})
	if err != nil || out.Code != 0 {
		return fmt.Errorf("prepare push repo: %s", lastLines(out.Output, 300))
	}
	g.pushDir = pushDir
	g.baseSHA = baseSHA
	return nil
}

func copyDirNoSymlinks(src, dst string, skipTop ...string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() && slices.Contains(skipTop, rel) {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		cerr := out.Close()
		if err != nil {
			return err
		}
		return cerr
	})
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

// stage resets the index (and any commits the sandbox made on top of baseSHA) so pre-staged or
// pre-committed excluded files cannot slip past the filter, then adds every allowed change under
// 5 MB and reports both lists.
func (g *Repo) stage(ctx context.Context) (files, skipped []string, err error) {
	resetTarget := "HEAD"
	if g.baseSHA != "" {
		resetTarget = g.baseSHA
	}
	if out, err := g.git(ctx, 60*time.Second, false, "reset", "-q", "--mixed", resetTarget); err != nil || out.Code != 0 {
		return nil, nil, stepErr("engine_error", "git reset failed: "+lastLines(out.Output, 300))
	}
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
		if st, err := os.Lstat(filepath.Join(g.dir, path)); err == nil && st.Size() > maxFileBytes {
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
	_, err := g.pushHead(ctx, branch, g.baseSHA)
	return err
}

// pushTarget is where a push, or a fresh read of the branch, goes: the URL the clone was made
// from, never what the working clone's .git/config now calls origin — the sandbox user owns that
// file and can point origin at another repository.
func (g *Repo) pushTarget() string {
	if g.remoteURL != "" {
		return g.remoteURL
	}
	return "origin"
}

// pushHead pushes HEAD to branch, never forced, retrying once on a network error. It returns git's
// output alongside the error, for a caller that needs to tell a refused fast-forward apart.
//
// Once the clone has been handed to the sandbox user the push goes out from the clean copy
// instead (pushViaCleanRepo), and parent is the commit HEAD has to sit directly on.
func (g *Repo) pushHead(ctx context.Context, branch, parent string) (procOut, error) {
	target := g.pushTarget()
	if g.pushDir != "" && g.baseSHA != "" {
		return g.pushViaCleanRepo(ctx, target, branch, parent)
	}
	return pushRetrying(ctx, func() (procOut, error) {
		return g.git(ctx, 3*time.Minute, true, "push", "-q", target, "HEAD:refs/heads/"+branch)
	})
}

// pushRetrying runs a push, and runs it once more when the first attempt failed on the network
// rather than being refused.
func pushRetrying(ctx context.Context, push func() (procOut, error)) (procOut, error) {
	var out procOut
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		out, err = push()
		if err == nil && out.Code == 0 {
			return out, nil
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
		return out, stepErr("push_failed", "git push failed: "+err.Error())
	}
	return out, stepErr("push_failed", "git push failed: "+lastLines(out.Output, 800))
}

// runClean runs git as root in the clean copy. Nothing the repository wrote is in its config, so a
// command there may carry the token where the same command in the working clone may not.
func (g *Repo) runClean(ctx context.Context, timeout time.Duration, network bool, args ...string) (procOut, error) {
	env := g.env
	if network {
		env = append(append([]string{}, env...), g.auth...)
	}
	return runCmd(ctx, cmdSpec{Dir: g.pushDir, Argv: append(append([]string{"git"}, gitPins...), args...), Env: env, Timeout: timeout, Sandbox: false})
}

// maxVerifyBytes bounds what runCleanAll reads back: a listing of every changed path, or of the
// whole tree, for the checks before a push.
const maxVerifyBytes = 8 << 20

// runCleanAll is runClean reading the whole output rather than its head and tail, since the checks
// before a push must see every line; output past maxVerifyBytes is refused, never cut.
func (g *Repo) runCleanAll(ctx context.Context, timeout time.Duration, args ...string) (procOut, error) {
	out, err := runCmd(ctx, cmdSpec{Dir: g.pushDir, Argv: append(append([]string{"git"}, gitPins...), args...), Env: g.env,
		Timeout: timeout, Head: maxVerifyBytes + 1, Tail: 1, Sandbox: false})
	if err == nil && len(out.Output) > maxVerifyBytes {
		return out, fmt.Errorf("the listing is over %d bytes", maxVerifyBytes)
	}
	return out, err
}

// pushViaCleanRepo carries the job's commit out of the sandbox-owned clone in a git bundle — made
// without the repository token — stops whatever the sandbox left running, and pushes from the
// root-owned clean copy, once it has checked there that the commit sits directly on parent and
// touches neither an excluded path nor a file over the size limit.
func (g *Repo) pushViaCleanRepo(ctx context.Context, target, branch, parent string) (procOut, error) {
	if parent == "" {
		return procOut{}, stepErr("push_failed", "refusing to push: the commit the change builds on is unknown")
	}
	bundlePath := filepath.Join(filepath.Dir(g.dir), "tmp", "push.bundle")
	defer os.Remove(bundlePath)
	ref := "refs/heads/" + branch
	if out, err := g.git(ctx, 2*time.Minute, false, "bundle", "create", bundlePath, parent+".."+ref); err != nil || out.Code != 0 {
		return procOut{}, stepErr("push_failed", "git bundle failed: "+lastLines(out.Output, 400))
	}
	killSandboxProcesses()
	// Forced here only, in the private copy: after a replay onto a moved branch the local ref no
	// longer fast-forwards from what the copy was made with. The push itself never forces.
	if out, err := g.runClean(ctx, 2*time.Minute, false, "fetch", "-q", bundlePath, "+"+ref+":"+ref); err != nil || out.Code != 0 {
		return procOut{}, stepErr("push_failed", "git fetch bundle failed: "+lastLines(out.Output, 400))
	}
	if out, err := g.runClean(ctx, 15*time.Second, false, "rev-parse", ref+"^"); err != nil || out.Code != 0 || strings.TrimSpace(out.Output) != parent {
		return procOut{}, stepErr("push_failed", "refusing to push: the commit does not sit directly on "+cut(parent, 12))
	}
	// Every path the commit touches, checked again where the sandbox cannot reach: it could have
	// staged or committed anything in its own clone. --no-renames lists a renamed file's old path
	// as well as its new one.
	diffOut, err := g.runCleanAll(ctx, 30*time.Second, "diff", "--name-only", "--no-renames", "-z", parent+".."+ref)
	if err != nil || diffOut.Code != 0 {
		return procOut{}, stepErr("push_failed", "git diff verify failed: "+lastLines(diffOut.Output, 400))
	}
	changed := map[string]bool{}
	for _, p := range strings.Split(diffOut.Output, "\x00") {
		if p == "" {
			continue
		}
		if excludedPath.MatchString(p) {
			return procOut{}, stepErr("push_failed", "refusing to push excluded path "+p)
		}
		changed[p] = true
	}
	if len(changed) == 0 {
		return procOut{}, stepErr("push_failed", "refusing to push an empty commit")
	}
	treeOut, err := g.runCleanAll(ctx, 30*time.Second, "ls-tree", "-r", "-l", "-z", ref)
	if err != nil || treeOut.Code != 0 {
		return procOut{}, stepErr("push_failed", "git ls-tree verify failed: "+lastLines(treeOut.Output, 400))
	}
	for _, rec := range strings.Split(treeOut.Output, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok || !changed[path] {
			continue
		}
		if fields := strings.Fields(meta); len(fields) >= 4 {
			if sz, err := strconv.ParseInt(fields[3], 10, 64); err == nil && sz > maxFileBytes {
				return procOut{}, stepErr("push_failed", fmt.Sprintf("refusing to push %s: exceeds %d bytes", path, maxFileBytes))
			}
		}
	}
	return pushRetrying(ctx, func() (procOut, error) {
		return g.runClean(ctx, 3*time.Minute, true, "push", "-q", target, ref+":"+ref)
	})
}

// notFastForward reports whether git refused a push because the branch has commits HEAD does not:
// somebody pushed while the job worked. A protected branch's refusal reads differently and is not
// one — no replay makes that push acceptable.
func notFastForward(out procOut) bool {
	msg := strings.ToLower(out.Output)
	return strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "fetch first")
}

// fetchMoved reads what branch holds now into remote, in the working clone, where the job's commit
// is replayed onto it, and in the clean copy, which the next push goes out from and so must hold
// the commit that push sits on.
//
// The read off the network happens in the clean copy, and its new commits reach the working clone
// in a bundle, so the token stays out of the sandbox here as in the push. A branch rewritten past
// every commit the clone holds cannot be bundled that way, and the job then pushes nothing rather
// than fetching with the token from the sandbox's own clone.
func (g *Repo) fetchMoved(ctx context.Context, branch, remote, start string) (procOut, error) {
	refspec := "+refs/heads/" + branch + ":" + remote
	target := g.pushTarget()
	if g.pushDir != "" {
		out, err := g.runClean(ctx, 2*time.Minute, true, "fetch", "-q", "--depth", "50", target, refspec)
		if err != nil || out.Code != 0 {
			return out, fmt.Errorf("fetch failed")
		}
		bundlePath := filepath.Join(filepath.Dir(g.dir), "tmp", "moved.bundle")
		defer os.Remove(bundlePath)
		if out, err := g.runClean(ctx, 2*time.Minute, false, "bundle", "create", bundlePath, remote, "--not", start); err != nil || out.Code != 0 {
			return out, fmt.Errorf("bundle failed")
		}
		out, err = g.git(ctx, 2*time.Minute, false, "fetch", "-q", bundlePath, "+"+remote+":"+remote)
		if err == nil && out.Code != 0 {
			err = fmt.Errorf("fetch from bundle failed")
		}
		return out, err
	}
	// No clean copy (a Repo used outside a job, as the tests do): nothing was handed to the
	// sandbox, so the clone may fetch for itself.
	out, err := g.gitNet(ctx, 2*time.Minute, "fetch", "-q", "--depth", "50", target, refspec)
	if err == nil && out.Code != 0 {
		err = fmt.Errorf("fetch failed")
	}
	return out, err
}

// pushOnto commits the job to an open pull request's own branch (app.JobModePR): HEAD goes to
// branch as one more commit on top of start, the head the job began from. Where push is held to
// the bot's own branch names, this is held to the one branch the bot named in the spec — never the
// branch the pull request merges into — and to HEAD building on start, so nothing the engine did
// with git in between can send another history there. It never forces.
//
// When somebody pushed to the branch while the job worked, the push is refused as not a
// fast-forward. The job's own commits are then replayed, once, onto what the branch holds now
// (rebase --onto, which replays exactly start..HEAD and so works after a force-push as well) and
// pushed again; a replay that conflicts is abandoned and nothing is pushed, rather than the job
// choosing between its change and somebody else's. moved is the head it was replayed onto, or ""
// when the first push went through.
func (g *Repo) pushOnto(ctx context.Context, branch, base, start string) (moved string, err error) {
	switch {
	case branch == "" || branch == base:
		return "", stepErr("push_failed", "refusing to push to the branch the pull request merges into")
	case g.currentBranch(ctx) != branch:
		return "", stepErr("push_failed", "HEAD is not on "+branch)
	case !g.descends(ctx, start):
		return "", stepErr("push_failed", "HEAD no longer builds on the pull request's head "+cut(start, 12))
	}
	out, err := g.pushHead(ctx, branch, start)
	if err == nil || !notFastForward(out) {
		return "", err
	}
	remote := "refs/remotes/origin/" + branch
	if out, err := g.fetchMoved(ctx, branch, remote, start); err != nil {
		return "", stepErr("push_failed", "the branch moved while the job worked, and reading it again failed: "+lastLines(out.Output, 300))
	}
	moved = strings.TrimSpace(g.revParse(ctx, remote))
	if out, _ := g.git(ctx, 2*time.Minute, false, "rebase", "--no-autostash", "--onto", remote, start); out.Code != 0 {
		g.git(ctx, 30*time.Second, false, "rebase", "--abort")
		return moved, stepErr("branch_moved", fmt.Sprintf("somebody pushed to %s while the job worked, and the change conflicts with what they pushed; nothing was pushed", branch))
	}
	if g.headSHA(ctx) == moved {
		// Every commit replayed came out empty: what was pushed meanwhile already holds the change.
		return moved, stepErr("empty_diff", fmt.Sprintf("somebody pushed to %s while the job worked, and what they pushed already holds the same change; nothing was left to push", branch))
	}
	if _, err := g.pushHead(ctx, branch, moved); err != nil {
		return moved, err
	}
	return moved, nil
}

// descends reports whether HEAD has commit as an ancestor (or is it).
func (g *Repo) descends(ctx context.Context, commit string) bool {
	if commit == "" {
		return false
	}
	out, err := g.git(ctx, 30*time.Second, false, "merge-base", "--is-ancestor", commit, "HEAD")
	return err == nil && out.Code == 0
}

func (g *Repo) revParse(ctx context.Context, ref string) string {
	out, _ := g.git(ctx, 10*time.Second, false, "rev-parse", "--verify", "-q", ref)
	return strings.TrimSpace(out.Output)
}
