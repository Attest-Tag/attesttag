package worker

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"attesttag/internal/app"
)

// locateFiles finds the files a brief names, in the clone: paths written out ("web/src/app.ts"),
// file names on their own ("PromptBox.tsx") and the names of components and modules the thread
// talks about ("WorkflowChat"), each matched against what the repository actually holds. An
// engine handed a ticket's words and not where they live spends its first many turns on glob and
// grep — on one job, 135 of 179 turns went to finding the code before the first edit, each turn
// re-sending a conversation that only grew. The brief's files_hint is the bot's guess at this;
// these are what the clone confirms.
func locateFiles(repo string, s app.JobSpec, limit int) []string {
	text := strings.Join(append([]string{s.Title, s.Requirement, s.Evidence, s.ThreadText, strings.Join(s.Acceptance, "\n"), strings.Join(s.FilesHint, "\n")},
		toolEvidenceText(s.ToolEvidence)...), "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	byBase, byStem := indexRepo(repo, 20000)
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] && len(out) < limit {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, tok := range pathTokenRe.FindAllString(text, -1) {
		tok = strings.Trim(strings.TrimPrefix(strings.TrimPrefix(tok, "./"), "/"), ".,:;)")
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "/") {
			// A path, or the tail of one ("components/ChatBox/ChatBox.tsx" from a stack trace
			// rooted somewhere else): every file whose path ends with it.
			if m := bySuffix(byBase[filepath.Base(tok)], tok); len(m) > 0 && len(m) <= 3 {
				for _, p := range m {
					add(p)
				}
			}
			continue
		}
		if m := byBase[tok]; len(m) > 0 && len(m) <= 3 {
			for _, p := range m {
				add(p)
			}
		}
	}
	for _, tok := range identRe.FindAllString(text, -1) {
		if m := byStem[tok]; len(m) > 0 && len(m) <= 3 {
			for _, p := range m {
				add(p)
			}
		}
	}
	return out
}

var (
	// pathTokenRe is a file name with an extension, with or without the directories before it.
	pathTokenRe = regexp.MustCompile(`[A-Za-z0-9_.@-]*(?:/[A-Za-z0-9_.@-]+)*\.[A-Za-z][A-Za-z0-9]{0,7}\b`)
	// identRe is a name a module or component file is likely named after: CamelCase, or
	// snake/kebab case with a separator, six characters or more so common words do not match.
	identRe = regexp.MustCompile(`\b(?:[A-Z][a-z0-9]+(?:[A-Z][a-z0-9]*)+|[a-z][a-z0-9]+(?:[_-][a-z0-9]+)+)\b`)
)

func toolEvidenceText(evs []app.ToolEvidence) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Args, cut(ev.Result, 6000))
	}
	return out
}

// skipDirs are folders whose files are never what a brief means: dependencies, build output,
// version control.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, "target": true,
	".next": true, "out": true, "coverage": true, "__pycache__": true, ".venv": true, "venv": true, ".tox": true, ".gradle": true, "bin": true, "obj": true}

// indexRepo maps base names ("ChatBox.tsx") and stems ("ChatBox", for names of six characters or
// more) to the repository-relative paths that carry them.
func indexRepo(repo string, max int) (byBase, byStem map[string][]string) {
	byBase, byStem = map[string][]string{}, map[string][]string{}
	n := 0
	filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != repo && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if n++; n > max {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		base := d.Name()
		byBase[base] = append(byBase[base], rel)
		if stem := strings.TrimSuffix(base, filepath.Ext(base)); len(stem) >= 6 && stem != base {
			byStem[stem] = append(byStem[stem], rel)
		}
		return nil
	})
	for _, m := range []map[string][]string{byBase, byStem} {
		for _, ps := range m {
			sort.Strings(ps)
		}
	}
	return byBase, byStem
}

func bySuffix(paths []string, tail string) []string {
	var out []string
	for _, p := range paths {
		if p == tail || strings.HasSuffix(p, "/"+tail) {
			out = append(out, p)
		}
	}
	return out
}
