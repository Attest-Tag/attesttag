package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// File is one entry of GitHub's GET /repos/{owner}/{repo}/pulls/{n}/files: the merge-base
// diff a review is scored against. The json tags are GitHub's own field names, so the API's
// response decodes straight into a []File; Binary and Hunks are ours, and Parse fills them.
type File struct {
	Path      string `json:"filename"`
	PrevPath  string `json:"previous_filename,omitempty"` // set when Status is renamed or copied
	Status    string `json:"status"`                      // added, removed, modified, renamed, copied, changed, unchanged
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	// Patch is this file's unified diff, hunks only, with no "diff --git" or ---/+++ headers.
	// GitHub leaves it out for a binary file and for a text diff too large to show, and the
	// two are told apart only by the line counts: see looksBinary.
	Patch string `json:"patch,omitempty"`

	Binary bool   `json:"-"`
	Hunks  []Hunk `json:"-"`
}

// Hunk is one "@@ -a,b +c,d @@" block. Its new-side lines are exactly NewStart through
// NewStart+NewLines-1 and its old-side lines OldStart through OldStart+OldLines-1, with no
// gaps — ParsePatch refuses a hunk whose lines do not add up to its header — which is what
// lets ValidAnchor treat "both ends are in this hunk" as "the whole range is".
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	// Section is the text git prints after the closing @@, its guess at the enclosing
	// function. Worth keeping because it is the cheapest context a prompt can be given.
	Section string
	Lines   []Line
}

// Line is one line of a hunk. A context line has both numbers; an added line has no old one
// and a deleted line no new one, and 0 stands for "not on that side" because no file has a
// line 0.
type Line struct {
	Kind byte // '+', '-' or ' '
	Old  int  // line number in the base file; 0 for an added line
	New  int  // line number in the head file; 0 for a deleted line
	Text string
}

// Side is which version of the file a comment's line numbers count in, using GitHub's own
// words for it: RIGHT is the head, where added and unchanged lines live, LEFT the base, where
// a deleted line still has a number. A finding about a removed auth check can only be
// anchored on the LEFT.
type Side string

const (
	Right Side = "RIGHT"
	Left  Side = "LEFT"
)

// on returns l's number on side s, or 0 when the line does not exist there.
func (l Line) on(s Side) int {
	switch s {
	case Right:
		return l.New
	case Left:
		return l.Old
	}
	return 0
}

// Parse fills Hunks from Patch and decides Binary. A file with no patch is not an error: it
// simply has nothing a comment can be anchored to, and NoPatchReason says why, for the "Not
// reviewed" list.
func (f *File) Parse() error {
	hunks, err := ParsePatch(f.Patch)
	if err != nil {
		return fmt.Errorf("%s: %w", f.Path, err)
	}
	f.Hunks = hunks
	f.Binary = looksBinary(*f)
	return nil
}

// looksBinary is GitHub's way of showing a binary file: no patch, and no lines counted either
// way. A too-large text diff also has no patch but keeps its counts. A pure rename or a mode
// change has neither, and its status says which it is. An empty file added or removed is
// indistinguishable from a binary one, and calling it binary costs nothing: there is no line
// in it to review.
func looksBinary(f File) bool {
	if f.Patch != "" || f.Additions != 0 || f.Deletions != 0 {
		return false
	}
	switch f.Status {
	case "added", "modified", "removed":
		return true
	}
	return false
}

// NoPatchReason says why a file has no hunks, in the words the "Not reviewed" list uses:
// "binary", "too large", or "" when it has a patch or when nothing in its content changed (a
// pure rename, a mode change), which is not a file the review skipped.
func (f File) NoPatchReason() string {
	switch {
	case f.Patch != "":
		return ""
	case f.Binary || looksBinary(f):
		return "binary"
	case f.Additions+f.Deletions > 0:
		return "too large"
	}
	return ""
}

// hunkHeader is "@@ -old[,count] +new[,count] @@ section". The counts are optional: diff
// leaves out a count of 1, so "@@ -1 +1 @@" is a one-line change, and that form turns up in
// any small file.
var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

