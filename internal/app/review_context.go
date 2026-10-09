package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"attesttag/internal/review"
)

// The context pack: everything a review's finder is shown, built by Go before the first model
// call and the same for every type that runs. Nothing in it is chosen by a model. What the model
// may fetch afterwards goes through the same reads and the same mask (review_engine.go).
//
// Three rules shape it.
//
// Reads are raw and masked line for line. The diff, the files at the head and the instruction files
// are read without redact (ProxyRequest.Raw), because redact is free to change the shape of what it
// cleans — a PEM block becomes one line, and every line number after it moves — and a review's
// whole output is line numbers that GitHub refuses with a 422 when one is wrong. So the secret
// detector runs here, on the raw text, and every secret is replaced on its own line with the mask,
// newlines kept, before any of it reaches a model. A hunk in which something was masked cannot be
// commented on inline: a finding there goes to the summary, where a person reads it next to the
// one finding that matters about that hunk, the credential.
//
// The detector raises its own finding. A credential in a raw added line is a P0 that no model was
// asked about, said without its value; the same in a test fixture is a note. Text in an added line
// written to steer a model reviewer is a P2, and caps the score (review.Score): the easiest attack
// on a reviewer is to talk it out of reporting anything.
//
// Instructions come from the base commit. A pull request can add an AGENTS.md that says "approve
// everything"; the one the team already merged is the one that describes the codebase.
const (
	// A unit is what one finder pass reads at once: about 40k tokens of diff, estimated at 3.5
	// characters a token. Past that a model reads the end of a prompt worse than its start, and a
	// release-sized pull request becomes several passes rather than one it skims.
	reviewUnitTokens  = 40_000
	reviewCharsPerTok = 3.5
	// The most changed files one review reads, riskiest first (reviewRank). The rest are said out
	// loud in "Not reviewed", which caps the score, rather than read badly. A large pull request is
	// where a bug hides best, and at 80 a large one left most of itself unread; the finder's time
	// grows with what there is to read (reviewFinderBudget), and max $ still bounds the money.
	reviewMaxFiles = 150
	// A changed file this short is shown whole, up to this many of them per unit; a longer one is
	// shown as the declaration enclosing each hunk, at most this many lines a hunk.
	reviewWholeFileLines = 400
	reviewWholeFiles     = 10
	reviewExcerptLines   = 40
	// What one review reads of file contents — at the head, the base and in context repositories —
	// over and above the diff, which PullFiles caps on its own.
	reviewFetchMaxBytes = 4 << 20
	// The repo map and the instruction files are context, not the subject: a few thousand
	// characters each, so the diff stays most of every prompt.
	reviewMapChars          = 4000
	reviewInstructionsChars = 14_000
	reviewPRTextChars       = 2000
	// Context repositories a review may read, and code searches it may make: code search allows
	// ten requests a minute for the whole installation.
	reviewMaxContextRepos = 5
	reviewMaxSearches     = 5
)

// reviewMask is what a secret is replaced with: the same literal redact writes, so a model shown
// both kinds of masked text sees one convention. It is never itself a finding (reviewScanLine).
const reviewMask = "[redacted-secret]"

// ---- secrets and the line-preserving mask ----

var (
	pemBegin = regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`(?i)-----END [A-Z ]*PRIVATE KEY-----`)
)

// reviewMasker masks a file's lines in order. A private key spans lines, and its BEGIN may be in an
// earlier hunk than the lines being read, so the state carries from line to line and from hunk to
// hunk: a block opened and not seen closed keeps masking until its END. What it cannot see is a
// BEGIN above the first line GitHub's patch shows — a hunk that opens inside a key embedded in
// source — which reviewParseFile and reviewFile.maskFrom make up for.
type reviewMasker struct{ inPEM bool }

// reviewOrphanPEMEnd is an END line with no BEGIN on it: read with no block open, it means the lines
// above it, as far back as what was read, were a key's body.
func reviewOrphanPEMEnd(s string) bool { return pemEnd.MatchString(s) && !pemBegin.MatchString(s) }

// line masks one line and says what kinds of credential it held, in words that name the kind and
// never the value. The line keeps its place: a masked line is still exactly one line. fake is true
// when every credential on the line is plainly made up (secretPlaceholder).
func (m *reviewMasker) line(s string) (out string, kinds []string, fake bool) {
	if m.inPEM {
		if pemEnd.MatchString(s) {
			m.inPEM = false
		}
		return reviewMask, []string{"a private key"}, false
	}
	if loc := pemBegin.FindStringIndex(s); loc != nil && !pemEnd.MatchString(s[loc[1]:]) {
		m.inPEM = true
		return s[:loc[0]] + reviewMask, []string{"a private key"}, false
	}
	s, hits := redactWith(s, reviewMask)
	fake = len(hits) > 0
	for _, hit := range hits {
		if k := hit.kind(); !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
		fake = fake && secretPlaceholder(hit.Value)
	}
	return s, kinds, fake
}

