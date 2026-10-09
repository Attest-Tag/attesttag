package review

import (
	"cmp"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// RenderContext is what rendering needs besides the finding or the summary itself: where the
// pull request lives, which commits the run read, what links may point at, and the key the
// markers are signed with. One is built per run; RenderFinding also reads the two fields about
// the one finding it is rendering.
type RenderContext struct {
	// Repo is the pull request's repository, owner/name, and PR its number.
	Repo string
	PR   int
	// HeadSHA and BaseSHA are the commits the run reviewed. Evidence at "head" or "base" links
	// to them, so a link keeps showing what the reviewer saw after the branch moves on.
	HeadSHA, BaseSHA string
	// DefaultSHAs maps a repository, owner/name in any case, to the commit its default branch
	// was read at, for evidence the finder cited from "default". Evidence whose ref cannot be
	// pinned to a commit is named but not linked: a link to a branch shows other code tomorrow.
	DefaultSHAs map[string]string
	// AllowedRepos are the other repositories links may point into: the context repositories
	// this review was allowed to read. Repo is always allowed.
	AllowedRepos []string
	// Slug is the GitHub App's slug, for the "@slug review" a stale summary suggests.
	Slug string
	// CommentHeader is the team's header for every inline comment, from the settings. It is a
	// person's text, not the model's, but it is posted under the bot's name all the same and is
	// sanitised like everything else.
	CommentHeader string
	// Types are the review types the run used, for their names and their rules' text. A key
	// missing here falls back to the built-in type of that key, and then to the key itself.
	Types []Type
	// ConsoleURL is this review's page in the console. It is linked from the summary, and model
	// text may link to its origin, only when it is a real https address: a link to localhost on
	// a pull request is a broken link for everybody but the operator.
	ConsoleURL string
	// PublicRepo hides the cost, which on a public repository tells strangers what the
	// organisation spends; ShowCost is the organisation choosing to show it at all.
	PublicRepo bool
	ShowCost   bool
	// MarkerKey and OrgID sign the markers, with Repo and PR (see Marker).
	MarkerKey []byte
	OrgID     int64

	// FindingID is the public id of the finding RenderFinding is rendering, for its marker;
	// with none, the comment carries no marker. VerifierConfidence is what the verifier said,
	// 0 to 100, shown under "Why this was flagged"; 0 means it was not recorded.
	FindingID          string
	VerifierConfidence int
	// ReplacedLines is the head's text of the lines the finding's suggestion would replace,
	// when the caller has it. An invisible character in a suggestion is let through only when
	// these already hold it; without them, a suggestion holding one is not offered.
	ReplacedLines string
	// FixBox offers the fix checkbox (FixBoxText) on every finding's comment: a fix can be asked
	// for on this pull request — the repository allows it, a fix worker runs here, and the branch
	// is the repository's own rather than a fork's, which the App could not push to.
	FixBox bool
}

func (ctx RenderContext) markerScope() MarkerScope {
	return MarkerScope{OrgID: ctx.OrgID, Repo: ctx.Repo, PR: ctx.PR}
}

// policy is the link policy for this run's model text.
func (ctx RenderContext) policy() *linkPolicy {
	origin := ""
	if u, ok := realConsoleURL(ctx.ConsoleURL); ok {
		origin = u.Scheme + "://" + u.Host
	}
	return newLinkPolicy(append([]string{ctx.Repo}, ctx.AllowedRepos...), origin)
}

// realConsoleURL reports whether raw is a console address worth putting on a pull request:
// https, with a host that is not this machine. A deployment that has only ever been reached
// on localhost learns that as its origin, and a link to it would send everybody else nowhere.
func realConsoleURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, false
	}
	if ip, err := netip.ParseAddr(host); err == nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return nil, false
	}
	return u, true
}

// typeNames is how the types a finding carries are called on a comment: one name, or, for a
// finding two types' passes merged, each of them joined by " + ", its own first.
func (ctx RenderContext) typeNames(f Finding, p *linkPolicy, mode sanMode) string {
	keys := f.TypeKeys()
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = ctx.typeName(k, p, mode)
	}
	return strings.Join(names, " + ")
}

// typeName is how a review type is called on a comment: the run's own type of that key, else
// the built-in one, else the key. A finding with no type came from the default one. mode says
// where the name goes: inline in markdown, or inside our own HTML (a <summary>).
func (ctx RenderContext) typeName(key string, p *linkPolicy, mode sanMode) string {
	if key == "" {
		key = DefaultType
	}
	name := key
	if t, ok := ctx.lookupType(key); ok && strings.TrimSpace(t.Name) != "" {
		name = t.Name
	}
	return sanitize(name, p, mode, MaxTypeNameLen)
}

func (ctx RenderContext) lookupType(key string) (Type, bool) {
	for _, t := range ctx.Types {
		if t.Key == key {
			return t, true
		}
	}
	return BuiltinType(key)
}

// RenderFinding renders one inline review comment from a finding's structured fields. The
// model wrote the title and the scenario, which pass through Sanitize; everything around them —
// the type and severity, the links, the suggestion, the agent prompt, the marker — is built
// here from fields that were validated, so nothing the model writes can change the comment's
// shape:
//
//	[comment header]
//	**Security · P1 · Tenant check missing on list query**
//	<scenario>
//	Evidence: links pinned to the commit that was read
//	```suggestion block, when there is a sound one
//	<details> Why this was flagged </details>
//	<details> Prompt for your coding agent </details>
//	- [ ] Fix this on the pull request …, when a fix can be asked for (FixBox)
//	<sub>Reply here …</sub>
//	<!-- attest_tag:finding=<id>.<mac> -->
func RenderFinding(f Finding, ctx RenderContext) string {
	p := ctx.policy()
	var blocks []string
	if h := trimBlock(sanitize(ctx.CommentHeader, p, blockMode, MaxHeaderLen)); h != "" {
		blocks = append(blocks, h)
	}
	blocks = append(blocks, fmt.Sprintf("**%s · %s · %s**", ctx.typeNames(f, p, inlineMode), severityLabel(f.Severity), titleText(f.Title, p)))
	if sc := trimBlock(sanitize(f.Scenario, p, blockMode, 2*ScenarioMaxLen)); sc != "" {
		blocks = append(blocks, sc)
	}
	if links := ctx.evidenceLinks(f, p); len(links) > 0 {
		blocks = append(blocks, "Evidence: "+strings.Join(links, " · "))
	}
	if s := suggestionBlock(f, ctx.ReplacedLines); s != "" {
		blocks = append(blocks, s)
	}
	blocks = append(blocks, ctx.whyFlagged(f, p), agentPrompt(f))
	hint := "<sub>Reply here if this is wrong or intended — every reply gets a verdict.</sub>"
	if ctx.FixBox {
		blocks = append(blocks, "- [ ] "+FixBoxText)
		if ctx.Slug != "" {
			// Where the box cannot be ticked — GitHub's newer "Files changed" view has shown task
			// lists in review comments greyed out — the command does the same.
			hint = "<sub>Reply here if this is wrong or intended — every reply gets a verdict — or reply <code>@" + ctx.Slug +
				" fix</code> to have it fixed on this pull request.</sub>"
		}
	}
	blocks = append(blocks, hint)
	if m := Marker(ctx.MarkerKey, ctx.markerScope(), MarkerFinding, ctx.FindingID); m != "" {
		blocks = append(blocks, m)
	}
	return strings.Join(blocks, "\n\n")
}

