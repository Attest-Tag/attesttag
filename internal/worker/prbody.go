package worker

import (
	"fmt"
	"strings"

	"attesttag/internal/app"
)

func prTitle(spec app.JobSpec) string {
	t := strings.TrimSpace(spec.Title)
	if len(t) > 72 {
		t = strings.TrimSpace(t[:69]) + "…"
	}
	if t == "" {
		t = "attest_tag fix"
	}
	return t
}

func commitMessage(spec app.JobSpec, summary string, jobID int64) string {
	body := strings.TrimSpace(summary)
	if body == "" {
		body = strings.TrimSpace(spec.Requirement)
	}
	return fmt.Sprintf("%s\n\n%s\n\nattest_tag fix job #%d", prTitle(spec), cut(body, 1500), jobID)
}

func fence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_none_"
	}
	return "```\n" + strings.ReplaceAll(s, "```", "'''") + "\n```"
}

func details(summary, body string) string {
	return "<details><summary>" + summary + "</summary>\n\n" + body + "\n\n</details>"
}

func checkRow(name, cmd string, t app.JobTestRun) string {
	switch {
	case cmd == "":
		return ""
	case !t.Ran:
		return fmt.Sprintf("| %s | `%s` | not run |", name, cmd)
	case t.OK && t.Passed > 0:
		return fmt.Sprintf("| %s | `%s` | pass (%d passed, %.0fs) |", name, cmd, t.Passed, t.Seconds)
	case t.OK:
		return fmt.Sprintf("| %s | `%s` | pass (%.0fs) |", name, cmd, t.Seconds)
	}
	return fmt.Sprintf("| %s | `%s` | **fail** (%d failed, %d passed) |", name, cmd, t.Failed, t.Passed)
}

// checksTable is the build and the suite, before and after, in one place. A reviewer's first
// question about a machine-written change is whether anything checked it, and this is the
// answer — including when the answer is "nothing could".
func checksTable(res *app.JobResult) string {
	return checkRows(res.Build, res.Tests, res.Lint)
}

// checkRows is one package's build, suite and linter, before and after, as a table.
func checkRows(build, tests, lint app.JobCheck) string {
	var rows []string
	for _, c := range []struct {
		label string
		check app.JobCheck
	}{{"build", build}, {"test", tests}, {"lint", lint}} {
		for _, when := range []struct {
			suffix string
			run    app.JobTestRun
		}{{"before", c.check.Before}, {"after", c.check.After}} {
			// The linter is only run after the change — a repository's existing lint debt is
			// not this job's business — so it has no "before" row to print.
			if c.label == "lint" && when.suffix == "before" {
				continue
			}
			if row := checkRow(c.label+" "+when.suffix, c.check.Command, when.run); row != "" {
				rows = append(rows, row)
			}
		}
	}
	if len(rows) == 0 {
		return ""
	}
	return "| | command | result |\n|---|---|---|\n" + strings.Join(rows, "\n") + "\n"
}