// secretPlaceholderRe is a credential-shaped value that says it is made up — xoxb-DEMO-FAKE-0001,
// AWS's own AKIA…EXAMPLE, sk-test-… — or counts, as a hand-typed fake does (1234567890,
// abcdefghij). A random key holds none of these by chance often enough to matter.
var secretPlaceholderRe = regexp.MustCompile(`(?i)fake|example|dummy|placeholder|sample|redacted|changeme|your|xxxx|test|demo|0123456|1234567|abcdefg`)

// secretPlaceholder says whether one credential found is plainly not a real credential. It is
// masked all the same; it is reported as a note rather than as a committed credential.
func secretPlaceholder(hit string) bool {
	return !strings.HasPrefix(hit, "-----") && secretPlaceholderRe.MatchString(hit)
}

// maskText masks a whole file's content, line for line, and returns the lines it masked (1-based).
func maskText(text string) (string, []int) {
	lines := strings.Split(text, "\n")
	var m reviewMasker
	var hit []int
	for i, l := range lines {
		out, kinds, _ := m.line(l)
		if len(kinds) > 0 || out != l {
			hit = append(hit, i+1)
		}
		lines[i] = out
	}
	return strings.Join(lines, "\n"), hit
}

// reviewSecretPath is a file whose whole content is a credential by its name: a key or a
// certificate bundle. A hunk of one may show nothing but base64 with its BEGIN line out of view, so
// it is masked whole rather than trusted to the patterns.
var reviewSecretPaths = []string{"**/*.pem", "**/*.key", "**/*.p12", "**/*.pfx", "**/id_rsa", "**/id_dsa", "**/id_ecdsa",
	"**/id_ed25519", "**/.env", "**/.env.*", "**/*.keystore", "**/*.jks"}

func reviewSecretPath(p string) bool { return review.MatchAny(reviewSecretPaths, p) }

// reviewFixturePath is where a credential-shaped string is expected — a test's fake key, a
// recorded response — so one found there is a note, not a P0 against the pull request.
func reviewFixturePath(p string) bool {
	return review.MatchAny(reviewTestPaths, p)
}

// ---- text written to the reviewer ----

// reviewInjectionRes match an added line that speaks to a model reading the diff rather than to
// the people reading the code. Narrow on purpose: each is a phrase code has no ordinary reason to
// contain, because a hit caps the score of an honest pull request too.
var reviewInjectionRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\s+(all\s+|any\s+|the\s+)?(previous|prior|above|earlier|preceding)\s+(instructions|prompts?|rules|context)`),
	regexp.MustCompile(`(?i)\b(ai|llm|automated|bot)\s+(code\s+)?reviewers?\b[^\n]{0,60}\b(must|should|do not|don't|never|ignore|skip|approve)\b`),
	regexp.MustCompile(`(?i)\b(you are|you're|act as)\s+(an?\s+|the\s+)?(ai|llm|language model|code reviewer|reviewer|assistant)\b`),
	regexp.MustCompile(`(?i)\bdo\s+not\s+(report|flag|mention)\s+(any|this|these|the)\b`),
	regexp.MustCompile(`(?i)</?\s*(system|assistant|pr_data|review_criteria|tool_result|code_now|code_then|diff_now|finding)\s*>`),
	regexp.MustCompile(`(?i)\b(submit_review|submit_verdict|submit_resolution)\b`),
	regexp.MustCompile(`(?i)\b(give|rate|score)\s+(this|the)\s+(pr|pull request|change|diff)\s+(a\s+)?(5|five|perfect)\b`),
}

