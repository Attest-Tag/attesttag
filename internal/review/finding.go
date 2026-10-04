package review

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Severity is how bad a finding is. The levels are defined by what happens, not by how sure
// the model is — confidence is its own field — so the score can be read as a statement about
// the code:
//
//   - P0: a security hole, data loss, or a crash or outage on a reachable path; a committed
//     secret.
//   - P1: wrong behaviour under a concrete, plausible trigger.
//   - P2: maintainability, a convention a cited rule asks for, performance, a missing test for
//     new logic.
type Severity string

const (
	P0 Severity = "P0"
	P1 Severity = "P1"
	P2 Severity = "P2"
)

// ParseSeverity reads a severity the way a model writes one, "p1" and " P1 " included.
func ParseSeverity(s string) (Severity, error) {
	v := Severity(strings.ToUpper(strings.TrimSpace(s)))
	if !v.Valid() {
		return "", fmt.Errorf("severity %q: want P0, P1 or P2", clip(s))
	}
	return v, nil
}

func (s Severity) Valid() bool { return s == P0 || s == P1 || s == P2 }

// Category is what kind of problem a finding is. It is part of a finding's identity (see
// Fingerprint), so a bug and a security hole on the same line stay two findings.
type Category string

const (
	CategoryBug         Category = "bug"
	CategorySecurity    Category = "security"
	CategoryData        Category = "data" // data loss or corruption, a migration that drops what it should keep
	CategoryConcurrency Category = "concurrency"
	CategoryContract    Category = "contract" // a broken API, schema or wire contract with another component
	CategoryPerf        Category = "perf"
	CategoryA11y        Category = "a11y"
	CategoryConvention  Category = "convention"
	CategoryTest        Category = "test"
)

// Categories lists every category, in the order the submit_review tool's schema offers them.
func Categories() []Category {
	return []Category{CategoryBug, CategorySecurity, CategoryData, CategoryConcurrency,
		CategoryContract, CategoryPerf, CategoryA11y, CategoryConvention, CategoryTest}
}

// ParseCategory reads a category case-insensitively.
func ParseCategory(s string) (Category, error) {
	v := Category(strings.ToLower(strings.TrimSpace(s)))
	if !v.Valid() {
		return "", fmt.Errorf("category %q: want one of %s", clip(s), joinCategories())
	}
	return v, nil
}

func (c Category) Valid() bool { return slices.Contains(Categories(), c) }

func joinCategories() string {
	var names []string
	for _, c := range Categories() {
		names = append(names, string(c))
	}
	return strings.Join(names, ", ")
}

// Finding is one problem the finder model reports through submit_review, in the shape its
// tool schema asks for, so the model's arguments decode straight into it. Nothing in it is
// trusted until Normalize and Validate have run, and even then it is only well-formed: whether
// its quotes exist and its lines sit in the diff is checked against the fetched code later.
type Finding struct {
	Path string `json:"path"`
	// Side says which file the line numbers count in: RIGHT (the head) for new and unchanged
	// code, LEFT (the base) for a deleted line. See Side.
	Side Side `json:"side"`
	// StartLine is the first line of a multi-line finding, or 0 for one line. GitHub refuses a
	// start_line equal to line, so Normalize turns that into 0 before anything is posted.
	StartLine int      `json:"start_line,omitempty"`
	Line      int      `json:"line"`
	Severity  Severity `json:"severity"`
	Category  Category `json:"category"`
	// Title names the failure in a few words; it is the bold line of the comment.
	Title string `json:"title"`
	// Scenario is the concrete trigger and its consequence, the body of the comment.
	Scenario string `json:"scenario"`
	// Symbol is the function, type or field the finding is about. It is part of the
	// fingerprint, which is what lets a finding keep its identity when lines shift.
	Symbol     string      `json:"symbol,omitempty"`
	Evidence   []Evidence  `json:"evidence,omitempty"`
	RuleIDs    []string    `json:"rule_ids,omitempty"`
	Suggestion *Suggestion `json:"suggestion,omitempty"`
	// PreExisting marks a problem this pull request did not introduce. It is reported in its
	// own collapsed section and never scored: a PR is not marked down for what it inherited.
	PreExisting bool `json:"pre_existing,omitempty"`
	// Confidence is the model's own 0–100. It ranks candidates for the verifier's budget and
	// nothing else; the verifier's confidence is what decides whether a finding is posted.
	Confidence int `json:"confidence,omitempty"`
	// ReviewType is the key of the review type whose pass found this, e.g. "security".
	ReviewType string `json:"review_type,omitempty"`
	// AlsoTypes are the other types whose passes raised the same finding, in the order they
	// ran: when two types flag the same line, the shared verify step merges them into one
	// finding carrying both tags. ReviewType stays the one it is filed under — the first, whose
	// rules its RuleIDs cite. Each finder pass is one type, so this is never the model's to set
	// and has no JSON name; review_findings.review_types stores the lot, ReviewType first.
	AlsoTypes []string `json:"-"`
}

