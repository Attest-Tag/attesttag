package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"attesttag/internal/app"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

var polyglot = map[string]string{
	"README.md":           "# monorepo\n",
	"a/Makefile":          "test:\n\t@true\n",
	"b/package.json":      `{"name":"b","scripts":{"test":"node t.js"}}`,
	"c/pyproject.toml":    "[project]\nname = \"c\"\nrequires-python = \">=3.10\"\n",
	"d/go.mod":            "module example.com/d\n\ngo 1.21\n",
	"docs/Pipfile":        "[packages]\n",
	"tools/x/Makefile":    "test:\n\t@true\n",
	"services/api/go.mod": "module example.com/api\n\ngo 1.21\n",
	"web/package.json":    `{"name":"web","scripts":{"test":"exit 1"}}`,
}

// The first job on a repository leaves what it detected on the connection, about whatever
// package that job was about. It must not steer the next job, which may be about another.
func TestRememberedDetectionDoesNotPinTheJob(t *testing.T) {
	root := writeTree(t, polyglot)
	remembered := &app.Recipe{Source: app.RecipeSourceDetected, Ecosystem: "go", Workdir: "services/api",
		Test: &app.RecipeStep{Name: "test", Argv: []string{"go", "test", "./..."}}}
	hints := []string{"web/src/app.ts"}
	if r := resolveRecipe(root, remembered, "", hints); r.Workdir != "web" || r.Source != app.RecipeSourceDetected || r.Test.String() != "npm run test" {
		t.Fatalf("a remembered guess steered the job: %s (%s, %s)", r.Workdir, r.Source, r.Test.String())
	}
	set := *remembered
	set.Source = app.RecipeSourceConnection
	if r := resolveRecipe(root, &set, "", hints); r.Workdir != "services/api" || r.Source != app.RecipeSourceConnection {
		t.Fatalf("an admin's recipe lost: %s (%s)", r.Workdir, r.Source)
	}
	old := *remembered
	old.Source = ""
	if r := resolveRecipe(root, &old, "", hints); r.Workdir != "services/api" {
		t.Fatalf("a recipe stored before Source existed is still somebody's decision: %s", r.Workdir)
	}
	if r := resolveRecipe(root, remembered, "npm test -- --ci", hints); r.Workdir != "web" || r.Test.String() != "npm test -- --ci" {
		t.Fatalf("the legacy test command was lost with the guess: %s %q", r.Workdir, r.Test.String())
	}
}

func TestResolvePackages(t *testing.T) {
	root := writeTree(t, polyglot)
	dirs := func(pkgs []*pkgRun) []string {
		var out []string
		for _, p := range pkgs {
			out = append(out, p.Dir)
		}
		return out
	}
	hints := []string{"a/src/x.c", "b/index.js", "docs/conf.py", "a/y.c", "c/m.py", "d/main.go"}
	primary := resolveRecipe(root, nil, "", hints)
	pkgs := resolvePackages(root, primary, hints)
	if got := dirs(pkgs); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("packages %v, want a, b, c (docs is not a package, a is not twice, d is past the cap)", got)
	}
	if !pkgs[0].Primary || pkgs[1].Primary || pkgs[1].Recipe.Ecosystem != "node" || pkgs[2].Recipe.Ecosystem != "python" {
		t.Fatalf("packages: %+v %+v %+v", pkgs[0], pkgs[1].Recipe, pkgs[2].Recipe)
	}

	// The root is a package of its own beside a child that declares one.
	nested := writeTree(t, map[string]string{"Makefile": "test:\n\t@true\n", "services/api/go.mod": "module x\n\ngo 1.21\n"})
	h := []string{"README.md", "services/api/main.go"}
	if got := dirs(resolvePackages(nested, resolveRecipe(nested, nil, "", h), h)); !reflect.DeepEqual(got, []string{".", "services/api"}) {
		t.Fatalf("root and child: %v", got)
	}

	// A recipe the repository wrote speaks for its package; the brief's other packages are still checked.
	authored := writeTree(t, map[string]string{".attest/recipe.yaml": "workdir: a\ntest: make test\n", "a/Makefile": "test:\n\t@true\n",
		"b/package.json": `{"scripts":{"test":"node t.js"}}`})
	h = []string{"b/index.js"}
	r := resolveRecipe(authored, nil, "", h)
	if got := dirs(resolvePackages(authored, r, h)); r.Source != app.RecipeSourceRepoFile || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("authored primary: %s %v", r.Source, got)
	}
}