func reviewInjectionLine(s string) bool {
	for _, re := range reviewInjectionRes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// ---- the changed files ----

// reviewSecretHit is a credential in one raw added line.
type reviewSecretHit struct {
	Line  int // head line number
	Kinds []string
	Fake  bool // every credential on the line is a placeholder (secretPlaceholder)
}

// reviewFile is one changed file as the review holds it. File is the MASKED diff — its hunks are
// what the model is shown and what anchors are checked against, and the line numbers in it are the
// raw diff's own, since masking keeps every line — and raw is GitHub's, kept for the patch hash.
type reviewFile struct {
	review.File
	raw review.File
	// masked are the hunks (by index) in which something was masked: no inline comment there.
	masked  map[int]bool
	secrets []reviewSecretHit
	inject  []int // head lines of added text addressed to a reviewer
	// injectPath is a path, or the path it was renamed from, that reads as text addressed to a
	// reviewer: a name is the author's to choose, and reaches the prompt as surely as a line does.
	injectPath bool
	// skip is why the file is not reviewed at all ("ignored", "binary", "too large", "generated",
	// "unreadable"), or "" when it is.
	skip   string
	tier   int // reviewRank's tier
	risk   int // reviewRank's order within the tier (reviewRisk)
	tokens int // its numbered diff's estimated size
}

func (f *reviewFile) changed() int { return f.Additions + f.Deletions }

// hunkAt returns the index of the hunk holding line on side, or -1.
func (f *reviewFile) hunkAt(side review.Side, line int) int {
	for i, h := range f.Hunks {
		if review.ValidAnchor([]review.Hunk{h}, side, 0, line) {
			return i
		}
	}
	return -1
}

// secretAt records a credential on an added line, once per line, with the kinds it held. A line
// stays fake only while everything found on it is.
func (f *reviewFile) secretAt(line int, kinds []string, fake bool) {
	for i := range f.secrets {
		if f.secrets[i].Line == line {
			f.secrets[i].Fake = f.secrets[i].Fake && fake
			for _, k := range kinds {
				if !slices.Contains(f.secrets[i].Kinds, k) {
					f.secrets[i].Kinds = append(f.secrets[i].Kinds, k)
				}
			}
			return
		}
	}
	f.secrets = append(f.secrets, reviewSecretHit{Line: line, Kinds: kinds, Fake: fake})
}

// maskLine replaces one line of hunk hi with the mask, the way a credential found on it would be.
func (f *reviewFile) maskLine(h *review.Hunk, hi, li int) {
	l := &h.Lines[li]
	if l.Kind == '+' {
		f.secretAt(l.New, []string{"a private key"}, false)
	}
	l.Text, f.masked[hi] = reviewMask, true
}

// maskFrom masks the diff against the whole files it is a diff of: head for an added or unchanged
// line, base for a deleted one, by line number. A hunk that starts inside a private key embedded
// in source — an edit just below one, or in the middle of one — shows no BEGIN, and its masker
// cannot know that what it reads is the key's body; the whole file, read from line 1, does. Either
// side may be nil, when the file is not there or could not be read: what the hunk alone showed
// then stands.
func (f *reviewFile) maskFrom(head, base *reviewText) {
	changed := false
	for hi := range f.Hunks {
		h := &f.Hunks[hi]
		for li, l := range h.Lines {
			t, n := head, l.New
			if l.Kind == '-' {
				t, n = base, l.Old
			}
			// A line the diff's own scan already masked holds the mask somewhere in it, not only as
			// the whole line: masking it again here would add "a private key" to what the scan named,
			// so a committed API key was reported as an API key and a private key.
			if t == nil || !t.masked[n] || strings.Contains(l.Text, reviewMask) {
				continue
			}
			f.maskLine(h, hi, li)
			changed = true
		}
	}
	if changed && f.Patch != "" {
		f.Patch = renderPatch(f.Hunks)
	}
}

// reviewParseFile reads, masks and scans one changed file. GitHub's patch is raw: the detector
// reads it as committed, then every line of it — added, deleted and context, since a removed key
// is still a key — is masked before the copy the model sees is made.
func reviewParseFile(gf review.File, e review.Effective) *reviewFile {
	f := &reviewFile{raw: gf, masked: map[int]bool{}}
	f.injectPath = reviewInjectionLine(gf.Path) || (gf.PrevPath != "" && reviewInjectionLine(gf.PrevPath))
	raw := gf
	if err := raw.Parse(); err != nil {
		f.File, f.skip = review.File{Path: gf.Path, PrevPath: gf.PrevPath, Status: gf.Status,
			Additions: gf.Additions, Deletions: gf.Deletions}, "unreadable"
		return f
	}
	f.raw = raw
	masked := raw
	masked.Hunks = make([]review.Hunk, len(raw.Hunks))
	var m reviewMasker
	whole := reviewSecretPath(raw.Path)
	for hi, h := range raw.Hunks {
		h.Lines = slices.Clone(h.Lines)
		for li, l := range h.Lines {
			if !m.inPEM && reviewOrphanPEMEnd(l.Text) {
				// The hunk opened inside a private key: GitHub's three lines of context began below its
				// BEGIN, so every line of the hunk above this END is the key's body, and so is the END.
				for k := 0; k <= li; k++ {
					if h.Lines[k].Text != reviewMask {
						f.maskLine(&h, hi, k)
					}
				}
				continue
			}
			out, kinds, fake := m.line(l.Text)
			if whole && strings.TrimSpace(l.Text) != "" {
				out = reviewMask
			}
			if out != l.Text {
				f.masked[hi] = true
				h.Lines[li].Text = out
			}
			if l.Kind != '+' {
				continue
			}
			if len(kinds) > 0 {
				f.secretAt(l.New, kinds, fake)
			}
			if reviewInjectionLine(l.Text) {
				f.inject = append(f.inject, l.New)
			}
		}
		masked.Hunks[hi] = h
	}
	masked.Patch = renderPatch(masked.Hunks)
	if raw.Patch == "" {
		masked.Patch = ""
	}
	f.File = masked
	f.tier, f.risk = reviewTier(raw.Path), reviewRisk(raw.Path, f.changed())
	f.tokens = reviewTokens(review.NumberedPatch(masked))
	switch {
	case e.IgnoresPath(raw.Path):
		f.skip = "ignored"
	case raw.NoPatchReason() != "":
		f.skip = raw.NoPatchReason()
	case reviewGenerated(raw):
		f.skip = "generated"
	}
	return f
}

// renderPatch writes hunks back out as a unified diff, so the masked copy's Patch says what its
// Hunks say and nothing that reads Patch finds the raw text.
func renderPatch(hunks []review.Hunk) string {
	var b strings.Builder
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		if h.Section != "" {
			b.WriteString(" " + h.Section)
		}
		b.WriteString("\n")
		for _, l := range h.Lines {
			b.WriteByte(l.Kind)
			b.WriteString(l.Text)
			b.WriteString("\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func reviewTokens(s string) int { return int(float64(len(s))/reviewCharsPerTok) + 1 }

// reviewGeneratedPaths are files nobody writes by hand. A review of them is a review of the
// generator's output, which the generator's own change already got.
var reviewGeneratedPaths = []string{"**/vendor/**", "**/node_modules/**", "**/*.min.js", "**/*.min.css", "**/*.map",
	"**/*.pb.go", "**/*_pb2.py", "**/*_pb2_grpc.py", "**/*_generated.go", "**/*.gen.go", "**/*.generated.*",
	"**/__snapshots__/**", "**/*.snap"}

var generatedHeader = regexp.MustCompile(`(?i)(code generated .*do not edit|@generated\b)`)

func reviewGenerated(f review.File) bool {
	if review.MatchAny(reviewGeneratedPaths, f.Path) {
		return true
	}
	// The convention Go and most generators follow: a header in the first lines of the file.
	for _, h := range f.Hunks {
		if h.NewStart > 5 {
			break
		}
		for _, l := range h.Lines {
			if l.New > 0 && l.New <= 5 && generatedHeader.MatchString(l.Text) {
				return true
			}
		}
	}
	return false
}

// reviewLockfiles are reviewed — a dependency change is the Security type's business — but last:
// a thousand lines of hashes say less per token than ten lines of source.
var reviewLockfiles = []string{"**/package-lock.json", "**/yarn.lock", "**/pnpm-lock.yaml", "**/go.sum",
	"**/Cargo.lock", "**/poetry.lock", "**/Pipfile.lock", "**/composer.lock", "**/Gemfile.lock", "**/uv.lock"}

var (
	reviewTestPaths = []string{"**/*_test.*", "**/*.test.*", "**/*.spec.*", "**/test_*.py", "**/test/**", "**/tests/**",
		"**/__tests__/**", "**/testdata/**", "**/fixtures/**"}
	reviewDocPaths = []string{"**/*.md", "**/*.mdx", "**/*.rst", "**/*.txt", "**/*.adoc", "docs/**", "doc/**", "**/LICENSE*"}
	// reviewSurfacePaths change how a thing looks or what a test is fed rather than what the code
	// does: stylesheets and images, stories, snapshots, fixtures and mocks, and translations. Read
	// after the tests, since a large pull request's cut falls on whatever is ranked last. Snapshot
	// files a test runner writes (__snapshots__, .snap) are generated (reviewGeneratedPaths) and
	// never get this far.
	reviewSurfacePaths = []string{"**/*.css", "**/*.scss", "**/*.sass", "**/*.less", "**/*.styl", "**/*.svg",
		"**/*.stories.*", "**/*.story.*", "**/stories/**", "**/snapshots/**", "**/fixtures/**", "**/__fixtures__/**",
		"**/testdata/**", "**/__mocks__/**", "**/mocks/**", "**/*.mock.*", "**/*_mock.*", "**/mock_*",
		"**/locales/**", "**/locale/**", "**/translations/**", "**/i18n/**/*.json", "**/*.po", "**/*.pot", "**/*.xlf", "**/*.xliff"}
)

// reviewTier ranks what a reviewer should read first: source, then tests, then the surface files
// (reviewSurfacePaths), then docs, then lockfiles.
func reviewTier(p string) int {
	switch {
	case review.MatchAny(reviewLockfiles, p):
		return 4
	case review.MatchAny(reviewSurfacePaths, p):
		return 2
	case review.MatchAny(reviewTestPaths, p):
		return 1
	case review.MatchAny(reviewDocPaths, p):
		return 3
	}
	return 0
}

// reviewRiskWords are the words in a path that mark code where a bug costs most: who may do what,
// money, the shape and storage of data, state a user interface shares, work that runs in the
// background or on a schedule, and the edges where data comes in and goes out. A word of four
// letters or more also matches the start of a longer one ("migrations", "authorize"); a shorter
// one only itself or its plural, so "api" is not "apiary". A word earlier in the list wins a
// path word both match, so "router" counts once.
var reviewRiskWords = []string{"auth", "permission", "policy", "security", "session", "token", "payment", "billing",
	"invoice", "migration", "schema", "model", "store", "state", "redux", "reducer", "logic", "saga", "hook", "api",
	"route", "router", "handler", "controller", "service", "worker", "job", "queue", "task", "cron", "db", "sql",
	"query", "cache", "lock", "sync", "upload", "download", "export", "import"}

// reviewPathWords splits a path into lowercase words at every separator, at a lower-to-upper case
// change, before the last capital of a run followed by a lower case letter ("HTTPHandler" is
// "http handler"), and between letters and digits.
func reviewPathWords(p string) []string {
	var words []string
	rs := []rune(p)
	start := -1
	flush := func(end int) {
		if start >= 0 {
			words = append(words, strings.ToLower(string(rs[start:end])))
			start = -1
		}
	}
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush(i)
			continue
		}
		if start >= 0 {
			prev := rs[i-1]
			switch {
			case unicode.IsLower(prev) && unicode.IsUpper(r),
				unicode.IsUpper(prev) && unicode.IsUpper(r) && i+1 < len(rs) && unicode.IsLower(rs[i+1]),
				unicode.IsDigit(prev) != unicode.IsDigit(r):
				flush(i)
			}
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(rs))
	return words
}

// reviewRiskWord is the risk word a path word is, or "".
func reviewRiskWord(w string) string {
	one := w
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		one = w[:len(w)-3] + "y"
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		one = w[:len(w)-1]
	}
	for _, rw := range reviewRiskWords {
		if one == rw || (len(rw) >= 4 && strings.HasPrefix(w, rw)) {
			return rw
		}
	}
	return ""
}

// reviewRisk is how early a changed file is read within its tier: three points for each distinct
// risk word in its path, up to three of them, a React hook's "use…" file counting as "hook", plus
// a point for each doubling of its changed lines. So one risk word weighs about as much as eight
// times the lines: a twenty-line change to a store is read before a hundred-line one to a page,
// and a large change still rises on its size alone.
func reviewRisk(p string, changed int) int {
	words := reviewPathWords(p)
	found := map[string]bool{}
	for i, w := range words {
		if rw := reviewRiskWord(w); rw != "" {
			found[rw] = true
		} else if w == "use" && i+2 < len(words) {
			// use-auth.ts, useSaveField.tsx: "use", a word, and the extension after it.
			found["hook"] = true
		}
	}
	return 3*min(len(found), 3) + bits.Len(uint(max(changed, 0)))
}

// reviewRank orders the reviewable files best first — by tier, then the riskier (reviewRisk), then
// the larger change, then the path, so the order never depends on how GitHub happened to list them.
func reviewRank(files []*reviewFile) {
	slices.SortStableFunc(files, func(a, b *reviewFile) int {
		return cmp.Or(cmp.Compare(a.tier, b.tier), cmp.Compare(b.risk, a.risk), cmp.Compare(b.changed(), a.changed()),
			cmp.Compare(a.Path, b.Path))
	})
}

// reviewUnit is the files one finder pass reads at once.
type reviewUnit struct {
	files  []*reviewFile
	tokens int
}

// reviewSplit packs ranked files into units of at most reviewUnitTokens, in rank order, so the
// first unit holds the best of the pull request. A file larger than a unit on its own is returned
// in tooLarge: it is said to be unreviewed rather than read as a fraction.
func reviewSplit(files []*reviewFile) (units []*reviewUnit, tooLarge []*reviewFile) {
	var cur *reviewUnit
	for _, f := range files {
		if f.tokens > reviewUnitTokens {
			tooLarge = append(tooLarge, f)
			continue
		}
		if cur == nil || cur.tokens+f.tokens > reviewUnitTokens {
			cur = &reviewUnit{}
			units = append(units, cur)
		}
		cur.files = append(cur.files, f)
		cur.tokens += f.tokens
	}
	return units, tooLarge
}

// ---- file contents ----

// reviewText is one file's content at one ref, masked, as the review read it.
type reviewText struct {
	lines     []string
	masked    map[int]bool // 1-based lines the mask replaced something on
	truncated bool
}

// window returns lines from through to (1-based, inclusive, clamped) with their numbers, as the
// model is shown a file: "  52| text".
func (t *reviewText) window(from, to int) string {
	from, to = max(from, 1), min(to, len(t.lines))
	if from > to {
		return ""
	}
	w := len(strconv.Itoa(to))
	var b strings.Builder
	for n := from; n <= to; n++ {
		fmt.Fprintf(&b, "%*d| %s\n", w, n, t.lines[n-1])
	}
	return b.String()
}

func newReviewText(raw string, truncated bool) *reviewText {
	masked, hit := maskText(strings.ReplaceAll(raw, "\r\n", "\n"))
	t := &reviewText{lines: strings.Split(strings.TrimSuffix(masked, "\n"), "\n"), masked: map[int]bool{}, truncated: truncated}
	for _, n := range hit {
		t.masked[n] = true
	}
	return t
}

// reviewDecl is a line that opens a declaration in the languages a review meets most: what a hunk
// is inside of, which is the context a reviewer reaches for first.
var reviewDecl = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?` +
	`(?:func|def|class|function|fn|impl|interface|struct|enum|trait|module|type)\b|` +
	`^\s*(?:public|private|protected|internal|static)\b[^;]*\(|` +
	`^\s*(?:export\s+)?(?:const|let|var)\s+\w+\s*=\s*(?:async\s*)?(?:\([^)]*\)|\w+)\s*=>`)

// excerptAbove is the enclosing declaration above a hunk: from the nearest line that opens one,
// within reviewExcerptLines, down to the line before the hunk. With none in reach, the few lines
// just above it.
func (t *reviewText) excerptAbove(h review.Hunk) (from, to int) {
	to = h.NewStart - 1
	if to < 1 {
		return 0, 0
	}
	for n := to; n >= max(1, to-reviewExcerptLines+1); n-- {
		if n <= len(t.lines) && reviewDecl.MatchString(t.lines[n-1]) {
			return n, to
		}
	}
	return max(1, to-7), to
}

// ---- the repo map ----

// reviewTrees caches recursive trees by organisation, repository and commit. A commit's tree never
// changes, so nothing expires; the cache only bounds how many it holds, since one tree of a large
// repository is a hundred thousand paths.
type reviewTrees struct {
	mu    sync.Mutex
	order []string
	m     map[string]reviewTree
}

type reviewTree struct {
	paths     []string // blobs only
	truncated bool
}

const reviewTreesHeld = 16

func (c *reviewTrees) get(key string) (reviewTree, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	return t, ok
}

func (c *reviewTrees) put(key string, t reviewTree) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]reviewTree{}
	}
	if _, ok := c.m[key]; !ok {
		c.order = append(c.order, key)
	}
	c.m[key] = t
	for len(c.order) > reviewTreesHeld {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

func treeOf(entries []reviewTreeEntry, truncated bool) reviewTree {
	t := reviewTree{truncated: truncated}
	for _, e := range entries {
		if e.Type == "blob" {
			t.paths = append(t.paths, e.Path)
		}
	}
	slices.Sort(t.paths)
	return t
}

// repoMap is a focused map of the repository at the head: what is at the top, and what sits beside
// each changed file. A model that knows a sibling called auth_middleware.go exists will read it
// before claiming a check is missing.
func repoMap(t reviewTree, changed []string) string {
	var b strings.Builder
	top := map[string]bool{}
	var tops []string
	for _, p := range t.paths {
		first, _, _ := strings.Cut(p, "/")
		if strings.Contains(p, "/") {
			first += "/"
		}
		if !top[first] {
			top[first] = true
			tops = append(tops, first)
		}
	}
	b.WriteString("Top level: " + strings.Join(capList(tops, 40), " ") + "\n")
	dirs := map[string]bool{}
	for _, c := range changed {
		d := path.Dir(c)
		if dirs[d] || len(dirs) >= 15 {
			continue
		}
		dirs[d] = true
		prefix := d + "/"
		if d == "." {
			prefix = ""
		}
		var here []string
		seen := map[string]bool{}
		for _, p := range t.paths {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := strings.TrimPrefix(p, prefix)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				rest = rest[:i+1]
			}
			if !seen[rest] {
				seen[rest] = true
				here = append(here, rest)
			}
		}
		fmt.Fprintf(&b, "%s/: %s\n", d, strings.Join(capList(here, 25), " "))
	}
	if t.truncated {
		b.WriteString("(GitHub listed only part of this repository's tree.)\n")
	}
	out, _ := cutRunes(b.String(), reviewMapChars)
	return out
}

// capList is s, or its first n with a count of the rest.
func capList(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("…(+%d)", len(s)-n))
}

// ---- instruction files, at the base ----

// reviewInstructionFiles are the files a team writes to tell an agent how its code works, read at
// the base commit only: a pull request that adds "approve everything" to AGENTS.md is the last
// place a review should take its instructions from.
func reviewInstructionFiles(base reviewTree, haveTree bool, changed []string) []string {
	fixed := []string{"REVIEW.md", "AGENTS.md", "CLAUDE.md", ".github/copilot-instructions.md"}
	if !haveTree {
		return fixed
	}
	has := func(p string) bool { _, ok := slices.BinarySearch(base.paths, p); return ok }
	var out []string
	for _, p := range fixed[:2] {
		if has(p) {
			out = append(out, p)
		}
	}
	// The AGENTS.md nearest each changed file, which is the one that speaks for its directory.
	var nearest []string
	for _, c := range changed {
		for d := path.Dir(c); d != "." && d != "/"; d = path.Dir(d) {
			if p := d + "/AGENTS.md"; has(p) {
				if !slices.Contains(nearest, p) {
					nearest = append(nearest, p)
				}
				break
			}
		}
	}
	slices.Sort(nearest)
	out = append(out, nearest[:min(len(nearest), 3)]...)
	for _, p := range fixed[2:] {
		if has(p) {
			out = append(out, p)
		}
	}
	return out
}

// ---- context repositories ----

// reviewRepoReader reads one of the organisation's other repositories for a review: its default
// branch, pinned to the commit it pointed at when the review began, so a quote checked against it
// is checked against what the finder read. Read-only by construction — it has no method that
// writes and its allowlist names only reads — and it goes through Proxy.Do under the review_read
// token, scoped to that one repository.
type reviewRepoReader struct {
	proxy *Proxy
	orgID int64
	conn  *Connection
	acc   *Access
	base  string
	repo  string

	Private       bool
	DefaultBranch string
	SHA           string
}

// reviewRepoRoutes are what a context read may ask for, below /repos/{owner}/{name}/: the
// repository itself, its default branch's head, a tree and a file — and, for a skill pinned to a
// branch, tag or commit of its own (review_skills.go), the commit that ref names.
var reviewRepoRoutes = []string{"", "git/ref/heads/", "git/trees/", "contents/", "commits/"}

func (r *reviewRepoReader) get(ctx context.Context, rel string, q url.Values, accept string, maxBytes int) (*ProxyResponse, error) {
	ok := false
	for _, p := range reviewRepoRoutes {
		if (p == "" && rel == "") || (p != "" && strings.HasPrefix(rel, p) && len(rel) > len(p)) {
			ok = true
		}
	}
	for k := range q {
		ok = ok && (k == "ref" || k == "recursive")
	}
	for _, seg := range strings.Split(rel, "/") {
		ok = ok && seg != "." && seg != ".."
	}
	if !ok {
		return nil, fmt.Errorf("code review may not read %s in %s", truncate(rel, 80), r.repo)
	}
	u := r.base + "/repos/" + r.repo
	if rel != "" {
		u += "/" + rel
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return reviewSend(ctx, r.proxy, r.orgID, r.acc, r.conn, "GET", u, "GET "+r.repo+"/"+truncate(rel, 120), "", accept, maxBytes)
}

func (r *reviewRepoReader) getJSON(ctx context.Context, rel string, q url.Values, out any) error {
	resp, err := r.get(ctx, rel, q, "", proxyMaxRead)
	if err != nil {
		return err
	}
	if resp.Truncated {
		return fmt.Errorf("GitHub's answer for %s is larger than code review reads", r.repo)
	}
	return json.Unmarshal([]byte(resp.Body), out)
}

// pin reads what the repository is and where its default branch points.
func (r *reviewRepoReader) pin(ctx context.Context) error {
	var meta struct {
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := r.getJSON(ctx, "", nil, &meta); err != nil {
		return err
	}
	if meta.DefaultBranch == "" {
		return fmt.Errorf("%s names no default branch", r.repo)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := r.getJSON(ctx, "git/ref/heads/"+pathEscapeSegments(meta.DefaultBranch), nil, &ref); err != nil {
		return err
	}
	if !commitSHA.MatchString(ref.Object.SHA) {
		return fmt.Errorf("%s's default branch did not resolve to a commit", r.repo)
	}
	r.Private, r.DefaultBranch, r.SHA = meta.Private, meta.DefaultBranch, ref.Object.SHA
	return nil
}

// pinAt is pin at a branch, tag or commit a skill link names, rather than the default branch: the
// repository's own facts still come from pin, and the commit from what GitHub says ref names now.
func (r *reviewRepoReader) pinAt(ctx context.Context, ref string) error {
	if err := r.pin(ctx); err != nil || ref == "" {
		return err
	}
	if commitSHA.MatchString(ref) {
		r.SHA = ref
		return nil
	}
	resp, err := r.get(ctx, "commits/"+pathEscapeSegments(ref), nil, "application/vnd.github.sha", 200)
	if err != nil {
		return err
	}
	sha := strings.TrimSpace(resp.Body)
	if !commitSHA.MatchString(sha) {
		return fmt.Errorf("%s did not resolve to a commit in %s", ref, r.repo)
	}
	r.SHA = sha
	return nil
}

func (r *reviewRepoReader) fileAt(ctx context.Context, p string) (string, bool, error) {
	resp, err := r.get(ctx, "contents/"+pathEscapeSegments(strings.TrimLeft(p, "/")), url.Values{"ref": {r.SHA}},
		"application/vnd.github.raw", githubRawMax)
	if err != nil {
		return "", false, err
	}
	return resp.Body, resp.Truncated, nil
}

func (r *reviewRepoReader) tree(ctx context.Context) (reviewTree, error) {
	var t struct {
		Truncated bool              `json:"truncated"`
		Tree      []reviewTreeEntry `json:"tree"`
	}
	if err := r.getJSON(ctx, "git/trees/"+r.SHA, url.Values{"recursive": {"1"}}, &t); err != nil {
		return reviewTree{}, err
	}
	return treeOf(t.Tree, t.Truncated), nil
}

// codeSearch is find_code's request for this repository: GitHub's code search, read-only token,
// the repository's own connection. findCodeIn builds the URL from conn.Repo, so a query cannot
// name another repository.
func (r *reviewRepoReader) codeSearch(ctx context.Context, _ *Connection, u string, out any) (ghResult, error) {
	if !strings.HasPrefix(u, r.base+"/search/code?") {
		return ghResult{}, errors.New("code review searches only GitHub's code search")
	}
	resp, err := r.proxy.Do(ctx, r.orgID, r.acc, ProxyRequest{Method: "GET", URL: u, Connection: r.conn.Name,
		Purpose: githubPurposeReviewRead, MaxBytes: proxyMaxRead,
		Headers: map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}},
		ProxyAudit{Requester: "github-review"}, true)
	if err != nil {
		return ghResult{}, err
	}
	res := ghResult{status: resp.Status, retry: resp.RetryAfter}
	if resp.Status >= 400 {
		if err := res.limited("GitHub"); err != nil {
			return res, err
		}
		return res, fmt.Errorf("GitHub returned %d for %s%s", resp.Status, r.repo, ghMessage(resp.Body))
	}
	return res, json.Unmarshal([]byte(resp.Body), out)
}

// newReviewRepoReader is a reader for repo through installation, under this organisation.
func (p *Proxy) newReviewRepoReader(orgID, installationID int64, repo, base string) (*reviewRepoReader, error) {
	conn, err := p.reviewConnection(installationID, repo)
	if err != nil {
		return nil, err
	}
	return &reviewRepoReader{proxy: p, orgID: orgID, conn: conn, acc: &Access{Rules: []Rule{{Conn: conn}}},
		base: base, repo: conn.Repo}, nil
}

// ---- small things ----

// reviewHash is a short stable digest of text, for a cache key or a code hash.
func reviewHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reviewTagText matches anything that could be read as one of the prompt's own section tags.
// code covers code_then and code_now, and diff the resolution check's diff_now (review_resolve.go).
var reviewTagText = regexp.MustCompile(`(?i)<(\s*/?\s*(?:pr_data|pr_diff|head_file|repo_map|repository_conventions|team_instructions|prior_findings|candidate|code|diff|finding|review_criteria|review_skills|skill))`)

// untrusted defuses text a stranger wrote before it goes between the prompt's tags, so a pull
// request cannot close its own section and open one that reads like ours.
func untrusted(s string) string { return reviewTagText.ReplaceAllString(s, "&lt;$1") }
