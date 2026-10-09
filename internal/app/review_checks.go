package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"attesttag/internal/review"
)

// What Go decides about a candidate, before and after the verifier. Nothing here asks a model
// anything: these are the checks a model's word is not trusted for — that a quote is really in the
// code, that a line is one GitHub will take a comment on, that a suggestion changes something and
// touches nothing it must not — and the caps that keep a review readable.

// reviewCandidate is one finding on its way through the checks.
type reviewCandidate struct {
	review.Finding
	ts            *reviewTypeSpec // nil for a finding Go raised itself
	file          *reviewFile     // nil for a file this pull request does not change
	where         string
	kind          string
	fp            string
	finderConf    int
	verifierConf  int
	deterministic bool
	codeHash      string
	replaced      string
}

func sevRank(s review.Severity) int {
	switch s {
	case review.P0:
		return 0
	case review.P1:
		return 1
	case review.P2:
		return 2
	}
	return 3
}

// strictnessThreshold is the verifier confidence a finding needs at each strictness.
func strictnessThreshold(s review.Strictness) int {
	switch s {
	case review.StrictnessLow:
		return 60
	case review.StrictnessHigh:
		return 85
	}
	return 70
}

func (r *reviewRun) dropC(c *reviewCandidate, reason, detail, dupOf string) {
	r.drop(reviewDrop{Type: c.ReviewType, Path: c.Path, Line: c.Line, Severity: c.Severity, Title: c.Title,
		Reason: reason, Detail: truncate(detail, 300), DuplicateOf: dupOf, Fingerprint: c.fp, Confidence: c.verifierConf})
}

// checkAll runs the Go checks on every candidate the finders submitted, adds the findings Go raises
// itself, folds the same problem raised twice into one, and drops what an earlier review already
// said. What is left goes to the verifier.
func (r *reviewRun) checkAll(ctx context.Context, found []reviewFound) []*reviewCandidate {
	// Go's own first, so a model's finding of the same committed key merges into the one Go stands
	// behind rather than the other way round.
	cands := r.detected()
	for _, f := range found {
		if c := r.check(ctx, f); c != nil {
			cands = r.merge(cands, c)
		}
	}
	var keep []*reviewCandidate
	for _, c := range cands {
		key := r.priorKeyOf(c)
		switch p, w := r.prior[key], r.withdrawn[c.fp]; {
		case p != nil && r.checking[key] != nil:
			// The finding it repeats is being checked for a fix: whether this is a duplicate or the
			// same problem back again waits for that answer (releaseHeld).
			r.held = append(r.held, c)
		case p != nil:
			r.dropC(c, "duplicate", "already open on this pull request", p.PublicID)
		case w != nil:
			r.dropC(c, "withdrawn", "withdrawn after discussion on this pull request", w.PublicID)
		case r.minorUnchanged(c):
			r.dropC(c, "rereview", "a minor finding on a file unchanged since the last review", "")
		default:
			keep = append(keep, c)
		}
	}
	return keep
}

// priorKeyOf is the key the earlier finding a candidate repeats is under: its fingerprint, or — for
// a finding a re-review moved to another file, whose fingerprint was taken again there without the
// symbol a stored finding does not keep — its fingerprint without its symbol.
func (r *reviewRun) priorKeyOf(c *reviewCandidate) string {
	if r.prior[c.fp] != nil || c.Symbol == "" {
		return c.fp
	}
	loose := c.Finding
	loose.Symbol = ""
	if k := review.Fingerprint(r.repo, loose); r.prior[k] != nil {
		return k
	}
	return c.fp
}

// minorUnchanged reports a re-review's minor finding about a file unchanged since the last review,
// which it does not raise: the last review already had its chance at the minor points, and a
// re-review raises something new about code that has not changed only when it matters.
func (r *reviewRun) minorUnchanged(c *reviewCandidate) bool {
	return r.spec.LastReviewedSHA != "" && (c.Severity == review.P2 || c.PreExisting) && !r.changedSince(c.Path)
}

