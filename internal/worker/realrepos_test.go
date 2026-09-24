package worker

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Detection against real repositories, not fixtures. ATTEST_REAL_REPOS points at a directory of
// clones; the test reports what the probe table makes of each one. It asserts nothing about a
// particular repository — those are other people's and they change — only that every clone
// resolves to a recipe with a runnable command, which is the claim the table exists to support.
func TestDetectAgainstRealRepos(t *testing.T) {
	root := os.Getenv("ATTEST_REAL_REPOS")
	if root == "" {
		t.Skip("set ATTEST_REAL_REPOS to a directory of cloned repositories")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var unresolved []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		r := resolveRecipe(dir, nil, "", nil)
		test, build := "—", "—"
		if r.Test != nil {
			test = r.Test.String()
		}
		if r.Build != nil {
			build = r.Build.String()
		}
		t.Logf("%-16s %-12s %-12s test=%-46s build=%s", e.Name(), r.Ecosystem, r.Source, test, build)
		if r.Test == nil && r.Build == nil && !strings.Contains(r.Why, "monorepo") {
			unresolved = append(unresolved, e.Name()+" ("+r.Why+")")
		}
	}
	for _, u := range unresolved {
		t.Errorf("no command at all for %s", u)
	}
}

// A monorepo with no hint is the one honest "I cannot tell": the brief names a file, and it
// resolves to that package. Anything else would be running some other package's suite and
// reporting it as this change's.
func TestMonorepoResolvesFromAFileHint(t *testing.T) {
	root := os.Getenv("ATTEST_REAL_REPOS")
	if root == "" {
		t.Skip("set ATTEST_REAL_REPOS to a directory of cloned repositories")
	}
	dir := filepath.Join(root, "dart-http")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("dart-http is not among the clones")
	}
	if r := resolveRecipe(dir, nil, "", nil); r.Test != nil {
		t.Errorf("guessed a package with nothing to go on: %s", r.Describe())
	}
	r := resolveRecipe(dir, nil, "", []string{"pkgs/http/lib/src/client.dart"})
	if r.Workdir != "pkgs/http" || r.Ecosystem != "dart" || r.Test.String() != "dart test" {
		t.Fatalf("hinted monorepo: workdir %q ecosystem %q test %q", r.Workdir, r.Ecosystem, r.Test.String())
	}
	t.Logf("dart-http + hint → %s in %s", r.Describe(), r.Workdir)
}
