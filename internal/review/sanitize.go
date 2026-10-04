package review

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeOptions says where a sanitised text's links may point and how long it may be.
type SanitizeOptions struct {
	// AllowedRepos are the repositories, as owner/name in any case, that a link may point into:
	// https://github.com/<owner>/<name> and anything under it. Normally the pull request's own
	// repository and the context repositories the review was allowed to read.
	AllowedRepos []string
	// ConsoleOrigin is the console's scheme://host[:port]. When set, a link to that origin is
	// kept as well; when empty, no console link survives.
	ConsoleOrigin string
	// MaxLen caps the result in characters (runes), "…" included; 0 means no cap.
	MaxLen int
}

// wordJoiner is U+2060, inserted after an '@' to stop GitHub reading a mention: the mention
// needs a letter or digit straight after the '@', and the joiner is neither, while a reader
// sees nothing.
const wordJoiner = "\u2060"

// maxSanitizeInput bounds the text Sanitize will read. GitHub refuses a comment over 65,536
// characters, so nothing longer can be posted anyway, and the bound keeps the few
// per-character searches below from being fed a megabyte by a model told to write one.
const maxSanitizeInput = 128 << 10

// Sanitize makes model-written markdown safe to post on GitHub under the bot's name. The model
// reads the pull request, and the pull request is a stranger's text, so whatever the model
// writes may have been put in its mouth; the bot's comments are trusted by the people who read
// them and by the coding agents some teams point at them. What comes out:
//
//   - HTML is gone except <details>, <summary>, <sub>, <code> and <br>, and those carry no
//     attributes and are balanced, so a stray <details> cannot swallow the rest of a comment.
//   - Images are gone, markdown and HTML alike: an image is a request to a server of the
//     author's choosing every time the comment is viewed.
//   - @user and @org/team mentions are defused with an invisible U+2060 after the '@', so a
//     review cannot be made to page people. Email addresses and code are left alone, as GitHub
//     itself does not mention from either.
//   - A link survives only to https://github.com/<owner>/<repo> for an allowed repository or to
//     the console origin; any other keeps its text and loses its URL. A bare URL anywhere else
//     is put in a code span, which GitHub does not autolink.
//   - HTML comments are gone, and "<!-- attest_tag:" is stripped from code too, so the model
//     can never forge — or replay — one of our markers.
//   - Fenced code blocks pass through untouched apart from that, re-fenced so that nothing
//     inside can end them early, and with an info string that would make GitHub do something
//     with them ("suggestion", "mermaid") dropped.
//
// It errs towards escaping. Every '[', ']' and '<' it does not keep as part of a construct it
// checked is escaped, and every unmatched backtick too, and the first of any run of three tildes
// outside a code block, so no link, tag, code span or nested fence can form
// in the output that was not one this function decided to keep — which is what keeps it safe
// where its reading of markdown and GitHub's differ, as two markdown parsers always do
// somewhere. The cost is a stray backslash on show in the rare places markdown does not
// unescape, such as inside raw HTML.
//
// The result is stable: Sanitize of a sanitised text is the same text.
func Sanitize(md string, opt SanitizeOptions) string {
	return sanitize(md, newLinkPolicy(opt.AllowedRepos, opt.ConsoleOrigin), blockMode, opt.MaxLen)
}

// sanMode is what kind of text is being sanitised.
type sanMode int

const (
	// blockMode is a markdown document of its own: a scenario, a summary.
	blockMode sanMode = iota
	// inlineMode is a line set inside our own markup — the bold title of a comment, a row of
	// the summary's list — so it may not open a block, carries no HTML at all, and has its
	// emphasis characters escaped so it cannot end the bold it sits in.
	inlineMode
	// htmlInlineMode is a line set inside our own HTML, such as a <summary>, where GitHub reads
	// no markdown at all: a code span there is two backticks around text, so nothing may be
	// left untouched on the strength of being "code". Like inlineMode it carries no HTML.
	htmlInlineMode
)

func sanitize(s string, p *linkPolicy, mode sanMode, maxLen int) string {
	if len(s) > maxSanitizeInput {
		cut := maxSanitizeInput
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	out := settle(s, p, mode)
	if maxLen <= 0 || utf8.RuneCountInString(out) <= maxLen {
		return out
	}
	// Cut the sanitised text and sanitise the cut again: a cut can land inside a link or a
	// code span and leave half of one behind, and the half must be judged afresh — the end of
	// a code span around "https://evil.example/login" cut off would otherwise leave a bare
	// URL that GitHub links. Sanitising again can lengthen the text a little (an escape, a
	// closing fence), so take the overshoot off and try again.
	budget := maxLen - 1
	for range 8 {
		if budget < 1 {
			break
		}
		cut := settle(cutRunes(out, budget)+"…", p, mode)
		n := utf8.RuneCountInString(cut)
		if n <= maxLen {
			return cut
		}
		budget -= n - maxLen
	}
	return "…"
}

// maxSanitizePasses bounds settle. Removing one thing can bring two others together into
// something new — "https:" and "//evil.example" either side of a removed tag — so a pass is
// repeated until it changes nothing; real text settles in one or two.
const maxSanitizePasses = 6

func settle(s string, p *linkPolicy, mode sanMode) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	for range maxSanitizePasses {
		next := sanitizeOnce(s, p, mode)
		if next == s {
			return s
		}
		s = next
	}
	// It did not settle, which only text built to do that achieves. Show it as code, where
	// nothing renders — or, inside HTML, where code is not code, as bare words.
	switch mode {
	case inlineMode:
		return codeSpan(s)
	case htmlInlineMode:
		return inertText(s)
	}
	body := stripMarkers(s)
	fence := strings.Repeat("`", max(3, longestRun(body, '`')+1))
	return fence + "\n" + body + "\n" + fence + "\n"
}

