package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"attesttag/internal/app"
)

// One real job per language, all the way through: clone a repository laid out the way that
// ecosystem lays repositories out, resolve the recipe, install its dependencies, build it, run
// its suite, let the engine change a file, run everything again, commit, push and open the pull
// request. Nothing is stubbed but the model and github.com.
//
// This is the test that says "any repository, any language" is true rather than intended. Each
// case skips when the toolchain is not on this machine, so it is honest about what it proved —
// the worker image has all of them, and the summary at the end names what ran and what did not.

// seedFiles makes a bare origin whose main branch holds exactly these files.
func seedFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitCmd(t, root, "init", "-q", "--bare", "--initial-branch=main", origin)
	scratch := filepath.Join(root, "scratch")
	gitCmd(t, root, "init", "-q", "--initial-branch=main", scratch)
	for name, body := range files {
		p := filepath.Join(scratch, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "w") && strings.HasPrefix(body, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, scratch, "add", "-A")
	gitCmd(t, scratch, "commit", "-q", "-m", "init")
	gitCmd(t, scratch, "push", "-q", origin, "main")
	return origin
}

type langCase struct {
	name      string
	needs     []string // programs without which this case proves nothing
	files     map[string]string
	ecosystem string
	test      string
	build     string
	edit      string // the file the fake engine appends to; must not break the build
	network   bool   // its install reaches a package registry
}

var langCases = []langCase{
	{
		name: "go", needs: []string{"go"}, ecosystem: "go",
		test: "go test ./...", build: "go build ./...", edit: "README.md",
		files: map[string]string{
			"go.mod":      "module example.com/demo\n\ngo 1.21\n",
			"add.go":      "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
			"add_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
			"README.md":   "# demo\n",
		},
	},
	{
		name: "python-requirements", needs: []string{"uv"}, ecosystem: "python", network: true,
		test:  ".venv/bin/python -m pytest -x -q -p no:cacheprovider",
		build: ".venv/bin/python -m compileall -q -x " + compileSkip + " .",
		edit:  "README.md",
		files: map[string]string{
			"requirements.txt":  "pytest==8.3.3\n",
			"demo.py":           "def add(a, b):\n    return a + b\n",
			"tests/test_add.py": "from demo import add\n\n\ndef test_add():\n    assert add(2, 2) == 4\n",
			"README.md":         "# demo\n",
		},
	},
	{
		name: "python-uv", needs: []string{"uv"}, ecosystem: "python", network: true,
		test:  "uv run python -m pytest -x -q -p no:cacheprovider",
		build: "uv run python -m compileall -q -x " + compileSkip + " .",
		edit:  "README.md",
		files: map[string]string{
			"pyproject.toml":    "[project]\nname = \"demo\"\nversion = \"0.1.0\"\nrequires-python = \">=3.9\"\ndependencies = []\n\n[dependency-groups]\ndev = [\"pytest\"]\n\n[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\n",
			"demo.py":           "def add(a, b):\n    return a + b\n",
			"tests/test_add.py": "from demo import add\n\n\ndef test_add():\n    assert add(2, 2) == 4\n",
			"README.md":         "# demo\n",
		},
	},
	{
		name: "node-npm", needs: []string{"node", "npm"}, ecosystem: "node", network: true,
		test: "npm run test", build: "npm run build", edit: "README.md",
		files: map[string]string{
			"package.json": `{"name":"demo","version":"1.0.0","private":true,"scripts":{"test":"node --test","build":"node -e \"require('./add.js')\""}}`,
			"add.js":       "module.exports = (a, b) => a + b;\n",
			"add.test.js":  "const test = require('node:test');\nconst assert = require('node:assert');\nconst add = require('./add.js');\ntest('add', () => assert.strictEqual(add(2, 2), 4));\n",
			"README.md":    "# demo\n",
		},
	},
	{
		name: "rust", needs: []string{"cargo"}, ecosystem: "rust", network: false,
		test: "cargo test --quiet", build: "cargo build --quiet", edit: "README.md",
		files: map[string]string{
			"Cargo.toml": "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\n",
			"src/lib.rs": "pub fn add(a: i32, b: i32) -> i32 { a + b }\n\n#[cfg(test)]\nmod tests {\n    use super::*;\n    #[test]\n    fn it_adds() { assert_eq!(add(2, 2), 4); }\n}\n",
			"README.md":  "# demo\n",
		},
	},
	{
		name: "make-over-go", needs: []string{"make", "go"}, ecosystem: "go",
		test: "make test", build: "make build", edit: "README.md",
		files: map[string]string{
			"Makefile":    "build:\n\tgo build ./...\n\ntest:\n\tgo test ./...\n",
			"go.mod":      "module example.com/demo\n\ngo 1.21\n",
			"add.go":      "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
			"add_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
			"README.md":   "# demo\n",
		},
	},
	{
		name: "monorepo-picks-the-package", needs: []string{"go"}, ecosystem: "go",
		test: "go test ./...", build: "go build ./...", edit: "services/api/README.md",
		files: map[string]string{
			"README.md":                "# monorepo\n",
			"web/package.json":         `{"name":"web","scripts":{"test":"exit 1"}}`,
			"services/api/go.mod":      "module example.com/api\n\ngo 1.21\n",
			"services/api/add.go":      "package api\n\nfunc Add(a, b int) int { return a + b }\n",
			"services/api/add_test.go": "package api\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
			"services/api/README.md":   "# api\n",
		},
	},
	{
		name: "repo-recipe-wins", needs: []string{"go"}, ecosystem: "go",
		test: "go test -count=1 ./...", build: "go build ./...", edit: "README.md",
		files: map[string]string{
			".attest/recipe.yaml": "test: go test -count=1 ./...\ntools:\n  go: \"1.21\"\n",
			"go.mod":              "module example.com/demo\n\ngo 1.21\n",
			"add.go":              "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
			"add_test.go":         "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
			"README.md":           "# demo\n",
		},
	},
}

func TestEveryLanguageEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("installs dependencies and runs real suites")
	}
	online := os.Getenv("ATTEST_E2E_OFFLINE") != "1"
	for _, c := range langCases {
		t.Run(c.name, func(t *testing.T) {
			for _, need := range c.needs {
				if _, err := exec.LookPath(need); err != nil {
					t.Skipf("%s is not installed on this machine (the worker image has it)", need)
				}
			}
			if c.network && !online {
				t.Skip("needs a package registry")
			}
			origin := seedFiles(t, c.files)
			fb := &fakeBot{claim: testClaim("fake")}
			fb.claim.Job.Spec.FilesHint = []string{c.edit}
			gh := &fakeGitHub{}
			r := newTestRunner(t, fb, gh, origin)
			if code := r.Run(context.Background()); code != 0 {
				t.Fatalf("worker exit %d", code)
			}
			res := fb.result
			if res == nil {
				t.Fatal("no result")
			}
			if res.Status != app.JobSucceeded {
				t.Fatalf("status %s: %s (%s)\nsetup: %s", res.Status, res.Error.Code, res.Error.Message, res.Setup.Output)
			}
			if res.Recipe == nil || res.Recipe.Ecosystem != c.ecosystem {
				t.Fatalf("ecosystem %+v, want %s", res.Recipe, c.ecosystem)
			}
			if res.Tests.Command != c.test {
				t.Errorf("test command %q, want %q", res.Tests.Command, c.test)
			}
			if res.Build.Command != c.build {
				t.Errorf("build command %q, want %q", res.Build.Command, c.build)
			}
			// The point of the whole exercise: something actually ran, twice, and passed.
			if !res.Tests.Before.Ran || !res.Tests.Before.OK || !res.Tests.After.Ran || !res.Tests.After.OK {
				t.Errorf("tests before %+v after %+v (setup: %v %s)", res.Tests.Before, res.Tests.After, res.Setup.OK, lastLines(res.Setup.Output, 600))
			}
			if !res.Build.Before.OK || !res.Build.After.OK {
				t.Errorf("build before %+v after %+v", res.Build.Before, res.Build.After)
			}
			if res.PR == nil || res.PR.URL == "" {
				t.Errorf("no pull request: %+v", res)
			}
			// Exactly the file the engine touched. A build runs twice in every one of these
			// jobs, and its output must not turn up in the pull request.
			if len(res.FilesChanged) != 1 || res.FilesChanged[0] != c.edit {
				t.Errorf("committed %v, want only %s — build output is leaking into the commit", res.FilesChanged, c.edit)
			}
			t.Logf("%s: %s · setup %.0fs · build %.0fs · test %.0fs (%d passed) · %d files",
				c.name, res.Recipe.Describe(), res.Setup.Seconds, res.Build.After.Seconds, res.Tests.After.Seconds,
				res.Tests.After.Passed, len(res.FilesChanged))
		})
	}
}