// trimBlock takes the trailing newlines off a sanitised block, which the caller separates
// from the next one itself.
func trimBlock(s string) string { return strings.TrimRight(s, "\n") }

func severityLabel(s Severity) string {
	if s.Valid() {
		return string(s)
	}
	return string(P2) // an unknown severity is scored as a P2, so it is shown as one
}

// titleText is a finding's title as a line inside our own markup.
func titleText(title string, p *linkPolicy) string {
	if t := sanitize(title, p, inlineMode, 2*TitleMaxLen); strings.TrimSpace(t) != "" {
		return t
	}
	return "(untitled)"
}

// evidenceLinks names each place the finding rests on, linked to the exact commit it was read
// at when that is known and the repository is one links may point into, and named in code
// otherwise.
func (ctx RenderContext) evidenceLinks(f Finding, p *linkPolicy) []string {
	var out []string
	for _, e := range f.Evidence {
		if len(out) == maxEvidence {
			break
		}
		repo := cmp.Or(strings.TrimSpace(e.Repo), ctx.Repo)
		label := lineLabel(e.Path, e.StartLine, e.EndLine)
		if !strings.EqualFold(repo, ctx.Repo) {
			label = repo + " " + label
		}
		item := codeSpan(label)
		if sha := ctx.pin(repo, e.Ref); sha != "" && e.StartLine > 0 {
			anchor := "#L" + strconv.Itoa(e.StartLine)
			if e.EndLine > e.StartLine {
				anchor += "-L" + strconv.Itoa(e.EndLine)
			}
			raw := "https://github.com/" + repo + "/blob/" + sha + "/" + escapeSegments(e.Path) + anchor
			if canon, ok := p.allow(raw); ok {
				item = "[" + item + "](" + canon + ")"
			}
		}
		if item != "" && !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out
}

// pin resolves an evidence ref to the commit the run read it at, or "".
func (ctx RenderContext) pin(repo, ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if shaRef.MatchString(ref) {
		return ref
	}
	own := strings.EqualFold(repo, ctx.Repo)
	var sha string
	switch {
	case own && (ref == "head" || ref == ""):
		sha = ctx.HeadSHA
	case own && ref == "base":
		sha = ctx.BaseSHA
	case ref == "default" || ref == "":
		for r, s := range ctx.DefaultSHAs {
			if strings.EqualFold(r, repo) {
				sha = s
			}
		}
	}
	sha = strings.ToLower(strings.TrimSpace(sha))
	if !shaRef.MatchString(sha) {
		return ""
	}
	return sha
}

func escapeSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// lineLabel is "path:L12" or "path:L12-14".
func lineLabel(path string, start, end int) string {
	switch {
	case start <= 0:
		return path
	case end > start:
		return fmt.Sprintf("%s:L%d-%d", path, start, end)
	}
	return fmt.Sprintf("%s:L%d", path, start)
}

// suggestionBlock renders the finding's suggestion as a GitHub suggestion block, which its
// author can commit with one click — so it is rendered only when it is exactly what it claims
// to be: on the head side, replacing exactly the finding's lines, short, unable to end its own
// fence, and free of anything a reader would not see (a marker, a control character, a
// bidirectional override that makes the code read differently from how it runs, or an invisible
// character that makes an identifier or a string look like one it is not — the other half of
// "Trojan Source"). An invisible character the replaced lines already hold is let through, so a
// suggestion on a file that uses one on purpose is not lost; replaced is those lines, or "".
//
// CI never gets one: a one-click change to a workflow, or to a composite action a workflow runs
// with the same secrets, is a change to what runs with the repository's secrets. Anything
// doubtful is left out rather than repaired, since repairing code is changing what would be
// committed.
func suggestionBlock(f Finding, replaced string) string {
	s := f.Suggestion
	if s == nil || f.Side != Right {
		return ""
	}
	ss, se := s.StartLine, s.Line
	if ss == 0 {
		ss = se
	}
	if fs, fe := f.Range(); ss != fs || se != fe || se-ss+1 > SuggestionMaxLines {
		return ""
	}
	if MatchAny(ciPaths, f.Path) {
		return ""
	}
	code := strings.TrimSuffix(strings.ReplaceAll(s.Code, "\r\n", "\n"), "\n")
	if strings.Contains(code, suggestionFence) || stripMarkers(code) != code || strings.ContainsFunc(code, dropRune) ||
		strings.ContainsFunc(code, func(r rune) bool { return invisibleRune(r) && !strings.ContainsRune(replaced, r) }) ||
		strings.Count(code, "\n")+1 > SuggestionMaxLines {
		return ""
	}
	if code == "" {
		return "```suggestion\n```" // an empty suggestion deletes the lines
	}
	return "```suggestion\n" + code + "\n```"
}

// ciPaths are the files GitHub Actions runs with the repository's secrets: workflows, and the
// composite actions they call, which live under .github/actions by convention and anywhere at all
// as an action.yml.
var ciPaths = []string{".github/workflows/**", ".github/actions/**", "**/action.yml", "**/action.yaml"}

// invisibleRune reports characters that show as nothing, or as a blank, and still change what
// code means: the format characters — zero-width spaces and joiners, the byte-order mark, the soft
// hyphen, the left-to-right and right-to-left marks — and the Hangul fillers, which render as
// blank space and are letters to most compilers.
func invisibleRune(r rune) bool {
	return unicode.Is(unicode.Cf, r) || r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0
}

// whyFlagged is the collapsed explanation under a finding: its category, what it is about, the
// team rules it cites in their own words, and what the verifier made of it.
func (ctx RenderContext) whyFlagged(f Finding, p *linkPolicy) string {
	var items []string
	if f.Category.Valid() {
		items = append(items, "Category: "+string(f.Category))
	}
	if s := codeSpan(f.Symbol); s != "" {
		items = append(items, "About: "+s)
	}
	t, _ := ctx.lookupType(cmp.Or(f.ReviewType, DefaultType))
	for i, id := range f.RuleIDs {
		if i == maxRuleIDs {
			break
		}
		if l, _, ok := t.Skill(id); ok {
			// Named by where it lives, which a reader can open, rather than by anything it says.
			where := codeSpan(l.String())
			if l.Repo == "" {
				where = codeSpan(l.Path) + " in this repository"
			}
			items = append(items, "Skill "+codeSpan(id)+": "+codeSpan(l.Name())+", "+where)
			continue
		}
		item := "Rule " + codeSpan(id)
		if r, ok := t.Rule(id); ok {
			item += ": " + sanitize(r.Text, p, inlineMode, MaxRuleLen)
		}
		items = append(items, item)
	}
	if c := ctx.VerifierConfidence; c > 0 {
		items = append(items, fmt.Sprintf("Verifier confidence: %d/100", min(c, 100)))
	}
	if f.PreExisting {
		items = append(items, "Pre-existing: this pull request did not introduce it, so it does not count against the score.")
	}
	return "<details><summary>Why this was flagged</summary>\n\n- " + strings.Join(items, "\n- ") + "\n\n</details>"
}

// agentPromptTitleLen bounds the title in the agent prompt, and agentPromptPathLen the path.
const (
	agentPromptTitleLen = 60
	agentPromptPathLen  = 120
)

// promptPath is a path that may be repeated in the agent prompt: the characters paths are made
// of in practice, and no space — without one there is no sentence to hide in it.
var promptPath = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)