// changedSince reports whether a file's change differs from what the last review read. Without the
// last review's hashes nothing can be told apart, so everything counts as changed.
func (r *reviewRun) changedSince(p string) bool {
	prior, ok := r.spec.PriorFileHashes[p]
	now := r.out.FileHashes[p]
	return !ok || prior == "" || now == "" || prior != now
}

// check is the Go checks on one candidate: shape, scope, quotes, anchor, suggestion. Nil means it
// was dropped, and why is recorded.
func (r *reviewRun) check(ctx context.Context, found reviewFound) *reviewCandidate {
	f := found.Finding
	f.Normalize()
	f.ReviewType, f.AlsoTypes = found.ts.Key, nil
	c := &reviewCandidate{Finding: f, ts: found.ts, kind: reviewKindFinding, finderConf: f.Confidence}
	r.citeRules(&c.Finding, found.ts)
	if err := c.Validate(); err != nil {
		// A finding whose only fault is its suggestion is still worth having without it.
		if c.Suggestion != nil && errors.Is(err, review.ErrSuggestion) {
			c.Suggestion = nil
			err = c.Validate()
		}
		if err != nil {
			r.dropC(c, "invalid", err.Error(), "")
			return nil
		}
	}
	if r.spec.Settings.IgnoresPath(c.Path) {
		r.dropC(c, "ignored", "a path this repository's settings leave out of review", "")
		return nil
	}
	c.file = r.byPath[c.Path]
	if ok, why := r.grounded(ctx, &c.Finding); !ok {
		r.dropC(c, "ungrounded", why, "")
		return nil
	}
	if c.where = r.place(c); c.where == "" {
		r.dropC(c, "unchanged_file", "not a P0, and in a file this pull request does not change", "")
		return nil
	}
	if c.PreExisting {
		c.where = reviewWherePreExisting
	}
	r.keepSuggestion(ctx, c)
	c.fp = review.Fingerprint(r.repo, c.Finding)
	c.codeHash = r.codeHash(ctx, c)
	return c
}

// citeRules keeps the rule ids a finding cites that are rules of its type, on and about its file,
// and holds its severity to the most severe they allow: a finding may be no more severe than the
// rule it rests on.
func (r *reviewRun) citeRules(f *review.Finding, ts *reviewTypeSpec) {
	var keep []string
	allowed := -1
	for _, id := range f.RuleIDs {
		id = strings.ToUpper(strings.TrimSpace(id))
		if strings.HasPrefix(id, "S") {
			// A skill the run read: it caps nothing, and is kept for the comment to name.
			if r.skillReadable(ts, id) && !slices.Contains(keep, id) {
				keep = append(keep, id)
			}
			continue
		}
		rule, ok := ts.Rule(id)
		if !ok || rule.Off || !rule.Covers(f.Path) || slices.Contains(keep, id) {
			continue
		}
		keep = append(keep, id)
		capRank := 0
		if rule.SeverityCap.Valid() {
			capRank = sevRank(rule.SeverityCap)
		}
		if allowed < 0 || capRank < allowed {
			allowed = capRank
		}
	}
	f.RuleIDs = keep
	if allowed >= 0 && f.Severity.Valid() && sevRank(f.Severity) < allowed {
		f.Severity = []review.Severity{review.P0, review.P1, review.P2}[allowed]
	}
}