func sanitizeOnce(s string, p *linkPolicy, mode sanMode) string {
	st := &scanState{p: p, mode: mode, rawHTML: mode == htmlInlineMode}
	if mode != blockMode {
		st.scan(strings.ReplaceAll(s, "\n", " "), 0)
		return string(st.out)
	}
	lines := strings.Split(s, "\n")
	var region []string
	flush := func() {
		for k, seg := range htmlSegments(region) {
			if k > 0 {
				st.write("\n")
			}
			st.rawHTML = seg.raw
			st.table = !seg.raw && hasTableDelimiter(seg.lines)
			st.scan(strings.Join(seg.lines, "\n"), 0)
		}
		st.rawHTML, st.table = false, false
		region = nil
	}
	for i := 0; i < len(lines); i++ {
		f, ok := openFence(lines[i])
		if !ok {
			region = append(region, lines[i])
			continue
		}
		j := i + 1
		for j < len(lines) && !f.closes(lines[j]) {
			j++
		}
		flush()
		st.fence(f, lines[i+1:j])
		i = j // the closing line, or past the end when the block was never closed
	}
	flush()
	st.closeAll()
	return string(st.out)
}

// segment is a run of lines that GitHub reads either as markdown or, raw, as HTML.
type segment struct {
	lines []string
	raw   bool
}

// htmlSegments splits lines into the HTML blocks GitHub might see in them and the markdown
// between. Inside an HTML block GitHub parses no markdown, so a code span there is no code span
// and the "@name" in it is a mention; the lines are therefore scanned as raw HTML, where nothing
// is left untouched for being code.
//
// It overestimates on purpose. A block is taken to start at any line whose first character,
// after indentation and any blockquote or list markers, opens a tag, and to run to the next line
// that is entirely blank, which is where a block of the kinds this package's output can start
// (CommonMark's types 6 and 7) must end. Taking markdown for HTML costs a stray backslash on show;
// the other way round would let a mention through.
//
// Blank is CommonMark's blank — nothing but spaces and tabs — and not strings.TrimSpace's. A line
// holding only a no-break space, U+2028 or an ideographic space is text to GitHub, so the HTML
// block goes on past it; ending the segment there would scan what follows as markdown and leave
// a "`@name`" untouched that GitHub renders as a live mention.
func htmlSegments(lines []string) []segment {
	var segs []segment
	var cur segment
	push := func(next segment) {
		if len(cur.lines) > 0 {
			segs = append(segs, cur)
		}
		cur = next
	}
	for _, l := range lines {
		switch {
		case cur.raw && strings.Trim(l, " \t") == "":
			push(segment{lines: []string{l}})
		case !cur.raw && startsHTMLBlock(l):
			push(segment{lines: []string{l}, raw: true})
		default:
			cur.lines = append(cur.lines, l)
		}
	}
	push(segment{})
	return segs
}

// listMarker is a bullet or an ordered list's number, with the space after it.
var listMarker = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])[ \t]+`)

func startsHTMLBlock(line string) bool {
	for {
		s := strings.TrimLeft(line, " \t>")
		if m := listMarker.FindString(s); m != "" {
			s = s[len(m):]
		}
		if s == line {
			break
		}
		line = s
	}
	// A tag's name, then what may follow a name in a tag. "<https://…" is an autolink, not a
	// tag, and starts no block.
	return htmlBlockTag.MatchString(line)
}

var htmlBlockTag = regexp.MustCompile(`^</?[A-Za-z][A-Za-z0-9-]*(?:[\s/>]|$)`)

// tableDelimiter is the line under a GFM table's header: cells of hyphens with optional colons,
// separated by pipes. Its presence is what makes the lines around it a table.
var tableDelimiter = regexp.MustCompile(`^[\s>]*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$`)

// hasTableDelimiter reports whether lines could hold a table. GitHub splits a table row into
// cells at every unescaped '|' before it reads any code span, so in a table "`a | @name`" is
// two cells and the second is markdown; pipes in code are escaped wherever this says yes. It
// looks at every line rather than tracking where a table starts and ends, so an unrelated code
// span in the same stretch is escaped too — a backslash on show, and only in text that has a
// table in it.
func hasTableDelimiter(lines []string) bool {
	for _, l := range lines {
		if strings.Contains(l, "|") && tableDelimiter.MatchString(l) {
			return true
		}
	}
	return false
}

// fenceInfo is an opening code fence: its character, its length, how far it was indented
// (the block's lines lose that much indentation, as CommonMark has it) and its info string.
type fenceInfo struct {
	char   byte
	n      int
	indent int
	info   string
}

// openFence reads a line as a code fence's opening, CommonMark's way: up to three columns of
// indentation, three or more backticks or tildes, and for backticks an info string with no
// backtick in it.
func openFence(line string) (fenceInfo, bool) {
	indent, rest := leadingIndent(line)
	if indent > 3 || rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return fenceInfo{}, false
	}
	n := runLen(rest, 0, rest[0])
	if n < 3 {
		return fenceInfo{}, false
	}
	info := strings.TrimSpace(rest[n:])
	if rest[0] == '`' && strings.ContainsRune(info, '`') {
		return fenceInfo{}, false
	}
	return fenceInfo{char: rest[0], n: n, indent: indent, info: info}, true
}

// closes reports whether line ends the block f opened: the same character, at least as many,
// and nothing after them.
func (f fenceInfo) closes(line string) bool {
	indent, rest := leadingIndent(line)
	if indent > 3 {
		return false
	}
	n := runLen(rest, 0, f.char)
	return n >= f.n && strings.TrimRight(rest[n:], " \t") == ""
}

// leadingIndent counts a line's indentation in columns, a tab reaching the next multiple of
// four, and returns what follows it.
func leadingIndent(line string) (int, string) {
	col := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col, line[i:]
		}
	}
	return col, ""
}

// deniedFenceInfo are info strings GitHub acts on rather than highlights. "suggestion" above
// all: a suggestion block in model-written text would offer the pull request's author a
// one-click commit of whatever the model was talked into writing, which only the finding's own
// checked suggestion may do.
var deniedFenceInfo = map[string]bool{
	"suggestion": true, "mermaid": true, "geojson": true, "topojson": true, "stl": true, "math": true,
}

var fenceLang = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,30}$`)