func TestUncheckedPackages(t *testing.T) {
	root := writeTree(t, polyglot)
	pkgs := []*pkgRun{{Dir: "a"}, {Dir: "b"}, {Dir: "c", Skipped: "no time"}}
	changed := []string{"a/x.c", "d/main.go", "docs/conf.py", "README.md", "d/sub/gone.go", "tools/x/run.sh", "c/m.py"}
	if got := uncheckedPackages(root, changed, pkgs); !reflect.DeepEqual(got, []string{"d"}) {
		t.Fatalf("unchecked %v, want [d]", got)
	}
}

func TestVersionFloors(t *testing.T) {
	for spec, want := range map[string]string{
		">=3.5": "", ">=3.9": "3.9", ">=3.10,<3.13": "3.10", "<3.12": "", "~=3.11": "3.11", "==3.12.*": "3.12",
		"==3.8.*": "", ">3.9": "3.9", ">=3.9, !=3.9.1": "3.9", "": "", ">= 3.11": "3.11",
	} {
		if got := pythonFloor(spec); got != want {
			t.Errorf("pythonFloor(%q) = %q, want %q", spec, got, want)
		}
	}
	for spec, want := range map[string]string{
		">=18.0.0": "18", "^18 || ^20": "18", "18.x": "18", ">= 16 < 21": "16", "<20": "", "*": "",
		"v20.1.0": "20", "~18.17": "18", "16 - 20": "16", "": "", ">=14 || <10": "",
	} {
		if got := nodeFloor(spec); got != want {
			t.Errorf("nodeFloor(%q) = %q, want %q", spec, got, want)
		}
	}
	// What the probes make of them: floors, and nothing for the pins mise reads itself.
	root := writeTree(t, map[string]string{
		"go/go.mod": "module x\n\ngo 1.16\n", "rs/Cargo.toml": "[package]\n", "rs/rust-toolchain.toml": "[toolchain]\nchannel = \"1.70\"\n",
		"py/pyproject.toml": "[project]\nrequires-python = \"<3.12\"\n", "py/.python-version": "3.11\n",
		"js/package.json": `{"engines":{"node":">=18.0.0"}}`, "pin/package.json": `{"engines":{"node":">=18"}}`, "pin/.nvmrc": "20\n",
	})
	for dir, want := range map[string]map[string]string{"go": nil, "rs": nil, "py": nil, "js": {"node": "18"}, "pin": nil} {
		if got := detectAt(root, dir).Tools; len(got) != len(want) || (want != nil && !reflect.DeepEqual(got, want)) {
			t.Errorf("%s: tools %v, want %v", dir, got, want)
		}
	}
}

func TestParseMiseList(t *testing.T) {
	raw := "mise WARN something about a config\n" + `{
  "node": [{"version": "20.19.0", "requested_version": "20.19.0", "install_path": "/work/.cache/mise/installs/node/20.19.0",
    "source": {"type": "idiomatic-version-file", "path": "/work/job/repo/a/.nvmrc"}, "installed": true, "active": true}],
  "python": [{"version": "3.5.10", "requested_version": "3.5.10", "install_path": "/work/.cache/mise/installs/python/3.5.10",
    "source": {"type": "idiomatic-version-file", "path": "/work/job/repo/p35/.python-version"}, "installed": false, "active": false}]
}`
	got, err := parseMiseList(raw)
	if err != nil || len(got) != 2 || got[0].Tool != "node" || !got[0].Installed || got[1].Tool != "python" || got[1].Installed || got[1].Requested != "3.5.10" {
		t.Fatalf("%+v %v", got, err)
	}
	if empty, err := parseMiseList("{}"); err != nil || len(empty) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	tc := &toolchains{repo: "/work/job/repo"}
	if tc.sourceLabel(got[1].Source.Path) != "p35/.python-version" || tc.sourceLabel("/work/job/home/.config/mise/config.toml") != "the job's default" {
		t.Fatal("source labels")
	}
}

