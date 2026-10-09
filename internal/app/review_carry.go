package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"attesttag/internal/review"
)

// Carrying reviews forward. A team that merges its feature pull requests into an integration branch
// then opens a release (that branch into the production one) and, after it, a back-merge (the other
// way). Each is made of changes that were reviewed one by one in the pull requests that brought them
// in, and reviewing them again from scratch is the worst review there is: hundreds of files cut off
// by the caps, the core ones timed out, findings refuted by files elsewhere in the same diff, and
// problems the author already answered on the feature pull request raised again.
//
// So before a review builds its units, it looks for earlier reviews that already read this pull
// request's files: the organisation's reviews of other pull requests in the same repository that
// merged, at the head each was last reviewed at, from the last reviewCarryWindow, at most
// reviewCarryPRs of them, each with the change it read in every file (review.PatchHash). A changed
// file whose change here — the lines this pull request adds and removes — is the change that review
// read is "reviewed in #N":
//
//   - it is left out of the finder's units for every review type that review ran — a type it did not
//     run has not read the file, and still does here — and listed in the summary, not as unreviewed,
//     so it neither caps the score nor takes a place under the caps;
//   - what that review left open on it is listed in the summary under its own heading, linked to its
//     thread, not posted again and not scored; a P0 or P1 among them is named in the summary's first
//     sentence, since it ships with this pull request whatever this review finds;
//   - what was decided on those pull requests — withdrawn after discussion, fixed, resolved — is told
//     to the finder as not to be raised again, and Go drops a candidate repeating a finding withdrawn
//     there, or one still open there on a file carried from it.
//
// A pull request every file of which was read that way gets its summary with no finder pass at all.
// Only what somebody was shown counts: a live review carries from posted reviews only, as it dedupes
// against posted findings only (reviewFindingsSaid). A try and a full review carry nothing: a try
// starts from nothing, and a full review is a second look at everything.
const (
	reviewCarryPRs    = 20
	reviewCarryWindow = 30 * 24 * time.Hour
	// reviewCarryRunsRead is how many runs the lane reads to find reviewCarryPRs pull requests: one
	// head is often read by more than one run — the opening's, a label's.
	reviewCarryRunsRead = 3 * reviewCarryPRs
	// reviewCarryPromptLines is the most decisions on merged pull requests one finder pass is told.
	reviewCarryPromptLines = 30
)

// reviewCarrySettled are the statuses a finding on a merged pull request can be settled in, which a
// review of the same code is told not to raise again — acknowledged among them, a finding the team
// accepted as a known risk in its thread.
var reviewCarrySettled = []review.FindingStatus{review.FindingWithdrawn, review.FindingFixed, review.FindingResolved,
	review.FindingAcknowledged}

// reviewCarryDeclined reports whether a settled finding was argued away rather than put right: the
// ones Go drops a repeat of, as it drops one withdrawn on the pull request itself. A fixed one is only
// told to the finder: the same problem raised about code that changed since may be the fix undone.
func reviewCarryDeclined(s review.FindingStatus) bool {
	return s == review.FindingWithdrawn || s == review.FindingAcknowledged
}

// reviewCarryIn is what the lane found for a review to carry forward (reviewSpec.Carry).
type reviewCarryIn struct {
	Sources  []reviewCarrySource // newest first
	Findings []reviewCarryFinding
}

// reviewCarrySource is one earlier review of a merged pull request: the head it read, the review
// types it ran in full, and, for each changed file a pass of it read, the change it read there
// (review.PatchHash).
type reviewCarrySource struct {
	PR     int
	Run    string // the run's public id
	Head   string
	Types  []string
	Hashes map[string]string
}

// reviewCarryFinding is a finding said on one of those pull requests, open or settled.
type reviewCarryFinding struct {
	PR int
	*ReviewFinding
}

// reviewCarry is what one run carries forward, worked out by carryForward.
type reviewCarry struct {
	// typed is, by review type, the files a source ran that type over with the change they have here, and
	// the pull request it was; whole is the files to review every type covering them was carried for,
	// which leaveOutCarried takes out of the review.
	typed map[string]map[string]int
	whole map[string]int
	// from is, by path, the pull requests a file was carried from, for what they left open on it.
	from map[string][]int
	// declined is, by fingerprint, the findings of those pull requests a candidate may not repeat.
	declined map[string]reviewCarryFinding
}

// ---- the lane ----

