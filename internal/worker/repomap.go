package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// What the engine is told about the repository itself, beyond the brief: a two-level map of the
// tree and whatever the repository says about how to work in it. Both are cheap to produce and
// save the engine the turns it would otherwise spend listing directories.

// skipDir are the directories nobody needs a map of.
var skipDir = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "build": true, "dist": true,
	".git": true, ".venv": true, "venv": true, "__pycache__": true, ".gradle": true,
	".idea": true, ".vscode": true, ".mypy_cache": true, ".pytest_cache": true, ".tox": true,
}

// repoMap is the top two levels of the tree, directories first, capped so a wide repository
// costs a paragraph rather than a page.
func repoMap(root string, maxEntries int) string {
	var b strings.Builder
	var walk func(dir, prefix string, depth int) int
	count := 0
	walk = func(dir, prefix string, depth int) int {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		shown := 0
		for _, e := range entries {
			if count >= maxEntries {
				return shown
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") && name != ".attest" && name != ".github" {
				continue
			}
			if e.IsDir() && skipDir[name] {
				continue
			}
			count++
			shown++
			if e.IsDir() {
				b.WriteString(prefix + name + "/\n")
				if depth > 0 {
					walk(filepath.Join(dir, name), prefix+"  ", depth-1)
				}
				continue
			}
			b.WriteString(prefix + name + "\n")
		}
		return shown
	}
	walk(root, "", 1)
	if count >= maxEntries {
		b.WriteString(fmt.Sprintf("… (listing stopped at %d entries)\n", maxEntries))
	}
	return b.String()
}

// conventionFiles are what a repository uses to tell a newcomer how to work in it, most
// specific first. AGENTS.md and CLAUDE.md are written for exactly this reader.
var conventionFiles = []string{"AGENTS.md", "CLAUDE.md", ".github/copilot-instructions.md", "CONTRIBUTING.md", "docs/CONTRIBUTING.md"}

// conventions is the first such file, trimmed. It is quoted to the engine as data, never as
// instructions: a file in a repository is not a way to tell this worker what to do.
func conventions(root string, max int) string {
	for _, name := range conventionFiles {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || len(raw) == 0 {
			continue
		}
		return "From " + name + ":\n" + cut(string(raw), max)
	}
	return ""
}