// grounded checks every quote against the code it names, at the commit the review read, masked:
// within three lines of where the finding says, whitespace aside. And at least one of them must be
// in this pull request's own code — at the head, or a deleted line at the base — since a finding
// resting only on another repository is not a finding about this change. Each evidence item is
// pinned to what was read, so a link to it shows the code that was checked.
func (r *reviewRun) grounded(ctx context.Context, f *review.Finding) (bool, string) {
	if len(f.Evidence) == 0 {
		return false, "no evidence"
	}
	own := false
	for i := range f.Evidence {
		e := &f.Evidence[i]
		repo, sha, err := r.repoRef(e.Repo, e.Ref)
		if err != nil {
			return false, err.Error()
		}
		t, err := r.text(ctx, repo, sha, e.Path)
		if err != nil {
			return false, fmt.Sprintf("%s could not be read at %s", truncate(e.Path, 200), shortSHA(sha))
		}
		if !quoteNear(t, *e) {
			return false, fmt.Sprintf("the quote is not at %s:%d", truncate(e.Path, 200), e.StartLine)
		}
		switch {
		case repo != r.repo:
			e.Repo, e.Ref = repo, "default"
		case sha == r.head:
			e.Repo, e.Ref, own = "", "head", true
		default:
			e.Repo, e.Ref = "", "base"
			if r.byPath[e.Path] != nil || slices.ContainsFunc(r.files, func(rf *reviewFile) bool { return rf.PrevPath == e.Path }) {
				own = true
			}
		}
	}
	if !own {
		return false, "no evidence in this pull request's own code"
	}
	return true, ""
}

// quoteNumbering is the line label a model copies along with a quote: the diff's "R52 +" or "L48 -",
// or read_file's "52| ".
var quoteNumbering = regexp.MustCompile(`^\s*(?:[RL]\d+\s*[+-]?|\d+\|)`)

// quoteNear reports whether a quote is in t within three lines of where it says it is. Whitespace
// is not compared — a model re-indents what it copies — and neither is a line label it copied with
// the code. A quote of fewer than six characters proves nothing and does not count.
func quoteNear(t *reviewText, e review.Evidence) bool {
	end := max(e.EndLine, e.StartLine)
	return quoteIn(t, e.Quote, e.StartLine-3, end+3)
}

// quoteIn reports whether a quote is in t between lines lo and hi, compared as quoteNear compares it.
func quoteIn(t *reviewText, quote string, lo, hi int) bool {
	lo, hi = max(1, lo), min(len(t.lines), hi)
	if lo > hi {
		return false
	}
	window := normSpace(strings.Join(t.lines[lo-1:hi], "\n"))
	lines := strings.Split(strings.ReplaceAll(quote, "\r\n", "\n"), "\n")
	labelled, marked := make([]string, len(lines)), make([]string, len(lines))
	for i, l := range lines {
		labelled[i] = quoteNumbering.ReplaceAllString(l, "")
		marked[i] = l
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			marked[i] = l[1:]
		}
	}
	for _, q := range []string{normSpace(quote), normSpace(strings.Join(labelled, "\n")), normSpace(strings.Join(marked, "\n"))} {
		if utf8.RuneCountInString(q) >= 6 && strings.Contains(window, q) {
			return true
		}
	}
	return false
}

func normSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// place decides where a finding can sit: inline only when GitHub will take a comment on its lines
// and nothing in their hunk was masked; otherwise the summary — "outside the diff" for a changed
// file, and for a file this pull request does not change only a P0, since anything less about code
// nobody touched is somebody else's review. "" means nowhere: drop it.
//
// A range only part of which the diff shows — a finder citing a function from its signature, above
// the hunk, down to an unchanged line inside it — is narrowed to the part one hunk shows, and
// commented on there. GitHub refuses the whole range, and "outside the diff" for a finding whose
// lines are on the screen in front of the author reads as the reviewer not knowing where it is. Its
// suggestion goes with the narrowing: it was written to replace the lines it no longer names.
func (r *reviewRun) place(c *reviewCandidate) string {
	f := c.file
	if f == nil {
		if c.Severity == review.P0 {
			return reviewWhereUnchanged
		}
		return ""
	}
	if !review.ValidAnchor(f.Hunks, c.Side, c.StartLine, c.Line) {
		start, end, ok := fitAnchor(f.Hunks, c.Side, c.StartLine, c.Line)
		if !ok {
			return reviewWhereOutside
		}
		c.StartLine, c.Line = start, end
		if start == end {
			c.StartLine = 0
		}
		c.Suggestion, c.replaced = nil, ""
	}
	if hi := f.hunkAt(c.Side, c.Line); hi >= 0 && f.masked[hi] {
		return reviewWhereMasked
	}
	return reviewWhereInline
}

