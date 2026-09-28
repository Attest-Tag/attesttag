package worker

import (
	"sort"

	"attesttag/internal/app"
)

// A repository is often several packages — a Go service beside a TypeScript console beside a
// Python job runner — each with its own toolchain, lockfile and suite. A job checks every
// package its brief points at, not only the first: each is set up on the versions its own folder
// pins, built and tested before the change and after it, and reported on its own. A package the
// change touched without the brief pointing at it is named as unchecked, so its silence is never
// read as a pass.

// pkgRun is one package a job sets up and checks.
type pkgRun struct {
	Dir     string
	Recipe  *app.Recipe
	Primary bool
	// PathPrefix and Prefix are what this package's own steps run with on top of the job's
	// environment: a toolchain this package alone needs ahead of the rest — a Python its folder
	// pins too old to obtain, run on the nearest one that exists. Prefix is KEY=VALUE, and is
	// also what the engine is told to put before this package's commands, so what it runs is
	// what the checks ran.
	PathPrefix []string
	Prefix     []string
	Tools      []string // what ran here, one line per toolchain the folder pins: "node 20.19.0 (web/.nvmrc)"
	Notes      []string // caveats on how it was checked
	// OldPython is the Python this package's checks ran on when its folder pins one older than
	// anything that can be obtained, which the engine is told plainly.
	OldPython string
	// Skipped says why this package was not checked at all, when it was not.
	Skipped string
	// beforeSecs is how long the before-checks took, which sizes the time kept for the after-checks.
	beforeSecs         float64
	Setup              app.JobStepRun
	Build, Tests, Lint app.JobCheck
}

// workspace is the job's workspace as this package's steps see it. It is made from the job's
// environment as it is now, so the shims that went on PATH after this package was provisioned
// are there too.
func (p *pkgRun) workspace(ws *Workspace) *Workspace {
	w := *ws
	w.Recipe = p.Recipe
	w.Env = withEnv(withPathPrefix(ws.Env, p.PathPrefix), p.Prefix...)
	return &w
}

// note records a caveat on how this package was checked, once.
func (p *pkgRun) note(s string) {
	for _, n := range p.Notes {
		if n == s {
			return
		}
	}
	p.Notes = append(p.Notes, s)
}

// result is the package as the job result carries it, each output cut to the tail a further
// package is allowed.
func (p *pkgRun) result() app.JobPackage {
	out := app.JobPackage{Workdir: p.Dir, Recipe: p.Recipe, Setup: p.Setup, Build: p.Build, Tests: p.Tests, Lint: p.Lint,
		Note: joinNotes(p.Notes), Skipped: p.Skipped}
	out.Setup.Output = app.KeepTail(out.Setup.Output, app.JobPackageOutputMaxBytes)
	for _, c := range []*app.JobCheck{&out.Build, &out.Tests, &out.Lint} {
		c.Before.Output = app.KeepTail(c.Before.Output, app.JobPackageOutputMaxBytes)
		c.After.Output = app.KeepTail(c.After.Output, app.JobPackageOutputMaxBytes)
	}
	return out
}

func joinNotes(notes []string) string {
	out := ""
	for i, n := range notes {
		if i > 0 {
			out += "; "
		}
		out += n
	}
	return out
}

// resolvePackages is every package this job sets up and checks: the primary, which
// resolveRecipe chose, then each further package the brief's files belong to, in the order it
// named them, up to app.JobCheckedPackagesMax in all. A further package is always detected — a
// recipe file or a console recipe speaks for the package it names, not for its neighbours — and
// one with nothing to install or run costs no slot, nor does one that is documentation, examples
// or tooling rather than code (isAPackage).
func resolvePackages(root string, primary *app.Recipe, hints []string) []*pkgRun {
	pkgs := []*pkgRun{{Dir: nonEmptyStr(primary.Workdir, "."), Recipe: primary, Primary: true}}
	seen := map[string]bool{pkgs[0].Dir: true}
	for _, h := range hints {
		if len(pkgs) == app.JobCheckedPackagesMax {
			break
		}
		dir := packageOf(root, h)
		if dir == "" || seen[dir] || !isAPackage(dir) {
			continue
		}
		seen[dir] = true
		r := finish(detectAt(root, dir), root)
		if !r.Runnable() && len(r.Setup) == 0 {
			continue
		}
		pkgs = append(pkgs, &pkgRun{Dir: dir, Recipe: r})
	}
	return pkgs
}

// uncheckedPackages are the packages the change touched that the job never set out to check,
// sorted. One it set out to check and skipped already says so on its own line.
func uncheckedPackages(root string, changed []string, pkgs []*pkgRun) []string {
	in := map[string]bool{}
	for _, p := range pkgs {
		in[p.Dir] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range changed {
		dir := packageOf(root, f)
		if dir == "" || in[dir] || seen[dir] || !isAPackage(dir) {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}
