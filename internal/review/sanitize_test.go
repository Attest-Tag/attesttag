package review

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var testSanitize = SanitizeOptions{
	AllowedRepos:  []string{"acme/web", "Acme/API"},
	ConsoleOrigin: "https://console.example.com",
}

const wj = "\u2060"

type sanitizeCase struct {
	name, in, want string
}

func runSanitizeCases(t *testing.T, cases []sanitizeCase) {
	t.Helper()
	for _, c := range cases {
		got := Sanitize(c.in, testSanitize)
		if got != c.want {
			t.Errorf("%s:\n Sanitize(%q)\n = %q\nwant %q", c.name, c.in, got, c.want)
		}
		if again := Sanitize(got, testSanitize); again != got {
			t.Errorf("%s: not stable: Sanitize(%q) = %q", c.name, got, again)
		}
	}
}

func TestSanitizeHTML(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"plain text is untouched", "Two refreshes race; the older one wins.", "Two refreshes race; the older one wins."},
		{"disallowed tags go, their text stays", "<b>bold</b> and <span style=\"color:red\">red</span>", "bold and red"},
		{"allowed tags lose their attributes", "<details open><summary class=x>More</summary>body</details>", "<details><summary>More</summary>body</details>"},
		{"allowed tags in any case", "<SUB>small</SUB> and <Code>x</Code><BR/>", "<sub>small</sub> and <code>x</code><br>"},
		{"an unclosed details is closed", "<details><summary>Steps</summary>one", "<details><summary>Steps</summary>one</details>"},
		{"a closer that closes nothing is dropped", "stray </summary> closer", "stray  closer"},
		{"closing an outer tag closes the inner one", "<details><sub>x</details>", "<details><sub>x</sub></details>"},
		{"script content is left as text", "<script>alert(1)</script>", "alert(1)"},
		{"an anchor keeps its text, not its href", `<a href="https://evil.example">click</a>`, "click"},
		{"a '<' that is not a tag is escaped", "x < y and a<b && c>d", "x &lt; y and a&lt;b && c>d"},
		{"a generic outside code is a tag to GitHub too", "returns Vec<String> here", "returns Vec here"},
		{"declarations are not left to open anything", "<!DOCTYPE html><?php echo 1 ?>", "&lt;!DOCTYPE html>&lt;?php echo 1 ?>"},
		{"removing a tag cannot reassemble another", "<scr<b>ipt>x", "&lt;script>x"},
	})
}

func TestSanitizeImages(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"markdown image", "![logo](https://evil.example/x.png) after", " after"},
		{"image to an allowed repo is still an image", "![x](https://github.com/acme/web/raw/main/a.png)", ""},
		{"html image", `<img src="https://evil.example/t.gif" onerror="alert(1)">`, ""},
		{"a badge link whose only text was an image is dropped whole", "[![ci](https://evil.example/b.svg)](https://github.com/acme/web)", ""},
		{"a reference-style image cannot form", "![x][ref]\n\n[ref]: https://evil.example/x.png", "!\\[x\\]\\[ref\\]\n\n\\[ref\\]: `https://evil.example/x.png`"},
		{"an image cannot be rebuilt around a removed tag", "!<b></b>[x](https://github.com/acme/web)", "!" + wj + "[x](https://github.com/acme/web)"},
	})
}

func TestSanitizeMentions(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"user and team", "ping @octocat and @octo-org/reviewers", "ping @" + wj + "octocat and @" + wj + "octo-org/reviewers"},
		{"at the start", "@octocat look", "@" + wj + "octocat look"},
		{"after punctuation", "(@octocat), -@octocat", "(@" + wj + "octocat), -@" + wj + "octocat"},
		{"email addresses are left alone", "write to dev@example.com or first.last@example.org", "write to dev@example.com or first.last@example.org"},
		{"a bare at sign", "@ and @ ", "@ and @ "},
		{"inline code is left alone", "use `@Override` or `@octocat`", "use `@Override` or `@octocat`"},
		{"a code block is left alone", "```java\n@Override\nvoid f() {}\n```", "```java\n@Override\nvoid f() {}\n```\n"},
		{"an escaped at sign still renders as one", `\@octocat`, "@" + wj + "octocat"},
		{"a character reference for at", "&#64;octocat &#x40;octocat &commat;octocat", "@" + wj + "octocat @" + wj + "octocat @" + wj + "octocat"},
		{"after emphasis", "_x_@octocat and **y**@octocat", "_x_@" + wj + "octocat and **y**@" + wj + "octocat"},
		{"a mention rebuilt by removing a tag", "@<b></b>octocat", "@" + wj + "octocat"},
		{"inside a kept link's text", "[@octocat](https://github.com/acme/web)", "[@" + wj + "octocat](https://github.com/acme/web)"},
	})
}

