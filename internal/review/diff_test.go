package review

import (
	"encoding/json"
	"strings"
	"testing"
)

// samplePatch is what GitHub sends for a modified file with two hunks: the first adds two
// lines and removes one, with an empty context line whose leading space was lost on the way,
// the second changes one line further down, where the head numbering has drifted one ahead
// of the base.
const samplePatch = "@@ -1,4 +1,5 @@ package main\n" +
	" import \"fmt\"\n" +
	"-func a() {}\n" +
	"+func a() { fmt.Println() }\n" +
	"+func b() {}\n" +
	"\n" +
	" func c() {}\n" +
	"@@ -20,3 +21,3 @@ func d() {\n" +
	" \tx := 1\n" +
	"-\ty := 2\n" +
	"+\ty := 3\n" +
	" \treturn"

func mustParse(t *testing.T, patch string) []Hunk {
	t.Helper()
	h, err := ParsePatch(patch)
	if err != nil {
		t.Fatalf("ParsePatch: %v", err)
	}
	return h
}

func TestParsePatchNumbersEveryLineOnEachSideItExists(t *testing.T) {
	hunks := mustParse(t, samplePatch)
	if len(hunks) != 2 {
		t.Fatalf("got %d hunks, want 2", len(hunks))
	}
	h := hunks[0]
	if h.OldStart != 1 || h.OldLines != 4 || h.NewStart != 1 || h.NewLines != 5 || h.Section != "package main" {
		t.Errorf("first header = %+v", h)
	}
	want := []Line{
		{' ', 1, 1, `import "fmt"`},
		{'-', 2, 0, "func a() {}"},
		{'+', 0, 2, "func a() { fmt.Println() }"},
		{'+', 0, 3, "func b() {}"},
		{' ', 3, 4, ""},
		{' ', 4, 5, "func c() {}"},
	}
	if len(h.Lines) != len(want) {
		t.Fatalf("first hunk has %d lines, want %d: %+v", len(h.Lines), len(want), h.Lines)
	}
	for i, l := range h.Lines {
		if l != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, l, want[i])
		}
	}
	h = hunks[1]
	want = []Line{
		{' ', 20, 21, "\tx := 1"},
		{'-', 21, 0, "\ty := 2"},
		{'+', 0, 22, "\ty := 3"},
		{' ', 22, 23, "\treturn"},
	}
	for i, l := range h.Lines {
		if l != want[i] {
			t.Errorf("second hunk line %d = %+v, want %+v", i, l, want[i])
		}
	}
}

func TestParsePatchAcceptsWhatGitHubActuallySends(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  []Hunk
	}{
		{"empty patch is a binary or too-large file, not an error", "", nil},
		{"omitted counts mean one line", "@@ -1 +1 @@\n-a\n+b", []Hunk{{
			OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
			Lines: []Line{{'-', 1, 0, "a"}, {'+', 0, 1, "b"}},
		}}},
		{"new file", "@@ -0,0 +1,2 @@\n+a\n+b", []Hunk{{
			OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 2,
			Lines: []Line{{'+', 0, 1, "a"}, {'+', 0, 2, "b"}},
		}}},
		{"deleted file", "@@ -1,2 +0,0 @@\n-a\n-b", []Hunk{{
			OldStart: 1, OldLines: 2, NewStart: 0, NewLines: 0,
			Lines: []Line{{'-', 1, 0, "a"}, {'-', 2, 0, "b"}},
		}}},
		{"no newline markers are not lines",
			"@@ -1,2 +1,3 @@\n a\n-b\n\\ No newline at end of file\n+b\n+c\n\\ No newline at end of file",
			[]Hunk{{
				OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 3,
				Lines: []Line{{' ', 1, 1, "a"}, {'-', 2, 0, "b"}, {'+', 0, 2, "b"}, {'+', 0, 3, "c"}},
			}}},
		{"CRLF line endings are not part of the text", "@@ -1,2 +1,2 @@\r\n a\r\n-b\r\n+c\r\n", []Hunk{{
			OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 2,
			Lines: []Line{{' ', 1, 1, "a"}, {'-', 2, 0, "b"}, {'+', 0, 2, "c"}},
		}}},
		{"a trailing newline and a blank line after the last hunk", "@@ -1 +1 @@\n-a\n+b\n\n", []Hunk{{
			OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
			Lines: []Line{{'-', 1, 0, "a"}, {'+', 0, 1, "b"}},
		}}},
		{"a section heading is kept without its space", "@@ -3 +3 @@ func (s *Store) Get() {\n-a\n+b", []Hunk{{
			OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Section: "func (s *Store) Get() {",
			Lines: []Line{{'-', 3, 0, "a"}, {'+', 0, 3, "b"}},
		}}},
		{"diff markers inside the text are text", "@@ -1 +1 @@\n---- a heading\n+++ b", []Hunk{{
			OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
			Lines: []Line{{'-', 1, 0, "--- a heading"}, {'+', 0, 1, "++ b"}},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustParse(t, c.patch)
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(c.want)
			if string(gj) != string(wj) {
				t.Errorf("got  %s\nwant %s", gj, wj)
			}
		})
	}
}