// TypeKeys is every review type that raised the finding, its own first — the default type
// when it names none — and each once.
func (f Finding) TypeKeys() []string {
	keys := []string{cmp.Or(f.ReviewType, DefaultType)}
	for _, k := range f.AlsoTypes {
		if k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// Evidence is one span of code a finding rests on, quoted exactly so Go can check the quote
// is really there before anything is posted. Repo is "owner/name" for code in another of the
// organisation's repositories, or empty for the pull request's own. Ref is head, base,
// default (the repository's default branch) or a commit sha.
type Evidence struct {
	Repo      string `json:"repo,omitempty"`
	Path      string `json:"path"`
	Ref       string `json:"ref,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line,omitempty"` // 0 for a single line
	Quote     string `json:"quote"`
}

// Suggestion is replacement code for the finding's own lines, posted as a GitHub suggestion
// block that the author can commit with one click.
type Suggestion struct {
	StartLine int    `json:"start_line,omitempty"`
	Line      int    `json:"line"`
	Code      string `json:"code"`
}

// The bounds a finding is held to. The title is a bold line in a comment and in the summary's
// list of open findings, the scenario a few short paragraphs; anything longer is a model
// rambling, and refusing it is cheaper than reading it.
const (
	TitleMinLen        = 3
	TitleMaxLen        = 80
	ScenarioMaxLen     = 900
	SuggestionMaxLines = 10

	maxPathLen      = 1024
	maxSymbolLen    = 200
	maxEvidence     = 8
	maxQuoteLen     = 2000
	maxRuleIDs      = 10
	maxRuleIDLen    = 40
	maxConfidence   = 100
	suggestionFence = "```"
)

// ErrSuggestion marks a problem with a finding's suggestion alone. A finding whose only fault
// is its suggestion is still worth posting without it, so the caller can test for this with
// errors.Is, drop the suggestion and validate again, rather than lose the finding.
var ErrSuggestion = errors.New("suggestion")

// Normalize puts a finding into the canonical form Validate expects, forgiving what a model
// commonly gets almost right: the case of an enum, stray whitespace, a missing side (GitHub's
// own default is RIGHT), a start_line equal to line, and a suggestion that leaves its lines
// out because they are the finding's. It never invents content, and leaves anything it does
// not recognise for Validate to refuse.
func (f *Finding) Normalize() {
	f.Path = strings.TrimSpace(f.Path)
	f.Side = Side(strings.ToUpper(strings.TrimSpace(string(f.Side))))
	if f.Side == "" {
		f.Side = Right
	}
	if s, err := ParseSeverity(string(f.Severity)); err == nil {
		f.Severity = s
	}
	if c, err := ParseCategory(string(f.Category)); err == nil {
		f.Category = c
	}
	f.Title = strings.Join(strings.Fields(f.Title), " ")
	f.Scenario = strings.TrimSpace(f.Scenario)
	f.Symbol = strings.TrimSpace(f.Symbol)
	f.ReviewType = strings.ToLower(strings.TrimSpace(f.ReviewType))
	for i, k := range f.AlsoTypes {
		f.AlsoTypes[i] = strings.ToLower(strings.TrimSpace(k))
	}
	if f.StartLine == f.Line {
		f.StartLine = 0
	}
	for i := range f.Evidence {
		e := &f.Evidence[i]
		e.Repo = strings.TrimSpace(e.Repo)
		e.Path = strings.TrimSpace(e.Path)
		e.Ref = strings.ToLower(strings.TrimSpace(e.Ref))
		if e.EndLine == e.StartLine {
			e.EndLine = 0
		}
	}
	for i, id := range f.RuleIDs {
		f.RuleIDs[i] = strings.TrimSpace(id)
	}
	if s := f.Suggestion; s != nil {
		if s.StartLine == 0 && s.Line == 0 {
			s.StartLine, s.Line = f.StartLine, f.Line
		}
		if s.StartLine == s.Line {
			s.StartLine = 0
		}
	}
}

// Range returns the finding's first and last line, start equal to end for one line.
func (f Finding) Range() (start, end int) {
	if f.StartLine == 0 {
		return f.Line, f.Line
	}
	return f.StartLine, f.Line
}

// Validate reports everything wrong with a finding at once, joined, so that a model asked to
// correct its submission hears every problem in one round rather than one per round. Problems
// with the suggestion alone wrap ErrSuggestion.
//
// It checks shape only. Whether the lines are inside one hunk (ValidAnchor), whether the
// quotes are really in the code, and whether a suggestion changes anything are checked by the
// engine against what it fetched.
func (f Finding) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch {
	case f.Path == "":
		bad("path is required")
	case len(f.Path) > maxPathLen:
		bad("path is longer than %d characters", maxPathLen)
	case strings.HasPrefix(f.Path, "/"):
		bad("path %q must be relative to the repository root", clip(f.Path))
	case strings.ContainsAny(f.Path, "\x00\n\r"):
		bad("path contains a control character")
	}
	if f.Side != Right && f.Side != Left {
		bad("side %q: want RIGHT or LEFT", clip(string(f.Side)))
	}
	if f.Line < 1 {
		bad("line must be 1 or more")
	}
	if f.StartLine < 0 || f.StartLine > f.Line {
		bad("start_line %d must be between 1 and line (%d), or left out", f.StartLine, f.Line)
	}
	if !f.Severity.Valid() {
		bad("severity %q: want P0, P1 or P2", clip(string(f.Severity)))
	}
	if !f.Category.Valid() {
		bad("category %q: want one of %s", clip(string(f.Category)), joinCategories())
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(f.Title)); n < TitleMinLen || n > TitleMaxLen {
		bad("title must be %d to %d characters, got %d", TitleMinLen, TitleMaxLen, n)
	}
	if strings.ContainsAny(f.Title, "\n\r") {
		bad("title must be one line")
	}
	switch n := utf8.RuneCountInString(f.Scenario); {
	case strings.TrimSpace(f.Scenario) == "":
		bad("scenario is required: the trigger and what goes wrong")
	case n > ScenarioMaxLen:
		bad("scenario must be at most %d characters, got %d", ScenarioMaxLen, n)
	}
	if utf8.RuneCountInString(f.Symbol) > maxSymbolLen || strings.ContainsAny(f.Symbol, "\n\r") {
		bad("symbol must be one line of at most %d characters", maxSymbolLen)
	}
	if f.Confidence < 0 || f.Confidence > maxConfidence {
		bad("confidence %d must be between 0 and %d", f.Confidence, maxConfidence)
	}
	if f.ReviewType != "" && !ValidTypeKey(f.ReviewType) {
		bad("review_type %q is not a review type key", clip(f.ReviewType))
	}
	if len(f.AlsoTypes) > maxRuleTypes {
		bad("at most %d other review types, got %d", maxRuleTypes, len(f.AlsoTypes))
	}
	for _, k := range f.AlsoTypes {
		if !ValidTypeKey(k) {
			bad("review type %q is not a review type key", clip(k))
		}
	}

	if len(f.Evidence) > maxEvidence {
		bad("at most %d evidence items, got %d", maxEvidence, len(f.Evidence))
	}
	for i, e := range f.Evidence {
		if err := e.validate(); err != nil {
			bad("evidence[%d]: %v", i, err)
		}
	}
	if len(f.RuleIDs) > maxRuleIDs {
		bad("at most %d rule_ids, got %d", maxRuleIDs, len(f.RuleIDs))
	}
	for _, id := range f.RuleIDs {
		if id == "" || len(id) > maxRuleIDLen || strings.ContainsFunc(id, unicode.IsSpace) {
			bad("rule id %q is not a rule id", clip(id))
		}
	}

	if f.Suggestion != nil {
		if reason := f.suggestionProblem(); reason != "" {
			errs = append(errs, fmt.Errorf("%w %s", ErrSuggestion, reason))
		}
	}
	return errors.Join(errs...)
}

// suggestionProblem says what is wrong with the suggestion, or "". A suggestion replaces
// exactly the lines its comment sits on — that is how GitHub applies one — so its range must
// be the finding's own; and a deleted line cannot be replaced, so only RIGHT findings carry
// one. The size cap keeps a one-click commit reviewable at a glance, and the fence check stops
// code that would close the suggestion block early and spill the rest into the comment.
func (f Finding) suggestionProblem() string {
	s := f.Suggestion
	// Only a LEFT finding is the suggestion's fault; an unknown side is already reported as
	// the finding's own problem, and dropping the suggestion would not fix it.
	if f.Side == Left {
		return "is only allowed on the RIGHT side; a deleted line cannot be replaced"
	}
	ss, se := s.StartLine, s.Line
	if ss == 0 {
		ss = se
	}
	fs, fe := f.Range()
	if ss != fs || se != fe {
		return fmt.Sprintf("must replace exactly the finding's lines %d-%d, not %d-%d", fs, fe, ss, se)
	}
	if se-ss+1 > SuggestionMaxLines {
		return fmt.Sprintf("may replace at most %d lines, not %d", SuggestionMaxLines, se-ss+1)
	}
	code := strings.TrimSuffix(strings.ReplaceAll(s.Code, "\r\n", "\n"), "\n")
	if n := strings.Count(code, "\n") + 1; n > SuggestionMaxLines {
		return fmt.Sprintf("code may be at most %d lines, not %d", SuggestionMaxLines, n)
	}
	if strings.Contains(code, suggestionFence) {
		return "code may not contain ``` (it would end the suggestion block)"
	}
	return ""
}

// shaRef is an abbreviated or full commit sha.
var shaRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func (e Evidence) validate() error {
	var errs []string
	if e.Repo != "" && !validRepoName(e.Repo) {
		errs = append(errs, fmt.Sprintf("repo %q: want owner/name", clip(e.Repo)))
	}
	if e.Path == "" || strings.HasPrefix(e.Path, "/") || len(e.Path) > maxPathLen {
		errs = append(errs, "path must be a path relative to the repository root")
	}
	switch e.Ref {
	case "", "head", "base", "default":
	default:
		if !shaRef.MatchString(e.Ref) {
			errs = append(errs, fmt.Sprintf("ref %q: want head, base, default or a commit sha", clip(e.Ref)))
		}
	}
	if e.StartLine < 1 {
		errs = append(errs, "start_line must be 1 or more")
	}
	if e.EndLine != 0 && e.EndLine < e.StartLine {
		errs = append(errs, "end_line comes before start_line")
	}
	if strings.TrimSpace(e.Quote) == "" {
		errs = append(errs, "quote is required: the exact text of those lines")
	} else if utf8.RuneCountInString(e.Quote) > maxQuoteLen {
		errs = append(errs, fmt.Sprintf("quote must be at most %d characters", maxQuoteLen))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// repoName is GitHub's owner/name: an owner of letters, digits and hyphens, a name that may
// also hold dots and underscores. "." and ".." are not repositories.
var repoName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

func validRepoName(s string) bool {
	if !repoName.MatchString(s) {
		return false
	}
	name := s[strings.IndexByte(s, '/')+1:]
	return name != "." && name != ".."
}

// Fingerprint is a finding's identity across runs: sha256 over the repository (lowercased,
// since GitHub's names are case-insensitive), the path, the category, the symbol and the
// title's terms. Two findings with the same fingerprint are the same finding, which is how a
// re-review recognises one it has already posted and how a withdrawn finding stays withdrawn.
//
// What is left out matters as much. Line numbers are out, so the finding survives code
// moving above it. Severity is out, so a downgrade is the same finding at a new level. The
// review type is out, so the Security pass and the General pass flagging one problem merge
// into one comment carrying both tags. And the title is reduced to its terms — lowercased,
// stop-words dropped, sorted, deduplicated — so "Tenant check missing on list query" and
// "Missing tenant check in the list query" are one finding, as a reader would say they are.
// Near misses beyond that are the verifier's call, not a hash's.
//
// The fields are joined with NUL rather than a printable separator, so no path or symbol can
// be written to collide with another finding's fields.
func Fingerprint(repo string, f Finding) string {
	h := sha256.New()
	for _, part := range []string{
		strings.ToLower(strings.TrimSpace(repo)),
		strings.TrimSpace(f.Path),
		strings.ToLower(strings.TrimSpace(string(f.Category))),
		strings.TrimSpace(f.Symbol),
		titleTerms(f.Title),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// titleTerms is a title as a sorted set of its meaningful words. Words are runs of letters
// and digits, so "org_id" is two terms and punctuation is no term at all; a single character
// is dropped as noise (the "t" of "doesn't"). Negations are kept: "not" and "no" change what
// a title claims.
func titleTerms(title string) string {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var terms []string
	for _, w := range words {
		if utf8.RuneCountInString(w) < 2 || stopWords[w] {
			continue
		}
		terms = append(terms, w)
	}
	slices.Sort(terms)
	return strings.Join(slices.Compact(terms), " ")
}

// stopWords are the words that change how a title reads without changing what it names:
// articles, prepositions, auxiliaries and the hedges a model reaches for ("may", "could").
// The list is part of every stored fingerprint, so changing it re-identifies every open
// finding at once; add to it only with that in mind.
var stopWords = map[string]bool{
	"an": true, "the": true, "of": true, "in": true, "on": true, "at": true, "to": true,
	"for": true, "from": true, "by": true, "with": true, "into": true, "onto": true,
	"and": true, "or": true, "as": true, "than": true, "then": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true, "being": true,
	"can": true, "could": true, "may": true, "might": true, "will": true, "would": true,
	"should": true, "does": true, "do": true, "did": true, "has": true, "have": true, "had": true,
	"this": true, "that": true, "these": true, "those": true, "it": true, "its": true,
	"when": true, "if": true, "which": true, "where": true, "while": true,
}