func TestSanitizeLinks(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"a link into an allowed repo", "[docs](https://github.com/acme/web/blob/main/README.md)", "[docs](https://github.com/acme/web/blob/main/README.md)"},
		{"repo names compare without case", "[x](https://GitHub.com/ACME/api/pull/7)", "[x](https://github.com/ACME/api/pull/7)"},
		{"another repository keeps only its text", "[x](https://github.com/octo-org/other/blob/main/a.go)", "x"},
		{"another site keeps only its text", "[sign in again](https://evil.example/login)", "sign in again"},
		{"a dot segment out of an allowed repo", "[x](https://github.com/acme/web/../../octo-org/other)", "x"},
		{"an encoded dot segment", "[x](https://github.com/acme/web/%2e%2e/%2e%2e/octo-org/other)", "x"},
		{"a character reference hiding a dot segment", "[x](https://github.com/acme/web/&#46;&#46;/&#46;&#46;/octo-org/other)", "x"},
		{"a doubly encoded dot segment stays a name", "[x](https://github.com/acme/web/%252e%252e/x)", "[x](https://github.com/acme/web/%252e%252e/x)"},
		{"not https", "[x](http://github.com/acme/web)", "x"},
		{"a port", "[x](https://github.com:8443/acme/web)", "x"},
		{"user info", "[x](https://acme@github.com/acme/web)", "x"},
		{"a repository's owner alone", "[x](https://github.com/acme)", "x"},
		{"javascript", "[x](javascript:alert(1))", "x"},
		{"the console origin", "[review](https://console.example.com/reviews/1?tab=findings)", "[review](https://console.example.com/reviews/1?tab=findings)"},
		{"a host that merely starts with the console's", "[x](https://console.example.com.evil.example/)", "x"},
		{"the console origin as user info", "[x](https://console.example.com@evil.example/)", "x"},
		{"query and fragment are kept, & is written as a reference", "[q](https://github.com/acme/web/blob/main/a.go?plain=1&x=2#L3-L5)", "[q](https://github.com/acme/web/blob/main/a.go?plain=1&amp;x=2#L3-L5)"},
		{"markdown-significant characters are encoded", "[x](https://github.com/acme/web/blob/main/a(1)*[2].md)", "[x](https://github.com/acme/web/blob/main/a%281%29%2A%5B2%5D.md)"},
		{"a space is not a URL", "[x](<https://github.com/acme/web/a b.md>)", "x"},
		{"a title is dropped", `[x](https://github.com/acme/web "hover me")`, "[x](https://github.com/acme/web)"},
		{"a link inside a kept link keeps its text only", "[a [b](https://evil.example) c](https://github.com/acme/web)", "[a b c](https://github.com/acme/web)"},
		{"brackets that are not a link are escaped", "a [b] c [d][ref]", `a \[b\] c \[d\]\[ref\]`},
		{"an empty destination", "[x]()", "x"},
	})
}

func TestSanitizeBareURLs(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"an outside URL goes in code", "see https://evil.example/login now", "see `https://evil.example/login` now"},
		{"trailing punctuation stays outside", "see https://evil.example/login.", "see `https://evil.example/login`."},
		{"an unbalanced closing parenthesis stays outside", "(see https://evil.example/a)", "(see `https://evil.example/a`)"},
		{"www", "go to www.evil.example today", "go to `www.evil.example` today"},
		{"an allowed URL stays a URL", "see https://github.com/acme/web/pull/1.", "see https://github.com/acme/web/pull/1."},
		{"an autolink to an allowed repo", "<https://github.com/acme/web/issues/2>", "<https://github.com/acme/web/issues/2>"},
		{"an autolink elsewhere", "<https://evil.example>", "`https://evil.example`"},
		{"an email autolink is plain text", "<dev@example.com>", "dev@example.com"},
		{"any scheme", "ftp://files.evil.example/x and xhttps://evil.example", "`ftp://files.evil.example/x` and `xhttps://evil.example`"},
		{"a URL rebuilt by removing a tag", "https:<b></b>//evil.example", "`https://evil.example`"},
		{"a URL as a dropped link's text", "[https://evil.example](https://evil.example)", "`https://evil.example`"},
		{"a URL with backticks in it", "https://evil.example/`x`", "`` https://evil.example/`x` ``"},
	})
}

