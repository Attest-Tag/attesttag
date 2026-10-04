package review

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// SkillLink is a skill a review type follows: a folder in a GitHub repository holding a SKILL.md
// and the Markdown and text files beside it — the way agent skills are kept — or one file. A
// repository may hold many skills; a link names one folder, and only that folder is read.
//
// Where it is read from decides who can change what the reviewer is told:
//
//   - Repo empty is the repository under review, at the pull request's base commit, as its
//     instruction files are: a pull request that edits the skill is not reviewed by its own edit.
//   - Repo set is another repository, at Ref, or at its default branch when Ref is empty, pinned to
//     one commit for the whole review. One of the organisation's connected repositories is read
//     through its connection; any other must be public, and is read without credentials. A public
//     repository somebody else owns changes whenever they push, so Ref may be a commit, which is
//     how a team keeps a stranger's push out of its reviews.
//
// The skill's text reaches the finder as criteria, like the type's own rules: it says what to look
// for and how to explain it, and it cannot give the reviewer a tool, widen what it may read, or
// change how or where the review is posted.
type SkillLink struct {
	Repo string `json:"repo,omitempty"`
	Path string `json:"path"`
	Ref  string `json:"ref,omitempty"`
}

// The bounds a type's skills are held to. Five folders is more than a team means one review to
// follow; what is read from each is bounded where it is read.
const (
	MaxTypeSkills   = 5
	MaxSkillPathLen = 300
	MaxSkillRefLen  = 100
)

// SkillID is the id the i-th skill is cited by in a finding's rule_ids: "S1" for the first. Like a
// rule's id it is the skill's place in the list, so the order a team gives them is the order the
// reviewer reads them in.
func SkillID(i int) string { return "S" + strconv.Itoa(i+1) }

// Skill returns the link a finding cites by id, and its index.
func (t Type) Skill(id string) (SkillLink, int, bool) {
	id = strings.TrimSpace(id)
	n, err := strconv.Atoi(strings.TrimPrefix(id, "S"))
	if err != nil || !strings.HasPrefix(id, "S") || n < 1 || n > len(t.Skills) {
		return SkillLink{}, 0, false
	}
	return t.Skills[n-1], n - 1, true
}

// Name is what a link is called before its SKILL.md has been read, and after, when it names none:
// the folder's last segment, or the file's name without its extension when that name is SKILL,
// README or the like, which say nothing.
func (l SkillLink) Name() string {
	p := strings.Trim(l.Path, "/")
	base := path.Base(p)
	if dir := path.Dir(p); dir != "." && isEntryFile(base) {
		base = path.Base(dir)
	}
	return base
}

// isEntryFile is a file name that names a folder's entry point rather than what the folder is about.
func isEntryFile(name string) bool {
	switch strings.ToLower(strings.TrimSuffix(name, path.Ext(name))) {
	case "skill", "readme", "index", "agents", "claude":
		return true
	}
	return false
}

// String is how the link is written for people: "owner/name@ref:path", "owner/name:path" for the
// default branch, and the path alone for the repository under review.
func (l SkillLink) String() string {
	if l.Repo == "" {
		return l.Path
	}
	if l.Ref != "" {
		return l.Repo + "@" + l.Ref + ":" + l.Path
	}
	return l.Repo + ":" + l.Path
}

// NormalizeSkillLink is a link as it is stored: spaces trimmed, the path without "./" or slashes at
// either end, so "skills/review/" and "/skills/review" are one link.
func NormalizeSkillLink(l SkillLink) SkillLink {
	l.Repo = strings.TrimSpace(l.Repo)
	l.Ref = strings.TrimSpace(l.Ref)
	p := strings.TrimSpace(l.Path)
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	l.Path = strings.Trim(p, "/")
	return l
}

// gitRefText is the characters a branch, tag or commit is written in here: git allows more, and a
// ref that needs them can be pinned by its commit instead.
var gitRefText = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// validSkillRef is a ref a link may name: a branch, a tag or a commit, by a subset of git's rules
// (check-ref-format) that keeps it one plain path-like word.
func validSkillRef(r string) bool {
	if len(r) > MaxSkillRefLen || !gitRefText.MatchString(r) {
		return false
	}
	if strings.HasPrefix(r, "-") || strings.HasPrefix(r, "/") || strings.HasPrefix(r, ".") || strings.HasSuffix(r, "/") ||
		strings.HasSuffix(r, ".") || strings.HasSuffix(r, ".lock") || strings.Contains(r, "..") || strings.Contains(r, "//") ||
		strings.Contains(r, "/.") {
		return false
	}
	return true
}