// fitAnchor is the part of the lines start through line that one hunk shows on side, as a range
// GitHub will take a comment on: the hunk sharing the most lines with it, the later one of two that
// share as many, since a finding's own line is its last. ok is false when no hunk shows any of them.
// Hunks are contiguous on each side (review.Hunk), so the overlap is a range of lines that all exist.
func fitAnchor(hunks []review.Hunk, side review.Side, start, line int) (from, to int, ok bool) {
	if start == 0 {
		start = line
	}
	if line < 1 || start < 1 || start > line {
		return 0, 0, false
	}
	best := 0
	for _, h := range hunks {
		lo, n := h.NewStart, h.NewLines
		if side == review.Left {
			lo, n = h.OldStart, h.OldLines
		} else if side != review.Right {
			return 0, 0, false
		}
		if n <= 0 {
			continue
		}
		s, e := max(start, lo), min(line, lo+n-1)
		if s > e || e-s+1 < best {
			continue
		}
		if review.ValidAnchor([]review.Hunk{h}, side, s, e) {
			from, to, ok, best = s, e, true, e-s+1
		}
	}
	return from, to, ok
}

// reviewNoSuggestPaths are files a one-click suggestion must never change: a workflow or an action
// runs with the repository's secrets, and a suggestion there is a change to what CI may do, made by
// whoever clicks "Commit suggestion" without reading it as one.
var reviewNoSuggestPaths = []string{".github/workflows/**", ".github/actions/**"}

// keepSuggestion keeps a suggestion only when it is sound: on an inline finding on the RIGHT, not
// in a file it must never touch, carrying no credential and no mask (committing "[redacted-secret]"
// over a real key would be a strange way to fix it), on lines the review could read unmasked, and
// different from what is there. Validate has already held it to the finding's own lines and ten of
// them.
func (r *reviewRun) keepSuggestion(ctx context.Context, c *reviewCandidate) {
	s := c.Suggestion
	c.Suggestion = nil
	if s == nil || c.where != reviewWhereInline || c.Side != review.Right || review.MatchAny(reviewNoSuggestPaths, c.Path) ||
		reviewSecretPath(c.Path) || strings.Contains(s.Code, reviewMask) || redact(s.Code) != s.Code {
		return
	}
	t, err := r.text(ctx, r.repo, r.head, c.Path)
	start, end := c.Range()
	if err != nil || end > len(t.lines) {
		return
	}
	for n := start; n <= end; n++ {
		if t.masked[n] {
			return
		}
	}
	existing := strings.Join(t.lines[start-1:end], "\n")
	if sameCode(existing, s.Code) {
		return
	}
	c.Suggestion, c.replaced = s, existing
}