func TestSanitizeCommentsAndMarkers(t *testing.T) {
	marker := Marker(testMarkerKey, scope(1), MarkerFinding, testFindingID)
	runSanitizeCases(t, []sanitizeCase{
		{"a comment", "a <!-- hidden --> b", "a  b"},
		{"a comment over lines", "a <!--\nhidden\n--> b", "a  b"},
		{"an unclosed comment loses its opener", "a <!-- hides the rest", "a  hides the rest"},
		{"a marker in text", "x " + marker + " y", "x  y"},
		{"a marker in a code block", "```\n" + marker + "\nkeep()\n```", "```\n\nkeep()\n```\n"},
		{"a marker in inline code", "`a" + marker + "b`", "`ab`"},
		{"a marker hidden inside another", "```\n<!-<!-- attest_tag:x -->- attest_tag:finding=a.0123456789abcdef -->\n```", "```\n\n```\n"},
		{"any spacing or case of the opener", "```\n<!--   ATTEST_TAG:run=x.y -->\n```", "```\n\n```\n"},
		{"other comments in code are code", "```html\n<!-- a comment -->\n```", "```html\n<!-- a comment -->\n```\n"},
	})
	if out := Sanitize("```\n"+marker+"\n```", testSanitize); len(ParseMarkers(out)) != 0 {
		t.Errorf("a marker survived in %q", out)
	}
}

func TestSanitizeCode(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"a fenced block passes through", "```go\nif a < b && [x](y) { @z }\n```", "```go\nif a < b && [x](y) { @z }\n```\n"},
		{"a tilde fence becomes a backtick fence", "text\n\n~~~py\nprint(1)\n~~~\nafter", "text\n\n```py\nprint(1)\n```\nafter"},
		{"an unclosed block is closed", "```go\ncode", "```go\ncode\n```\n"},
		{"a fence longer than anything inside", "~~~\n```\nnot a closer\n```\n~~~", "````\n```\nnot a closer\n```\n````\n"},
		{"a suggestion block is not one", "```suggestion\nrm -rf /\n```", "```\nrm -rf /\n```\n"},
		{"neither is a diagram", "```mermaid\ngraph TD; A-->B\n```", "```\ngraph TD; A-->B\n```\n"},
		{"an info string is one word", "```go title=\"x\"\nf()\n```", "```go\nf()\n```\n"},
		{"a block inside raw HTML gets a blank line first", "<details>\n```\n<img src=x>\n```\n</details>", "<details>\n\n```\n<img src=x>\n```\n</details>"},
		{"an indented opener", "  ```\n  two\n   three\n  ```", "```\ntwo\n three\n```\n"},
		{"inline code is untouched", "call `f(<b>, [x](y))` now", "call `f(<b>, [x](y))` now"},
		{"a double-backtick span", "``a ` b``", "``a ` b``"},
		{"an unmatched backtick is escaped", "a ` b", "a \\` b"},
		{"a span does not cross lines", "a `x\n[y](https://evil.example)` b", "a \\`x\ny\\` b"},
	})
}