// agentPrompt is a prompt a reader can hand to a coding agent. Teams point agents at review
// comments, so this block is an instruction to a machine that will act on it, and it is built
// only from fields that cannot carry one: the location, the severity, the title cut short,
// and a fixed sentence. Never the scenario or the evidence, which are long enough to hold an
// injected instruction relayed from the pull request through the model to the agent.
//
// The path is the pull request author's to choose, so it is held to the same standard as the
// title: quoted as a literal, shortened in the middle so its extension survives, and left out
// altogether when it holds a space or anything else a path does not need — the comment sits on
// the file anyway, so the agent loses nothing it cannot see beside the prompt.
func agentPrompt(f Finding) string {
	start, end := f.Range()
	lines := strconv.Itoa(end)
	if start != end {
		lines = fmt.Sprintf("%d-%d", start, end)
	}
	side := "RIGHT (the pull request's version of the file)"
	if f.Side == Left {
		side = "LEFT (the base version; these lines were removed)"
	}
	body := strings.Join([]string{
		"File: " + promptFile(f.Path),
		"Lines: " + lines + ", " + side,
		"Severity: " + severityLabel(f.Severity),
		"Finding: " + plainText(f.Title, agentPromptTitleLen),
		"Check whether this finding is real at this location; if it is, fix it.",
	}, "\n")
	fence := strings.Repeat("`", max(3, longestRun(body, '`')+1))
	return "<details><summary>Prompt for your coding agent</summary>\n\n" + fence + "text\n" + body + "\n" + fence + "\n\n</details>"
}

func promptFile(p string) string {
	p = strings.TrimSpace(p)
	if !promptPath.MatchString(p) {
		return "(not repeated here: the path holds characters a path does not need; it is the file this comment is on)"
	}
	if r := []rune(p); len(r) > agentPromptPathLen {
		half := (agentPromptPathLen - 1) / 2
		p = string(r[:half]) + "…" + string(r[len(r)-(agentPromptPathLen-1-half):])
	}
	return strconv.Quote(p)
}

// FindingStatus is where a finding stands, as review_findings.status stores it.
type FindingStatus string

const (
	FindingOpen      FindingStatus = "open"
	FindingWithdrawn FindingStatus = "withdrawn"
	FindingFixed     FindingStatus = "fixed"
	FindingResolved  FindingStatus = "resolved_by_human"
	FindingOutdated  FindingStatus = "outdated"
	// FindingDisputed is a finding the author pushed back on twice without new evidence. The
	// bot still stands by it, so it is still open and still scored.
	FindingDisputed FindingStatus = "disputed"
	// FindingAcknowledged is a finding accepted in its thread as a known risk — intended, tracked
	// separately, out of this change's scope — by somebody who may settle it. The bot does not say
	// it was wrong, which is what withdrawn says; the team has decided to live with it, so it is no
	// longer scored and is listed apart with the reason it was given.
	FindingAcknowledged FindingStatus = "acknowledged"
)

// Placement is where a finding was posted, as review_findings.placement stores it.
type Placement string

const (
	PlacementInline  Placement = "inline"  // an inline comment on the diff
	PlacementSummary Placement = "summary" // the summary only: outside the diff, or GitHub refused the anchor
)

// Where a finding that is not on the diff was put, as the engine says it (review_findings.place):
// which heading of the summary lists it. Anything else — outside its file's hunks, or an anchor
// GitHub refused — is "Outside the diff".
const (
	PlaceMoreNotes   = "more_notes"            // over the review's caps on comments or minor findings
	PlaceBelowInline = "below_inline_severity" // its review type comments inline only on worse
	PlaceMasked      = "masked"                // in a hunk a credential was masked in
	PlaceUnchanged   = "unchanged_file"        // a P0 in a file the pull request does not change
)