func fenceInfoString(info string) string {
	fields := strings.Fields(info)
	if len(fields) == 0 || !fenceLang.MatchString(fields[0]) || deniedFenceInfo[strings.ToLower(fields[0])] {
		return ""
	}
	return fields[0]
}

// scanState writes one sanitised text. It is not reused between texts.
type scanState struct {
	p    *linkPolicy
	mode sanMode
	out  []byte
	// tags are the allowed HTML elements open at this point, innermost last.
	tags []string
	// inLink is set while an allowed link's text is written: a link may not hold another.
	inLink bool
	// rawHTML is set while the text is one GitHub reads as HTML (see htmlSegments), and table
	// while it may hold a table (see hasTableDelimiter).
	rawHTML, table bool
	// noCloser remembers, for a backtick run length, the end of the line on which a search for
	// a closing run of that length already failed, so a line of unmatched backticks is searched
	// once per length rather than once per backtick.
	noCloser map[int]int
}

func (st *scanState) write(s string) { st.out = append(st.out, s...) }

// last is the last character written, or 0 at the start.
func (st *scanState) last() rune {
	r, _ := utf8.DecodeLastRune(st.out)
	if r == utf8.RuneError {
		return 0
	}
	return r
}

// fence writes a fenced code block canonically: from the left margin, after a blank line, with
// a backtick fence longer than any run of backticks inside it. The blank line ends any HTML
// block above — inside one, GitHub would not read the fence as a fence at all, and would
// render the "code" as HTML — and the length means nothing in the block can close it early.
// Whatever this function took for a code block is therefore exactly what GitHub shows as one.
func (st *scanState) fence(f fenceInfo, content []string) {
	for i, line := range content {
		content[i] = dedent(line, f.indent)
	}
	body := stripMarkers(strings.Join(content, "\n"))
	fence := strings.Repeat("`", max(3, longestRun(body, '`')+1))
	switch {
	case len(st.out) == 0:
	case bytes.HasSuffix(st.out, []byte("\n\n")):
	case st.out[len(st.out)-1] == '\n':
		st.write("\n")
	default:
		st.write("\n\n")
	}
	st.write(fence + fenceInfoString(f.info) + "\n")
	if len(content) > 0 {
		st.write(body + "\n")
	}
	st.write(fence + "\n")
}

// dedent removes up to n leading spaces.
func dedent(line string, n int) string {
	i := 0
	for i < n && i < len(line) && line[i] == ' ' {
		i++
	}
	return line[i:]
}

// Bounds on what is read as one construct. A link text, a destination or a tag longer than
// these is not one — its first character is escaped instead — which keeps every lookahead
// short whatever the text is.
const (
	maxLinkText  = 1000
	maxLinkDest  = 2048
	maxLinkTitle = 500
	maxLinkDepth = 3
	maxTagLen    = 512
)