// The shims only resolve what mise was told it may read, and a shim that resolved by other
// settings than the install used would find a different version than the one installed.
func TestMiseSettingsAreSharedByInstallAndShims(t *testing.T) {
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise is not installed here")
	}
	job := t.TempDir()
	ws := &Workspace{JobDir: job, RepoDir: filepath.Join(job, "repo"), Env: baseEnv(job, t.TempDir())}
	tc := newToolchains(ws, Options{WorkDir: t.TempDir(), MiseBin: "mise"})
	os.MkdirAll(tc.shims, 0o755)
	os.WriteFile(filepath.Join(tc.shims, "node"), nil, 0o755)
	install := tc.installEnv(ws)
	tc.activate(ws)
	for _, kv := range tc.settings {
		key, _, _ := strings.Cut(kv, "=")
		if envValue(install, key) != envValue(ws.Env, key) {
			t.Errorf("%s differs between install and run", key)
		}
	}
	if !strings.HasPrefix(envValue(ws.Env, "PATH"), tc.shims+string(os.PathListSeparator)) || strings.Contains(envValue(install, "PATH"), tc.shims) {
		t.Errorf("shims: run PATH %q, install PATH %q", envValue(ws.Env, "PATH"), envValue(install, "PATH"))
	}
	if envValue(ws.Env, "MISE_NOT_FOUND_AUTO_INSTALL") != "0" || envValue(install, "MISE_YES") != "1" || envValue(ws.Env, "MISE_YES") != "" {
		t.Error("a shim may install, or an install may prompt")
	}
	if strings.Contains(envValue(install, "MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS"), "go") {
		t.Error("mise reads go.mod, where GOTOOLCHAIN already does")
	}
}

