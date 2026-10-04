package review

import "strings"

// Match reports whether name matches pattern, a glob of the kind people write for branch
// names and file paths, matched against the whole name:
//
//   - '*' matches any run of characters except '/', so "release/*" is release/1.2 but not
//     release/1.2/hotfix — the same reading GitHub's branch protection gives it;
//   - '**' matches anything, '/' included, so "dependabot/**" is every Dependabot branch;
//   - "**/" at the start of a pattern or after a '/' matches zero or more whole directories,
//     so "**/*.snap" is a snapshot at the root as well as one deep in a tree, and "a/**/b" is
//     a/b too;
//   - '?' matches one character other than '/'.
//
// Everything else is literal — there are no character classes, braces or escapes, which
// keeps a pattern meaning what it looks like in a settings box. A leading '/' is dropped,
// because people write "/vendor/**" to mean the repository's root and GitHub's paths never
// start with one. An empty pattern matches anything: it is how a branch rule says "any
// branch".
//
// Matching is case-sensitive, as git's paths and refs are. It runs in time proportional to
// the pattern's length times the name's, with no backtracking, so a pattern like "*a*a*a*b"
// pasted into a setting cannot make a review lane spin.
func Match(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	if len(pattern) > 1 {
		pattern = strings.TrimPrefix(pattern, "/")
	}
	toks := globTokens(pattern)
	s := []rune(name)

	// next[j] says whether the tokens after the current one match s[j:]; cur is being filled
	// for the current token. Walking the pattern backwards needs only these two rows.
	next := make([]bool, len(s)+1)
	cur := make([]bool, len(s)+1)
	next[len(s)] = true
	for i := len(toks) - 1; i >= 0; i-- {
		t := toks[i]
		// For "**/": whether some '/' at or after j ends a run of directories after which the
		// rest of the pattern matches. Filled as j walks down, so each cell costs O(1).
		dirs := false
		for j := len(s); j >= 0; j-- {
			more := j < len(s)
			var m bool
			switch t.kind {
			case globLit:
				m = more && s[j] == t.r && next[j+1]
			case globOne:
				m = more && s[j] != '/' && next[j+1]
			case globStar:
				m = next[j] || (more && s[j] != '/' && cur[j+1])
			case globAny:
				m = next[j] || (more && cur[j+1])
			case globDirs:
				if more && s[j] == '/' && next[j+1] {
					dirs = true
				}
				m = next[j] || dirs
			}
			cur[j] = m
		}
		cur, next = next, cur
	}
	return next[0]
}

// MatchAny reports whether any of patterns matches name. An empty list matches nothing — an
// empty ignore list ignores nothing — so a caller for whom "no patterns" means "everything",
// such as a review type's path filter, says so itself.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}

const (
	globLit  = iota // one literal character
	globOne         // ?
	globStar        // * — any run without '/'
	globAny         // ** — any run at all
	globDirs        // **/ as a whole segment — zero or more directories
)

type globTok struct {
	kind int
	r    rune
}

func globTokens(pattern string) []globTok {
	p := []rune(pattern)
	toks := make([]globTok, 0, len(p))
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '?':
			toks = append(toks, globTok{kind: globOne})
		case '*':
			j := i
			for j < len(p) && p[j] == '*' {
				j++
			}
			switch {
			case j-i == 1:
				toks = append(toks, globTok{kind: globStar})
			case j < len(p) && p[j] == '/' && (i == 0 || p[i-1] == '/'):
				// Only a whole "**/" segment means "any number of directories". In "a**/b"
				// the stars are glued to a name, and "a" followed by anything then "/b" is the
				// only sensible reading.
				toks = append(toks, globTok{kind: globDirs})
				j++ // the '/' belongs to the token
			default:
				toks = append(toks, globTok{kind: globAny})
			}
			i = j - 1
		default:
			toks = append(toks, globTok{kind: globLit, r: p[i]})
		}
	}
	return toks
}