// validSkillPath is a plain path from a repository's root: no pattern characters, no "." or ".."
// segments, nothing that would read as a URL or a flag.
func validSkillPath(p string) bool {
	if p == "" || len(p) > MaxSkillPathLen || strings.ContainsFunc(p, unicode.IsControl) || strings.ContainsAny(p, `\*?[]{}`) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || seg == ".git" {
			return false
		}
	}
	return true
}

// validateSkills checks a type's skill links, in terms a console form can show.
func validateSkills(links []SkillLink) []error {
	var errs []error
	if len(links) > MaxTypeSkills {
		errs = append(errs, fmt.Errorf("at most %d skills, got %d", MaxTypeSkills, len(links)))
	}
	seen := map[string]bool{}
	for i, l := range links {
		id := SkillID(i)
		if n := NormalizeSkillLink(l); n != l {
			// Stored links are normalised (the API does it), so one that is not was never meant.
			errs = append(errs, fmt.Errorf("skill %s: %q is not written plainly; want %q", id, clip(l.String()), clip(n.String())))
			continue
		}
		if l.Repo != "" && !validRepoName(l.Repo) {
			errs = append(errs, fmt.Errorf("skill %s: repository %q: want owner/name", id, clip(l.Repo)))
		}
		if !validSkillPath(l.Path) {
			errs = append(errs, fmt.Errorf("skill %s: path %q: want a folder or file from the repository's root, such as skills/review, of at most %d characters", id, clip(l.Path), MaxSkillPathLen))
		}
		switch {
		case l.Ref != "" && l.Repo == "":
			errs = append(errs, fmt.Errorf("skill %s: a skill in the repository under review is read at the pull request's base commit, so it takes no branch, tag or commit", id))
		case l.Ref != "" && !validSkillRef(l.Ref):
			errs = append(errs, fmt.Errorf("skill %s: %q is not a branch, tag or commit", id, clip(l.Ref)))
		}
		key := strings.ToLower(l.Repo) + "|" + l.Ref + "|" + l.Path
		if seen[key] {
			errs = append(errs, fmt.Errorf("skill %s: %s is linked twice", id, clip(l.String())))
		}
		seen[key] = true
	}
	return errs
}

// ValidateSkillLinks reports everything wrong with a list of links at once, as ValidateType would.
func ValidateSkillLinks(links []SkillLink) error { return errors.Join(validateSkills(links)...) }

// ErrSkillLink is a skill link that could not be read from what a person typed.
var ErrSkillLink = errors.New("not a link to a skill in a GitHub repository")

// githubTreeURL is a folder or file on github.com: /owner/name/tree/<ref>/<path>, or /blob/ for a
// file. The ref is taken as one segment, the common case; a branch with a slash in it is entered in
// the ref field instead.
var githubTreeURL = regexp.MustCompile(`^https?://(?:www\.)?github\.com/([A-Za-z0-9-]+/[A-Za-z0-9._-]+?)(?:\.git)?(?:/(?:tree|blob)/([^/?#]+)(?:/([^?#]*))?)?/?(?:[?#].*)?$`)

// ParseSkillURL reads a github.com address a person pasted — a folder (/tree/) or a file (/blob/)
// — as a link: repository, ref and path. It leaves the ref as written, a branch or a commit.
func ParseSkillURL(raw string) (SkillLink, error) {
	m := githubTreeURL.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil || m[3] == "" {
		return SkillLink{}, ErrSkillLink
	}
	// The address escapes what a path may hold, a space as %20; the link holds the path itself.
	p, err := url.PathUnescape(m[3])
	if err != nil {
		return SkillLink{}, ErrSkillLink
	}
	l := NormalizeSkillLink(SkillLink{Repo: m[1], Ref: m[2], Path: p})
	if !validRepoName(l.Repo) || !validSkillPath(l.Path) || !validSkillRef(l.Ref) {
		return SkillLink{}, ErrSkillLink
	}
	return l, nil
}
