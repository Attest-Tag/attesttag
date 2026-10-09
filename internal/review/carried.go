package review

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Carried forward: what a pull request's review takes from the reviews of merged pull requests that
// brought the same code in. A release (an integration branch into the production one) and a
// back-merge (the other way) are made of changes each of which was reviewed in its own pull request;
// a file whose contents are exactly what one of those reviews read is not read again, and what that
// review left open on it is listed here rather than raised a second time. The summary says both, so a
// reader can tell a file nobody read from one somebody already did.

// CarriedFile is a changed file an earlier review of a merged pull request read at exactly the
// contents it has here, so this review left it out.
type CarriedFile struct {
	Path string `json:"path"`
	PR   int    `json:"pr"`
	// Types are the review types it was left out of, when this review ran others over it: a type the
	// earlier review did not run has not read it. Empty when it was left out of every one.
	Types []string `json:"types,omitempty"`
}

// CarriedFinding is a finding still open on a merged pull request, about a file this one carries
// forward from it: listed in the summary, never posted again, and not scored here. Its title is the
// model's words and is sanitised wherever it is shown.
type CarriedFinding struct {
	PR         int      `json:"pr"`
	ID         string   `json:"id"`
	Severity   Severity `json:"severity"`
	Type       string   `json:"type,omitempty"`
	Title      string   `json:"title"`
	Path       string   `json:"path"`
	StartLine  int      `json:"start_line,omitempty"`
	Line       int      `json:"line,omitempty"`
	CommentURL string   `json:"comment_url,omitempty"`
}

// carriedHeadingPRs is how many pull requests a heading names before it says how many more.
const carriedHeadingPRs = 5

// carriedRisk is what the summary's first sentence adds when a P0 or P1 is still open on a merged
// pull request whose code this one carries: it ships with this one, whatever this review found.
func (ctx RenderContext) carriedRisk(s SummaryState, p *linkPolicy) string {
	var serious []CarriedFinding
	for _, f := range s.CarriedOpen {
		if f.Severity == P0 || f.Severity == P1 {
			serious = append(serious, f)
		}
	}
	if len(serious) == 0 {
		return ""
	}
	slices.SortStableFunc(serious, compareCarried)
	top := serious[0]
	what := fmt.Sprintf("**%s** (%s, %s)", titleText(top.Title, nil), severityLabel(top.Severity), ctx.prRef(top.PR, p))
	if n := len(serious) - 1; n > 0 {
		what += fmt.Sprintf(" and %d more", n)
	}
	return "Still open on the merged pull request this code came in with: " + what + "."
}

// carriedSections are the summary's lists of what this review took from merged pull requests: the
// findings still open there, then the files it did not read again, each collapsed.
func (ctx RenderContext) carriedSections(s SummaryState, p *linkPolicy) []string {
	var out []string
	if len(s.CarriedOpen) > 0 {
		fs := slices.Clone(s.CarriedOpen)
		slices.SortStableFunc(fs, compareCarried)
		var lines []string
		for i, f := range fs {
			if i == maxSummaryRows {
				lines = append(lines, fmt.Sprintf("- …and %d more in the console", len(fs)-i))
				break
			}
			title := titleText(f.Title, p)
			if canon, ok := p.allow(f.CommentURL); ok && f.CommentURL != "" {
				// Inside a link the title may hold no link of its own.
				title = "[" + titleText(f.Title, nil) + "](" + canon + ")"
			}
			start := cmp.Or(f.StartLine, f.Line)
			lines = append(lines, fmt.Sprintf("- **%s** · %s · %s · %s · %s", severityLabel(f.Severity),
				ctx.typeName(f.Type, p, inlineMode), title, codeSpan(lineLabel(f.Path, start, f.Line)), ctx.prRef(f.PR, p)))
		}
		body := "<sub>Raised on the pull request the code came in with, and still open there. The files are unchanged " +
			"since, so they are not raised again here, and not scored.</sub>\n\n" + strings.Join(lines, "\n")
		out = append(out, section(false, fmt.Sprintf("Open from merged pull requests (%d)", len(fs)), body))
	}
	if len(s.Carried) > 0 {
		files := slices.Clone(s.Carried)
		slices.SortStableFunc(files, func(a, b CarriedFile) int { return cmp.Or(cmp.Compare(a.PR, b.PR), cmp.Compare(a.Path, b.Path)) })
		var prs []int
		for _, f := range files {
			if !slices.Contains(prs, f.PR) {
				prs = append(prs, f.PR)
			}
		}
		var lines []string
		for i, f := range files {
			if i == maxSummaryRows {
				lines = append(lines, fmt.Sprintf("- …and %d more", len(files)-i))
				break
			}
			line := "- " + cmp.Or(codeSpan(f.Path), "(unnamed file)") + " · " + ctx.prRef(f.PR, p)
			if len(f.Types) > 0 {
				names := make([]string, 0, len(f.Types))
				for _, k := range f.Types {
					names = append(names, ctx.typeName(k, p, inlineMode))
				}
				line += " · for " + joinNames(names) + " only"
			}
			lines = append(lines, line)
		}
		var refs []string
		for _, n := range prs {
			refs = append(refs, ctx.prRef(n, p))
		}
		body := "<sub>Read by the review of " + joinNames(refs) + " at exactly the contents they have here, so not read " +
			"again, and not counted as unreviewed.</sub>\n\n" + strings.Join(lines, "\n")
		out = append(out, section(false, fmt.Sprintf("Reviewed earlier, unchanged since (%s, in %s)",
			plural(len(files), "file", "files"), carriedHeadingRefs(prs)), body))
	}
	return out
}

// carriedHeadingRefs names pull requests in a <summary>, where a link is not rendered: "#12, #15",
// then how many more.
func carriedHeadingRefs(prs []int) string {
	var refs []string
	for i, n := range prs {
		if i == carriedHeadingPRs {
			refs = append(refs, fmt.Sprintf("%d more", len(prs)-i))
			break
		}
		refs = append(refs, "#"+strconv.Itoa(n))
	}
	return strings.Join(refs, ", ")
}

// prRef is a pull request of this repository as "#12", linked to it.
func (ctx RenderContext) prRef(n int, p *linkPolicy) string {
	ref := "#" + strconv.Itoa(n)
	if ctx.Repo == "" || n <= 0 {
		return ref
	}
	if canon, ok := p.allow(fmt.Sprintf("https://github.com/%s/pull/%d", ctx.Repo, n)); ok {
		return "[" + ref + "](" + canon + ")"
	}
	return ref
}

// compareCarried orders carried findings worst first, then by pull request and place.
func compareCarried(a, b CarriedFinding) int {
	return cmp.Or(cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)), cmp.Compare(a.PR, b.PR),
		cmp.Compare(a.Path, b.Path), cmp.Compare(a.Line, b.Line), cmp.Compare(a.ID, b.ID))
}