// scan writes the sanitised form of t, a stretch of markdown with no fenced code block in it.
func (st *scanState) scan(t string, depth int) {
	// noCloser holds offsets into t, so a link's text, scanned on its own, starts afresh.
	saved := st.noCloser
	st.noCloser = nil
	defer func() { st.noCloser = saved }()
	lastClose := strings.LastIndex(t, "-->")
	for i := 0; i < len(t); {
		c := t[i]
		switch {
		case c == '\\':
			if i+1 < len(t) && isASCIIPunct(t[i+1]) {
				switch {
				case t[i+1] == '@':
					i++ // "\@user" renders as "@user", which GitHub mentions; judge the '@'
					continue
				case st.rawHTML && (t[i+1] == '<' || t[i+1] == '&'):
					// HTML knows no backslash escapes: "\<img>" is a backslash and an image
					// there, so what follows is judged as itself.
					st.write(`\`)
					i++
					continue
				}
				st.write(t[i : i+2])
				i += 2
				continue
			}
			// A lone backslash. At the very end, or in a line set inside our own markup, it
			// would escape whatever we write next — the "**" closing a bold title.
			if st.mode == inlineMode || i == len(t)-1 {
				st.write(`\\`)
			} else {
				st.write(`\`)
			}
			i++
		case (c == '~' || c == '`' && st.rawHTML) && st.mode != inlineMode && runLen(t, i, c) >= 3:
			// Three or more of either can open a fence. One at the left margin was taken as a fence
			// before this text was split up, but GitHub also opens one inside a blockquote or a list
			// item — "> ~~~suggestion" — and keeps its info string there. The first is escaped, so
			// none opens: a backtick run in markdown already is (codeSpan); a tilde run is not, and a
			// backtick run in what was taken for HTML must be too, in case GitHub reads it as a
			// paragraph a fence may interrupt. In real HTML that shows one stray backslash.
			n := runLen(t, i, c)
			st.write(`\` + t[i:i+n])
			i += n
		case c == '`' && st.rawHTML:
			n := runLen(t, i, '`')
			st.write(t[i : i+n]) // literal in HTML, and harmless if this is markdown after all
			i += n
		case c == '`':
			i = st.codeSpan(t, i)
		case c == '!' && i+1 < len(t) && t[i+1] == '[':
			if _, _, end, ok := parseLink(t, i+1); ok {
				i = end // an image: dropped, alt text and all
				continue
			}
			st.write("!")
			i++
		case c == '[':
			if depth < maxLinkDepth {
				if text, dest, end, ok := parseLink(t, i); ok {
					st.link(text, dest, depth)
					i = end
					continue
				}
			}
			st.write(`\[`)
			i++
		case c == ']':
			st.write(`\]`)
			i++
		case c == '<':
			i = st.angle(t, i, lastClose, depth)
		case c == '@':
			st.at(t[i+1:])
			i++
		case c == '&':
			i += st.ampersand(t[i:])
		case st.mode == inlineMode && (c == '*' || c == '_' || c == '~'):
			st.write(`\` + string(c))
			i++
		default:
			if n := urlAt(t, i); n > 0 {
				st.url(t[i : i+n])
				i += n
				continue
			}
			r, size := utf8.DecodeRuneInString(t[i:])
			if !dropRune(r) {
				st.write(t[i : i+size])
			}
			i += size
		}
	}
}

// codeSpan writes the code span starting at t[i], or the backticks escaped when they open
// none. A span here never crosses a line: GitHub's may, but one that crosses into another
// block in GitHub's reading would have this function pass text through untouched that GitHub
// renders as markdown. Taking a real multi-line span for text costs only an escape on show.
func (st *scanState) codeSpan(t string, i int) int {
	n := runLen(t, i, '`')
	end := -1
	lineEnd := strings.IndexByte(t[i:], '\n')
	if lineEnd < 0 {
		lineEnd = len(t)
	} else {
		lineEnd += i
	}
	if failedAt, ok := st.noCloser[n]; !ok || failedAt != lineEnd {
		end = codeSpanEnd(t, i, n)
		if end < 0 {
			if st.noCloser == nil {
				st.noCloser = map[int]int{}
			}
			st.noCloser[n] = lineEnd
		}
	}
	if end < 0 {
		st.write(strings.Repeat("\\`", n))
		return i + n
	}
	st.writeCode(t[i:i+n] + stripMarkers(t[i+n:end-n]) + t[end-n:end])
	return end
}

// writeCode writes a code span, its pipes escaped where it may sit in a table row.
func (st *scanState) writeCode(span string) {
	if st.table {
		span = escapePipes(span)
	}
	st.write(span)
}

// escapePipes escapes each '|' that is not escaped already, which GitHub's table reader honours
// inside a code span too.
func escapePipes(s string) string {
	if !strings.Contains(s, "|") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && (i == 0 || s[i-1] != '\\') {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// codeSpanEnd returns the index just past the run of exactly n backticks that closes the span
// opened at t[i], on the same line, or -1.
func codeSpanEnd(t string, i, n int) int {
	j := i + n
	for j < len(t) && t[j] != '\n' {
		if t[j] != '`' {
			j++
			continue
		}
		k := j + runLen(t, j, '`')
		if k-j == n {
			return k
		}
		j = k
	}
	return -1
}

// link writes a markdown link that parseLink found: as a link when the policy allows its
// destination, otherwise as its text alone.
func (st *scanState) link(text, dest string, depth int) {
	canon, ok := st.p.allow(dest)
	if !ok || st.inLink {
		st.scan(text, depth+1)
		return
	}
	mark := len(st.out)
	// "!" before the link would make it an image.
	if st.last() == '!' {
		st.write(wordJoiner)
	}
	st.write("[")
	textAt := len(st.out)
	st.inLink = true
	st.scan(text, depth+1)
	st.inLink = false
	if len(st.out) == textAt {
		// Nothing left of the text (it was an image badge): an empty link is an invisible
		// one, which is worse than none.
		st.out = st.out[:mark]
		return
	}
	st.write("](" + canon + ")")
}

var (
	// openTag and closeTag are CommonMark's raw HTML tags, attributes and all, so that what is
	// removed is what GitHub would have taken for a tag; "a<b && c>d" is not one, and its '<'
	// is escaped and shown instead.
	openTag  = regexp.MustCompile("^<([A-Za-z][A-Za-z0-9-]*)(?:\\s+[A-Za-z_:][A-Za-z0-9_.:-]*(?:\\s*=\\s*(?:[^\\s\"'=<>`]+|'[^']*'|\"[^\"]*\"))?)*\\s*/?>")
	closeTag = regexp.MustCompile(`^</([A-Za-z][A-Za-z0-9-]*)\s*>`)
	// autolink and emailAutolink are CommonMark's <scheme:…> and <address@host>.
	autolink      = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*)>`)
	emailAutolink = regexp.MustCompile(`^<([A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*)>`)
)

// allowedTags are the HTML elements a sanitised text may keep, by the name they are written
// with. br is void and never held open.
var allowedTags = map[string]bool{"details": true, "summary": true, "sub": true, "code": true, "br": true}

// angle handles a '<' at t[i] and returns where to continue.
func (st *scanState) angle(t string, i, lastClose, depth int) int {
	if strings.HasPrefix(t[i:], "<!--") {
		// A comment ends at the first "-->" after "<!--" ("<!-->" included); one that never
		// ends would hide the rest of the comment it is posted in, so its opener goes too.
		if lastClose >= i+2 {
			if e := strings.Index(t[i+2:], "-->"); e >= 0 {
				return i + 2 + e + 3
			}
		}
		return i + 4
	}
	window := t[i:min(len(t), i+maxTagLen)]
	if m := closeTag.FindStringSubmatch(window); m != nil {
		st.closeTag(strings.ToLower(m[1]))
		return i + len(m[0])
	}
	if m := openTag.FindStringSubmatch(window); m != nil {
		st.openTag(strings.ToLower(m[1]))
		return i + len(m[0])
	}
	if m := emailAutolink.FindStringSubmatch(window); m != nil {
		st.scan(m[1], depth+1)
		return i + len(m[0])
	}
	if m := autolink.FindStringSubmatch(window); m != nil {
		if canon, ok := st.p.allow(m[1]); ok && !st.inLink {
			st.write("<" + canon + ">")
		} else {
			st.refuseURL(m[1])
		}
		return i + len(m[0])
	}
	st.write("&lt;")
	return i + 1
}

// openTag writes an allowed tag. Text set inside our own markup gets none, in markdown or in
// HTML: a line inside a <summary> needs no markup, and a tag opened there could only be closed by
// the rest of a comment that is not this text's to shape.
func (st *scanState) openTag(name string) {
	if st.mode != blockMode || !allowedTags[name] {
		return
	}
	st.write("<" + name + ">")
	if name != "br" {
		st.tags = append(st.tags, name)
	}
}

// closeTag closes name and anything opened inside it, or drops a closing tag that closes
// nothing.
func (st *scanState) closeTag(name string) {
	if st.mode != blockMode {
		return
	}
	for k := len(st.tags) - 1; k >= 0; k-- {
		if st.tags[k] != name {
			continue
		}
		for j := len(st.tags) - 1; j >= k; j-- {
			st.write("</" + st.tags[j] + ">")
		}
		st.tags = st.tags[:k]
		return
	}
}

func (st *scanState) closeAll() {
	for j := len(st.tags) - 1; j >= 0; j-- {
		st.write("</" + st.tags[j] + ">")
	}
	st.tags = nil
}

// at writes an '@' whose text continues with after, defused when GitHub would read a mention.
// GitHub mentions an '@' that starts the text or follows anything but a letter, digit or
// underscore, and is followed by a letter or digit; what precedes it is judged by what was
// written, since that is what GitHub will read. An underscore before it is treated as
// mentioning too — "_x_@team" is emphasis followed by a mention once rendered — and a letter
// or digit is not, which is what leaves addresses like name@example.com alone.
func (st *scanState) at(after string) {
	prev := st.last()
	if after != "" && isASCIIAlnum(after[0]) && !(prev < utf8.RuneSelf && isASCIIAlnum(byte(prev))) {
		st.write("@" + wordJoiner)
		return
	}
	st.write("@")
}

// namedRef is a complete named character reference. Which names exist does not matter here:
// an unknown one is shown as written, and the only name for '@', "&commat;", is judged first.
var namedRef = regexp.MustCompile(`^&[A-Za-z][A-Za-z0-9]{0,31};`)

// ampersand writes the '&' that starts s and whatever reference it opens, and returns how much
// of s it consumed. A reference is judged by the character it renders as, since that is what
// GitHub's mention filter and a reader see: "&#64;user" is "@user", and "&#x202E;" is the
// bidirectional override dropRune exists to keep out.
//
// Where the text is HTML, a numeric reference needs no ';' — HTML decodes "&#64user" as "@user",
// a parse error but a character all the same — and any other '&' is escaped unless it starts a
// complete reference, so nothing reaches the page as a reference this function did not read. In
// markdown cmark wants the ';', and writes a '&' that starts no reference as "&amp;" itself.
func (st *scanState) ampersand(s string) int {
	if r, n, ok := numericRef(s, st.rawHTML); ok {
		switch {
		case r == '@':
			st.at(s[n:])
		case dropRune(r):
			// dropped, like the character it names
		case s[n-1] != ';':
			// Valid HTML for a harmless character, but written out it would be one more
			// reference whose reading depends on what comes after it; escape it and show it.
			st.write("&amp;")
			return 1
		default:
			st.write(s[:n])
		}
		return n
	}
	if strings.HasPrefix(s, "&commat;") {
		st.at(s[len("&commat;"):])
		return len("&commat;")
	}
	if st.rawHTML {
		if m := namedRef.FindString(s); m != "" {
			st.write(m)
			return len(m)
		}
		st.write("&amp;")
		return 1
	}
	st.write("&")
	return 1
}

// numericRef reads the numeric character reference at the start of s — "&#64;" or "&#x40;" —
// and returns the character it names and its length. With semicolonOptional, as HTML reads one,
// the ';' may be missing: the reference then ends at the first character that cannot extend the
// number, so "&#640" is U+0280 and not an '@' followed by a zero. A number past Unicode names
// U+FFFD, as both HTML and CommonMark have it.
func numericRef(s string, semicolonOptional bool) (r rune, n int, ok bool) {
	if len(s) < 3 || s[0] != '&' || s[1] != '#' {
		return 0, 0, false
	}
	i, hex := 2, s[2] == 'x' || s[2] == 'X'
	if hex {
		i++
	}
	start := i
	for i < len(s) && (hex && isHex(s[i]) || !hex && '0' <= s[i] && s[i] <= '9') {
		i++
	}
	if i == start {
		return 0, 0, false
	}
	semi := i < len(s) && s[i] == ';'
	if !semi && !semicolonOptional {
		return 0, 0, false
	}
	r = utf8.RuneError
	base := 10
	if hex {
		base = 16
	}
	if v, err := strconv.ParseUint(s[start:i], base, 32); err == nil && v <= unicode.MaxRune {
		r = rune(v)
	}
	if semi {
		i++
	}
	return r, i, true
}

// url writes a bare URL: as itself, canonically escaped, when the policy allows it, and in a
// form GitHub never autolinks otherwise.
func (st *scanState) url(tok string) {
	if !st.inLink && strings.Contains(tok, "://") {
		if canon, ok := st.p.allow(tok); ok {
			st.write(canon)
			return
		}
	}
	st.refuseURL(tok)
}

// refuseURL writes a URL that may not be linked. In markdown it goes in a code span, which
// stays copyable; in HTML, where a code span is not one, a word joiner after the scheme or the
// "www" keeps any autolinker from seeing a URL at all.
func (st *scanState) refuseURL(tok string) {
	if !st.rawHTML {
		st.writeCode(codeSpan(tok))
		return
	}
	if i := strings.Index(tok, "://"); i >= 0 {
		tok = tok[:i+1] + wordJoiner + tok[i+1:]
	} else if len(tok) >= 4 && strings.EqualFold(tok[:4], "www.") {
		tok = tok[:3] + wordJoiner + tok[3:]
	}
	tok = strings.Map(func(r rune) rune {
		if r == '<' || r == '`' || dropRune(r) {
			return -1
		}
		return r
	}, tok)
	st.scanPlain(tok)
}

// scanPlain writes text that holds no markup worth keeping, defusing what still needs it.
func (st *scanState) scanPlain(s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '@':
			st.at(s[i+1:])
		case '[', ']':
			st.write(`\` + string(c))
		case '&':
			st.write("&amp;")
		default:
			st.out = append(st.out, c)
		}
	}
}

// urlAt returns the length of the URL GitHub would autolink starting at t[i], or 0. It looks
// for more than GitHub does — any scheme before "://", not only http, https and ftp, and "www."
// after any character that is not a letter or digit — since a URL caught that GitHub would not
// have linked only ends up in a code span.
func urlAt(t string, i int) int {
	c := t[i]
	if !isASCIILetter(c) {
		return 0
	}
	if (i == 0 || !isASCIIAlnum(t[i-1])) && len(t)-i > 4 && strings.EqualFold(t[i:i+4], "www.") {
		return urlEnd(t, i, i+4)
	}
	if i > 0 && isSchemeChar(t[i-1]) {
		return 0 // inside a word already looked at from its start
	}
	j := i
	for j < len(t) && j-i <= 32 && isSchemeChar(t[j]) {
		j++
	}
	if j-i > 32 || !strings.HasPrefix(t[j:], "://") {
		return 0
	}
	return urlEnd(t, i, j+3)
}

// urlEnd finds where an autolinked URL that starts at start ends, as GFM does: at whitespace
// or '<', then less any trailing punctuation and any closing bracket it does not balance. It
// returns 0 when nothing is left after the scheme or "www.".
func urlEnd(t string, start, from int) int {
	j := from
	for j < len(t) && t[j] != '<' && t[j] != '|' && !isSpaceByte(t[j]) {
		j++ // '|' too: in a table row it ends the cell, and so the URL
	}
	for j > from {
		c := t[j-1]
		switch {
		case strings.IndexByte("?!.,:*_~'\";>", c) >= 0:
			j--
			continue
		case c == ')' && strings.Count(t[start:j], ")") > strings.Count(t[start:j], "("):
			j--
			continue
		case c == ']' && strings.Count(t[start:j], "]") > strings.Count(t[start:j], "["):
			j--
			continue
		}
		break
	}
	if j <= from {
		return 0
	}
	return j - start
}

// parseLink reads an inline link — [text](destination "title") — whose '[' is t[i], and
// returns its text, its destination with backslash escapes removed, and the index after it.
// Reference links ([text][ref]) are not read: their '[' is escaped, which also leaves any
// reference definition in the text with nothing to define.
func parseLink(t string, i int) (text, dest string, end int, ok bool) {
	depth, j := 0, i+1
	limit := min(len(t), i+1+maxLinkText)
	found := false
	for ; j < limit && !found; j++ {
		switch t[j] {
		case '\\':
			j++
		case '`':
			n := runLen(t, j, '`')
			if k := codeSpanEnd(t, j, n); k > 0 {
				j = k - 1
			} else {
				j += n - 1
			}
		case '[':
			depth++
		case ']':
			if depth == 0 {
				found = true
				j-- // undo the loop's increment: j is the ']'
			} else {
				depth--
			}
		}
	}
	if !found || j >= len(t) || t[j] != ']' || j+1 >= len(t) || t[j+1] != '(' {
		return "", "", 0, false
	}
	text = t[i+1 : j]
	if strings.Contains(text, "\n\n") || strings.Contains(text, "\n \n") {
		return "", "", 0, false // a blank line ends the paragraph, and a link with it
	}
	k := skipLinkSpace(t, j+2)
	if k < len(t) && t[k] == '<' {
		e := k + 1
		for e < len(t) && e-k <= maxLinkDest && t[e] != '>' && t[e] != '<' && t[e] != '\n' {
			if t[e] == '\\' && e+1 < len(t) {
				e++
			}
			e++
		}
		if e >= len(t) || t[e] != '>' {
			return "", "", 0, false
		}
		dest, k = t[k+1:e], e+1
	} else {
		start, parens := k, 0
	scanDest:
		for k < len(t) && k-start <= maxLinkDest {
			switch c := t[k]; {
			case c == '\\' && k+1 < len(t) && isASCIIPunct(t[k+1]):
				k += 2
				continue
			case c <= ' ' || c == 0x7f:
				break scanDest
			case c == '(':
				parens++
				if parens > 32 {
					return "", "", 0, false
				}
			case c == ')':
				if parens == 0 {
					break scanDest
				}
				parens--
			}
			k++
		}
		if parens != 0 || k-start > maxLinkDest {
			return "", "", 0, false
		}
		dest = t[start:k]
	}
	k = skipLinkSpace(t, k)
	if k < len(t) && (t[k] == '"' || t[k] == '\'' || t[k] == '(') {
		closer := t[k]
		if closer == '(' {
			closer = ')'
		}
		e := k + 1
		for e < len(t) && e-k <= maxLinkTitle && t[e] != closer {
			if t[e] == '\\' {
				e++
			}
			e++
		}
		if e >= len(t) || t[e] != closer {
			return "", "", 0, false
		}
		k = skipLinkSpace(t, e+1)
	}
	if k >= len(t) || t[k] != ')' {
		return "", "", 0, false
	}
	return text, unescapePunct(dest), k + 1, true
}

// skipLinkSpace skips spaces and tabs and at most one line ending, which is all the room
// CommonMark gives a link between its parts.
func skipLinkSpace(t string, k int) int {
	newline := false
	for k < len(t) {
		switch {
		case t[k] == ' ' || t[k] == '\t':
		case t[k] == '\n' && !newline:
			newline = true
		default:
			return k
		}
		k++
	}
	return k
}

func unescapePunct(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// linkPolicy decides which URLs a sanitised text may link to, and writes the ones it allows
// in one canonical form.
type linkPolicy struct {
	repos   map[string]bool
	console *url.URL
}

func newLinkPolicy(repos []string, consoleOrigin string) *linkPolicy {
	p := &linkPolicy{repos: map[string]bool{}}
	for _, r := range repos {
		if r = strings.ToLower(strings.TrimSpace(r)); validRepoName(r) {
			p.repos[r] = true
		}
	}
	if o := strings.TrimRight(strings.TrimSpace(consoleOrigin), "/"); o != "" {
		u, err := url.Parse(o)
		if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" &&
			u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" {
			p.console = u
		}
	}
	return p
}

// allow reports whether raw may be linked to and returns it rewritten canonically: every
// character markdown could read as syntax percent-encoded, and '&' written as "&amp;". The
// check is made on the URL as GitHub will resolve it — character references decoded, as
// markdown decodes them in a destination, and dot segments resolved, as a browser does — so
// that https://github.com/<allowed>/x/../../<other> is refused, and the rewrite makes the URL
// in the output mean exactly the one that was checked.
func (p *linkPolicy) allow(raw string) (string, bool) {
	if p == nil {
		return "", false // no policy allows no link: how a title set inside a link of ours is written
	}
	raw = html.UnescapeString(strings.TrimSpace(raw))
	if raw == "" || len(raw) > maxLinkDest || strings.ContainsFunc(raw, func(r rune) bool {
		return r <= ' ' || r == 0x7f || r == '\\' || unicode.IsSpace(r) || dropRune(r)
	}) {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return "", false
	}
	tail := escapeQuery(u.RawQuery, "?") + escapeQuery(u.EscapedFragment(), "#")
	switch {
	case strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Host, "github.com"):
		clean := path.Clean("/" + u.Path)
		if u.Path != clean && u.Path != clean+"/" {
			return "", false
		}
		parts := strings.SplitN(strings.TrimPrefix(clean, "/"), "/", 3)
		if len(parts) < 2 || !p.repos[strings.ToLower(parts[0]+"/"+parts[1])] {
			return "", false
		}
		return "https://github.com" + escapePath(u.Path) + tail, true
	case p.console != nil && strings.EqualFold(u.Scheme, p.console.Scheme) && strings.EqualFold(u.Host, p.console.Host):
		return p.console.Scheme + "://" + p.console.Host + escapePath(u.Path) + tail, true
	}
	return "", false
}

// escapePath percent-encodes a decoded path, '%' included, so an encoded dot segment the check
// read as a name cannot become a real one in a browser.
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		writeURLByte(&b, p[i], "/:@!$,;=+")
	}
	return b.String()
}

// escapeQuery re-escapes a raw query or fragment, keeping the escapes already in it.
func escapeQuery(raw, lead string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(lead)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '%' && i+2 < len(raw) && isHex(raw[i+1]) && isHex(raw[i+2]) {
			b.WriteByte(c)
			continue
		}
		writeURLByte(&b, c, "/:@!$,;=+?")
	}
	return b.String()
}

func writeURLByte(b *strings.Builder, c byte, keep string) {
	switch {
	case isASCIIAlnum(c) || c == '-' || c == '.' || c == '_' || c == '~' || strings.IndexByte(keep, c) >= 0:
		b.WriteByte(c)
	case c == '&':
		b.WriteString("&amp;")
	default:
		fmt.Fprintf(b, "%%%02X", c)
	}
}

// codeSpan renders s as inline code that shows it literally whatever it holds: on one line,
// fenced by more backticks than any run inside it, with markers and control characters taken
// out. It is how this package shows any text it does not trust as markdown — a path, a URL it
// will not link — and "" for text with nothing to show.
func codeSpan(s string) string {
	s = stripMarkers(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case dropRune(r):
			return -1
		}
		return r
	}, s)
	if strings.TrimSpace(s) == "" {
		return ""
	}
	fence := strings.Repeat("`", longestRun(s, '`')+1)
	if s[0] == '`' || s[len(s)-1] == '`' || (s[0] == ' ' && s[len(s)-1] == ' ') {
		s = " " + s + " " // CommonMark takes one space off each side
	}
	return fence + s + fence
}