func TestBriefNamesEveryPackage(t *testing.T) {
	legacy := &app.Recipe{Ecosystem: "python", Workdir: "legacy", Test: step("test", ".venv/bin/python", "-m", "pytest")}
	web := &pkgRun{Dir: "web", Recipe: &app.Recipe{Ecosystem: "node", Workdir: "web", Test: step("test", "npm", "run", "test")},
		Tools: []string{"node 20.19.0 (web/.nvmrc)"},
		Tests: app.JobCheck{Before: app.JobTestRun{Ran: true, OK: false, Output: "1 failing: login"}}}
	b := Brief{JobID: 1, Recipe: legacy, Baseline: app.JobTestRun{Ran: true, OK: true}, Shims: true,
		Primary: &pkgRun{Dir: "legacy", Primary: true, Prefix: []string{"UV_PYTHON=3.8"}, OldPython: "3.8.20",
			Notes: []string{"legacy/.python-version pins Python 3.5.10, which the worker cannot provide"}},
		Extras: []*pkgRun{web, {Dir: "tools", Skipped: "there was not enough time left in the job to set it up and check it"}}}
	got := recipeBlock(b)
	for _, want := range []string{
		"- test: UV_PYTHON=3.8 .venv/bin/python -m pytest  (passed before your change)",
		"Python 3.8.20", "not the bug", "do not port",
		"Also checked: web/ (node). Run these from web:", "- test: npm run test  (FAILED before your change",
		"Toolchains: node 20.19.0 (web/.nvmrc).", "Its test output before your change:\n1 failing: login",
		"Not checked: tools/ — there was not enough time", "cd web && npm test", "A package you change outside",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	// A job about one package is told exactly what it always was.
	one := Brief{JobID: 1, Recipe: &app.Recipe{Ecosystem: "go", Workdir: "services/api", Test: step("test", "go", "test", "./...")}}
	withPrimary := one
	withPrimary.Primary = &pkgRun{Dir: "services/api", Primary: true}
	if recipeBlock(one) != recipeBlock(withPrimary) {
		t.Errorf("a single package's brief changed:\n%s\nvs\n%s", recipeBlock(one), recipeBlock(withPrimary))
	}
}

func TestPRBodyShowsEveryPackage(t *testing.T) {
	spec := app.JobSpec{Requirement: "Fix the login.", Constraints: app.JobConstraints{Engine: "fake", Model: "m"}}
	pass := app.JobTestRun{Ran: true, OK: true, Seconds: 2}
	res := &app.JobResult{Recipe: &app.Recipe{Source: app.RecipeSourceDetected, Workdir: "services/api", Test: step("test", "go", "test", "./...")},
		Tests: app.JobCheck{Command: "go test ./...", Before: pass, After: pass},
		Packages: []app.JobPackage{
			{Workdir: "web", Recipe: &app.Recipe{Source: app.RecipeSourceDetected, Workdir: "web", Test: step("test", "npm", "run", "test")},
				Tests: app.JobCheck{Command: "npm run test", Before: pass, After: app.JobTestRun{Ran: true, OK: false, Failed: 1, Output: "FAIL login.test.js"}},
				Note:  "web/.nvmrc pins node 0.0.1, which could not be installed"},
			{Workdir: "tools", Skipped: "there was not enough time left in the job to set it up and check it"},
		},
		Unchecked: []string{"docs-site", "."}, PR: &app.JobPR{URL: "https://github.com/acme/app/pull/3"}}
	pkgs := []*pkgRun{{Dir: "services/api"}, {Dir: "web", Tools: []string{"node 22.23.2 (the job's default)"}}, {Dir: "tools"}}
	body := prBody(spec, res, "Fixed it.", res.Recipe, 1, nil, pkgs)
	for _, want := range []string{
		"> **Tests in `web/` still fail after this change.**",
		"> **This change also touches `docs-site/` and the repository root, which this job did not check.**",
		"### `services/api/`", "### `web/`", "| test after | `npm run test` | **fail** (1 failed, 0 passed) |",
		"> web/.nvmrc pins node 0.0.1, which could not be installed.", "FAIL login.test.js",
		"Toolchains: node 22.23.2 (the job's default)", "### `tools/`\nNot checked: there was not enough time",
		"### Changed but not checked\n- `docs-site/`\n- the repository root\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if got := ticketComment(spec, res); !strings.Contains(got, "tests still failing in web/; 2 other changed packages not checked") {
		t.Errorf("ticket comment: %q", got)
	}
	one := &app.JobResult{Recipe: res.Recipe, Tests: res.Tests, PR: res.PR}
	if b := prBody(spec, one, "Fixed it.", one.Recipe, 1, nil, nil); strings.Contains(b, "###") || strings.Contains(b, "also touches") {
		t.Errorf("a single package's body grew sections:\n%s", b)
	}
	if got := ticketComment(spec, one); !strings.Contains(got, "(checks pass)") {
		t.Errorf("single ticket comment: %q", got)
	}
}

func TestTimeShares(t *testing.T) {
	if got := extraBudget(45*time.Minute, 2*time.Minute, 0, 2); got != extraMax {
		t.Errorf("roomy job: %s", got)
	}
	if got := extraBudget(20*time.Minute, 2*time.Minute, 0, 2); got >= extraMin {
		t.Errorf("a tight job still gives an extra package %s", got)
	}
	deadline := time.Now().Add(40 * time.Minute)
	if got := time.Until(engineStop(deadline, 4*time.Minute, false)); got < 31*time.Minute || got > 33*time.Minute {
		t.Errorf("engine gets %s of 40m after 4m of checks", got)
	}
	if got := time.Until(engineStop(time.Now().Add(10*time.Minute), 0, false)); got < 5*time.Minute || got > 7*time.Minute {
		t.Errorf("engine gets %s of 10m with instant checks", got)
	}
	if got := afterCap(5*time.Minute, time.Now().Add(4*time.Minute)); got > time.Minute || got < 50*time.Second {
		t.Errorf("after-check cap %s", got)
	}
	if got := afterCap(5*time.Minute, time.Now().Add(3*time.Minute+10*time.Second)); got != 0 {
		t.Errorf("no time left, yet a cap of %s", got)
	}
}

// A toolchain is full of links — npx, python3, mise's version aliases — and dropping them left an
// install mise counted as present with half its programs gone.
func TestCacheKeepsToolchainLinks(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o755)
	}
	link := func(rel, to string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(to, p); err != nil {
			t.Fatal(err)
		}
	}
	mk("mise/installs/node/20.19.0/bin/node", "node")
	mk("mise/installs/node/20.19.0/lib/npx-cli.js", "npx")
	mk("mise/installs/node/20.19.0/lib/libnode.so", strings.Repeat("n", 1<<20))
	link("mise/installs/node/20.19.0/bin/npx", "../lib/npx-cli.js")
	link("mise/installs/node/20", "./20.19.0")
	mk("uv-python/cpython-3.8.20/bin/python3.8", "py")
	link("uv-python/cpython-3.8.20/bin/python3", "python3.8")
	link("uv-python/cpython-3.8", filepath.Join(root, "uv-python", "cpython-3.8.20")) // absolute, inside
	link("npm/evil", "/etc/passwd")
	link("npm/climb", "../../outside")
	mk("npm/_cacache/index", strings.Repeat("x", 5<<20))

	var buf bytes.Buffer
	if _, err := tarFrom(&buf, root, app.JobCacheMaxBytes); err != nil {
		t.Fatal(err)
	}
	packed := buf.Bytes()
	out := t.TempDir()
	if _, err := untarInto(bytes.NewReader(packed), out); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"mise/installs/node/20.19.0/bin/npx": "../lib/npx-cli.js", "mise/installs/node/20": "./20.19.0",
		"uv-python/cpython-3.8.20/bin/python3": "python3.8", "uv-python/cpython-3.8": "cpython-3.8.20",
	} {
		got, err := os.Readlink(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", rel, got, err, want)
		}
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s does not resolve: %v", rel, err)
		}
	}
	for _, rel := range []string{"npm/evil", "npm/climb"} {
		if _, err := os.Lstat(filepath.Join(out, rel)); err == nil {
			t.Errorf("%s, a link out of the cache, was carried", rel)
		}
	}

	// A toolchain that does not fit is left out whole, never cut off inside.
	buf.Reset()
	if _, err := tarFrom(&buf, root, 5<<20+100); err != nil {
		t.Fatal(err)
	}
	cut := t.TempDir()
	untarInto(bytes.NewReader(buf.Bytes()), cut)
	if _, err := os.Stat(filepath.Join(cut, "npm/_cacache/index")); err != nil {
		t.Fatalf("the store that fit was not carried: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cut, "mise/installs/node/20.19.0")); err == nil {
		t.Error("a toolchain was carried past the cap")
	}

	// A restore that breaks off partway leaves no toolchain that reads as installed.
	broken := t.TempDir()
	if _, err := untarInto(bytes.NewReader(packed[:len(packed)*3/4]), broken); err == nil {
		t.Fatal("a truncated cache restored without an error")
	}
	if _, err := os.Stat(filepath.Join(broken, "mise")); err == nil {
		t.Error("a partial toolchain was left behind")
	}
}