// ReviewStatus is which variant of the summary to render.
type ReviewStatus string

const (
	ReviewDone      ReviewStatus = "done"
	ReviewReviewing ReviewStatus = "reviewing"
	ReviewFailed    ReviewStatus = "failed"
)

// FailReason is why a review failed, in the words the summary may use on a pull request. It is
// a closed set on purpose. The error itself — a provider's reply, a database address, a request
// id — is about the deployment and may land on a public repository; it stays in review_runs.error
// and the console, and the pull request is told only which kind of failure it was.
type FailReason string

const (
	FailBudget   FailReason = "budget"   // the review's money cap, or the organisation's, ran out
	FailModel    FailReason = "model"    // the model provider failed or refused
	FailGitHub   FailReason = "github"   // GitHub refused a read or the post
	FailTimeout  FailReason = "timeout"  // the run outlived its deadline
	FailInternal FailReason = "internal" // anything else
)

// Text is what the summary heading says for the reason, and the channel told of a failure
// (review_notify.go); anything unknown is FailInternal's.
func (r FailReason) Text() string {
	switch r {
	case FailBudget:
		return "the review budget ran out"
	case FailModel:
		return "the model provider did not answer"
	case FailGitHub:
		return "GitHub refused a request"
	case FailTimeout:
		return "it ran out of time"
	}
	return "an internal error; the console has the details"
}

// SummaryState is everything the sticky summary comment says, read from stored state. The
// summary is rendered from this and never from a model's prose about it, so that the score and
// the list move the moment a finding does — withdrawn after a reply, fixed by a push — instead
// of waiting for somebody to re-run a review.
type SummaryState struct {
	// ReviewID is the public id the summary's marker names.
	ReviewID string
	// Status picks the variant; empty is ReviewDone. FailReason is said when it failed.
	Status     ReviewStatus
	FailReason FailReason
	// Shadow marks a review recorded in the console and not posted.
	Shadow bool
	// Summary is the model's account of what the pull request does.
	Summary string
	// Types are the review types the run was asked for, in the order it ran them.
	Types []TypeRun
	// Skills are the skills the run's types followed and could read, so a reader knows whose
	// standards the review held the change to.
	Skills []SkillRead
	// Findings are the pull request's findings in every status; the summary picks.
	Findings []SummaryFinding
	// NotReviewed are the changed files the review did not read.
	NotReviewed []NotReviewedFile
	// FullCoverage is whether every reviewable changed line was read, and InjectionDetected
	// whether the diff carried text written to steer the reviewer; see Score. The zero value
	// of both is the cautious one.
	FullCoverage      bool
	InjectionDetected bool
	// Reviews is how many reviews the pull request has had.
	Reviews int
	// ReviewedSHA is the head the last finished review read, and HeadSHA the pull request's
	// head now (or, while reviewing, the head being reviewed).
	ReviewedSHA string
	HeadSHA     string
	// Rule is the branch rule the pull request fell under, as BranchRule.String writes it: the
	// rule whose settings the review ran with, and whose types unless a person named others.
	Rule    string
	CostUSD float64
	// Paused is a pull request whose automatic reviews are paused, which the footer says along
	// with how to start them again: the reviews nobody asked for stopped, and a reader wondering
	// why the last push was not reviewed should not have to ask. PausedAfter is how many ran
	// before they paused by themselves; 0 is a pause somebody asked for.
	Paused      bool
	PausedAfter int
}

// SkillRead is a skill a run followed: its name, from its SKILL.md or its folder, and where it was
// read, as "owner/name@commit:path", or the path for one in the repository under review. Private is
// one read from another repository that was private, which a summary on a public repository leaves
// out: its readers may not see it.
type SkillRead struct {
	Name    string
	Source  string
	Private bool
}

// maxSummarySkills bounds the skills the summary names; a type links at most MaxTypeSkills.
const maxSummarySkills = 10

// TypeRun is one review type a run was asked for: what it reported, or why it did not run.
type TypeRun struct {
	Key     string
	Summary string
	// Skipped says why the type did not run, e.g. "budget"; empty when it ran.
	Skipped string
}

// NotReviewedFile is a changed file the review did not read, and why: "binary", "too large",
// "budget", "ignored".
type NotReviewedFile struct {
	Path   string
	Reason string
}

// SummaryFinding is a finding as stored, with what the summary needs about where it stands.
type SummaryFinding struct {
	Finding
	ID        string
	Status    FindingStatus
	Placement Placement
	// PossiblyOutdated marks a finding on a file that changed after the commit it was found
	// on, so it was kept off the diff and is listed with a warning instead.
	PossiblyOutdated bool
	// Place is where the engine put a finding that is not on the diff (Place*), which picks its
	// heading; empty is "Outside the diff".
	Place string
	// Note is a finding said and never scored — a credential-shaped value in a test fixture — which
	// is listed under Notes, apart from the open findings and the score.
	Note bool
	// ClaimedFixedSHA is a reply's "fixed in <sha>", not yet checked.
	ClaimedFixedSHA string
	// Reason is why an acknowledged finding was accepted, as the reply that accepted it gave it: a
	// person's words, one line, sanitised where it is shown.
	Reason string
	// AnchorSHA is the commit the finding's line numbers belong to: the head it was raised on, or
	// the later head a re-review found its lines unchanged at and moved it to. Its location links
	// there when no snippet says otherwise — never to a newer head its numbers were not counted in.
	AnchorSHA string
	// ResolvedIn is the commit a later review found a fixed finding fixed at, or an outdated one's
	// code gone at; empty for one that is neither.
	ResolvedIn string
	// CommentURL is the finding's inline comment, which its row links to.
	CommentURL string
	// Snippet is the code the finding points at, as the review read it — masked — so the summary
	// can show the lines the way GitHub shows them above an inline comment. Nil shows none.
	Snippet *Snippet
}

// Snippet is a few numbered lines of one file at one commit: the lines a finding points at and two
// either side. SHA is the commit they were read at, which the finding's location links to; Lines
// are already masked by whoever read them, and are rendered as code, never as markdown.
type Snippet struct {
	SHA   string   `json:"sha,omitempty"`
	Start int      `json:"start"`
	Lines []string `json:"lines"`
}