// ParsePatch reads GitHub's patch for one file into hunks, numbering every line on each side
// it exists on. An empty patch — a binary file, or a diff GitHub found too large to show — is
// no hunks and no error.
//
// It is strict where being lenient would be a lie to the model: a hunk whose lines do not add
// up to its header, or a line that belongs to no hunk, is an error rather than a guess, because
// the numbers parsed here are the ones the model is told it may comment on, and a wrong number
// is a 422 from GitHub for the whole review. The caller treats a file it cannot parse as one
// it could not review. It is lenient only about what carries no line: a CR before each newline
// (a file with Windows line endings), the "\ No newline at end of file" marker, a trailing
// newline, and a blank line between hunks. An empty line inside a hunk is read as an empty
// context line whose leading space was stripped on the way, which is what git apply does too.
func ParsePatch(patch string) ([]Hunk, error) {
	if patch == "" {
		return nil, nil
	}
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	patch = strings.TrimSuffix(patch, "\n")

	var (
		hunks            []Hunk
		cur              *Hunk
		oldLeft, newLeft int // lines the current hunk's header still owes on each side
		oldN, newN       int // the number the next line on each side will have
	)
	for i, raw := range strings.Split(patch, "\n") {
		at := i + 1
		if strings.HasPrefix(raw, "@@") {
			if cur != nil && (oldLeft > 0 || newLeft > 0) {
				return nil, fmt.Errorf("line %d: hunk ends %d old and %d new lines short of its header", at, oldLeft, newLeft)
			}
			h, err := parseHunkHeader(raw)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", at, err)
			}
			hunks = append(hunks, h)
			cur = &hunks[len(hunks)-1]
			oldLeft, newLeft = h.OldLines, h.NewLines
			oldN, newN = h.OldStart, h.NewStart
			continue
		}
		if cur == nil {
			// GitHub's patch starts with its first hunk; anything before one is not a patch
			// this package was written for, and guessing past it would number lines wrongly.
			return nil, fmt.Errorf("line %d: expected a hunk header, got %q", at, clip(raw))
		}
		if strings.HasPrefix(raw, `\`) {
			// "\ No newline at end of file": about the line before it, and not a line itself.
			continue
		}
		if oldLeft == 0 && newLeft == 0 {
			if raw == "" {
				continue
			}
			return nil, fmt.Errorf("line %d: %q is outside any hunk", at, clip(raw))
		}
		kind, text := byte(' '), ""
		if raw != "" {
			kind, text = raw[0], raw[1:]
		}
		switch kind {
		case ' ':
			if oldLeft == 0 || newLeft == 0 {
				return nil, fmt.Errorf("line %d: more context lines than the hunk header counts", at)
			}
			cur.Lines = append(cur.Lines, Line{Kind: ' ', Old: oldN, New: newN, Text: text})
			oldN, newN, oldLeft, newLeft = oldN+1, newN+1, oldLeft-1, newLeft-1
		case '-':
			if oldLeft == 0 {
				return nil, fmt.Errorf("line %d: more deleted lines than the hunk header counts", at)
			}
			cur.Lines = append(cur.Lines, Line{Kind: '-', Old: oldN, Text: text})
			oldN, oldLeft = oldN+1, oldLeft-1
		case '+':
			if newLeft == 0 {
				return nil, fmt.Errorf("line %d: more added lines than the hunk header counts", at)
			}
			cur.Lines = append(cur.Lines, Line{Kind: '+', New: newN, Text: text})
			newN, newLeft = newN+1, newLeft-1
		default:
			return nil, fmt.Errorf("line %d: %q is not a diff line", at, clip(raw))
		}
	}
	if oldLeft > 0 || newLeft > 0 {
		return nil, fmt.Errorf("last hunk ends %d old and %d new lines short of its header", oldLeft, newLeft)
	}
	return hunks, nil
}

func parseHunkHeader(s string) (Hunk, error) {
	m := hunkHeader.FindStringSubmatch(s)
	if m == nil {
		return Hunk{}, fmt.Errorf("malformed hunk header %q", clip(s))
	}
	var h Hunk
	var err error
	num := func(v string, omitted int) int {
		if v == "" || err != nil {
			return omitted
		}
		var n int
		n, err = strconv.Atoi(v)
		return n
	}
	h.OldStart, h.OldLines = num(m[1], 0), num(m[2], 1)
	h.NewStart, h.NewLines = num(m[3], 0), num(m[4], 1)
	if err != nil {
		return Hunk{}, fmt.Errorf("hunk header %q: %w", clip(s), err)
	}
	// A side with lines starts at line 1 or later. "-0,0" is how a new file's empty base is
	// written; "-0,3" would hand out a line 0, which Line uses to mean "not on this side".
	if (h.OldLines > 0 && h.OldStart < 1) || (h.NewLines > 0 && h.NewStart < 1) {
		return Hunk{}, fmt.Errorf("hunk header %q numbers a line 0", clip(s))
	}
	h.Section = strings.TrimPrefix(m[5], " ")
	return h, nil
}

// clip keeps an error message about a bad line short, since the line came from a stranger's
// pull request and may be a megabyte of minified JavaScript.
func clip(s string) string {
	const limit = 60
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ValidAnchor reports whether a comment on lines startLine through line, counted on side, is
// one GitHub will accept: both ends must exist on that side within one hunk. A RIGHT comment
// may sit on added and context lines, by their head number; a LEFT one on deleted and context
// lines, by their base number. A startLine of 0 means a single-line comment on line.
//
// GitHub answers a bad anchor with a 422 that does not say which comment was wrong, so the
// whole review fails; that is why this is checked for every finding before posting rather
// than learned from the response. A range spanning two hunks is refused even though both ends
// are in the diff, because the lines between them are not, and GitHub refuses it too.
func ValidAnchor(hunks []Hunk, side Side, startLine, line int) bool {
	if startLine == 0 {
		startLine = line
	}
	if line < 1 || startLine < 1 || startLine > line {
		return false
	}
	for _, h := range hunks {
		var haveStart, haveEnd bool
		for _, l := range h.Lines {
			switch l.on(side) {
			case 0:
			case startLine:
				haveStart = true
				if startLine == line {
					haveEnd = true
				}
			case line:
				haveEnd = true
			}
		}
		if haveStart && haveEnd {
			return true
		}
	}
	return false
}

// NumberedPatch renders a file's hunks for the model with every line carrying the number a
// comment would use: R<n> for a line that exists in the head (added or context), L<n> for a
// deleted line, by its number in the base. The diff's own +, - or space follows the number,
// so the model can still tell what this pull request added from what it merely shows around
// it. The model answers in these coordinates, and ValidAnchor checks them against the same
// hunks, so a finding is never re-numbered between the prompt and the post.
//
// Labels are padded to one width per file to keep the code's indentation lined up, which is
// most of what makes a diff readable to anyone, a model included.
func NumberedPatch(f File) string {
	var b strings.Builder
	b.WriteString("## File: ")
	b.WriteString(f.Path)
	switch {
	case f.PrevPath != "" && f.PrevPath != f.Path:
		fmt.Fprintf(&b, " (%s from %s, +%d -%d)\n", f.Status, f.PrevPath, f.Additions, f.Deletions)
	default:
		fmt.Fprintf(&b, " (%s, +%d -%d)\n", f.Status, f.Additions, f.Deletions)
	}

	hunks := f.Hunks
	if hunks == nil && f.Patch != "" {
		var err error
		if hunks, err = ParsePatch(f.Patch); err != nil {
			b.WriteString("(the diff of this file could not be read)\n")
			return b.String()
		}
	}
	if len(hunks) == 0 {
		if why := f.NoPatchReason(); why != "" {
			fmt.Fprintf(&b, "(no diff shown: %s)\n", why)
		} else {
			b.WriteString("(no change to the content)\n")
		}
		return b.String()
	}

	width := 1
	for _, h := range hunks {
		for _, l := range h.Lines {
			width = max(width, len(strconv.Itoa(max(l.Old, l.New))))
		}
	}
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		if h.Section != "" {
			b.WriteString(" ")
			b.WriteString(h.Section)
		}
		b.WriteString("\n")
		for _, l := range h.Lines {
			side, n := 'R', l.New
			if l.Kind == '-' {
				side, n = 'L', l.Old
			}
			fmt.Fprintf(&b, "%c%-*d %c%s\n", side, width, n, l.Kind, l.Text)
		}
	}
	return b.String()
}

// PatchHash fingerprints what a pull request does to one file: a sha256 over its added and
// removed lines only, in order, with no hunk headers and no context. Merging the base into the
// branch shifts line numbers and changes the context around a hunk without changing what the
// branch does, and must not make a file look changed since it was last reviewed; a force push
// that rewrites history but leaves the same edits needs no ancestry check for the same reason.
//
// A file with no patch returns "", which callers treat as "cannot tell, so changed": a binary
// file can change without its (empty) patch changing. A patch that will not parse hashes as
// raw text, so an identical bad patch still compares equal and any other change still shows.
func PatchHash(f File) string {
	hunks := f.Hunks
	if hunks == nil {
		if f.Patch == "" {
			return ""
		}
		var err error
		if hunks, err = ParsePatch(f.Patch); err != nil {
			sum := sha256.Sum256([]byte("raw\x00" + f.Patch))
			return hex.EncodeToString(sum[:])
		}
	}
	if len(hunks) == 0 {
		return ""
	}
	h := sha256.New()
	for _, hk := range hunks {
		for _, l := range hk.Lines {
			if l.Kind == ' ' {
				continue
			}
			h.Write([]byte{l.Kind})
			h.Write([]byte(l.Text))
			h.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