// inertText keeps only letters, digits, spaces and a little punctuation: what is left of text
// that would not settle, in a place where even a code span would render.
func inertText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune(".,;-()", r):
			return r
		case unicode.IsSpace(r):
			return ' '
		}
		return -1
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// plainText flattens s to one line of at most maxLen characters with nothing in it that could
// be a marker: for text set inside a code block we build, where nothing renders but the raw
// body is still searched for markers.
func plainText(s string, maxLen int) string {
	s = stripMarkers(s)
	s = strings.Map(func(r rune) rune {
		if dropRune(r) {
			return -1
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if maxLen > 0 && utf8.RuneCountInString(s) > maxLen {
		s = strings.TrimSpace(cutRunes(s, maxLen-1)) + "…"
	}
	return s
}

// StripMarkers removes every attest_tag marker from s — "<!--", any whitespace, "attest_tag:" in
// any case — taking the whole comment with it when it is closed and the opener alone when it is
// not, and touching nothing else. Sanitize does this to model text; the app's proxy does it to
// every other body it sends to GitHub, since the tools that post there do so as the same bot
// whose markers code review trusts.
func StripMarkers(s string) string { return stripMarkers(s) }

// stripMarkers removes every "<!-- attest_tag:" from s, the whole comment when it is closed
// and the opener when it is not. It works in one pass over s and checks for a new opener every
// time it writes a ':', since removing one can bring the pieces of another together:
// "<!-<!-- attest_tag:x -->- attest_tag:" hides a second one inside the first.
func stripMarkers(s string) string {
	if !strings.Contains(strings.ToLower(s), "attest_tag:") {
		return s
	}
	lastClose := strings.LastIndex(s, "-->")
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		b = append(b, s[i])
		i++
		if s[i-1] != ':' {
			continue
		}
		start := markerOpenerAt(b)
		if start < 0 {
			continue
		}
		b = b[:start]
		if lastClose >= i {
			if e := strings.Index(s[i:], "-->"); e >= 0 {
				i += e + 3
			}
		}
	}
	return string(b)
}

// markerOpenerAt returns where b's trailing "<!--", whitespace and "attest_tag:" begins, in
// any case, or -1.
func markerOpenerAt(b []byte) int {
	const word = "attest_tag:"
	if len(b) < len(word)+4 || !strings.EqualFold(string(b[len(b)-len(word):]), word) {
		return -1
	}
	k := len(b) - len(word)
	for k > 0 && isSpaceByte(b[k-1]) {
		k--
	}
	if k < 4 || string(b[k-4:k]) != "<!--" {
		return -1
	}
	return k - 4
}

// dropRune reports characters that have no business in a comment: control characters other
// than tab and newline, and the bidirectional overrides and isolates that make text display in
// another order than it reads ("Trojan Source").
func dropRune(r rune) bool {
	switch {
	case r == '\t' || r == '\n':
		return false
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// cutRunes returns at most n runes of s.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for k := 0; k < n && i < len(s); k++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

func runLen(s string, i int, c byte) int {
	j := i
	for j < len(s) && s[j] == c {
		j++
	}
	return j - i
}

func longestRun(s string, c byte) int {
	best := 0
	for i := 0; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		n := runLen(s, i, c)
		best = max(best, n)
		i += n
	}
	return best
}

func isASCIIPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

func isASCIIAlnum(c byte) bool { return isASCIILetter(c) || '0' <= c && c <= '9' }

func isSchemeChar(c byte) bool { return isASCIIAlnum(c) || c == '+' || c == '.' || c == '-' }

func isHex(c byte) bool { return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
