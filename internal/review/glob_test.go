package review

import (
	"strings"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// An empty pattern is a branch rule's "any branch".
		{"", "anything/at/all", true},
		{"", "", true},

		{"main", "main", true},
		{"main", "Main", false}, // git's names are case-sensitive
		{"main", "main2", false},
		{"main", "xmain", false},

		{"*", "main", true},
		{"*", "", true},
		{"*", "feature/x", false},
		{"**", "feature/x/y", true},
		{"***", "a/b", true}, // a run of stars is "**"

		// Branches, read the way GitHub's branch protection reads them.
		{"release/*", "release/1.2", true},
		{"release/*", "release/", true},
		{"release/*", "release/1.2/hotfix", false},
		{"release/**", "release/1.2/hotfix", true},
		{"release/**", "release", false},
		{"dependabot/**", "dependabot/npm_and_yarn/lodash-4.17.21", true},
		{"hotfix-?", "hotfix-1", true},
		{"hotfix-?", "hotfix-12", false},
		{"a?c", "a/c", false},

		// Paths.
		{"*.lock", "yarn.lock", true},
		{"*.lock", "web/yarn.lock", false}, // '*' stays in its directory; "**/*.lock" is how to say anywhere
		{"**/*.lock", "yarn.lock", true},
		{"**/*.lock", "web/yarn.lock", true},
		{"**/*.lock", "web/yarn.lockx", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/sub/a.md", false},
		{"internal/**/*.go", "internal/main.go", true},
		{"internal/**/*.go", "internal/app/x/main.go", true},
		{"internal/**/*.go", "internalx/main.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/xb", false},
		{"**/node_modules/**", "node_modules/x.js", true},
		{"**/node_modules/**", "web/node_modules/x/y.js", true},
		{"**/node_modules/**", "web/node_modulesx/y.js", false},
		// Stars glued to a name are "anything", not "any directories".
		{"a**/b", "a/b", true},
		{"a**/b", "ax/y/b", true},
		{"a**/b", "b", false},
		// A leading slash means the root, which is where GitHub's paths already start.
		{"/vendor/**", "vendor/x.go", true},
		{"/vendor/**", "web/vendor/x.go", false},
		{"/", "x", false},

		// Characters, not bytes.
		{"?ber", "über", true},
		{"ü*", "über", true},
		// No classes or escapes: brackets are literal.
		{"[ab]", "[ab]", true},
		{"[ab]", "a", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// A backtracking matcher takes exponential time on this; a settings box is not where a
// review lane should hang.
func TestMatchHasNoPathologicalPattern(t *testing.T) {
	pattern := strings.Repeat("*a", 30) + "*b"
	name := strings.Repeat("a", 400)
	start := time.Now()
	if Match(pattern, name) {
		t.Fatal("there is no b to match")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestMatchAny(t *testing.T) {
	if MatchAny(nil, "x") {
		t.Error("an empty list must match nothing: an empty ignore list ignores nothing")
	}
	if !MatchAny([]string{"*.md", "dist/**"}, "dist/app.js") {
		t.Error("the second pattern matches")
	}
	if MatchAny([]string{"*.md", "dist/**"}, "src/app.js") {
		t.Error("neither pattern matches")
	}
}