// prBody is the pull request description: the brief, the evidence, what changed, and every gate
// that ran, with an honest note up top when one still fails or none could be found. pkgs are the
// packages the job checked, the primary first, for the toolchains each ran on.
func prBody(spec app.JobSpec, res *app.JobResult, summary string, recipe *app.Recipe, jobID int64, skipped []string, pkgs []*pkgRun) string {
	var b strings.Builder
	multi := len(res.Packages) > 0
	if res.Note != "" {
		b.WriteString("> **Note:** " + res.Note + ".\n\n")
	}
	if res.CheckNote != "" && !multi {
		b.WriteString("> **Note:** " + res.CheckNote + ".\n\n")
	}
	switch {
	case recipe == nil || !recipe.Runnable():
		why := "no build or test command could be found for this repository"
		if recipe != nil && recipe.Why != "" {
			why = recipe.Why
		}
		b.WriteString("> **Nothing here was checked automatically: " + why + ".** Please build and test by hand before merging.\n")
		b.WriteString("> You can tell attest_tag how this repository is built by committing `.attest/recipe.yaml`.\n\n")
	case res.Tests.After.Ran && !res.Tests.After.OK:
		b.WriteString("> **Tests still fail after this change.** Opened as a draft so a person can pick it up; see the output below.\n\n")
	case res.Build.After.Ran && !res.Build.After.OK:
		b.WriteString("> **The build still fails after this change.** Opened as a draft so a person can pick it up; see the output below.\n\n")
	case res.Setup.Ran && !res.Setup.OK:
		b.WriteString("> **Dependencies did not install cleanly**, so the checks below may not mean much. Please run them before merging.\n\n")
	case !res.Tests.After.Ran && !res.Build.After.Ran:
		b.WriteString("> **The checks could not be run after the change.** Please run them before merging.\n\n")
	}
	for _, p := range res.Packages {
		switch p.StillFailing() {
		case "tests":
			b.WriteString("> **Tests in " + folderMD(p.Workdir) + " still fail after this change.** See its checks below.\n\n")
		case "build":
			b.WriteString("> **The build in " + folderMD(p.Workdir) + " still fails after this change.** See its checks below.\n\n")
		}
	}
	if len(res.Unchecked) > 0 {
		b.WriteString("> **This change also touches " + foldersMD(res.Unchecked) + ", which this job did not check.** Please run those checks before merging.\n\n")
	}
	b.WriteString(strings.TrimSpace(spec.Requirement) + "\n\n")
	b.WriteString("## Evidence\n")
	if spec.Ticket != "" {
		b.WriteString("- Ticket: " + spec.Ticket + "\n")
	}
	if spec.ThreadLink != "" {
		b.WriteString("- Slack thread: " + spec.ThreadLink + "\n")
	}
	// What the thread observed and what the bot's tools returned — log lines, stack traces,
	// responses from the organisation's own connected services — reach the engine in its brief
	// and nowhere else. A pull request is read by everyone with access to the repository, and
	// none of that was gathered with them in mind; only token-shaped text is scrubbed, not
	// customer data. The links above are how a reviewer gets to the evidence when entitled to.
	if len(spec.Acceptance) > 0 {
		b.WriteString("\n## Acceptance criteria\n")
		for _, a := range spec.Acceptance {
			b.WriteString("- [ ] " + a + "\n")
		}
	}
	b.WriteString("\n## Changes\n" + strings.TrimSpace(cut(summary, 4000)) + "\n")
	if len(res.FilesChanged) > 0 {
		b.WriteString("\nFiles:\n")
		for i, f := range res.FilesChanged {
			if i >= 50 {
				b.WriteString(fmt.Sprintf("- … and %d more\n", len(res.FilesChanged)-50))
				break
			}
			b.WriteString("- `" + f + "`\n")
		}
	}
	if len(skipped) > 0 {
		b.WriteString("\nLeft out of the commit (secrets, CI config or oversized): " + strings.Join(skipped, ", ") + "\n")
	}
	b.WriteString("\n## Checks\n")
	if multi {
		packageSections(&b, res, pkgs)
	} else if t := checksTable(res); t != "" {
		b.WriteString(t)
		if out := strings.TrimSpace(res.Tests.After.Output); out != "" {
			b.WriteString("\n" + details("Test output after the change (tail)", fence(cut(out, 3000))) + "\n")
		}
		if out := strings.TrimSpace(res.Build.After.Output); out != "" && !res.Build.After.OK {
			b.WriteString("\n" + details("Build output after the change (tail)", fence(cut(out, 3000))) + "\n")
		}
	} else {
		why := "nothing was detected"
		if recipe != nil && recipe.Why != "" {
			why = recipe.Why
		}
		b.WriteString("None ran: " + why + ".\n")
	}
	if !multi {
		if len(pkgs) > 0 && len(pkgs[0].Tools) > 0 {
			b.WriteString("\nToolchains: " + strings.Join(pkgs[0].Tools, ", ") + "\n")
		}
		if recipe != nil {
			fmt.Fprintf(&b, "\n<sub>Recipe: %s (from %s)</sub>\n", recipe.Describe(), recipeSourceLabel(recipe.Source))
		}
	}
	fmt.Fprintf(&b, "\n---\nGenerated by attest_tag fix job #%d (engine %s, model %s). Review before merging; nothing here merges on its own.\n", jobID, spec.Constraints.Engine, spec.Constraints.Model)
	return b.String()
}

