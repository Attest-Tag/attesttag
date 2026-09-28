package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"attesttag/internal/app"
)

// resultFilesMax matches what the bot keeps (jobs_api.go), so the cap costs nothing and the
// body stays under the result size limit.
const resultFilesMax = 200

var errDeadline = errors.New("job deadline reached")

// Runner is one job from claim to result. Every path ends in exactly one result post, made with
// a context that survives the job's cancellation.
type Runner struct {
	opts     Options
	client   *Client
	scrub    *Scrubber
	cloneURL string // tests: clone from a local bare repository instead of GitHub
	github   *GitHub
	// cacheDir is set once the pipeline has a work directory; Run saves it after the result is
	// posted, never before — an upload of up to a couple of minutes must not sit between a
	// finished job and the thread being told about it.
	cacheDir string
	// pkgs are the packages this job checks, the primary first, once they are known: the result
	// reports every one of them however far the job got.
	pkgs []*pkgRun
}

// How the time left is kept for the other packages a job checks. The engine is the point of the
// job, so it is never squeezed below engineFloor for them; each further package gets at most
// extraMax to set up and check before the change, is skipped with the reason when less than
// extraMin is left for it, and has its after-checks sized from how long its before-checks took.
const (
	engineFloor = 12 * time.Minute
	extraMin    = 3 * time.Minute
	extraMax    = 10 * time.Minute
	tailReserve = 3 * time.Minute // commit, push and the pull request
	afterFactor = 1.25            // the after-checks, as a multiple of the before-checks
)

func scaled(d time.Duration, f float64) time.Duration { return time.Duration(float64(d) * f) }

// beforeTime is how long every package's checks took before the change.
func beforeTime(pkgs []*pkgRun) time.Duration {
	var total float64
	for _, p := range pkgs {
		total += p.beforeSecs
	}
	return time.Duration(total * float64(time.Second))
}

// extraBudget is how long further package i of n may take to set up and check before the change:
// what is left once the engine's floor, the after-checks of what already ran and the push are
// kept back, shared among the packages still to go, and never more than extraMax.
func extraBudget(left, before time.Duration, i, n int) time.Duration {
	spare := left - engineFloor - scaled(before, afterFactor) - tailReserve
	if spare <= 0 || n-i <= 0 {
		return 0
	}
	return min(extraMax, spare/time.Duration(n-i))
}

// engineStop is when the engine must stop so that the after-checks of every package and the push
// still fit: the before-checks' time with a margin, never less than four minutes and never more
// than two fifths of what is left. It used to be two minutes before the deadline, and a long
// engine run then left the after-checks to run into it, ending the job as a timeout with no pull
// request for the work it had done.
func engineStop(deadline time.Time, before time.Duration, lint bool) time.Time {
	reserve := scaled(before, afterFactor) + tailReserve
	if lint {
		reserve += time.Minute
	}
	reserve = min(max(reserve, 4*time.Minute), scaled(time.Until(deadline), 0.4))
	return deadline.Add(-reserve)
}

// afterCap is a check's share after the change, cut so the push and the pull request still fit
// before the deadline; zero when there is no time left for it at all.
func afterCap(want time.Duration, deadline time.Time) time.Duration {
	want = min(want, time.Until(deadline)-tailReserve)
	if want < 30*time.Second {
		return 0
	}
	return want
}

// provisionCap is how long the primary's toolchains may take to install.
func provisionCap(deadline time.Time) time.Duration {
	if left := time.Until(deadline); left < 12*time.Minute {
		return left / 3
	}
	return 12 * time.Minute
}

// fillPackages puts what every package's checks said onto the result: the primary's caveats as
// CheckNote beside its own fields, and the others as Packages.
func (r *Runner) fillPackages(res *app.JobResult) {
	if len(r.pkgs) == 0 {
		return
	}
	res.CheckNote = joinNotes(r.pkgs[0].Notes)
	res.Packages = nil
	for _, p := range r.pkgs[1:] {
		res.Packages = append(res.Packages, p.result())
	}
}

// How the job's remaining time is shared out. These used to be flat ten- and fifteen-minute
// constants, which is a cap a large repository walks straight through: a cold Gradle install is
// longer than that, and the failure then read as a failing suite. They are now a share of what
// is actually left, with a floor so a short job still gets a fair try and a ceiling so a
// pathological install cannot eat the engine's time.
type caps struct{ setup, build, test time.Duration }