// Every one of these would number a line wrongly if it were guessed past, and a wrong number
// is a 422 for the whole review.
func TestParsePatchRefusesWhatItCannotNumber(t *testing.T) {
	cases := []struct{ name, patch, want string }{
		{"not a patch", "hello", "expected a hunk header"},
		{"full git diff headers", "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b", "expected a hunk header"},
		{"malformed header", "@@ -x +1 @@\n+a", "malformed hunk header"},
		{"header without closing @@", "@@ -1 +1\n+a", "malformed hunk header"},
		{"a line 0", "@@ -0,2 +1,2 @@\n a\n b", "numbers a line 0"},
		{"a number too large to read", "@@ -99999999999999999999 +1 @@\n-a\n+b", "hunk header"},
		{"hunk cut short at the end", "@@ -1,2 +1,2 @@\n a", "last hunk ends 1 old and 1 new lines short"},
		{"hunk cut short before the next", "@@ -1,2 +1,2 @@\n a\n@@ -9 +9 @@\n-x\n+y", "hunk ends 1 old and 1 new lines short"},
		{"more added lines than counted", "@@ -1 +1 @@\n-a\n+b\n+c", "outside any hunk"},
		{"more deleted lines than counted", "@@ -1 +1,2 @@\n-a\n-b\n+c", "more deleted lines"},
		{"more context than counted on one side", "@@ -1 +1,2 @@\n-a\n+b\n c", "more context lines"},
		{"added overflow inside an open hunk", "@@ -1,2 +1 @@\n+a\n+b", "more added lines"},
		{"not a diff line", "@@ -1,2 +1,2 @@\n a\n*b", "is not a diff line"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePatch(c.patch)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestParsePatchErrorsKeepAHostileLineShort(t *testing.T) {
	_, err := ParsePatch("@@ -1 +1 @@\n*" + strings.Repeat("é", 500))
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("err = %v; a bad line should be clipped in the message", err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Errorf("clipped line should say so: %v", err)
	}
}

func TestValidAnchorAllowsOnlyLinesOnThatSideInOneHunk(t *testing.T) {
	hunks := mustParse(t, samplePatch)
	cases := []struct {
		name        string
		side        Side
		start, line int
		want        bool
	}{
		{"an added line", Right, 0, 2, true},
		{"a context line on the right", Right, 0, 5, true},
		{"a range inside one hunk", Right, 2, 5, true},
		{"start equal to line", Right, 3, 3, true},
		{"the second hunk by its head numbers", Right, 21, 23, true},
		{"a head number between hunks", Right, 0, 6, false},
		{"a range spanning two hunks", Right, 4, 22, false},
		{"a base-only number on the right", Right, 0, 20, false},
		{"a deleted line on the left", Left, 0, 2, true},
		{"a context line on the left", Left, 0, 4, true},
		{"the whole first hunk on the left", Left, 1, 4, true},
		{"a deleted line in the second hunk", Left, 0, 21, true},
		{"past the base side of the second hunk", Left, 0, 23, false},
		{"start after line", Right, 5, 2, false},
		{"line 0", Right, 0, 0, false},
		{"negative start", Right, -1, 2, false},
		{"an unknown side", Side("MIDDLE"), 0, 2, false},
		{"lowercase side is not GitHub's", Side("right"), 0, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidAnchor(hunks, c.side, c.start, c.line); got != c.want {
				t.Errorf("ValidAnchor(%s, %d, %d) = %v, want %v", c.side, c.start, c.line, got, c.want)
			}
		})
	}
	if ValidAnchor(nil, Right, 0, 1) {
		t.Error("a file with no hunks has no line to anchor to")
	}
}

func TestNumberedPatchLabelsEveryLineWithTheNumberACommentWouldUse(t *testing.T) {
	f := File{Path: "cmd/main.go", Status: "modified", Additions: 3, Deletions: 2, Patch: samplePatch}
	if err := f.Parse(); err != nil {
		t.Fatal(err)
	}
	want := "## File: cmd/main.go (modified, +3 -2)\n" +
		"@@ -1,4 +1,5 @@ package main\n" +
		"R1   import \"fmt\"\n" +
		"L2  -func a() {}\n" +
		"R2  +func a() { fmt.Println() }\n" +
		"R3  +func b() {}\n" +
		"R4   \n" +
		"R5   func c() {}\n" +
		"@@ -20,3 +21,3 @@ func d() {\n" +
		"R21  \tx := 1\n" +
		"L21 -\ty := 2\n" +
		"R22 +\ty := 3\n" +
		"R23  \treturn\n"
	if got := NumberedPatch(f); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// A caller that never called Parse gets the same rendering.
	f.Hunks = nil
	if got := NumberedPatch(f); got != want {
		t.Errorf("unparsed file rendered differently:\n%s", got)
	}
}

func TestNumberedPatchSaysWhyAFileHasNoLines(t *testing.T) {
	cases := []struct {
		f    File
		want string
	}{
		{File{Path: "logo.png", Status: "added"}, "## File: logo.png (added, +0 -0)\n(no diff shown: binary)\n"},
		{File{Path: "big.sql", Status: "modified", Additions: 50000, Deletions: 3}, "## File: big.sql (modified, +50000 -3)\n(no diff shown: too large)\n"},
		{File{Path: "new.go", PrevPath: "old.go", Status: "renamed"}, "## File: new.go (renamed from old.go, +0 -0)\n(no change to the content)\n"},
		{File{Path: "x.go", Status: "modified", Additions: 1, Patch: "@@ nonsense"}, "## File: x.go (modified, +1 -0)\n(the diff of this file could not be read)\n"},
	}
	for _, c := range cases {
		if got := NumberedPatch(c.f); got != c.want {
			t.Errorf("%s:\ngot  %q\nwant %q", c.f.Path, got, c.want)
		}
	}
}

func TestFileParseTellsBinaryFromTooLarge(t *testing.T) {
	cases := []struct {
		name   string
		f      File
		binary bool
		reason string
	}{
		{"binary added", File{Status: "added"}, true, "binary"},
		{"binary modified", File{Status: "modified"}, true, "binary"},
		{"too large to show", File{Status: "modified", Additions: 9000, Deletions: 12}, false, "too large"},
		{"pure rename", File{Status: "renamed", PrevPath: "a"}, false, ""},
		{"mode change", File{Status: "changed"}, false, ""},
		{"ordinary text change", File{Status: "modified", Additions: 1, Patch: "@@ -1 +1 @@\n-a\n+b"}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			if err := f.Parse(); err != nil {
				t.Fatal(err)
			}
			if f.Binary != c.binary || f.NoPatchReason() != c.reason {
				t.Errorf("Binary = %v, reason = %q; want %v, %q", f.Binary, f.NoPatchReason(), c.binary, c.reason)
			}
		})
	}
	bad := File{Path: "web/app.ts", Patch: "garbage"}
	if err := bad.Parse(); err == nil || !strings.HasPrefix(err.Error(), "web/app.ts: ") {
		t.Errorf("a parse error should name the file: %v", err)
	}
}

func TestFileDecodesFromGitHubsFilesResponse(t *testing.T) {
	body := `[{"sha":"abc","filename":"src/new.ts","previous_filename":"src/old.ts","status":"renamed",
		"additions":1,"deletions":1,"changes":2,"patch":"@@ -1 +1 @@\n-a\n+b"}]`
	var files []File
	if err := json.Unmarshal([]byte(body), &files); err != nil {
		t.Fatal(err)
	}
	f := files[0]
	if f.Path != "src/new.ts" || f.PrevPath != "src/old.ts" || f.Status != "renamed" ||
		f.Additions != 1 || f.Deletions != 1 || f.Patch == "" {
		t.Errorf("decoded %+v", f)
	}
}

func TestPatchHashIgnoresWhatABaseMergeChanges(t *testing.T) {
	reviewed := File{Patch: "@@ -10,3 +10,4 @@ func x() {\n a\n-b\n+c\n+d\n e"}
	afterMerge := File{Patch: "@@ -40,3 +42,4 @@ func y() {\n z\n-b\n+c\n+d\n w"}
	if PatchHash(reviewed) != PatchHash(afterMerge) {
		t.Error("moved hunks with new context but the same edits should hash the same")
	}
	split := File{Patch: "@@ -1,2 +1,2 @@\n-b\n+c\n q\n@@ -9,1 +9,2 @@\n+d\n r"}
	if PatchHash(reviewed) != PatchHash(split) {
		t.Error("the same edits split differently across hunks should hash the same")
	}
	crlf := File{Patch: strings.ReplaceAll(reviewed.Patch, "\n", "\r\n")}
	if PatchHash(reviewed) != PatchHash(crlf) {
		t.Error("line endings should not change the hash")
	}
	parsed := reviewed
	if err := parsed.Parse(); err != nil {
		t.Fatal(err)
	}
	if PatchHash(parsed) != PatchHash(reviewed) {
		t.Error("a parsed file and an unparsed one should hash the same")
	}

	for name, other := range map[string]File{
		"an added line changed":   {Patch: "@@ -10,3 +10,4 @@\n a\n-b\n+c\n+D\n e"},
		"a removed line changed":  {Patch: "@@ -10,3 +10,4 @@\n a\n-B\n+c\n+d\n e"},
		"an added line reordered": {Patch: "@@ -10,3 +10,4 @@\n a\n-b\n+d\n+c\n e"},
		"add and remove swapped":  {Patch: "@@ -10,3 +10,4 @@\n a\n+b\n-c\n+d\n e"},
	} {
		if PatchHash(other) == PatchHash(reviewed) {
			t.Errorf("%s: hash did not change", name)
		}
	}
}

func TestPatchHashOfAFileWithoutAPatch(t *testing.T) {
	if h := PatchHash(File{Status: "added"}); h != "" {
		t.Errorf("no patch should hash as unknown (\"\"), got %q", h)
	}
	a, b := PatchHash(File{Patch: "garbage one"}), PatchHash(File{Patch: "garbage two"})
	if a == "" || a == b || a != PatchHash(File{Patch: "garbage one"}) {
		t.Errorf("an unparseable patch should hash its raw text: %q %q", a, b)
	}
}