// Inside an HTML block GitHub reads no markdown, so code there is not code: the mention in
// "`@name`" is a mention, and a URL in backticks is a URL. A table row is split at its pipes
// before code spans are read, so a pipe in a code span would open the rest of it to markdown.
func TestSanitizeRawHTMLAndTables(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"code inside an HTML block is not code", "<details>\n`@octocat`\n</details>", "<details>\n`@" + wj + "octocat`\n</details>"},
		{"after a blank line it is markdown again", "<details><summary>x</summary>\n\n`@octocat`\n\n</details>", "<details><summary>x</summary>\n\n`@octocat`\n\n</details>"},
		{"an HTML block in a list item", "- <details>\n  `@octocat`", "- <details>\n  `@" + wj + "octocat`</details>"},
		{"an HTML block in a quote", "> <sub>\n> `@octocat`", "> <sub>\n> `@" + wj + "octocat`</sub>"},
		{"a URL inside an HTML block", "<details>\nsee https://evil.example/x\n</details>", "<details>\nsee https:" + wj + "//evil.example/x\n</details>"},
		{"a backslash escapes nothing in HTML", "<details>\n\\<img src=x> \\&#64;octocat \\[x\\]\n</details>", "<details>\n\\ @" + wj + "octocat \\[x\\]\n</details>"},
		{"but does in markdown", "\\<img src=x> \\&#64;octocat", "\\<img src=x> \\&#64;octocat"},
		{"a line that starts with an autolink is no block", "<https://evil.example> `@octocat`", "`https://evil.example` `@octocat`"},
		{"a pipe in code in a table is escaped", "| a | b |\n|---|---|\n| `x | @octocat` | y |", "| a | b |\n|---|---|\n| `x \\| @octocat` | y |"},
		{"an escaped pipe stays escaped", "|a|\n|-|\n|`x \\| y`|", "|a|\n|-|\n|`x \\| y`|"},
		{"no table, no escape", "`a || b`", "`a || b`"},
		{"a URL ends at a pipe", "| https://evil.example/a|b |", "| `https://evil.example/a`|b |"},
		{"a link cannot span a blank line", "[a\n\nb](https://evil.example)", "\\[a\n\nb\\](`https://evil.example`)"},
	})
	p := newLinkPolicy([]string{"acme/web"}, "")
	if got, want := sanitize("`@octocat` <b>x</b> https://evil.example **b**", p, htmlInlineMode, 0),
		"`@"+wj+"octocat` x https:"+wj+"//evil.example **b**"; got != want {
		t.Errorf("html inline: got %q, want %q", got, want)
	}
	// Text set inside our own <summary> may not open a tag at all: one left open would fold the
	// rest of the comment into it.
	if got, want := sanitize("<details>Sec <sub>x", p, htmlInlineMode, 0), "Sec x"; got != want {
		t.Errorf("html inline tags: got %q, want %q", got, want)
	}
}

// Only spaces and tabs make a line blank to CommonMark. A line of some other Unicode space is
// text, so the HTML block above it goes on, and code after it is still not code.
func TestSanitizeUnicodeSpaceDoesNotEndAnHTMLBlock(t *testing.T) {
	for _, space := range []string{" ", " ", "　", "  \t"} {
		in := "<details>\n" + space + "\n`@octocat`\n</details>"
		got := Sanitize(in, testSanitize)
		if !strings.Contains(got, "@"+wj+"octocat") {
			t.Errorf("a line of %q ended the HTML block: %q", space, got)
		}
		if again := Sanitize(got, testSanitize); again != got {
			t.Errorf("not stable: %q -> %q", got, again)
		}
	}
}

// HTML decodes a numeric character reference with no semicolon too, so inside an HTML block
// "&#64octocat" is "@octocat" once rendered.
func TestSanitizeReferencesWithoutASemicolon(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"decimal and hex at signs in HTML", "<details>\n&#64octocat and &#x40octocat and &#00064;octocat\n</details>",
			"<details>\n@" + wj + "octocat and @" + wj + "octocat and @" + wj + "octocat\n</details>"},
		{"a longer number is another character", "<details>\n&#640x &#x40a\n</details>", "<details>\n&amp;#640x &amp;#x40a\n</details>"},
		{"a bare ampersand in HTML is escaped", "<details>\nTom & Jerry &copy &amp; &lt;\n</details>", "<details>\nTom &amp; Jerry &amp;copy &amp; &lt;\n</details>"},
		{"markdown leaves them to cmark, which escapes them", "&#64octocat", "&#64octocat"},
	})
	p := newLinkPolicy(nil, "")
	if got, want := sanitize("&#64octocat", p, htmlInlineMode, 0), "@"+wj+"octocat"; got != want {
		t.Errorf("html inline: got %q, want %q", got, want)
	}
}

// A fence inside a blockquote or a list item is a fence to GitHub, info string and all, even
// though it does not start at the left margin where the block scanner looks for fences.
func TestSanitizeNestedFencesLoseTheirPower(t *testing.T) {
	for _, in := range []string{
		"> ~~~suggestion\n> evil()\n> ~~~",
		"- item\n\n    ~~~suggestion\n    evil()\n    ~~~",
		"-   ~~~ suggestion\n    evil()\n    ~~~",
		"> ```suggestion\n> evil()\n> ```",
		// The tag makes the scanner read the quote as raw HTML, where backticks are left alone;
		// GitHub reads it as a paragraph, which a fence may interrupt.
		"> <sub>x\n> ```suggestion\n> evil()\n> ```",
		"> <sub>x\n> ~~~mermaid\n> graph TD\n> ~~~",
	} {
		got := Sanitize(in, testSanitize)
		for _, line := range strings.Split(got, "\n") {
			rest := strings.TrimLeft(line, " \t>-*+")
			if strings.HasPrefix(rest, "~~~") || strings.HasPrefix(rest, "```") {
				t.Errorf("Sanitize(%q) = %q leaves a nested fence", in, got)
				break
			}
		}
		if again := Sanitize(got, testSanitize); again != got {
			t.Errorf("not stable: %q -> %q", got, again)
		}
	}
}