// packageSections are the checks of a job that checked several packages: one section each, the
// primary first, then what changed without being checked.
func packageSections(b *strings.Builder, res *app.JobResult, pkgs []*pkgRun) {
	for i, p := range res.Checked() {
		b.WriteString("\n### " + folderTitle(p.Workdir) + "\n")
		if p.Skipped != "" {
			b.WriteString("Not checked: " + p.Skipped + ".\n")
			continue
		}
		if p.Note != "" {
			b.WriteString("> " + p.Note + ".\n\n")
		}
		if t := checkRows(p.Build, p.Tests, p.Lint); t != "" {
			b.WriteString(t)
		} else {
			why := "nothing was detected"
			if p.Recipe != nil && p.Recipe.Why != "" {
				why = p.Recipe.Why
			}
			b.WriteString("None ran: " + why + ".\n")
		}
		if p.Setup.Ran && !p.Setup.OK {
			b.WriteString("\n" + details("Dependency install output (tail)", fence(cut(p.Setup.Output, 2000))) + "\n")
		}
		if out := strings.TrimSpace(p.Tests.After.Output); out != "" && !p.Tests.After.OK {
			b.WriteString("\n" + details("Test output after the change (tail)", fence(cut(out, 3000))) + "\n")
		}
		if out := strings.TrimSpace(p.Build.After.Output); out != "" && !p.Build.After.OK {
			b.WriteString("\n" + details("Build output after the change (tail)", fence(cut(out, 3000))) + "\n")
		}
		if i < len(pkgs) && len(pkgs[i].Tools) > 0 {
			b.WriteString("\nToolchains: " + strings.Join(pkgs[i].Tools, ", ") + "\n")
		}
		if p.Recipe != nil {
			fmt.Fprintf(b, "\n<sub>Recipe: %s (from %s)</sub>\n", p.Recipe.Describe(), recipeSourceLabel(p.Recipe.Source))
		}
	}
	if len(res.Unchecked) > 0 {
		b.WriteString("\n### Changed but not checked\n")
		for _, d := range res.Unchecked {
			b.WriteString("- " + folderMD(d) + "\n")
		}
	}
}

func folderTitle(dir string) string {
	if dir == "" || dir == "." {
		return "The repository root"
	}
	return "`" + dir + "/`"
}

func folderMD(dir string) string {
	if dir == "" || dir == "." {
		return "the repository root"
	}
	return "`" + dir + "/`"
}

func foldersMD(dirs []string) string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, folderMD(d))
	}
	if len(out) > 1 {
		return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
	}
	return strings.Join(out, "")
}

func recipeSourceLabel(src string) string {
	switch src {
	case app.RecipeSourceRepoFile:
		return "this repository's .attest/recipe.yaml"
	case app.RecipeSourceConnection:
		return "the attest_tag console"
	case app.RecipeSourceDetected:
		return "detection"
	}
	return "nothing"
}

func ticketComment(spec app.JobSpec, res *app.JobResult) string {
	if res.PR == nil || res.PR.URL == "" {
		return ""
	}
	state := "checks pass"
	switch {
	case res.Tests.After.Ran && !res.Tests.After.OK:
		state = "tests still failing"
	case res.Build.After.Ran && !res.Build.After.OK:
		state = "build still failing"
	case !res.Tests.After.Ran && !res.Build.After.Ran:
		state = "nothing could be checked"
	case !res.Tests.After.Ran:
		state = "builds; no tests here"
	}
	if len(res.Packages) > 0 {
		var failing []string
		for _, p := range res.Checked() {
			if f := p.StillFailing(); f != "" {
				failing = append(failing, f+" still failing in "+folderName(p.Workdir))
			}
		}
		if len(failing) > 0 {
			state = strings.Join(failing, "; ")
		} else if state == "checks pass" {
			state = fmt.Sprintf("checks pass in all %d packages", len(res.Packages)+1)
		}
	}
	if n := len(res.Unchecked); n == 1 {
		state += "; 1 other changed package not checked"
	} else if n > 1 {
		state += fmt.Sprintf("; %d other changed packages not checked", n)
	}
	return fmt.Sprintf("attest_tag opened a draft pull request for this: %s (%s). Review before merging.", res.PR.URL, state)
}