// open reports whether a finding is still standing: open, or disputed but not withdrawn.
func (f SummaryFinding) open() bool { return Standing(f.Status) }

// maxSummaryRows bounds each list in the summary; the console has the rest.
const maxSummaryRows = 25

// RenderSummary renders the sticky summary comment from stored state:
//
//	<!-- attest_tag:review=<id>.<mac> -->
//	### attest_tag review · Confidence 3/5 (advisory)
//	one sentence on the worst open finding
//	<details open> Summary · Open findings (n) </details>
//	<details> a section per review type, when more than one ran </details>
//	<details> Outside the diff · More notes · Beside a masked credential · In unchanged files ·
//	          Notes · Pre-existing · Acknowledged · Not reviewed · Possibly outdated · Fixed ·
//	          Outdated </details>
//	<sub>Reviews (n) · Last reviewed abc1234 · …</sub>
//	<!-- attest_tag:state sha=… score=… open=… -->
//
// The score is computed here, from the open findings, with Score. A failed review shows no
// score at all, rather than the last good one next to the word "failed". Rendering the same
// state twice gives the same text, so an edit that changes nothing can be skipped.
func RenderSummary(s SummaryState, ctx RenderContext) string {
	p := ctx.policy()
	findings := slices.Clone(s.Findings)
	slices.SortStableFunc(findings, compareFindings)

	var open, pre, outside, outdated, more, masked, unchanged, notes, fixed, gone, acked []SummaryFinding
	for _, f := range findings {
		if !f.open() {
			// What a later review closed is listed apart, collapsed, so a push that fixed something is
			// seen to have; withdrawn and resolved ones were settled in their threads, and are not. An
			// acknowledged one is listed too: the risk is still in the code, and whoever reads the
			// summary before merging should see that somebody accepted it, and why.
			switch f.Status {
			case FindingFixed:
				fixed = append(fixed, f)
			case FindingOutdated:
				gone = append(gone, f)
			case FindingAcknowledged:
				acked = append(acked, f)
			}
			continue
		}
		switch {
		case f.Note:
			notes = append(notes, f)
			continue
		case f.PreExisting:
			pre = append(pre, f)
			continue
		case f.PossiblyOutdated:
			outdated = append(outdated, f)
		case f.Placement != PlacementInline:
			switch f.Place {
			case PlaceMoreNotes, PlaceBelowInline:
				more = append(more, f)
			case PlaceMasked:
				masked = append(masked, f)
			case PlaceUnchanged:
				unchanged = append(unchanged, f)
			default:
				outside = append(outside, f)
			}
		}
		open = append(open, f)
	}
	plain := make([]Finding, len(open))
	for i, f := range open {
		plain[i] = f.Finding
	}
	score := Score(plain, s.FullCoverage, s.InjectionDetected)
	status := cmp.Or(s.Status, ReviewDone)

	var blocks []string
	if m := Marker(ctx.MarkerKey, ctx.markerScope(), MarkerReview, s.ReviewID); m != "" {
		blocks = append(blocks, m+"\n"+summaryHeading(s, status, score))
	} else {
		blocks = append(blocks, summaryHeading(s, status, score))
	}
	if s.Shadow {
		blocks = append(blocks, "<sub>Shadow review: recorded in the console, not posted to the pull request.</sub>")
	}
	// A failed review has said nothing it can stand behind, and a first review still running has
	// not said anything yet; neither gets a sentence about what stands in the way of merging.
	if status == ReviewDone || (status == ReviewReviewing && s.ReviewedSHA != "") {
		lead := riskSentence(open)
		if status == ReviewDone && score == 4 && Score(plain, true, false) == 5 {
			lead += " " + capReason(s)
		}
		blocks = append(blocks, lead)
	}

	ran := ranTypes(s.Types)
	summary := s.Summary
	if strings.TrimSpace(summary) == "" && len(ran) == 1 {
		summary = ran[0].Summary
	}
	if t := trimBlock(sanitize(summary, p, blockMode, 4000)); t != "" {
		blocks = append(blocks, "<details open><summary>Summary</summary>\n\n"+t+"\n\n</details>")
	}
	if len(open) > 0 {
		blocks = append(blocks, section(true, fmt.Sprintf("Open findings (%d)", len(open)), ctx.rows(open, p)))
	}
	if len(ran) > 1 {
		// One list, each finding tagged with its types, and a count per type under it: a section
		// per type repeated every finding of two types twice more, code and all.
		var counts []string
		for _, tr := range ran {
			n := 0
			for _, f := range open {
				if slices.Contains(f.TypeKeys(), tr.Key) {
					n++
				}
			}
			counts = append(counts, fmt.Sprintf("%s %d", ctx.typeName(tr.Key, p, htmlInlineMode), n))
		}
		blocks = append(blocks, "<sub>Reviewed as "+joinNames(ctx.typeNames2(ran, p))+" · open: "+strings.Join(counts, " · ")+"</sub>")
	}
	if len(s.Skills) > 0 && len(ran) > 0 {
		var named []string
		for _, k := range s.Skills {
			if ctx.PublicRepo && k.Private {
				continue
			}
			if len(named) == maxSummarySkills {
				named = append(named, "more")
				break
			}
			if name := codeSpan(k.Name); name != "" {
				named = append(named, name+" ("+codeSpan(k.Source)+")")
			}
		}
		if len(named) > 0 {
			blocks = append(blocks, "<sub>Followed skills: "+strings.Join(named, " · ")+"</sub>")
		}
	}
	if len(outside) > 0 {
		blocks = append(blocks, section(false, fmt.Sprintf("Outside the diff (%d)", len(outside)), ctx.entries(outside, p)))
	}
	for _, sec := range []struct {
		title, why string
		fs         []SummaryFinding
	}{
		{"More notes", "Over this review's limit on inline comments, or less severe than its review type comments on inline.", more},
		{"Beside a masked credential", "In a part of the diff where this review masked a credential, so not commented on inline.", masked},
		{"In unchanged files", "In files this pull request does not change; only the most severe findings are listed there.", unchanged},
	} {
		if len(sec.fs) > 0 {
			body := "<sub>" + sec.why + "</sub>\n\n" + ctx.entries(sec.fs, p)
			blocks = append(blocks, section(false, fmt.Sprintf("%s (%d)", sec.title, len(sec.fs)), body))
		}
	}
	if len(notes) > 0 {
		body := ctx.entries(notes, p) + "\n\n<sub>Said for somebody to check, and not scored.</sub>"
		blocks = append(blocks, section(false, fmt.Sprintf("Notes (%d)", len(notes)), body))
	}
	if len(pre) > 0 {
		body := ctx.entries(pre, p) + "\n\n<sub>Not introduced by this pull request, so not scored.</sub>"
		blocks = append(blocks, section(false, fmt.Sprintf("Pre-existing (%d)", len(pre)), body))
	}
	if len(acked) > 0 {
		body := "<sub>Accepted in their threads as known risks, so not scored.</sub>\n\n" + ctx.acknowledgedRows(acked, p)
		blocks = append(blocks, section(false, fmt.Sprintf("Acknowledged (%d)", len(acked)), body))
	}
	if nr := ctx.notReviewed(s, p); nr != "" {
		blocks = append(blocks, nr)
	}
	if len(outdated) > 0 {
		body := ctx.entries(outdated, p)
		if sha := shortSHA(s.ReviewedSHA); sha != "" {
			body = "<sub>These files changed after `" + sha + "` was reviewed, so these findings may no longer apply.</sub>\n\n" + body
		}
		blocks = append(blocks, section(false, fmt.Sprintf("Possibly outdated (%d)", len(outdated)), body))
	}
	if len(fixed) > 0 {
		blocks = append(blocks, section(false, fmt.Sprintf("Fixed (%d)", len(fixed)), ctx.closedRows(fixed, p, "fixed in")))
	}
	if len(gone) > 0 {
		body := "<sub>The code these were about is no longer in the pull request, so they no longer apply and are not scored.</sub>\n\n" +
			ctx.closedRows(gone, p, "gone at")
		blocks = append(blocks, section(false, fmt.Sprintf("Outdated (%d)", len(gone)), body))
	}
	blocks = append(blocks, ctx.footer(s, status, p)+"\n"+stateMarker(s, status, score, len(open)))
	return strings.Join(blocks, "\n\n")
}