func TestQwenRunsOnTheImageNode(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "fakeqwen"), []byte("#!/usr/bin/env node\nconsole.log(1)\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "shqwen"), []byte("#!/bin/sh\necho 1\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "node"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", dir)
	script, _ := filepath.EvalSymlinks(filepath.Join(dir, "fakeqwen"))
	if got := qwenArgv("fakeqwen"); !reflect.DeepEqual(got, []string{filepath.Join(dir, "node"), script}) {
		t.Errorf("a node script is started by name: %v", got)
	}
	if got := qwenArgv("shqwen"); !reflect.DeepEqual(got, []string{"shqwen"}) {
		t.Errorf("a non-node program: %v", got)
	}
}

// Four packages hinted: the first three are each set up and checked before and after on their
// own, and the fourth, past the cap, is named as changed and unchecked.
func TestRunnerChecksEveryHintedPackage(t *testing.T) {
	fb, gh := runPolyglotJob(t, time.Hour)
	res := fb.result
	if res == nil || res.Status != app.JobSucceeded {
		t.Fatalf("result: %+v", res)
	}
	if res.Recipe.Workdir != "a" || !res.Tests.Before.OK || !res.Tests.After.OK {
		t.Fatalf("primary: %s %+v", res.Recipe.Workdir, res.Tests)
	}
	if len(res.Packages) != 2 || res.Packages[0].Workdir != "b" || res.Packages[1].Workdir != "c" {
		t.Fatalf("packages: %+v", res.Packages)
	}
	b := res.Packages[0]
	if !b.Tests.Before.Ran || b.Tests.Before.OK || !b.Tests.After.OK || b.Skipped != "" {
		t.Fatalf("b/ was not checked before and after on its own: %+v", b.Tests)
	}
	if !reflect.DeepEqual(res.Unchecked, []string{"d"}) || len(res.FilesChanged) != 4 {
		t.Fatalf("unchecked %v, files %v", res.Unchecked, res.FilesChanged)
	}
	if ph := fb.phases(); ph["test_before"] != "ok" || ph["test_after"] != "ok" {
		t.Fatalf("the primary's phases: %v", ph)
	}
	if !strings.Contains(fb.allText(), "b/ test before: fail") || !strings.Contains(fb.allText(), "b/ test after: pass") {
		t.Errorf("b/'s checks were not reported as lines:\n%s", fb.allText())
	}
	body, _ := gh.bodies[0]["body"].(string)
	for _, want := range []string{"### `a/`", "### `b/`", "### `c/`", "### Changed but not checked\n- `d/`"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, body)
		}
	}
}