// reviewCarryFor reads what earlier reviews of the organisation's merged pull requests in pr's
// repository read and decided, for r to carry forward. Nil when there is nothing, or when it could not
// be read: carrying is an economy, and a review that cannot have it reads everything, as before.
func (b *Bot) reviewCarryFor(ctx context.Context, r *ReviewRun, pr *ReviewPR, shadow bool) *reviewCarryIn {
	runs, err := b.store.reviewCarryRuns(ctx, r.OrgID, pr.Repo, pr.ID, shadow, nowMinus(reviewCarryWindow), reviewCarryRunsRead)
	if err != nil {
		slog.Warn("code review: earlier reviews not read; this one reads every file", "run", r.PublicID, "err", err)
		return nil
	}
	in := &reviewCarryIn{}
	numbers := map[int64]int{}
	var ids []int64
	for _, run := range runs { // newest first
		if _, ok := numbers[run.ReviewPRID]; !ok {
			if len(ids) == reviewCarryPRs {
				continue
			}
			numbers[run.ReviewPRID] = run.PRNumber
			ids = append(ids, run.ReviewPRID)
		}
		ck, ok := checkpointFrom(run)
		if !ok || !commitSHA.MatchString(run.HeadSHA) {
			continue
		}
		src := reviewCarrySource{PR: run.PRNumber, Run: run.PublicID, Head: run.HeadSHA, Hashes: map[string]string{}}
		// A type the diff brought in read only the parts its pattern matched, and one cut short did
		// not finish every pass it had — and which file was read is kept per file, not per type, so a
		// file another type read is not listed as unread for it — so neither vouches for a file whole.
		for _, t := range ck.Types {
			if t.Skipped == "" && !t.Auto && !t.Cut {
				src.Types = append(src.Types, t.Key)
			}
		}
		// What it did not read itself is not its to vouch for: a file no pass finished, or one it
		// carried from a review before it, whose own source is a candidate in its own right.
		skip := map[string]bool{}
		for _, f := range ck.NotReviewed {
			skip[f.Path] = true
		}
		for _, f := range ck.Carried {
			skip[f.Path] = true
		}
		for p, h := range ck.FileHashes {
			if !skip[p] && h != "" {
				src.Hashes[p] = h
			}
		}
		if len(src.Types) > 0 && len(src.Hashes) > 0 {
			in.Sources = append(in.Sources, src)
		}
	}
	if len(in.Sources) == 0 {
		return nil
	}
	fs, err := b.store.reviewCarryFindings(ctx, r.OrgID, ids, shadow)
	if err != nil {
		slog.Warn("code review: earlier findings of merged pull requests not read; none is carried", "run", r.PublicID, "err", err)
	}
	for _, f := range fs {
		in.Findings = append(in.Findings, reviewCarryFinding{PR: numbers[f.ReviewPRID], ReviewFinding: f})
	}
	return in
}

// ---- the engine ----

// carryForward works out, before the files are prepared, which of them earlier reviews already read
// the very change of (reviewCarry). A file is carried from a merged pull request when this pull
// request's change to it — the lines it adds and removes — is the change that pull request's review
// read there, nothing more and nothing less (review.PatchHash). The same contents at the two heads
// would not do: that review read its own diff, not the file, and a commit nobody reviewed — pushed
// straight to the branch, or merged while reviews were paused — that changed the file before it
// would ride along unread. Its lines, added or removed, make this pull request's change another one.
// Sources are taken newest first; the comparison needs nothing but what both reviews stored.
func (r *reviewRun) carryForward(ctx context.Context, files []review.File) {
	in := r.spec.Carry
	if in == nil || len(in.Sources) == 0 || len(files) == 0 {
		return
	}
	c := &r.carry
	c.typed, c.whole, c.from = map[string]map[string]int{}, map[string]int{}, map[string][]int{}
	c.declined = map[string]reviewCarryFinding{}
	for _, src := range in.Sources {
		for _, f := range files {
			h := review.PatchHash(f)
			if h == "" || src.Hashes[f.Path] != h {
				continue
			}
			kinds := r.carryable(src, f.Path)
			if len(kinds) == 0 {
				continue
			}
			for _, k := range kinds {
				if c.typed[k] == nil {
					c.typed[k] = map[string]int{}
				}
				c.typed[k][f.Path] = src.PR
			}
			if !slices.Contains(c.from[f.Path], src.PR) {
				c.from[f.Path] = append(c.from[f.Path], src.PR)
			}
		}
	}
	r.carryFindings()
}

