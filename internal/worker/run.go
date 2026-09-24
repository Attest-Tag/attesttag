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
	// toolchains it names can be installed and the engine can be told the commands.
	recipe := resolveRecipe(ws.RepoDir, spec.Constraints.Recipe, spec.Constraints.TestCmd, spec.FilesHint)
	ws.Recipe = recipe
	res.Recipe = recipe
	rep.Log("recipe (" + recipe.Source + "): " + recipe.Describe())
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
	// runnable on an image that does not bake those in.
	if err := provisionTools(ctx, ws, recipe, r.opts); err != nil {
		rep.Warn("toolchain setup: " + err.Error())
	}
	pruneMissing(recipe, ws.RepoDir, ws.Env)
	if recipe.Why != "" {
		rep.Log("recipe note: " + recipe.Why)
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
		res.Setup = runSetup(ctx, ws, recipe, c.setup)
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
	res.Build.Before = r.gate(ctx, ws, rep, res, "build_before", recipe.Build, c.build)
	res.Tests.Before = r.gate(ctx, ws, rep, res, "test_before", recipe.Test, c.test)
	if stopped(ctx, res) {
		return
	}

	// 5. the engine
	eng, err := newEngine(spec.Constraints.Engine, r.opts, claim)
	if err != nil {
		rep.Phase("engine", "failed", err.Error())
		failWith(res, err)
		return
	}
	rep.Phase("engine", "started", eng.Name()+" on "+spec.Constraints.Model)
	brief := Brief{JobID: claim.Job.ID, Spec: spec, Recipe: recipe, Baseline: res.Tests.Before, Build: res.Build.Before,
		Notes: recipe.Why, RepoMap: repoMap(ws.RepoDir, 250), Conv: conventions(ws.RepoDir, 6000)}
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
		note := "the engine hit the job's spend cap before finishing on its own"
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
	res.Build.After = r.gate(ctx, ws, rep, res, "build_after", recipe.Build, left.build)
	res.Tests.After = r.gate(ctx, ws, rep, res, "test_after", recipe.Test, left.test)
	if recipe.Lint != nil {
		res.Lint.After = runStep(ctx, ws, recipe.Lint, left.build)
		rep.Tests(stepLine("lint", res.Lint.After))
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
	pr, err := gh.createPR(ctx, spec.Repo, prInput{Title: prTitle(spec), Head: spec.Branch, Base: spec.BaseBranch, Draft: true,
		Body: r.scrub.Clean(prBody(spec, res, er.Summary, recipe, claim.Job.ID, skipped))})
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
	switch {
	case res.Tests.After.Ran && !res.Tests.After.OK:
		res.Summary = "Tests still fail after the change. " + res.Summary
	case res.Build.After.Ran && !res.Build.After.OK:
		res.Summary = "The build still fails after the change. " + res.Summary
	}
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