func summaryHeading(s SummaryState, status ReviewStatus, score int) string {
	const h = "### attest_tag review · "
	switch status {
	case ReviewReviewing:
		if sha := shortSHA(s.HeadSHA); sha != "" {
			return h + "Reviewing `" + sha + "`…"
		}
		return h + "Reviewing…"
	case ReviewFailed:
		return h + "review failed: " + s.FailReason.Text()
	}
	return h + fmt.Sprintf("Confidence %d/5 (advisory)", score)
}

// riskSentence says what stands between the pull request and merging, from the worst open
// finding. open is sorted worst first.
func riskSentence(open []SummaryFinding) string {
	if len(open) == 0 {
		return "No blocking issues found."
	}
	top := open[0]
	what := fmt.Sprintf("**%s** (%s, %s)", titleText(top.Title, nil), severityLabel(top.Severity), findingLocation(top.Finding))
	if n := len(open) - 1; n > 0 {
		what += fmt.Sprintf(" and %d more", n)
	}
	switch top.Severity {
	case P0:
		return "Do not merge yet: " + what + "."
	case P1:
		return "Merge after fixing " + what + "."
	}
	return "Only minor findings: " + what + "."
}

// capReason says why a review with nothing open scored 4 and not 5.
func capReason(s SummaryState) string {
	var why []string
	if !s.FullCoverage {
		why = append(why, "not every changed line was reviewed")
	}
	if s.InjectionDetected {
		why = append(why, "this pull request contains text written to steer the reviewer")
	}
	return "The score is capped at 4 because " + strings.Join(why, ", and ") + "."
}

func ranTypes(types []TypeRun) []TypeRun {
	var ran []TypeRun
	for _, t := range types {
		if strings.TrimSpace(t.Skipped) == "" {
			ran = append(ran, t)
		}
	}
	return ran
}

// section wraps a body in a collapsible block, with the blank lines that make GitHub render
// the markdown inside it.
func section(open bool, title, body string) string {
	tag := "<details>"
	if open {
		tag = "<details open>"
	}
	return tag + "<summary>" + title + "</summary>\n\n" + body + "\n\n</details>"
}

// rows is the open-findings list, one line per finding, worst first, each reading
// "**P1** · Security · Tenant check missing on list query · `api/list.go:L40-52` · open".
func (ctx RenderContext) rows(fs []SummaryFinding, p *linkPolicy) string {
	var lines []string
	for i, f := range fs {
		if i == maxSummaryRows {
			lines = append(lines, fmt.Sprintf("- …and %d more in the console", len(fs)-i))
			break
		}
		row := fmt.Sprintf("**%s** · %s · %s · %s · %s", severityLabel(f.Severity),
			ctx.typeNames(f.Finding, p, inlineMode), threadTitle(f, p), ctx.location(f, p), statusLabel(f))
		if code := ctx.snippetBlock(f); code != "" {
			// A row with code is a paragraph, not a list item: a fenced block inside a list item
			// renders only when indented to the item, and a collapsed one inside it not at all.
			lines = append(lines, row+"\n<details><summary>Code</summary>\n\n"+code+"\n\n</details>")
			continue
		}
		lines = append(lines, "- "+row)
	}
	return strings.Join(lines, "\n\n")
}

// threadTitle is a finding's title, linked to its inline comment's thread when it has one.
func threadTitle(f SummaryFinding, p *linkPolicy) string {
	if f.CommentURL != "" {
		if canon, ok := p.allow(f.CommentURL); ok {
			// Inside a link the title may hold no link of its own.
			return "[" + titleText(f.Title, nil) + "](" + canon + ")"
		}
	}
	return titleText(f.Title, p)
}