func shareOut(left time.Duration) caps {
	pick := func(frac float64, min, max time.Duration) time.Duration {
		d := time.Duration(float64(left) * frac)
		if d < min {
			d = min
		}
		if d > max {
			d = max
		}
		return d
	}
	return caps{
		setup: pick(0.30, 5*time.Minute, 25*time.Minute),
		build: pick(0.15, 3*time.Minute, 15*time.Minute),
		test:  pick(0.25, 5*time.Minute, 25*time.Minute),
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// Run claims the job, runs the pipeline and posts the result. Exit codes: 0 result posted,
// 1 result could not be posted, 3 claim refused (the bot owns the job; nothing to report).
func (r *Runner) Run(ctx context.Context) (code int) {
	start := time.Now()
	claim, err := r.client.Claim(ctx, app.JobWorkerInfo{Mode: r.opts.Mode, Execution: os.Getenv("CLOUD_RUN_EXECUTION"), Hostname: hostname(), Version: r.opts.Version})
	if err != nil {
		slog.Error("claim failed", "job", r.opts.JobID, "err", err)
		if isFatal(err) {
			return 3
		}
		return 1
	}
	spec := claim.Job.Spec
	r.scrub.add(claim.Secrets.GitHubToken, claim.Secrets.LLM.APIKey, claim.Secrets.EngineAPIKey)
	slog.Info("claimed", "job", claim.Job.ID, "repo", spec.Repo, "base", spec.BaseBranch, "branch", spec.Branch, "engine", spec.Constraints.Engine, "model", spec.Constraints.Model)

	deadline := time.Now().Add(r.opts.MaxWall - 4*time.Minute)
	if t, err := time.Parse(time.RFC3339, claim.Limits.Deadline); err == nil && t.Before(deadline) {
		deadline = t
	}
	jctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopTimer := time.AfterFunc(time.Until(deadline), func() { cancel(errDeadline) })
	defer stopTimer.Stop()
	rep := newReporter(r.client, r.scrub, cancel)
	go rep.run(jctx)
	// Baseline the model spend before any generation, so the result can carry what the job
	// actually cost rather than the $0.00 the engines' token-only accounting comes to.
	meter := newSpendMeter(claim.Secrets.LLM)
	meter.Start(ctx)

	res := &app.JobResult{Status: app.JobFailed, Engine: spec.Constraints.Engine, Model: spec.Constraints.Model}
	defer func() { // the one exit: post the result whatever happened
		rep.Flush(10 * time.Second)
		res.Seq = rep.NextSeq()
		res.DurationS = int(time.Since(start).Seconds())
		if u := rep.Total(); u.CostUSD > res.Usage.CostUSD || u.In > res.Usage.In {
			res.Usage = u
		}
		// Metered last, on a context that outlives the job's: a cancelled or timed-out job
		// still spent the money, and by now every generation has settled at the provider.
		mctx, mcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		if spent := meter.Since(mctx); spent > res.Usage.CostUSD {
			res.Usage.CostUSD = spent
		}
		mcancel()
		r.fillPackages(res)
		res.Summary = r.scrub.Clean(cut(res.Summary, app.JobSummaryMaxBytes-100))
		res.LogTail = r.scrub.Clean(lastLines(res.LogTail, app.JobLogTailMaxBytes-100))
		res.Error.Message = r.scrub.Clean(cut(res.Error.Message, 1900))
		// The file list is unbounded, and the result body has a hard 128 KB cap whose 413 is
		// fatal — a change touching thousands of files would report nothing at all, so a
		// successful job would look like a failed one. The bot keeps 200 anyway; sending more was
		// only ever a way to lose the whole result.
		if n := len(res.FilesChanged); n > resultFilesMax {
			res.FilesChanged = append(res.FilesChanged[:resultFilesMax:resultFilesMax],
				fmt.Sprintf("… and %d more files", n-resultFilesMax))
		}
		// Three packages' outputs can take a result past the bot's cap, whose refusal loses the
		// whole of it; the least useful parts go first, with room left for the scrubber's marks.
		if !res.Fit(app.JobResultMaxBytes - 4<<10) {
			slog.Warn("result is over the size cap even after trimming", "job", claim.Job.ID)
		}
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer pcancel()
		if err := r.client.Result(pctx, *res); err != nil {
			raw, _ := json.Marshal(res)
			slog.Error("result not delivered", "err", err)
			fmt.Fprintln(os.Stderr, "RESULT_JSON: "+r.scrub.Clean(string(raw)))
			code = 1
		} else {
			slog.Info("result posted", "job", claim.Job.ID, "status", res.Status, "pr", prURL(res), "cost_usd", fmt.Sprintf("%.4f", res.Usage.CostUSD))
		}
		rep.Stop()
		// Last of all, and only now: the next job on this repository installs from what this one
		// downloaded. Nobody is waiting for it, and a job that failed late still leaves a cache
		// worth having.
		if r.cacheDir != "" {
			r.saveCache(ctx, claim, r.cacheDir)
		}
	}()
	defer func() {
		if p := recover(); p != nil {
			res.Status = app.JobFailed
			res.Error = app.JobError{Code: "engine_error", Message: fmt.Sprintf("panic: %v", p)}
			res.LogTail = string(debug.Stack())
			slog.Error("panic", "err", p)
		}
	}()
	r.execute(jctx, claim, rep, res, deadline)
	return 0
}

func prURL(res *app.JobResult) string {
	if res.PR != nil {
		return res.PR.URL
	}
	return ""
}

// stopped maps a cancelled or expired context onto the result.
func stopped(ctx context.Context, res *app.JobResult) bool {
	if ctx.Err() == nil {
		return false
	}
	if context.Cause(ctx) == errDeadline {
		res.Status, res.Error = app.JobTimeout, app.JobError{Code: "timeout", Message: "the job's time ran out"}
	} else {
		res.Status, res.Error = app.JobCancelled, app.JobError{Code: "cancelled", Message: "cancelled"}
	}
	return true
}

func failWith(res *app.JobResult, err error) {
	res.Status = app.JobFailed
	var se *stepError
	if errors.As(err, &se) {
		res.Error = app.JobError{Code: se.code, Message: se.msg}
		return
	}
	res.Error = app.JobError{Code: "engine_error", Message: err.Error()}
}

func (r *Runner) execute(ctx context.Context, claim *app.JobClaim, rep *Reporter, res *app.JobResult, deadline time.Time) {
	spec := claim.Job.Spec
	jobDir := filepath.Join(r.opts.WorkDir, fmt.Sprintf("job-%d", claim.Job.ID))
	os.RemoveAll(jobDir)
	for _, d := range []string{"home", "tmp", ".cache"} {
		os.MkdirAll(filepath.Join(jobDir, d), 0o700)
	}
	if !r.opts.KeepWork {
		defer os.RemoveAll(jobDir)
	}
	ws := &Workspace{JobDir: jobDir, RepoDir: filepath.Join(jobDir, "repo"), Env: baseEnv(jobDir, r.opts.WorkDir), Deadline: deadline, Reporter: rep, Scrub: r.scrub}
	if err := configureSandbox(r.opts); err != nil {
		rep.Phase("clone", "failed", err.Error())
		failWith(res, stepErr("sandbox_unavailable", err.Error()))
		return
	}
	cacheDir := filepath.Join(r.opts.WorkDir, ".cache")
	os.MkdirAll(cacheDir, 0o755)
	r.cacheDir = cacheDir

	// 1. clone
	rep.Phase("clone", "started", fmt.Sprintf("cloning %s@%s", spec.Repo, spec.BaseBranch))
	repo, err := cloneRepo(ctx, ws, spec, claim.Secrets.GitHubToken, r.cloneFor(spec))
	if err != nil {
		if stopped(ctx, res) {
			rep.Phase("clone", "failed", res.Error.Message)
			return
		}
		rep.Phase("clone", "failed", err.Error())
		failWith(res, err)
		return
	}
	if err := repo.checkoutBranch(ctx, spec.Branch); err != nil {
		rep.Phase("clone", "failed", err.Error())
		failWith(res, err)
		return
	}
	baseSHA := repo.headSHA(ctx)
	rep.Phase("clone", "ok", "at "+cut(baseSHA, 12))

	// 2. what this repository is, and how it is built. Resolved before anything runs, so the
	// toolchains it names can be installed and the engine can be told the commands. The brief's
	// files decide which packages are checked: the first one's is the primary, and every other
	// package they point into is checked the same way, on its own toolchains.
	recipe := resolveRecipe(ws.RepoDir, spec.Constraints.Recipe, spec.Constraints.TestCmd, spec.FilesHint)
	ws.Recipe = recipe
	res.Recipe = recipe
	pkgs := resolvePackages(ws.RepoDir, recipe, spec.FilesHint)
	r.pkgs = pkgs
	primary, extras := pkgs[0], pkgs[1:]
	rep.Log("recipe (" + recipe.Source + "): " + recipe.Describe())
	for _, p := range extras {
		rep.Log("also checking " + folderName(p.Dir) + ": " + p.Recipe.Describe())
	}
	if len(recipe.Services) > 0 {
		if missing := servicesMissing(recipe.Services, ws); missing != "" {
			// Not a failure: the change is still worth making and reviewing. But the suite
			// cannot run, and the pull request has to say that rather than "no tests found".
			recipe.Why = "this suite needs " + missing + ", which the worker cannot start"
			recipe.Test = nil
			rep.Warn(recipe.Why)
		}
	}
	// The previous run's package-manager stores, before anything reaches for the network.
	r.restoreCache(ctx, claim, cacheDir)
	// From here on the repository's own code runs, as the sandbox user where there is one. The
	// hand-over comes before the toolchains rather than after them: `mise install` is handed the
	// repository's own .tool-versions and mise.toml, which is the repository deciding what runs,
	// so it belongs on the same side of this line as the tests do.
	grantSandbox(jobDir, cacheDir)
	// Toolchains the repository pins, installed into the job's cache before anything uses them —
	// and before any step is judged unrunnable, since this is what makes a Ruby or JVM step
	// runnable on an image that does not bake those in. From here the job's PATH carries mise's
	// shims, so every command gets what the folder it runs in pins.
	tc := newToolchains(ws, r.opts)
	if tc != nil {
		tc.provision(ctx, ws, primary, time.Now().Add(provisionCap(deadline)))
		tc.activate(ws)
	}
	pw := primary.workspace(ws)
	pruneMissing(recipe, ws.RepoDir, pw.Env)
	if recipe.Why != "" {
		rep.Log("recipe note: " + recipe.Why)
	}
	for _, n := range primary.Notes {
		rep.Warn(n)
	}
	// Again, for anything the worker itself wrote while provisioning: a file the sandbox user
	// cannot read is a step that fails for a reason nobody can see from the outside.
	grantSandbox(jobDir, cacheDir)
	// From here the repository's own code owns the clone, .git/config included. Every git the
	// worker runs from now on (stage, diff, commit, push) runs as the sandbox user too, so a
	// config key that code planted executes as that user rather than as root.
	repo.sandbox = true

	c := shareOut(time.Until(deadline))

	// 3. dependencies
	if len(recipe.Setup) > 0 {
		rep.Phase("setup", "started", setupLine(recipe))
		res.Setup = runSetup(ctx, pw, recipe, c.setup)
		if res.Setup.OK {
			rep.Phase("setup", "ok", fmt.Sprintf("%.0fs", res.Setup.Seconds))
		} else {
			// A failed install is its own phase with its own reason. It used to be a warning,
			// and the tests then failed for a reason the pull request blamed on the tests.
			rep.Phase("setup", "failed", "dependencies did not install; the checks below may not mean anything")
			rep.Warn("dependency install failed\n" + lastLines(res.Setup.Output, 1200))
		}
	} else {
		rep.Phase("setup", "skipped", "nothing to install")
	}
	if stopped(ctx, res) {
		return
	}

	// 4. the baseline: does it build, do its tests pass, before anything is changed
	res.Build.Command, res.Tests.Command, res.Lint.Command = recipe.Build.String(), recipe.Test.String(), recipe.Lint.String()
	if !recipe.Runnable() {
		res.Tests.Skipped, res.Build.Skipped = recipe.Why, recipe.Why
	}
	res.Build.Before = r.gate(ctx, pw, rep, res, "build_before", recipe.Build, c.build)
	res.Tests.Before = r.gate(ctx, pw, rep, res, "test_before", recipe.Test, c.test)
	primary.beforeSecs = res.Build.Before.Seconds + res.Tests.Before.Seconds
	if stopped(ctx, res) {
		return
	}

	// 4b. every other package the brief points into, set up and checked the same way on its own
	// toolchains, within what the engine can spare.
	for i, p := range extras {
		r.checkBefore(ctx, ws, tc, p, extraBudget(time.Until(deadline), beforeTime(pkgs), i, len(extras)))
		if stopped(ctx, res) {
			return
		}
	}

	// 5. the engine, stopped early enough that every package's after-checks and the push fit
	ws.EngineStop = engineStop(deadline, beforeTime(pkgs), hasLint(pkgs))
	eng, err := newEngine(spec.Constraints.Engine, r.opts, claim)
	if err != nil {
		rep.Phase("engine", "failed", err.Error())
		failWith(res, err)
		return
	}
	rep.Phase("engine", "started", eng.Name()+" on "+spec.Constraints.Model)
	brief := Brief{JobID: claim.Job.ID, Spec: spec, Recipe: recipe, Baseline: res.Tests.Before, Build: res.Build.Before,
		Notes: recipe.Why, RepoMap: repoMap(ws.RepoDir, 250), Conv: conventions(ws.RepoDir, 6000),
		Primary: primary, Extras: extras, Shims: tc != nil && tc.active}
	er, err := eng.Run(ctx, ws, brief)
	res.Summary = er.Summary
	res.LogTail = er.LogTail
	if err != nil {
		rep.Phase("engine", "failed", err.Error())
		if !stopped(ctx, res) {
			failWith(res, err)
		}
		return
	}
	switch er.Stopped {
	case "cancelled":
		rep.Phase("engine", "failed", "cancelled")
		res.Status, res.Error = app.JobCancelled, app.JobError{Code: "cancelled", Message: "cancelled while the engine was working"}
		return
	case "timeout":
		rep.Phase("engine", "failed", "out of time")
		res.Status, res.Error = app.JobTimeout, app.JobError{Code: "timeout", Message: "the engine ran out of time"}
		return
	case "error":
		rep.Phase("engine", "failed", cut(er.Summary, 300))
		res.Status, res.Error = app.JobFailed, app.JobError{Code: "engine_error", Message: cut(er.Summary, 1500)}
		return
	case "budget", "max_rounds":
		// Not a failure: what the engine had changed is committed and opened as a draft. The
		// caveat travels as a note, so a job that produced a pull request does not read as an
		// error in the thread and the console.
		note := "the engine reached its time or tool-call limit before finishing on its own"
		if er.Stopped == "max_rounds" {
			note = fmt.Sprintf("the engine used all %d turns before finishing on its own", maxRoundsFor(spec))
		}
		res.Note = note + "; what it had changed was committed and pushed, so check the pull request for loose ends"
		rep.Phase("engine", "ok", "stopped early: "+er.Stopped+"; committing what exists")
		rep.Warn(res.Note)
	default:
		rep.Phase("engine", "ok", fmt.Sprintf("%d turns", er.Rounds))
	}
	if stopped(ctx, res) {
		return
	}

	// 6. the same gates again. A repository whose workdir moved with the change re-resolves so
	// the checks run where the code now is.
	left := shareOut(time.Until(deadline))
	pw = primary.workspace(ws) // another package's install may have put the shims on PATH since
	res.Build.After = r.gate(ctx, pw, rep, res, "build_after", recipe.Build, afterCap(left.build, deadline))
	res.Tests.After = r.gate(ctx, pw, rep, res, "test_after", recipe.Test, afterCap(left.test, deadline))
	if recipe.Lint != nil {
		if lc := afterCap(left.build, deadline); lc > 0 {
			res.Lint.After = runStep(ctx, pw, recipe.Lint, lc)
			rep.Tests(stepLine("lint", res.Lint.After))
		}
	}
	for _, p := range extras {
		r.checkAfter(ctx, ws, p, deadline)
	}
	if stopped(ctx, res) {
		return
	}

	// 7. commit
	rep.Phase("commit", "started", "")
	files, skipped, err := repo.stage(ctx)
	if err != nil {
		rep.Phase("commit", "failed", err.Error())
		failWith(res, err)
		return
	}
	if len(files) == 0 {
		rep.Phase("commit", "failed", "the engine changed nothing")
		res.Status, res.Error = app.JobFailed, app.JobError{Code: "empty_diff", Message: "no changes to commit"}
		if len(skipped) > 0 {
			res.Error.Message += " (left out: " + strings.Join(skipped, ", ") + ")"
		}
		return
	}
	res.FilesChanged = files
	res.Unchecked = uncheckedPackages(ws.RepoDir, files, pkgs)
	res.DiffStat = repo.diffStat(ctx)
	if err := repo.commit(ctx, commitMessage(spec, er.Summary, claim.Job.ID)); err != nil {
		rep.Phase("commit", "failed", err.Error())
		failWith(res, err)
		return
	}
	res.HeadSHA = repo.headSHA(ctx)
	if diff := repo.diff(ctx, baseSHA, app.JobDiffMaxBytes); diff != "" {
		if err := r.client.PutDiff(ctx, r.scrub.Clean(diff)); err != nil {
			slog.Warn("diff not delivered", "err", err)
		}
	}
	rep.Phase("commit", "ok", fmt.Sprintf("%d files, +%d −%d", res.DiffStat.Files, res.DiffStat.Insertions, res.DiffStat.Deletions))
	if stopped(ctx, res) {
		return
	}

	// 8. push
	rep.Phase("push", "started", spec.Branch)
	if err := repo.push(ctx, spec.Branch, spec.BaseBranch, spec.Constraints.BranchPrefix, spec.Constraints.BranchSuffix); err != nil {
		rep.Phase("push", "failed", err.Error())
		if !stopped(ctx, res) {
			failWith(res, err)
		}
		return
	}
	res.Branch = spec.Branch
	rep.Phase("push", "ok", "")

	// 9. pull request — always a draft (the user's decision), with an honest note when a gate
	// still fails or none could be found.
	rep.Phase("pr", "started", "")
	gh := r.github
	if gh == nil {
		gh = newGitHubFor(claim.Secrets.GitHubToken, r.opts)
	}
	r.fillPackages(res)
	pr, err := gh.createPR(ctx, spec.Repo, prInput{Title: prTitle(spec), Head: spec.Branch, Base: spec.BaseBranch, Draft: true,
		Body: r.scrub.Clean(prBody(spec, res, er.Summary, recipe, claim.Job.ID, skipped, pkgs))})
	if pr != nil {
		res.PR = pr
	}
	if err != nil {
		rep.Phase("pr", "failed", err.Error())
		failWith(res, err)
		return
	}
	rep.Phase("pr", "ok", pr.URL)
	res.Status = app.JobSucceeded
	res.TicketComment = ticketComment(spec, res)
	for _, p := range res.Packages {
		switch p.StillFailing() {
		case "tests":
			res.Summary = "Tests in " + folderName(p.Workdir) + " still fail after the change. " + res.Summary
		case "build":
			res.Summary = "The build in " + folderName(p.Workdir) + " still fails after the change. " + res.Summary
		}
	}
	switch {
	case res.Tests.After.Ran && !res.Tests.After.OK:
		res.Summary = "Tests still fail after the change. " + res.Summary
	case res.Build.After.Ran && !res.Build.After.OK:
		res.Summary = "The build still fails after the change. " + res.Summary
	}
}

func hasLint(pkgs []*pkgRun) bool {
	for _, p := range pkgs {
		if p.Recipe != nil && p.Recipe.Lint != nil {
			return true
		}
	}
	return false
}

// checkBefore sets one more package up and checks it before the change, inside its budget, or
// records why it could not: toolchains get a fifth of the budget, the install two fifths, the
// build a sixth or so, and the suite the rest.
func (r *Runner) checkBefore(ctx context.Context, ws *Workspace, tc *toolchains, p *pkgRun, budget time.Duration) {
	rep := ws.Reporter
	if budget < extraMin {
		p.Skipped = "there was not enough time left in the job to set it up and check it"
		rep.Warn(folderName(p.Dir) + " was not checked: " + p.Skipped)
		return
	}
	end := time.Now().Add(budget)
	if tc != nil {
		tc.provision(ctx, ws, p, time.Now().Add(budget/5))
		tc.activate(ws)
	}
	pw := p.workspace(ws)
	pruneMissing(p.Recipe, ws.RepoDir, pw.Env)
	for _, n := range p.Notes {
		rep.Warn(n)
	}
	if p.Recipe.Why != "" {
		rep.Log(folderName(p.Dir) + " recipe note: " + p.Recipe.Why)
	}
	if len(p.Recipe.Setup) > 0 {
		rep.Log(folderName(p.Dir) + " installing: " + setupLine(p.Recipe))
		p.Setup = runSetup(ctx, pw, p.Recipe, min(budget*2/5, time.Until(end)))
		if !p.Setup.OK {
			rep.Warn(folderName(p.Dir) + ": dependencies did not install\n" + lastLines(p.Setup.Output, 600))
		}
	}
	p.Build.Command, p.Tests.Command, p.Lint.Command = p.Recipe.Build.String(), p.Recipe.Test.String(), p.Recipe.Lint.String()
	if !p.Recipe.Runnable() {
		p.Build.Skipped, p.Tests.Skipped = p.Recipe.Why, p.Recipe.Why
	}
	p.Build.Before = r.check(ctx, pw, p, "build before", p.Recipe.Build, min(budget*3/20, time.Until(end)))
	p.Tests.Before = r.check(ctx, pw, p, "test before", p.Recipe.Test, time.Until(end))
	p.beforeSecs = p.Build.Before.Seconds + p.Tests.Before.Seconds
}

// checkAfter runs a further package's checks again after the change, each allowed half as long
// again as it took before plus a minute, and never past the time the push needs.
func (r *Runner) checkAfter(ctx context.Context, ws *Workspace, p *pkgRun, deadline time.Time) {
	if p.Skipped != "" {
		return
	}
	pw := p.workspace(ws)
	capFor := func(before app.JobTestRun) time.Duration {
		return afterCap(time.Duration(1.5*before.Seconds*float64(time.Second))+time.Minute, deadline)
	}
	p.Build.After = r.check(ctx, pw, p, "build after", p.Recipe.Build, capFor(p.Build.Before))
	p.Tests.After = r.check(ctx, pw, p, "test after", p.Recipe.Test, capFor(p.Tests.Before))
	if p.Recipe.Lint != nil {
		p.Lint.After = r.check(ctx, pw, p, "lint", p.Recipe.Lint, capFor(p.Build.Before))
	}
}

// check runs one of a further package's gates and reports it as a line rather than a phase: the
// phases are the primary's, and a second "test_before" would read as the first one's.
func (r *Runner) check(ctx context.Context, ws *Workspace, p *pkgRun, label string, s *app.RecipeStep, cap time.Duration) app.JobTestRun {
	if s == nil {
		return app.JobTestRun{}
	}
	if cap <= 0 {
		p.note("its " + label + " check did not run: the job was out of time")
		return app.JobTestRun{}
	}
	out := runStep(ctx, ws, s, cap)
	ws.Reporter.Tests(folderName(p.Dir) + " " + stepLine(label, out))
	return out
}

// gate runs one repeated check and reports it as a phase. A recipe with nothing for this gate
// reports skipped with the reason, which is the difference the pull request turns on: "no suite
// here" is not "the suite failed".
func (r *Runner) gate(ctx context.Context, ws *Workspace, rep *Reporter, res *app.JobResult, phase string, s *app.RecipeStep, cap time.Duration) app.JobTestRun {
	name := strings.TrimSuffix(strings.TrimSuffix(phase, "_before"), "_after")
	if s == nil {
		why := ws.Recipe.Why
		if why == "" {
			why = "the recipe has no " + name + " command"
		}
		rep.Phase(phase, "skipped", why)
		return app.JobTestRun{}
	}
	if stopped(ctx, res) {
		rep.Phase(phase, "failed", res.Error.Message)
		return app.JobTestRun{}
	}
	if cap <= 0 { // runCmd would read a zero as its two-minute default
		rep.Phase(phase, "skipped", "the job ran out of time before this check")
		return app.JobTestRun{}
	}
	rep.Phase(phase, "started", s.String())
	out := runStep(ctx, ws, s, cap)
	rep.Tests(stepLine(strings.ReplaceAll(phase, "_", " "), out))
	if out.OK {
		rep.Phase(phase, "ok", "")
	} else if strings.HasSuffix(phase, "_before") {
		rep.Phase(phase, "failed", name+" fails before the change (that may be the bug)")
	} else {
		rep.Phase(phase, "failed", name+" still fails; opening a draft anyway")
	}
	return out
}

// cloneFor is where this job's repository is cloned from: GitHub, the address a test set, or —
// in local mode only — a directory of bare repositories, which is how a whole job runs end to
// end without leaving the machine.
func (r *Runner) cloneFor(spec app.JobSpec) string {
	if r.cloneURL != "" {
		return r.cloneURL
	}
	if r.opts.Mode == "local" && r.opts.GitBase != "" {
		return r.opts.GitBase + "/" + spec.Repo + ".git"
	}
	return ""
}
