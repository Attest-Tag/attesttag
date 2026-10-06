package review

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// A review's check run, as GitHub shows it among a pull request's checks: a title on one line beside
// the check's name, and a summary on the check's own page. It is drawn from the state the summary
// comment is drawn from, through the same sanitiser, and says less: the summary comment is the
// review's record, and the check points at it. A title is plain text, which GitHub shows as it is.

// maxCheckRows bounds the findings a check's summary lists; the summary comment lists the rest.
const maxCheckRows = 10

// CheckStart is the check's text while a review runs: the commit it reviews and as what.
func CheckStart(ctx RenderContext, sha string, keys []string) (title, summary string) {
	p := ctx.policy()
	short := shortSHA(sha)
	title, summary = "Reviewing", "Reviewing this pull request"
	if short != "" {
		title, summary = "Reviewing "+short, "Reviewing `"+short+"`"
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, ctx.typeName(k, p, inlineMode))
	}
	if len(names) > 0 {
		summary += " as " + joinNames(names)
	}
	return title, summary + ". Its findings are posted on the pull request when it is done, and the summary comment keeps the score."
}

// RenderCheck is the check's text once a review is posted: the score and how many findings are open
// for the title, and for the summary what stands in the way of merging and the open findings, worst
// first, each linked to its thread and its lines. summaryURL is the summary comment, linked when it
// is on the pull request's own repository.
func RenderCheck(s SummaryState, ctx RenderContext, summaryURL string) (title, summary string) {
	p := ctx.policy()
	findings := slices.Clone(s.Findings)
	slices.SortStableFunc(findings, compareFindings)
	var open []SummaryFinding
	for _, f := range findings {
		if f.open() && !f.Note && !f.PreExisting {
			open = append(open, f)
		}
	}
	plain := make([]Finding, len(open))
	for i, f := range open {
		plain[i] = f.Finding
	}
	score := Score(plain, s.FullCoverage, s.InjectionDetected)
	title = fmt.Sprintf("Confidence %d/5 · no open findings", score)
	if len(open) > 0 {
		title = fmt.Sprintf("Confidence %d/5 · %s", score, plural(len(open), "open finding", "open findings"))
	}

	lead := riskSentence(open)
	if score == 4 && Score(plain, true, false) == 5 {
		lead += " " + capReason(s)
	}
	blocks := []string{fmt.Sprintf("**Confidence %d/5** (advisory). %s", score, lead)}
	var rows []string
	for i, f := range open {
		if i == maxCheckRows {
			rows = append(rows, fmt.Sprintf("- …and %d more in the summary comment", len(open)-i))
			break
		}
		rows = append(rows, fmt.Sprintf("- **%s** · %s · %s · %s", severityLabel(f.Severity),
			ctx.typeNames(f.Finding, p, inlineMode), threadTitle(f, p), ctx.location(f, p)))
	}
	if len(rows) > 0 {
		blocks = append(blocks, strings.Join(rows, "\n"))
	}
	if canon, ok := p.allow(summaryURL); ok {
		blocks = append(blocks, "The [summary comment]("+canon+") has the code each finding is about, and what was not reviewed.")
	}
	var foot []string
	reviewed := ""
	if sha := shortSHA(s.ReviewedSHA); sha != "" {
		reviewed = "Reviewed `" + sha + "`"
	}
	if names := ctx.typeNames2(ranTypes(s.Types), p); len(names) > 0 {
		reviewed = strings.TrimSpace(cmp.Or(reviewed, "Reviewed") + " as " + joinNames(names))
	}
	if reviewed != "" {
		foot = append(foot, reviewed)
	}
	if rule := codeSpan(s.Rule); rule != "" {
		foot = append(foot, "Rule: "+rule)
	}
	if len(foot) > 0 {
		blocks = append(blocks, "<sub>"+strings.Join(foot, " · ")+"</sub>")
	}
	return title, strings.Join(blocks, "\n\n")
}

// CheckStanding is the check's text for a review answered from an earlier one of the same commits
// under the same settings: that review stands, with its score.
func CheckStanding(sha string, score int, summaryURL string, ctx RenderContext) (title, summary string) {
	title = "Already reviewed"
	if score >= 0 && score <= 5 {
		title = fmt.Sprintf("Confidence %d/5 · already reviewed", score)
	}
	summary = "These commits were already reviewed under the same settings, and that review stands."
	if short := shortSHA(sha); short != "" {
		summary = "`" + short + "` was already reviewed under the same settings, and that review stands."
	}
	if canon, ok := ctx.policy().allow(summaryURL); ok {
		summary += " The [summary comment](" + canon + ") has it."
	}
	return title, summary
}

// CheckEnded is the check's text for a review that ended without being posted. status is the run's —
// failed, cancelled, superseded, skipped — and reason a failure's kind, which is all a pull request is
// told of one: the error itself is the deployment's (FailReason). slug, when it is the App's, names
// the command that asks again.
func CheckEnded(status, sha string, reason FailReason, slug string) (title, summary string) {
	of := "The review"
	if short := shortSHA(sha); short != "" {
		of = "The review of `" + short + "`"
	}
	again := ""
	if s := strings.TrimPrefix(strings.TrimSpace(slug), "@"); validAppSlug(s) {
		// In code, as the summary's footer has it, so nobody who shares the App's slug is mentioned.
		again = " To ask for another: `@" + s + " review`."
	}
	switch status {
	case "failed":
		return "Review failed: " + reason.Text(), of + " did not finish: " + reason.Text() + ". Nothing was posted for it." + again
	case "cancelled":
		return "Review cancelled", of + " was cancelled before it was posted."
	case "superseded":
		return "Superseded by a newer commit", of + " was set aside: the pull request moved on, and its newer head has a review of its own."
	}
	return "Review not run", of + " stopped before it ran, and nothing was posted for it." + again
}