// The same job with ten minutes: the other packages are skipped with the reason, and the job
// still ends in a pull request.
func TestRunnerSkipsExtrasWhenTimeIsShort(t *testing.T) {
	fb, _ := runPolyglotJob(t, 10*time.Minute)
	res := fb.result
	if res == nil || res.Status != app.JobSucceeded || len(res.Packages) != 2 {
		t.Fatalf("result: %+v", res)
	}
	for _, p := range res.Packages {
		if !strings.Contains(p.Skipped, "not enough time") || p.Tests.Before.Ran {
			t.Errorf("%s: %+v", p.Workdir, p)
		}
	}
	if !reflect.DeepEqual(res.Unchecked, []string{"d"}) {
		t.Errorf("unchecked %v", res.Unchecked)
	}
}

func runPolyglotJob(t *testing.T, wall time.Duration) (*fakeBot, *fakeGitHub) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	files := map[string]string{}
	for _, d := range []string{"a", "b", "c", "d"} {
		files[d+"/README.md"] = "# " + d + "\n"
		files[d+"/Makefile"] = "test:\n\t@true\n"
	}
	files["b/Makefile"] = "test:\n\tgrep -q 'attest_tag fix job' README.md\n"
	origin := seedFiles(t, files)
	fb := &fakeBot{claim: testClaim("fake")}
	fb.claim.Job.Spec.FilesHint = []string{"a/README.md", "b/README.md", "c/README.md", "d/README.md"}
	fb.claim.Limits.Deadline = time.Now().Add(wall).UTC().Format(time.RFC3339)
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	r.opts.MaxWall = wall
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("worker exit %d", code)
	}
	return fb, gh
}

// langCases[6] with the first job's guess remembered about web/: the job about services/api
// still runs there, and its suite — not web/'s failing one — is what passes.
func TestRememberedRecipeDoesNotFreezeTheMonorepo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	origin := seedFiles(t, langCases[6].files)
	fb := &fakeBot{claim: testClaim("fake")}
	fb.claim.Job.Spec.FilesHint = []string{"services/api/README.md"}
	fb.claim.Job.Spec.Constraints.Recipe = &app.Recipe{Source: app.RecipeSourceDetected, Ecosystem: "node", Workdir: "web",
		Setup: []app.RecipeStep{{Name: "install", Argv: []string{"npm", "install"}}}, Test: &app.RecipeStep{Name: "test", Argv: []string{"npm", "run", "test"}}}
	r := newTestRunner(t, fb, &fakeGitHub{}, origin)
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("worker exit %d", code)
	}
	if res := fb.result; res.Recipe.Workdir != "services/api" || res.Recipe.Source != app.RecipeSourceDetected || !res.Tests.After.OK {
		t.Fatalf("the remembered web/ recipe steered the job: %+v %+v", res.Recipe, res.Tests.After)
	}
}