func sameCode(a, b string) bool {
	norm := func(s string) string {
		lines := strings.Split(strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n"), "\n")
		for i, l := range lines {
			lines[i] = strings.TrimRight(l, " \t")
		}
		return strings.Join(lines, "\n")
	}
	return norm(a) == norm(b)
}

// codeHash is a hash of the lines a finding is about, whitespace aside, so a later push can tell
// whether they moved, changed or went.
func (r *reviewRun) codeHash(ctx context.Context, c *reviewCandidate) string {
	start, end := c.Range()
	var lines []string
	if c.Side == review.Left {
		if c.file == nil {
			return ""
		}
		for _, h := range c.file.Hunks {
			for _, l := range h.Lines {
				if l.Kind != '+' && l.Old >= start && l.Old <= end {
					lines = append(lines, normSpace(l.Text))
				}
			}
		}
	} else {
		t, err := r.text(ctx, r.repo, r.head, c.Path)
		if err != nil || end > len(t.lines) {
			return ""
		}
		for _, l := range t.lines[start-1 : end] {
			lines = append(lines, normSpace(l))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return reviewHash(lines...)
}

// merge adds c to cands, or folds it into the candidate it repeats: the same fingerprint, or the
// same kind of problem on overlapping lines of the same file. The first stays the one it is filed
// under — its type, its rules, its evidence — and gains the other's type as a tag; the pair is as
// severe as the more severe of the two, for the verifier to bring down if it should be.
func (r *reviewRun) merge(cands []*reviewCandidate, c *reviewCandidate) []*reviewCandidate {
	for _, e := range cands {
		same := e.fp == c.fp
		if !same && e.Path == c.Path && e.Side == c.Side && e.Category == c.Category && e.PreExisting == c.PreExisting {
			es, ee := e.Range()
			cs, ce := c.Range()
			same = cs <= ee && es <= ce
		}
		if !same {
			continue
		}
		if !slices.Contains(e.TypeKeys(), c.ReviewType) {
			e.AlsoTypes = append(e.AlsoTypes, c.ReviewType)
		}
		if !e.deterministic && sevRank(c.Severity) < sevRank(e.Severity) {
			e.Severity = c.Severity
		}
		e.finderConf = max(e.finderConf, c.finderConf)
		return cands
	}
	return append(cands, c)
}

// detected are the findings Go raises itself from the raw diff, with no model asked: a credential in
// an added line, and text in an added line written to a reviewer.
func (r *reviewRun) detected() []*reviewCandidate {
	var out []*reviewCandidate
	var first *reviewFile
	injected, injectFiles := 0, 0
	for _, f := range r.files {
		if len(f.inject) > 0 {
			if first == nil {
				first = f
			}
			injected += len(f.inject)
			injectFiles++
		}
		if len(f.secrets) == 0 {
			continue
		}
		var lines []int
		var kinds []string
		fake := true
		for _, s := range f.secrets {
			lines = append(lines, s.Line)
			fake = fake && s.Fake
			for _, k := range s.Kinds {
				if !slices.Contains(kinds, k) {
					kinds = append(kinds, k)
				}
			}
		}
		fd := review.Finding{Path: f.Path, Side: review.Right, Line: lines[0], Category: review.CategorySecurity,
			ReviewType: "security"}
		kind := reviewKindFinding
		what := fmt.Sprintf("%s adds what looks like %s at %s.", f.Path, joinWords(kinds), lineList(lines))
		switch {
		case reviewFixturePath(f.Path):
			// A fake key in a test is how tests of key handling are written. Said, not scored.
			kind, fd.Severity, fd.Title = reviewKindNote, review.P2, "Credential-shaped value in a test fixture"
			fd.Scenario = what + " It is in a test fixture, so it is most likely a fake made for the test. If it is a real credential, revoke it and replace it with a fake."
		case fake:
			// xoxb-DEMO-FAKE-0001 in sample data names itself. Said, not scored, like a test's.
			kind, fd.Severity, fd.Title = reviewKindNote, review.P2, "Placeholder credential"
			fd.Scenario = what + " The value reads as a placeholder (it says fake, example, demo or test, or counts up), so it is most likely not a real one. If it is real, revoke it and replace it with a fake."
		default:
			fd.Severity, fd.Title = review.P0, "Credential committed in this change"
			fd.Scenario = what + " A credential that reaches a repository stays in its history after the line is deleted, and anybody who can read the repository can use it. Revoke it and issue a new one, keep the new one in a secret store, and read it at run time. This review masked the value and showed it to no model."
		}
		c := &reviewCandidate{Finding: fd, file: f, kind: kind, deterministic: true}
		c.where = r.place(c)
		c.fp = review.Fingerprint(r.repo, fd)
		out = append(out, c)
	}
	const steer = "Text like this can steer a model-based review into missing problems, so this review's score is capped at 4. " +
		"If it is deliberate, a test of a reviewer or documentation about prompts, say so in a reply."
	var fd review.Finding
	switch {
	case first != nil:
		line := first.inject[0]
		more := ""
		if injected > 1 {
			more = fmt.Sprintf(" It is one of %d such lines, in %d of the changed files.", injected, injectFiles)
		}
		fd = review.Finding{Path: first.Path, Side: review.Right, Line: line,
			Scenario: fmt.Sprintf("Line %d of %s reads as an instruction to an automated reviewer rather than as code or a comment for people.%s %s",
				line, first.Path, more, steer)}
	default:
		// No line spoke to the reviewer, but a file's name did: a path reaches the prompt as surely as
		// a line, so it is said and capped the same way, on the first line of the file's change.
		for _, f := range r.files {
			if !f.injectPath {
				continue
			}
			first = f
			side, line := reviewFirstLine(f)
			fd = review.Finding{Path: f.Path, Side: side, Line: line,
				Scenario: "The name this pull request gives a file reads as an instruction to an automated reviewer rather than as a path. " + steer}
			break
		}
	}
	if first != nil {
		fd.Severity, fd.Category, fd.ReviewType, fd.Title = review.P2, review.CategorySecurity, "security", "Text addressed to an AI reviewer"
		c := &reviewCandidate{Finding: fd, file: first, kind: reviewKindFinding, deterministic: true}
		c.where = r.place(c)
		c.fp = review.Fingerprint(r.repo, fd)
		out = append(out, c)
	}
	return out
}

// reviewFirstLine is the first line a file's change shows — added or unchanged at the head, or for
// a file that was removed, the first line it had at the base — or line 1 when it shows none.
func reviewFirstLine(f *reviewFile) (review.Side, int) {
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind != '-' && l.New > 0 {
				return review.Right, l.New
			}
		}
	}
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Old > 0 {
				return review.Left, l.Old
			}
		}
	}
	return review.Right, 1
}