// closedRows lists findings a later review closed, one line each with the commit that closed them:
// "**P1** · General · Scan error ignored · `store.go:L11` · fixed in `abc1234`". No code: what was
// wrong is in the thread, and the code it was about has changed or gone.
func (ctx RenderContext) closedRows(fs []SummaryFinding, p *linkPolicy, how string) string {
	var lines []string
	for i, f := range fs {
		if i == maxSummaryRows {
			lines = append(lines, fmt.Sprintf("- …and %d more in the console", len(fs)-i))
			break
		}
		row := fmt.Sprintf("- **%s** · %s · %s · %s", severityLabel(f.Severity), ctx.typeNames(f.Finding, p, inlineMode),
			threadTitle(f, p), ctx.location(f, p))
		if sha := shortSHA(f.ResolvedIn); sha != "" {
			row += " · " + how + " `" + sha + "`"
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// acknowledgedRows lists findings accepted as known risks, one line each with the reason given:
// "**P1** · General · Scan error ignored · `store.go:L11` · “tracked separately”". The reason is a
// person's words, sanitised like the model's and kept to a line.
func (ctx RenderContext) acknowledgedRows(fs []SummaryFinding, p *linkPolicy) string {
	var lines []string
	for i, f := range fs {
		if i == maxSummaryRows {
			lines = append(lines, fmt.Sprintf("- …and %d more in the console", len(fs)-i))
			break
		}
		row := fmt.Sprintf("- **%s** · %s · %s · %s", severityLabel(f.Severity), ctx.typeNames(f.Finding, p, inlineMode),
			threadTitle(f, p), ctx.location(f, p))
		if why := strings.TrimSpace(sanitize(strings.Join(strings.Fields(f.Reason), " "), p, inlineMode, maxReasonLen)); why != "" {
			row += " · “" + why + "”"
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// maxReasonLen bounds an acknowledged finding's reason in the summary: a line, not a paragraph.
const maxReasonLen = 200

// entries lists findings with their scenario, for the sections whose findings have no inline
// comment to read it in.
func (ctx RenderContext) entries(fs []SummaryFinding, p *linkPolicy) string {
	var parts []string
	for i, f := range fs {
		if i == maxSummaryRows {
			parts = append(parts, fmt.Sprintf("…and %d more in the console.", len(fs)-i))
			break
		}
		head := fmt.Sprintf("**%s · %s · %s** · %s", ctx.typeNames(f.Finding, p, inlineMode), severityLabel(f.Severity),
			titleText(f.Title, p), ctx.location(f, p))
		if f.Placement == PlacementInline && !f.PossiblyOutdated {
			parts = append(parts, head)
			continue
		}
		// No inline comment shows this one's code, so the entry does, the way the diff would.
		if code := ctx.snippetBlock(f); code != "" {
			head += "\n\n" + code
		}
		if sc := trimBlock(sanitize(f.Scenario, p, blockMode, 2*ScenarioMaxLen)); sc != "" {
			head += "\n\n" + sc
		}
		parts = append(parts, head)
	}
	return strings.Join(parts, "\n\n")
}

// typeNames2 is the names of the types that ran, in order.
func (ctx RenderContext) typeNames2(ran []TypeRun, p *linkPolicy) []string {
	names := make([]string, 0, len(ran))
	for _, tr := range ran {
		names = append(names, ctx.typeName(tr.Key, p, htmlInlineMode))
	}
	return names
}

// joinNames is "General", "General and Security", "General, Security and Tests".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func (ctx RenderContext) notReviewed(s SummaryState, p *linkPolicy) string {
	var lines []string
	for i, f := range s.NotReviewed {
		if i == maxSummaryRows {
			lines = append(lines, fmt.Sprintf("- …and %d more in the console", len(s.NotReviewed)-i))
			break
		}
		line := "- " + cmp.Or(codeSpan(f.Path), "(unnamed file)")
		if why := sanitize(f.Reason, p, inlineMode, 80); strings.TrimSpace(why) != "" {
			line += " — " + why
		}
		lines = append(lines, line)
	}
	skipped := 0
	for _, t := range s.Types {
		if why := strings.TrimSpace(t.Skipped); why != "" {
			skipped++
			lines = append(lines, "- "+ctx.typeName(t.Key, p, inlineMode)+" review — not run: "+sanitize(why, p, inlineMode, 80))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	counts := plural(len(s.NotReviewed), "file", "files")
	switch {
	case skipped > 0 && len(s.NotReviewed) == 0:
		counts = plural(skipped, "review type", "review types")
	case skipped > 0:
		counts += ", " + plural(skipped, "review type", "review types")
	}
	return section(false, "Not reviewed ("+counts+")", strings.Join(lines, "\n"))
}

func (ctx RenderContext) footer(s SummaryState, status ReviewStatus, p *linkPolicy) string {
	parts := []string{fmt.Sprintf("Reviews (%d)", s.Reviews)}
	reviewed := shortSHA(s.ReviewedSHA)
	if reviewed != "" {
		parts = append(parts, "Last reviewed `"+reviewed+"`")
	}
	if head := shortSHA(s.HeadSHA); status != ReviewReviewing && head != "" && reviewed != "" &&
		!strings.EqualFold(s.HeadSHA, s.ReviewedSHA) {
		moved := "head `" + head + "` not reviewed"
		if slug := strings.TrimPrefix(strings.TrimSpace(ctx.Slug), "@"); validAppSlug(slug) {
			// In code, so a GitHub user who happens to share the App's slug is not mentioned.
			moved += " — `@" + slug + " review`"
		}
		parts = append(parts, moved)
	}
	if rule := codeSpan(s.Rule); rule != "" {
		parts = append(parts, "Rule: "+rule)
	}
	if s.Paused {
		paused := "Automatic reviews paused"
		if s.PausedAfter > 0 {
			paused += fmt.Sprintf(" after %d", s.PausedAfter)
		}
		if slug := strings.TrimPrefix(strings.TrimSpace(ctx.Slug), "@"); validAppSlug(slug) {
			// In code, as "@slug review" above is, so nobody who shares the App's slug is mentioned.
			paused += " — `@" + slug + " resume`"
		}
		parts = append(parts, paused)
	}
	if ctx.ShowCost && !ctx.PublicRepo {
		if c := max(s.CostUSD, 0); c > 0 && c < 0.005 {
			parts = append(parts, "cost <$0.01") // "$0.00" reads as free, which it was not
		} else {
			parts = append(parts, fmt.Sprintf("cost $%.2f", c))
		}
	}
	if u, ok := realConsoleURL(ctx.ConsoleURL); ok {
		if canon, ok := p.allow(u.String()); ok {
			parts = append(parts, "[details ↗]("+canon+")")
		}
	}
	return "<sub>" + strings.Join(parts, " · ") + "</sub>"
}

// stateMarker is an informational line for whoever reads the raw comment: which commit the
// summary describes, its score and how many findings are open. It is not signed, and nothing
// trusts it; the database is the state.
func stateMarker(s SummaryState, status ReviewStatus, score, open int) string {
	sha := "-"
	if shaRef.MatchString(strings.ToLower(s.ReviewedSHA)) {
		sha = strings.ToLower(s.ReviewedSHA)
	}
	sc := "-"
	if status == ReviewDone {
		sc = strconv.Itoa(score)
	}
	return fmt.Sprintf("<!-- attest_tag:state sha=%s score=%s open=%d -->", sha, sc, open)
}

// validAppSlug reports whether s is shaped like a GitHub App's slug — lowercase letters,
// digits and hyphens — which is all the footer's "@slug review" may hold.
func validAppSlug(s string) bool {
	return s != "" && len(s) <= 100 && !strings.ContainsFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '-')
	})
}

func statusLabel(f SummaryFinding) string {
	label := "open"
	if f.Status == FindingDisputed {
		label = "disputed"
	}
	if sha := shortSHA(f.ClaimedFixedSHA); sha != "" {
		label = "claimed fixed in `" + sha + "`"
	}
	if f.PossiblyOutdated {
		label += ", possibly outdated"
	}
	return label
}

// findingLocation is a finding's place as inline code: `path:L12-14`, with "(base)" for lines
// that exist only in the base.
func findingLocation(f Finding) string {
	start, end := f.Range()
	label := lineLabel(f.Path, start, end)
	if f.Side == Left {
		label += " (base)"
	}
	return codeSpan(label)
}

// location is findingLocation linked to those lines at the commit they were counted in — its
// snippet's, else the base for a finding on deleted lines, else its anchor, else the reviewed head
// — and plain when there is no commit to pin it to: a link to a branch shows other code tomorrow.
// The head reviewed now is the last resort, not the first: a finding raised two pushes ago and
// never moved has its first head's numbers, and those lines at today's head are other code.
func (ctx RenderContext) location(f SummaryFinding, p *linkPolicy) string {
	loc := findingLocation(f.Finding)
	sha := ctx.HeadSHA
	switch {
	case f.Snippet != nil && shortSHA(f.Snippet.SHA) != "":
		sha = f.Snippet.SHA
	case f.Side == Left:
		sha = ctx.BaseSHA
	case shortSHA(f.AnchorSHA) != "":
		sha = f.AnchorSHA
	}
	start, end := f.Range()
	if ctx.Repo == "" || shortSHA(sha) == "" || end <= 0 || strings.TrimSpace(f.Path) == "" {
		return loc
	}
	anchor := "#L" + strconv.Itoa(max(start, 1))
	if end > start && start > 0 {
		anchor += "-L" + strconv.Itoa(end)
	}
	raw := "https://github.com/" + ctx.Repo + "/blob/" + sha + "/" + escapeSegments(f.Path) + anchor
	if canon, ok := p.allow(raw); ok {
		return "[" + loc + "](" + canon + ")"
	}
	return loc
}

// snippetMaxLines and snippetMaxRunes bound a summary's code: enough to see the problem, with the
// link for the rest.
const (
	snippetMaxLines = 14
	snippetMaxRunes = 200
)

// snippetBlock renders a finding's snippet as a fenced block with line numbers, the finding's own
// lines marked "›". The lines are a file's text, which the pull request wrote: they are only ever
// code here, the fence is longer than any run of backticks in them, markers and the characters
// that reorder text are removed, and each line is cut short.
func (ctx RenderContext) snippetBlock(f SummaryFinding) string {
	sn := f.Snippet
	if sn == nil || sn.Start <= 0 || len(sn.Lines) == 0 {
		return ""
	}
	lines := sn.Lines
	if len(lines) > snippetMaxLines {
		lines = lines[:snippetMaxLines]
	}
	start, end := f.Range()
	start = max(start, 1)
	w := len(strconv.Itoa(sn.Start + len(lines) - 1))
	var b strings.Builder
	longest := 0
	for i, l := range lines {
		n := sn.Start + i
		mark := " "
		if n >= start && n <= end {
			mark = "›"
		}
		text := strings.Map(func(r rune) rune {
			if dropRune(r) && r != '\t' {
				return -1
			}
			return r
		}, stripMarkers(strings.TrimRight(l, "\r\n")))
		text = cutRunes(text, snippetMaxRunes)
		longest = max(longest, longestRun(text, '`'))
		fmt.Fprintf(&b, "%*d %s %s\n", w, n, mark, text)
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + snippetLang(f.Path) + "\n" + b.String() + fence
}

// snippetLang names a fenced block's language from the file's extension, for GitHub's colouring;
// an unknown one gets none rather than a guess.
func snippetLang(p string) string {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".go", ".py", ".rb", ".rs", ".java", ".kt", ".swift", ".php", ".sql", ".sh", ".tsx", ".jsx", ".ts", ".js", ".cs",
		".c", ".h", ".cpp", ".scala", ".yaml", ".yml", ".json", ".toml", ".css", ".scss", ".html", ".vue", ".dart":
		return strings.TrimPrefix(ext, ".")
	case ".mjs", ".cjs":
		return "js"
	}
	return ""
}

// shortSHA is a commit's first seven characters, or "" for anything that is not a commit sha.
func shortSHA(sha string) string {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if !shaRef.MatchString(sha) {
		return ""
	}
	return sha[:7]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// compareFindings orders findings worst first, then by place, so a summary lists them the same
// way however they were loaded.
func compareFindings(a, b SummaryFinding) int {
	return cmp.Or(
		cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)),
		cmp.Compare(a.Path, b.Path),
		cmp.Compare(a.StartLine, b.StartLine),
		cmp.Compare(a.Line, b.Line),
		cmp.Compare(a.Title, b.Title),
		cmp.Compare(a.ID, b.ID),
	)
}

func severityRank(s Severity) int {
	switch s {
	case P0:
		return 0
	case P1:
		return 1
	case P2:
		return 2
	}
	return 3
}