// The workdir the harness picked is the workdir the engine is told about, and the one the
// commands ran in: a monorepo where those disagree runs the wrong package's suite and reports it
// as this one's.
func TestMonorepoRunsInThePackageDirectory(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	origin := seedFiles(t, langCases[6].files)
	fb := &fakeBot{claim: testClaim("fake")}
	fb.claim.Job.Spec.FilesHint = []string{"services/api/README.md"}
	r := newTestRunner(t, fb, &fakeGitHub{}, origin)
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("worker exit %d", code)
	}
	res := fb.result
	if res.Recipe.Workdir != "services/api" {
		t.Fatalf("workdir %q", res.Recipe.Workdir)
	}
	// web/'s test script is `exit 1`; if the harness had run the root or the wrong package, the
	// suite would have failed rather than passed.
	if !res.Tests.After.OK {
		t.Fatalf("ran the wrong package's suite: %+v", res.Tests.After)
	}
	brief := briefText(Brief{JobID: 1, Spec: fb.claim.Job.Spec, Recipe: res.Recipe, Baseline: res.Tests.Before, Build: res.Build.Before})
	for _, want := range []string{"Run these from services/api", "test: go test ./...", "passed before your change"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the engine was not told %q\n%s", want, brief)
		}
	}
}

// A repository nothing matches is not a failing suite: the job still opens a pull request, and
// says plainly that nothing checked it and how to fix that.
func TestUnknownRepositoryIsHonest(t *testing.T) {
	origin := seedFiles(t, map[string]string{"README.md": "# docs only\n", "notes.txt": "hello\n"})
	fb := &fakeBot{claim: testClaim("fake")}
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("worker exit %d", code)
	}
	res := fb.result
	if res.Status != app.JobSucceeded || res.PR == nil {
		t.Fatalf("result: %+v", res)
	}
	if res.Recipe == nil || res.Recipe.Source != app.RecipeSourceNone || res.Tests.Before.Ran || res.Build.Before.Ran {
		t.Fatalf("recipe: %+v", res.Recipe)
	}
	body, _ := gh.bodies[0]["body"].(string)
	for _, want := range []string{"Nothing here was checked automatically", ".attest/recipe.yaml", "no marker file for any ecosystem"} {
		if !strings.Contains(body, want) {
			t.Errorf("pr body lacks %q\n%s", want, body)
		}
	}
	var _ = json.Marshal
}

// A repository whose only marker file lives in docs/ is not a docs project. jq is C with a
// Python-tooled docs/ directory: detection used to stand in docs/, and a job would have compiled
// the documentation and reported it as the project's build — passing, and about nothing the
// change touched. Nothing to build honestly beats something irrelevant that looks green.
func TestDocsToolingDoesNotBecomeTheProject(t *testing.T) {
	root := t.TempDir()
	for path, body := range map[string]string{
		"configure.ac":      "AC_INIT([jqish], [1.0])\n",
		"Makefile.am":       "TESTS = tests/run\n",
		"src/main.c":        "int main(void){return 0;}\n",
		"docs/Pipfile":      "[packages]\nmkdocs = \"*\"\n",
		"docs/Pipfile.lock": "{}\n",
		"docs/mkdocs.yml":   "site_name: jqish\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := resolveRecipe(root, nil, "", nil)
	if r.Workdir != "." {
		t.Errorf("stood in %q; the project is the repository, not its documentation", r.Workdir)
	}
	if r.Ecosystem == "python" {
		t.Errorf("called a C repository python because docs/ is: %s", r.Describe())
	}
	if r.Test != nil || r.Build != nil {
		t.Errorf("invented a command out of the docs tooling: %s", r.Describe())
	}
	if r.Why == "" {
		t.Error("said nothing about why there is no command")
	}
	// A real package one level down is still found — this must not blind the monorepo case.
	pkg := filepath.Join(root, "pkgs", "core")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "go.mod"), []byte("module x\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := resolveRecipe(root, nil, "", nil); r.Workdir != "pkgs/core" {
		t.Errorf("lost the real package: workdir %q (%s)", r.Workdir, r.Describe())
	}
}