func joinWords(ws []string) string {
	switch len(ws) {
	case 0:
		return ""
	case 1:
		return ws[0]
	}
	return strings.Join(ws[:len(ws)-1], ", ") + " and " + ws[len(ws)-1]
}

// lineList is "line 12" or "lines 12, 13 and 15", at most a dozen of them named.
func lineList(ns []int) string {
	var s []string
	for _, n := range ns[:min(len(ns), 12)] {
		s = append(s, strconv.Itoa(n))
	}
	if len(ns) > 12 {
		s = append(s, fmt.Sprintf("%d more", len(ns)-12))
	}
	if len(s) == 1 {
		return "line " + s[0]
	}
	return "lines " + joinWords(s)
}

// ---- verification ----

// verifyAll sends the model's candidates to the verifier, best first, as many as the run's caps and
// money allow, and keeps what it confirms. Go's own findings need no verifier and pass straight
// through. A candidate that is not verified — the money ran out, the time ran out, the verifier
// would not say — is dropped, never kept: and since what it found may have been real, the review
// then cannot claim to have seen everything, and its score is capped.
func (r *reviewRun) verifyAll(ctx context.Context, cands []*reviewCandidate) ([]*reviewCandidate, error) {
	var kept, todo []*reviewCandidate
	for _, c := range cands {
		if c.deterministic {
			kept = append(kept, c)
		} else {
			todo = append(todo, c)
		}
	}
	slices.SortStableFunc(todo, func(a, b *reviewCandidate) int {
		return cmp.Or(cmpBool(a.PreExisting, b.PreExisting), cmp.Compare(b.finderConf, a.finderConf),
			cmp.Compare(sevRank(a.Severity), sevRank(b.Severity)), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Line, b.Line))
	})
	limit := 2 * max(r.spec.Settings.MaxComments, 1)
	afford := len(todo)
	if est := r.verifyEstimate(ctx); est > 0 {
		afford = max(0, int(math.Floor(r.money.verifierLeft()/est)))
	}
	n := min(len(todo), limit, afford)
	for i, c := range todo[n:] {
		if n+i >= limit {
			r.dropC(c, "cap", "more candidates than one review verifies", "")
			continue
		}
		r.dropC(c, "budget", "the review's money could not cover verifying it", "")
		r.out.FullCoverage = false
	}
	todo = todo[:n]

	vctx, cancel := context.WithTimeout(ctx, r.e.verifyWall)
	defer cancel()
	verdicts := make([]*reviewVerdict, len(todo))
	errs := make([]error, len(todo))
	broke := make([]bool, len(todo))
	sem := make(chan struct{}, reviewVerifyParallel)
	var wg sync.WaitGroup
	for i, c := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := vctx.Err(); err != nil {
				errs[i] = err
				return
			}
			est := r.verifyEstimate(vctx)
			if !r.money.hold(est) {
				broke[i] = true
				return
			}
			v, cost, err := r.verify(vctx, c)
			r.money.settle(est, cost)
			r.verifiedOne(cost)
			verdicts[i], errs[i] = v, err
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for i, c := range todo {
		switch err := errs[i]; {
		case broke[i]:
			r.dropC(c, "budget", "the review's money ran out before it was verified", "")
			r.out.FullCoverage = false
		case errors.Is(err, errReviewNoSubmission):
			r.dropC(c, "unverified", "the verifier gave no verdict", "")
			r.out.FullCoverage = false
		case err != nil && vctx.Err() != nil:
			r.dropC(c, "unverified", "verification ran out of time", "")
			r.out.FullCoverage = false
		case err != nil:
			return nil, reviewFail(review.FailModel, err)
		default:
			if r.accept(ctx, c, verdicts[i]) {
				kept = append(kept, c)
			}
		}
	}
	return kept, nil
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// accept applies a verdict. A finding stands only when the verifier confirmed it at the confidence
// the strictness asks for — more for a P0, which says "do not merge", and for a P2 that cites no rule,
// which is the easiest kind of noise to produce. The verifier may bring the severity down and move
// the lines within the diff; it may not make a finding more severe.
func (r *reviewRun) accept(ctx context.Context, c *reviewCandidate, v *reviewVerdict) bool {
	c.verifierConf = min(max(v.Confidence, 0), 100)
	switch v.Verdict {
	case "refuted":
		r.dropC(c, "refuted", v.Reason, "")
		return false
	case "uncertain":
		r.dropC(c, "uncertain", v.Reason, "")
		return false
	case "duplicate":
		r.dropC(c, "duplicate", v.Reason, truncate(strings.TrimSpace(v.DuplicateOf), 64))
		return false
	}
	if s, err := review.ParseSeverity(v.Severity); err == nil && sevRank(s) > sevRank(c.Severity) {
		c.Severity = s
	}
	need := strictnessThreshold(r.strictness(c.ts))
	if c.Severity == review.P0 || (c.Severity == review.P2 && len(c.RuleIDs) == 0) {
		need = max(need, 85)
	}
	if c.verifierConf < need {
		r.dropC(c, "low_confidence", fmt.Sprintf("confirmed at %d, and this needs %d: %s", c.verifierConf, need, v.Reason), "")
		return false
	}
	if c.where == reviewWhereUnchanged && c.Severity != review.P0 {
		r.dropC(c, "unchanged_file", "brought below P0, in a file this pull request does not change", "")
		return false
	}
	if cl := v.CorrectedLines; cl != nil && cl.Line > 0 && c.file != nil {
		start := cl.StartLine
		if start == cl.Line {
			start = 0
		}
		if (start != c.StartLine || cl.Line != c.Line) && review.ValidAnchor(c.file.Hunks, c.Side, start, cl.Line) {
			c.StartLine, c.Line = start, cl.Line
			c.Suggestion, c.replaced = nil, ""
			if !c.PreExisting {
				c.where = r.place(c)
			}
			c.codeHash = r.codeHash(ctx, c)
		}
	}
	if s := strings.TrimSpace(v.Scenario); len(s) >= 20 && utf8.RuneCountInString(s) <= review.ScenarioMaxLen {
		c.Scenario = s
	}
	return true
}