// carryable is the review types of this run covering p that src ran and that p has not been carried
// for already.
func (r *reviewRun) carryable(src reviewCarrySource, p string) []string {
	var out []string
	for i := range r.spec.Types {
		ts := &r.spec.Types[i]
		if _, done := r.carry.typed[ts.Key][p]; !done && ts.Covers(p) && slices.Contains(src.Types, ts.Key) {
			out = append(out, ts.Key)
		}
	}
	return out
}

// carriedWhole reports whether every review type of this run covering p was carried for it, and from
// which pull request the first of them was.
func (r *reviewRun) carriedWhole(p string) (int, bool) {
	pr, covered := 0, false
	for i := range r.spec.Types {
		ts := &r.spec.Types[i]
		if !ts.Covers(p) {
			continue
		}
		n, ok := r.carry.typed[ts.Key][p]
		if !ok {
			return 0, false
		}
		if !covered {
			pr, covered = n, true
		}
	}
	return pr, covered
}

// carryFindings sorts the sources' findings into what this run does with them: one still open on a
// file carried from its pull request is listed in the summary (reviewOutcome.CarriedOpen), and it and
// one argued away there are what a candidate may not repeat.
func (r *reviewRun) carryFindings() {
	for _, cf := range r.spec.Carry.Findings {
		f := cf.ReviewFinding
		fp := cmp.Or(f.Fingerprint, review.Fingerprint(r.repo, f.Finding))
		switch {
		case f.Status == review.FindingOpen || f.Status == review.FindingDisputed:
			if !slices.Contains(r.carry.from[f.Path], cf.PR) || f.PreExisting || f.Kind == reviewKindNote {
				continue
			}
			r.carry.declined[fp] = cf
			out := review.CarriedFinding{PR: cf.PR, ID: f.PublicID, Severity: f.Severity, Type: f.ReviewType, Title: f.Title,
				Path: f.Path, StartLine: f.StartLine, Line: f.Line}
			if f.GitHubCommentID > 0 {
				out.CommentURL = fmt.Sprintf("https://github.com/%s/pull/%d#discussion_r%d", strings.ToLower(r.repo), cf.PR, f.GitHubCommentID)
			}
			r.out.CarriedOpen = append(r.out.CarriedOpen, out)
		case reviewCarryDeclined(f.Status):
			r.carry.declined[fp] = cf
		}
	}
}

// leaveOutCarried takes out of the files to review the ones every review type covering them was
// carried for, and records each file carried at all for the summary. It runs after the files are
// ranked and before they are capped, so the cap falls on what is left to read; a file the settings
// ignore here is not one this review would have read, and is not said to be carried either.
func (r *reviewRun) leaveOutCarried(files []*reviewFile) []*reviewFile {
	if len(r.carry.from) == 0 {
		return files
	}
	kept := make([]*reviewFile, 0, len(files))
	for _, f := range files {
		if pr, ok := r.carriedWhole(f.Path); ok {
			r.carry.whole[f.Path] = pr
			r.out.Carried = append(r.out.Carried, review.CarriedFile{Path: f.Path, PR: pr})
			continue
		}
		var types []string
		pr := 0
		for i := range r.spec.Types {
			if n, ok := r.carry.typed[r.spec.Types[i].Key][f.Path]; ok {
				types, pr = append(types, r.spec.Types[i].Key), cmp.Or(pr, n)
			}
		}
		if len(types) > 0 {
			r.out.Carried = append(r.out.Carried, review.CarriedFile{Path: f.Path, PR: pr, Types: types})
		}
		kept = append(kept, f)
	}
	return kept
}

// typeFiles is the changed files a review type reads in this run — the ones it covers, less those
// carried for it — and what the run records of the type when there are none: that it ran, when its
// files were all read by the same type before, or that it had nothing in its scope.
func (r *reviewRun) typeFiles(ts *reviewTypeSpec) ([]*reviewFile, review.TypeRun) {
	var files []*reviewFile
	carried := false
	for _, f := range r.reviewable {
		if !ts.Covers(f.Path) {
			continue
		}
		if _, ok := r.carry.typed[ts.Key][f.Path]; ok {
			carried = true
			continue
		}
		files = append(files, f)
	}
	for p := range r.carry.whole {
		carried = carried || ts.Covers(p)
	}
	if carried {
		return files, review.TypeRun{Key: ts.Key}
	}
	return files, review.TypeRun{Key: ts.Key, Skipped: "no changed files in its scope"}
}