func TestSanitizeText(t *testing.T) {
	runSanitizeCases(t, []sanitizeCase{
		{"bidirectional overrides go", "abc\u202Edef\u2066g", "abcdefg"},
		{"control characters go", "a\x00b\x1bc\td", "abc\td"},
		{"carriage returns become newlines", "a\r\nb\rc", "a\nb\nc"},
		{"a backslash at the very end cannot escape what follows", `ends with \`, `ends with \\`},
		{"escapes pass through", `\*not emphasis\* \[x\]`, `\*not emphasis\* \[x\]`},
		{"invalid UTF-8 is replaced", "a\xffb", "a\uFFFDb"},
	})
}

// Inline mode is for a line set inside our own markup: no blocks, no HTML, and nothing that
// could end the bold it sits in.
func TestSanitizeInline(t *testing.T) {
	p := newLinkPolicy([]string{"acme/web"}, "")
	for _, c := range []struct{ in, want string }{
		{"Older totals can overwrite newer ones", "Older totals can overwrite newer ones"},
		{"**fake bold** and __init__ ~~x~~", `\*\*fake bold\*\* and \_\_init\_\_ \~\~x\~\~`},
		{"two\nlines", "two lines"},
		{"<details>no html</details>", "no html"},
		{"```suggestion", "\\`\\`\\`suggestion"},
		{`trailing \`, `trailing \\`},
		{"`org_id` filter", "`org_id` filter"},
	} {
		if got := sanitize(c.in, p, inlineMode, 0); got != c.want {
			t.Errorf("inline %q = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeMaxLen(t *testing.T) {
	opt := testSanitize
	opt.MaxLen = 10
	if got := Sanitize(strings.Repeat("a", 100), opt); got != "aaaaaaaaa…" {
		t.Errorf("got %q", got)
	}
	if got := Sanitize("short", opt); got != "short" {
		t.Errorf("text under the cap was changed: %q", got)
	}
	opt.MaxLen = 25
	if got := Sanitize(strings.Repeat("é", 100), opt); utf8.RuneCountInString(got) != 25 || !utf8.ValidString(got) {
		t.Errorf("cut %q is not 25 whole runes", got)
	}

	// A cut that lands inside the code span around an outside URL must not leave a bare URL
	// behind for GitHub to link.
	in := "Read `https://evil.example/login/now/please` first."
	for n := 5; n < len(in); n++ {
		opt.MaxLen = n
		got := Sanitize(in, opt)
		if utf8.RuneCountInString(got) > n {
			t.Fatalf("MaxLen %d: %q is longer", n, got)
		}
		if i := strings.Index(got, "https://"); i >= 0 && (i == 0 || got[i-1] != '`') {
			t.Errorf("MaxLen %d: %q leaves a bare URL", n, got)
		}
	}

	// A cut inside a code block closes it, so it cannot swallow what the comment says next.
	opt.MaxLen = 30
	got := Sanitize("```go\nfunc a() {}\nfunc b() {}\nfunc c() {}\n```", opt)
	if !strings.HasSuffix(got, "```\n") || strings.Count(got, "```") != 2 {
		t.Errorf("cut code block %q is not closed", got)
	}
	if utf8.RuneCountInString(got) > 30 {
		t.Errorf("%q is over the cap", got)
	}
}

func TestLinkPolicyAllow(t *testing.T) {
	p := newLinkPolicy([]string{"acme/web", "not a repo"}, "https://console.example.com/")
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"https://github.com/acme/web", "https://github.com/acme/web", true},
		{"https://github.com/acme/web/", "https://github.com/acme/web/", true},
		{"  https://github.com/acme/web  ", "https://github.com/acme/web", true},
		{"https://github.com/acme/web//x", "", false},
		{"https://github.com/acme/web/./x", "", false},
		{"https://github.com/acme/web\\x", "", false},
		{"https://github.com/acme/web/a b", "", false},
		{"https://github.com/acme/web/%2F..%2F..%2Fx", "", false},
		{"https://github.com/acme/web/x?q=<script>", "https://github.com/acme/web/x?q=%3Cscript%3E", true},
		{"https://github.com/acme/web/x#frag'ment", "https://github.com/acme/web/x#frag%27ment", true},
		{"https://github.com/acme/web/caf%C3%A9", "https://github.com/acme/web/caf%C3%A9", true},
		{"https://github.com/not/a", "", false},
		{"https://console.example.com", "https://console.example.com", true},
		{"https://CONSOLE.example.com/x", "https://console.example.com/x", true},
		{"http://console.example.com/x", "", false},
		{"https://console.example.com:444/x", "", false},
		{"//github.com/acme/web", "", false},
		{"mailto:dev@example.com", "", false},
		{"", "", false},
	} {
		got, ok := p.allow(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("allow(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	var none *linkPolicy
	if _, ok := none.allow("https://github.com/acme/web"); ok {
		t.Error("a nil policy allowed a link")
	}
	if p := newLinkPolicy(nil, "javascript:alert(1)"); p.console != nil {
		t.Error("a console origin that is not http(s) was accepted")
	}
}

func TestStripMarkersIsLinear(t *testing.T) {
	// Nested openers peel one at a time in a naive loop; one pass must take them all.
	in := strings.Repeat("<!-", 2000) + "<!-- attest_tag:x -->" + strings.Repeat("- attest_tag:y -->", 2000)
	if out := stripMarkers(in); markerOpener.MatchString(out) {
		t.Errorf("an opener survived: %q", out[:min(len(out), 80)])
	}
}

func TestCodeSpan(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"src/a.go:L3", "`src/a.go:L3`"},
		{"a`b", "``a`b``"},
		{"`edge`", "`` `edge` ``"},
		{"two\nlines", "`two lines`"},
		{"  ", ""},
		{"x<!-- attest_tag:run=a.0123456789abcdef -->y", "`xy`"},
	} {
		if got := codeSpan(c.in); got != c.want {
			t.Errorf("codeSpan(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// markerOpener is what must never reach GitHub from model text, in any spelling a reader of
// raw markdown might honour.
var markerOpener = regexp.MustCompile(`(?i)<!--\s*attest_tag:`)

// FuzzSanitize holds Sanitize to what callers rely on whatever the model writes: it returns
// valid UTF-8, never carries a marker opener, respects MaxLen, and is stable.
func FuzzSanitize(f *testing.F) {
	for _, s := range []string{
		"", "plain", "[x](https://github.com/acme/web) @octocat <details>x",
		"```\n<!-- attest_tag:finding=a.0123456789abcdef -->\n```",
		"<!-<!-- attest_tag:x -->- attest_tag:run=a.0123456789abcdef -->",
		"![i](https://evil.example/x.png) https://evil.example `a` ``b`` ~~~\nc",
		"<a href='`'>`</a> [a`](b)` <!-- x",
		"\\@x &#64;y [[[[[]]]]](https://github.com/acme/web)",
		"https:<b></b>//evil.example www.<i></i>evil.example",
		"<details>\n`@x` https://evil.example\n\n`@y`\n</details>",
		"| a | b |\n|---|---|\n| `x | @y` | [z](https://evil.example) |",
		"- > 1. <sub>\n`@x`",
		"<details>\n\u00a0\n`@x`\n\u2028\n`@y`\n\u3000\n</details>",
		"<details>\n&#64x &#x40y &#640 &#x202E; &copy & z\n</details>",
		"> ~~~suggestion\n> x\n> ~~~\n- a\n\n    ~~~ mermaid\n    b\n    ~~~",
		"> <sub>x\n> ```suggestion\n> y\n> ```",
	} {
		f.Add(s, 0)
		f.Add(s, 12)
	}
	f.Fuzz(func(t *testing.T, in string, maxLen int) {
		opt := testSanitize
		opt.MaxLen = maxLen % 200
		if opt.MaxLen < 0 {
			opt.MaxLen = -opt.MaxLen
		}
		out := Sanitize(in, opt)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 from %q", in)
		}
		if markerOpener.MatchString(out) {
			t.Fatalf("a marker opener survived: %q -> %q", in, out)
		}
		if opt.MaxLen > 0 && utf8.RuneCountInString(out) > opt.MaxLen {
			t.Fatalf("%q is over MaxLen %d", out, opt.MaxLen)
		}
		if again := Sanitize(out, opt); again != out {
			t.Fatalf("not stable:\n in    %q\n once  %q\n twice %q", in, out, again)
		}
	})
}
