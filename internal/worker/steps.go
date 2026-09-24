package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"attesttag/internal/app"
)

// Running a recipe's steps. Everything here executes a repository's own code, so every step goes
// through runCmd with Sandbox set: as the sandbox user where there is one, with the job's own
// HOME and TMPDIR, and never the environment git carries the repository token in.

func exists(dir string, names ...string) bool {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

func hasPytestSection(dir string) bool {
	for _, f := range []string{"pyproject.toml", "setup.cfg"} {
		if raw := readText(dir, f); strings.Contains(raw, "[tool.pytest") || strings.Contains(raw, "[tool:pytest]") {
			return true
		}
	}
	return false
}

// hasTestFiles looks two levels deep for test_*.py / *_test.py.
func hasTestFiles(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || strings.Count(rel, string(filepath.Separator)) >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		n := d.Name()
		if strings.HasSuffix(n, ".py") && (strings.HasPrefix(n, "test_") || strings.HasSuffix(n, "_test.py")) {
			found = true
		}
		return nil
	})
	return found
}

// stepDir is where one step runs: its own directory when it names one, else the recipe's.
func stepDir(root string, r *app.Recipe, s *app.RecipeStep) string {
	dir := s.Dir
	if dir == "" && r != nil {
		dir = r.Workdir
	}
	if dir == "" {
		dir = "."
	}
	return filepath.Join(root, filepath.FromSlash(dir))
}

// runStep runs one step under a cap and reports it the way a test run is reported, so the
// build, the linter and the suite all read the same in the result and the pull request.
func runStep(ctx context.Context, ws *Workspace, s *app.RecipeStep, cap time.Duration) app.JobTestRun {
	if s == nil {
		return app.JobTestRun{}
	}
	if s.TimeoutS > 0 && time.Duration(s.TimeoutS)*time.Second < cap {
		cap = time.Duration(s.TimeoutS) * time.Second
	}
	out, err := runCmd(ctx, cmdSpec{Dir: stepDir(ws.RepoDir, ws.Recipe, s), Argv: s.Argv, Env: ws.Env, Timeout: cap, Sandbox: true})
	tr := app.JobTestRun{Ran: true, OK: err == nil && out.Code == 0, Seconds: out.Duration.Seconds(), Output: lastLines(ws.Scrub.Clean(out.Output), 4000)}
	tr.Passed, tr.Failed = parseTestCounts(out.Output)
	if out.TimedOut {
		tr.OK = false
		tr.Output = "timed out after " + cap.String() + "\n" + tr.Output
	}
	if !tr.OK && tr.Failed == 0 {
		tr.Failed = 1 // a non-zero exit without a count is still a failure
	}
	if tr.OK {
		tr.Failed = 0
	}
	return tr
}

// runSetup runs the install steps in order under one overall cap. An optional step that fails is
// noted and the rest carry on; a required one that fails ends setup, because everything after it
// would be measuring a half-installed tree.
func runSetup(ctx context.Context, ws *Workspace, r *app.Recipe, cap time.Duration) app.JobStepRun {
	run := app.JobStepRun{Command: setupLine(r)}
	if r == nil || len(r.Setup) == 0 {
		return run
	}
	run.Ran, run.OK = true, true
	deadline := time.Now().Add(cap)
	start := time.Now()
	var log strings.Builder
	for i := range r.Setup {
		s := &r.Setup[i]
		left := time.Until(deadline)
		if left < 10*time.Second {
			log.WriteString("the setup cap ran out before: " + s.String() + "\n")
			run.OK = false
			break
		}
		ws.Reporter.Log("installing: " + s.String())
		out, err := runCmd(ctx, cmdSpec{Dir: stepDir(ws.RepoDir, r, s), Argv: s.Argv, Env: ws.Env, Timeout: left, Sandbox: true})
		log.WriteString("$ " + s.String() + "\n" + lastLines(out.Output, 2000) + "\n")
		if err == nil && out.Code == 0 {
			continue
		}
		if s.Optional {
			ws.Reporter.Log("optional install step failed, carrying on: " + s.String())
			continue
		}
		run.OK = false
		break
	}
	run.Seconds = time.Since(start).Seconds()
	run.Output = lastLines(ws.Scrub.Clean(log.String()), 4000)
	return run
}

func setupLine(r *app.Recipe) string {
	if r == nil {
		return ""
	}
	var parts []string
	for i := range r.Setup {
		parts = append(parts, r.Setup[i].String())
	}
	return strings.Join(parts, " && ")
}

var (
	passedRe = regexp.MustCompile(`(?i)\b(\d+) (?:tests? )?passed\b`)
	failedRe = regexp.MustCompile(`(?i)\b(\d+) (?:tests? )?failed\b`)
	errorRe  = regexp.MustCompile(`(?i)\b(\d+) errors?\b`)
	goFailRe = regexp.MustCompile(`(?m)^--- FAIL:`)
	goOKRe   = regexp.MustCompile(`(?m)^ok\s`)
	// JUnit-style, which Maven, Gradle and most JVM runners print.
	junitRe = regexp.MustCompile(`(?i)Tests run:\s*(\d+),\s*Failures:\s*(\d+),\s*Errors:\s*(\d+)`)
)

func parseTestCounts(output string) (passed, failed int) {
	if m := junitRe.FindStringSubmatch(output); m != nil {
		run, _ := strconv.Atoi(m[1])
		f, _ := strconv.Atoi(m[2])
		e, _ := strconv.Atoi(m[3])
		return run - f - e, f + e
	}
	num := func(re *regexp.Regexp) int {
		if m := re.FindStringSubmatch(output); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	passed, failed = num(passedRe), num(failedRe)+num(errorRe)
	if passed == 0 && failed == 0 {
		passed, failed = len(goOKRe.FindAllString(output, -1)), len(goFailRe.FindAllString(output, -1))
	}
	return
}

func stepLine(name string, t app.JobTestRun) string {
	switch {
	case !t.Ran:
		return name + ": not run"
	case t.OK:
		if t.Passed > 0 {
			return fmt.Sprintf("%s: pass (%d passed, %.0fs)", name, t.Passed, t.Seconds)
		}
		return fmt.Sprintf("%s: pass (%.0fs)", name, t.Seconds)
	}
	return fmt.Sprintf("%s: fail (%d failed, %d passed)", name, t.Failed, t.Passed)
}