// sayCarried writes the summary of a review that read no file because every one left to review had
// been read before, with the same change, by the same review types: no finder ran to write one, and
// a summary with none would read as a review that looked and found nothing to say.
func (r *reviewRun) sayCarried() {
	if len(r.carry.whole) == 0 || r.out.Summary != "" {
		return
	}
	for i := range r.spec.Types {
		if files, _ := r.typeFiles(&r.spec.Types[i]); len(files) > 0 {
			return
		}
	}
	var prs []int
	for _, f := range r.out.Carried {
		if !slices.Contains(prs, f.PR) {
			prs = append(prs, f.PR)
		}
	}
	slices.Sort(prs)
	refs := make([]string, 0, len(prs))
	for _, n := range prs {
		refs = append(refs, fmt.Sprintf("#%d", n))
	}
	where := strings.Join(refs, ", ")
	if len(refs) > 1 {
		where = strings.Join(refs[:len(refs)-1], ", ") + " or " + refs[len(refs)-1]
	}
	r.out.Summary = "Every changed file this review would read was reviewed in " + where +
		" with exactly the change it makes here, so none was read again."
}

// carriedPrompt is what the finder is told of the decisions on the merged pull requests this code
// came in with, about the files this pull request changes: what was settled there, and what is still
// open there on files it carries unchanged, which the summary lists. Neither is to be raised again.
func (r *reviewRun) carriedPrompt() string {
	in := r.spec.Carry
	if in == nil {
		return ""
	}
	changed := map[string]bool{}
	for _, f := range r.files {
		changed[f.Path] = true
	}
	var open, settled []string
	for _, cf := range in.Findings {
		f := cf.ReviewFinding
		if !changed[f.Path] {
			continue
		}
		switch {
		case f.Status == review.FindingOpen || f.Status == review.FindingDisputed:
			if slices.Contains(r.carry.from[f.Path], cf.PR) && !f.PreExisting && f.Kind != reviewKindNote {
				open = append(open, fmt.Sprintf("- %s · %s · %s (#%d)", f.Severity, untrusted(oneLine(f.Title)), untrusted(f.Path), cf.PR))
			}
		case slices.Contains(reviewCarrySettled, f.Status):
			settled = append(settled, fmt.Sprintf("- %s · %s (#%d, %s)", untrusted(oneLine(f.Title)), untrusted(f.Path), cf.PR,
				strings.ReplaceAll(string(f.Status), "_", " ")))
		}
	}
	if len(open)+len(settled) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n<prior_findings source=\"merged pull requests\">\n")
	if len(settled) > 0 {
		b.WriteString("Settled on the merged pull requests this code came in with: withdrawn after discussion, fixed, or " +
			"resolved there. Do not raise them again:\n" + strings.Join(capList(settled, reviewCarryPromptLines), "\n") + "\n")
	}
	if len(open) > 0 {
		b.WriteString("Still open on the merged pull requests this code came in with, about files this pull request carries " +
			"unchanged; its summary lists them. Do not raise them again:\n" + strings.Join(capList(open, reviewCarryPromptLines), "\n") + "\n")
	}
	b.WriteString("</prior_findings>\n")
	return b.String()
}

// carriedDecided drops a candidate that repeats what a merged pull request this code came in with
// already decided — withdrawn there, or still open there on a file carried from it — and reports
// whether it did. A finding moved to another file there is fingerprinted without its symbol, as
// priorKeyOf finds one on this pull request.
func (r *reviewRun) carriedDecided(c *reviewCandidate) bool {
	if len(r.carry.declined) == 0 {
		return false
	}
	cf, ok := r.carry.declined[c.fp]
	if !ok && c.Symbol != "" {
		loose := c.Finding
		loose.Symbol = ""
		cf, ok = r.carry.declined[review.Fingerprint(r.repo, loose)]
	}
	if !ok {
		return false
	}
	if cf.Status == review.FindingOpen || cf.Status == review.FindingDisputed {
		r.dropC(c, "duplicate", fmt.Sprintf("already open on #%d, which this code came in with", cf.PR), "")
	} else {
		r.dropC(c, "withdrawn", fmt.Sprintf("%s on #%d, which this code came in with", strings.ReplaceAll(string(cf.Status), "_", " "), cf.PR), "")
	}
	return true
}

// ---- the summary ----

// reviewCarriedState puts into a summary what its review carried forward, from the run's checkpoint.
func reviewCarriedState(st *review.SummaryState, ck *reviewCheckpoint) {
	st.Carried, st.CarriedOpen = ck.Carried, ck.CarriedOpen
}