// ---- the caps and the outcome ----

// finish applies the caps — pre-existing findings, a re-review's new P2, the type's inline minimum,
// the run's comment count and its P2 count — and writes the outcome. Nothing is dropped for
// crowding except a third pre-existing finding and a re-review's second new P2; the rest move to
// "More notes", where they are said without a comment of their own.
//
// The comment count is the setting max_comments alone. The plan also ties a count to strictness
// (12, 8, 5); strictness here only sets the confidence a finding needs, so a team that asks for
// fewer, surer comments gets them from the threshold, and the one number for how many comments a
// review may leave is the one the settings show.
func (r *reviewRun) finish(kept []*reviewCandidate) {
	slices.SortStableFunc(kept, func(a, b *reviewCandidate) int {
		return cmp.Or(cmp.Compare(sevRank(a.Severity), sevRank(b.Severity)), cmp.Compare(b.verifierConf, a.verifierConf),
			cmp.Compare(b.finderConf, a.finderConf), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Line, b.Line))
	})
	maxInline := max(r.spec.Settings.MaxComments, 0)
	var pre, newP2, p2, inline int
	for _, c := range kept {
		scored := c.kind == reviewKindFinding && !c.PreExisting
		if c.PreExisting {
			if pre++; pre > reviewMaxPreExisting {
				r.dropC(c, "cap", "more pre-existing findings than a review lists", "")
				continue
			}
			c.where = reviewWherePreExisting
		}
		if r.spec.LastReviewedSHA != "" && scored && c.Severity == review.P2 {
			if newP2++; newP2 > reviewRereviewMaxP2 {
				r.dropC(c, "rereview", "a re-review adds at most one new minor finding", "")
				continue
			}
		}
		if c.where == reviewWhereInline && c.ts != nil && c.ts.InlineMinSeverity.Valid() &&
			sevRank(c.Severity) > sevRank(c.ts.InlineMinSeverity) {
			c.where = reviewWhereBelowInline
		}
		if scored {
			if c.Severity == review.P2 {
				if p2++; p2 > reviewMaxP2 {
					c.where = reviewWhereMore
				}
			}
			if c.where == reviewWhereInline {
				if inline >= maxInline {
					c.where = reviewWhereMore
				} else {
					inline++
				}
			}
		}
		placement := review.PlacementSummary
		if c.where == reviewWhereInline {
			placement = review.PlacementInline
		}
		r.out.Findings = append(r.out.Findings, reviewResult{Finding: c.Finding, Placement: placement, Where: c.where,
			Kind: c.kind, Fingerprint: c.fp, VerifierConfidence: c.verifierConf, CodeHash: c.codeHash, ReplacedLines: c.replaced,
			Snippet: r.snippetFor(c.Finding)})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	paths := make([]string, 0, len(r.notReviewed))
	for p := range r.notReviewed {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		why := r.notReviewed[p]
		r.out.NotReviewed = append(r.out.NotReviewed, review.NotReviewedFile{Path: p, Reason: why})
		switch why {
		case "ignored", "binary", "generated":
			// Not reviewable lines: nothing a reviewer should have read.
		default:
			r.out.FullCoverage = false
		}
	}
	r.out.FilesReviewed = len(r.reviewed)
	for _, t := range r.out.TypeRuns {
		if t.Skipped == "" && t.Summary != "" {
			r.out.Summary = t.Summary
			break
		}
	}
	r.out.Risk = "No blocking issues found."
	for _, f := range r.out.Findings {
		if f.Scored() {
			r.out.Risk = fmt.Sprintf("%s: %s (%s:%d).", f.Severity, f.Title, f.Path, f.Line)
			break
		}
	}
}
