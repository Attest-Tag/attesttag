package review

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// RepoRule is one coding rule a repository states in its own instruction files — "never abbreviate
// column as col", "boolean props must start with is or has" — numbered for a finding to cite, as
// C1, C2…, the way it cites a review type's rules as R1, R2.
//
// A team writes its standards where its people and its agents read them, not in the console, and a
// reviewer that read them as background and then left naming and style "to the tools" enforced none
// of them: the findings an author fixes within minutes of being told. Numbered, they are criteria a
// finding can rest on and a reader can check, while the file they came from stays text a stranger
// may have written — which is why they are read at the base commit, and why a finding resting on
// one alone is at most RepoRuleCap.
type RepoRule struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source"` // the file it was read from, at the base commit
}

// RepoRuleCap is the most severe a finding resting only on repository rules may be. A rule's own
// words could claim any severity, and the file is not the team's console, where a rule's cap is set
// by somebody with the permission to set it.
const RepoRuleCap = P2

// The bounds a run's repository rules are held to: as many as a review type's rules, each short
// enough to read as one line.
const (
	MaxRepoRules   = 40
	MaxRepoRuleLen = 300
	minRepoRuleLen = 12
)

// InstructionFile is one instruction file as a review read it, most specific first.
type InstructionFile struct {
	Path string
	Text string
}

// repoDirective is a line that tells somebody what to do or not to do. Narrow on purpose: prose
// that describes the code ("the store keeps one row per…") is context, not a rule, and a rule that
// is not one is a finding nobody can act on.
var repoDirective = regexp.MustCompile(`(?i)\b(?:must|mustn['’]t|never|always|avoid|prefer|required|should|shouldn['’]t|do not|don['’]t)\b|\buse\b.{1,80}\binstead\b`)

var (
	repoListItem  = regexp.MustCompile(`^\s{0,6}(?:[-*+]|\d{1,3}[.)])\s+(.*)$`)
	repoFence     = regexp.MustCompile("^\\s*(```|~~~)")
	repoSentenceE = regexp.MustCompile(`[.!?](?:\s+|$)`)
)

// ExtractRepoRules reads the directive lines out of instruction files: each list item, and each
// sentence of a paragraph, that says must, never, always, do not, avoid, prefer, should, required
// or "use … instead". Code blocks, headings and tables are passed over. The same rule in two files
// is kept once, from the first.
//
// The forty are shared fairly: each file in turn gives one more until the files or the forty run
// out, so one long CONTRIBUTING.md cannot push the nearest AGENTS.md out of the list, and the files
// first in the list — the most specific — keep what is left over on a tie. The rules are then
// numbered file by file, in the files' order.
func ExtractRepoRules(files []InstructionFile) []RepoRule {
	seen := map[string]bool{}
	per := make([][]string, len(files))
	for i, f := range files {
		for _, d := range repoDirectives(f.Text) {
			key := strings.ToLower(d)
			if seen[key] {
				continue
			}
			seen[key] = true
			per[i] = append(per[i], d)
		}
	}
	take := make([]int, len(files))
	for n, more := 0, true; more && n < MaxRepoRules; {
		more = false
		for i := range files {
			if take[i] < len(per[i]) && n < MaxRepoRules {
				take[i]++
				n++
				more = true
			}
		}
	}
	var out []RepoRule
	for i, f := range files {
		for _, t := range per[i][:take[i]] {
			out = append(out, RepoRule{ID: "C" + strconv.Itoa(len(out)+1), Text: t, Source: f.Path})
		}
	}
	return out
}

// repoDirectives is one file's directive lines, in order.
func repoDirectives(text string) []string {
	var out []string
	add := func(s string) {
		s = oneLine(strings.TrimSpace(s))
		if utf8.RuneCountInString(s) < minRepoRuleLen || !repoDirective.MatchString(s) {
			return
		}
		if r := []rune(s); len(r) > MaxRepoRuleLen {
			s = strings.TrimSpace(string(r[:MaxRepoRuleLen-1])) + "…"
		}
		out = append(out, s)
	}
	var item, para []string
	flush := func() {
		if len(item) > 0 {
			add(strings.Join(item, " "))
			item = nil
		}
		if len(para) > 0 {
			for _, s := range sentences(strings.Join(para, " ")) {
				add(s)
			}
			para = nil
		}
	}
	fenced := false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if repoFence.MatchString(line) {
			flush()
			fenced = !fenced
			continue
		}
		trim := strings.TrimSpace(line)
		switch {
		case fenced:
		case trim == "", strings.HasPrefix(trim, "#"), strings.HasPrefix(trim, "|"), strings.HasPrefix(trim, "<!--"):
			flush()
		case repoListItem.MatchString(line):
			flush()
			item = []string{repoListItem.FindStringSubmatch(line)[1]}
		case len(item) > 0 && (line[0] == ' ' || line[0] == '\t'):
			item = append(item, trim) // a list item continued on an indented line
		default:
			if len(item) > 0 {
				flush()
			}
			para = append(para, trim)
		}
	}
	flush()
	return out
}

// sentences splits a paragraph at the end of each sentence, keeping its full stop.
func sentences(p string) []string {
	var out []string
	for p != "" {
		loc := repoSentenceE.FindStringIndex(p)
		if loc == nil {
			out = append(out, p)
			break
		}
		out = append(out, p[:loc[0]+1])
		p = p[loc[1]:]
	}
	return out
}

// FindRepoRule returns the repository rule a finding cites by id.
func FindRepoRule(rules []RepoRule, id string) (RepoRule, bool) {
	id = strings.ToUpper(strings.TrimSpace(id))
	for _, r := range rules {
		if r.ID == id {
			return r, true
		}
	}
	return RepoRule{}, false
}

// RepoRuleID reports whether id is shaped like a repository rule's: C and a number.
func RepoRuleID(id string) bool {
	id = strings.ToUpper(strings.TrimSpace(id))
	n, err := strconv.Atoi(strings.TrimPrefix(id, "C"))
	return strings.HasPrefix(id, "C") && err == nil && n > 0
}
