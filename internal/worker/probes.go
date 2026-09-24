package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"attesttag/internal/app"
)

// The ecosystem table. Each entry recognises one kind of package from the files next to it and
// says how that kind is installed, built, linted and tested. Adding a language is adding a row
// here — that is the whole point of the shape: the worker image decides what can run, this table
// decides what to run, and neither is a chain of special cases inside the pipeline.
//
// rank orders a directory that declares several (a Go service with a TypeScript console): the
// lower rank names the recipe and contributes the commands, and every match contributes its
// install step.
// rank is how strongly a marker file means "this repository IS this ecosystem", not how much
// anyone likes the language. It matters because real repositories carry several: Phoenix ships a
// package.json for its asset pipeline, nlohmann/json ships a Package.swift beside its CMake
// build, and a Django service ships one for its frontend. A package.json is the weakest claim
// any manifest makes — half the world has one for tooling — so it ranks last among manifests,
// and mix.exs or CMakeLists.txt, which nobody adds by accident, rank first.
var probes = []probe{
	{name: "go", rank: 10, markers: []string{"go.mod"}, plan: planGo},
	{name: "rust", rank: 12, markers: []string{"Cargo.toml"}, plan: planRust},
	{name: "elixir", rank: 14, markers: []string{"mix.exs"}, plan: planElixir},
	{name: "java-gradle", rank: 16, markers: []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"}, plan: planGradle},
	{name: "java-maven", rank: 18, markers: []string{"pom.xml"}, plan: planMaven},
	{name: "dotnet", rank: 20, markers: []string{"*.sln", "*.csproj", "*.fsproj", "global.json"}, plan: planDotnet},
	{name: "ruby", rank: 22, markers: []string{"Gemfile"}, plan: planRuby},
	{name: "php", rank: 24, markers: []string{"composer.json"}, plan: planPHP},
	{name: "dart", rank: 26, markers: []string{"pubspec.yaml"}, plan: planDart},
	// SwiftPM and CMake are told apart by layout, not by which file is rarer: plenty of C and
	// C++ libraries ship a Package.swift to offer a Swift binding, and their real build is the
	// CMake one (nlohmann/json: include/, src/, tests/). A genuine Swift package keeps its code
	// in Sources/, which is SwiftPM's own convention and not something a C++ project has.
	{name: "swift", rank: 28, markers: []string{"Package.swift"}, plan: planSwift,
		when: func(dir string) bool { return isDir(dir, "Sources") }},
	{name: "cmake", rank: 30, markers: []string{"CMakeLists.txt"}, plan: planCMake},
	{name: "swift-binding", rank: 31, markers: []string{"Package.swift"}, plan: planSwift},
	{name: "python", rank: 32, markers: []string{"pyproject.toml", "uv.lock", "poetry.lock", "Pipfile", "requirements.txt", "setup.py", "setup.cfg", "tox.ini"}, plan: planPython},
	{name: "node", rank: 40, markers: []string{"package.json"}, plan: planNode},
	// Last: a repository whose build is a Makefile and nothing else — C, a shell toolkit, a
	// Dockerfile-driven project. Without this row it would come back as "no ecosystem" even
	// though `make test` is sitting right there.
	{name: "make", rank: 90, markers: []string{"Makefile", "makefile", "GNUmakefile"}, plan: planMake},
}

// ---- helpers ----

func isDir(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && st.IsDir()
}

func readText(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || len(raw) > 1<<20 {
		return ""
	}
	return string(raw)
}

// firstLine is how the little version files are read: .nvmrc, .python-version, .ruby-version.
func firstLine(dir, name string) string {
	s := strings.TrimSpace(readText(dir, name))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if len(s) > 32 || strings.ContainsAny(s, " \t/\\") {
		return ""
	}
	return s
}

func hasGlob(dir, pattern string) bool {
	hits, _ := filepath.Glob(filepath.Join(dir, pattern))
	return len(hits) > 0
}

func tools(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			m[kv[i]] = kv[i+1]
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// local prefers a wrapper script the repository ships (./gradlew, ./mvnw) over whatever the
// image happens to have, which is what those wrappers exist for.
func local(dir, wrapper, fallback string) string {
	if st, err := os.Stat(filepath.Join(dir, wrapper)); err == nil && !st.IsDir() {
		return "./" + wrapper
	}
	return fallback
}

var makeTargetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+)\s*:(?:[^=]|$)`)

// makeTargets is the set of targets a Makefile defines, so `make test` is only offered when
// there is a test target to run.
func makeTargets(dir string) map[string]bool {
	out := map[string]bool{}
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		for _, m := range makeTargetRe.FindAllStringSubmatch(readText(dir, name), -1) {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- go ----

var goVersionRe = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+(?:\.\d+)?)`)

func planGo(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "go", Setup: []app.RecipeStep{setupStep("go", "mod", "download")},
		Build: step("build", "go", "build", "./..."), Test: step("test", "go", "test", "./...")}
	if m := goVersionRe.FindStringSubmatch(readText(dir, "go.mod")); m != nil {
		r.Tools = tools("go", m[1])
	}
	if exists(dir, ".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json") {
		r.Lint = step("lint", "golangci-lint", "run")
	}
	return r
}

// ---- rust ----

var rustChannelRe = regexp.MustCompile(`(?m)^\s*channel\s*=\s*"([^"]+)"`)

func planRust(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "rust", Setup: []app.RecipeStep{setupStep("cargo", "fetch")},
		Build: step("build", "cargo", "build", "--quiet"), Test: step("test", "cargo", "test", "--quiet")}
	for _, f := range []string{"rust-toolchain.toml", "rust-toolchain"} {
		if m := rustChannelRe.FindStringSubmatch(readText(dir, f)); m != nil {
			r.Tools = tools("rust", m[1])
			break
		}
	}
	if exists(dir, "clippy.toml", ".clippy.toml") {
		r.Lint = step("lint", "cargo", "clippy", "--quiet", "--all-targets")
	}
	return r
}

// ---- python ----

var (
	pyRequiresRe = regexp.MustCompile(`requires-python\s*=\s*"[^0-9]*(\d+\.\d+)`)
	pytestArgs   = []string{"-x", "-q", "-p", "no:cacheprovider"}
	// compileall is the closest Python has to a compile step, and it is worth having: a syntax
	// error is the most common way a change breaks a repository with no suite to catch it.
	compileSkip = `(^|/)(\.venv|venv|\.tox|node_modules|\.git|build|dist|site-packages)/`
)

func planPython(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "python"}
	// runner is how anything in this project is invoked, which is decided by how it is locked.
	var runner []string
	switch {
	case exists(dir, "uv.lock"):
		r.Setup = []app.RecipeStep{setupStep("uv", "sync", "--frozen")}
		runner = []string{"uv", "run", "python", "-m"}
	case exists(dir, "poetry.lock"):
		r.Setup = []app.RecipeStep{setupStep("poetry", "install", "--no-interaction", "--no-ansi")}
		runner = []string{"poetry", "run", "python", "-m"}
	case exists(dir, "Pipfile.lock", "Pipfile"):
		r.Setup = []app.RecipeStep{setupStep("pipenv", "sync", "--dev")}
		runner = []string{"pipenv", "run", "python", "-m"}
	case exists(dir, "pyproject.toml"):
		r.Setup = []app.RecipeStep{setupStep("uv", "sync")}
		runner = []string{"uv", "run", "python", "-m"}
	case exists(dir, "requirements.txt"):
		r.Setup = []app.RecipeStep{setupStep("uv", "venv", ".venv"),
			setupStep("uv", "pip", "install", "--python", ".venv/bin/python", "-r", "requirements.txt")}
		for _, dev := range []string{"requirements-dev.txt", "requirements_dev.txt", "dev-requirements.txt"} {
			if exists(dir, dev) {
				s := setupStep("uv", "pip", "install", "--python", ".venv/bin/python", "-r", dev)
				s.Optional = true
				r.Setup = append(r.Setup, s)
			}
		}
		runner = []string{".venv/bin/python", "-m"}
	case exists(dir, "setup.py"):
		r.Setup = []app.RecipeStep{setupStep("uv", "venv", ".venv"),
			setupStep("uv", "pip", "install", "--python", ".venv/bin/python", "-e", ".")}
		runner = []string{".venv/bin/python", "-m"}
	default:
		runner = []string{"python3", "-m"}
	}
	// Every runner ends "python -m", so a step is the interpreter and a module rather than a
	// program that has to exist on PATH: `uv run compileall` looks for a binary called
	// compileall and does not find one, where `uv run python -m compileall` is the stdlib.
	run := func(mod string, args ...string) *app.RecipeStep {
		argv := append(append([]string{}, runner...), mod)
		return step(mod, append(argv, args...)...)
	}
	if m := pyRequiresRe.FindStringSubmatch(readText(dir, "pyproject.toml")); m != nil {
		r.Tools = tools("python", m[1])
	}
	if v := firstLine(dir, ".python-version"); v != "" {
		r.Tools = tools("python", v)
	}
	if hasPytest(dir) {
		t := run("pytest", pytestArgs...)
		t.Name = "test"
		r.Test = t
	}
	b := run("compileall", "-q", "-x", compileSkip, ".")
	b.Name = "build"
	r.Build = b
	if exists(dir, "mypy.ini", ".mypy.ini") || strings.Contains(readText(dir, "pyproject.toml"), "[tool.mypy]") {
		l := run("mypy", ".")
		l.Name = "lint"
		r.Lint = l
	}
	return r
}

// hasPytest is whether there is anything for pytest to collect: configuration, a tests
// directory, or test files within two levels.
func hasPytest(dir string) bool {
	if exists(dir, "pytest.ini", "conftest.py", "tox.ini") || hasPytestSection(dir) {
		return true
	}
	for _, d := range []string{"tests", "test"} {
		if st, err := os.Stat(filepath.Join(dir, d)); err == nil && st.IsDir() {
			return true
		}
	}
	return hasTestFiles(dir)
}

// ---- node ----

type pkgJSON struct {
	Scripts        map[string]string `json:"scripts"`
	PackageManager string            `json:"packageManager"`
	Workspaces     any               `json:"workspaces"`
	Engines        struct {
		Node string `json:"node"`
	} `json:"engines"`
}

func readPkg(dir string) (pkgJSON, bool) {
	var p pkgJSON
	raw := readText(dir, "package.json")
	if raw == "" || json.Unmarshal([]byte(raw), &p) != nil {
		return p, false
	}
	return p, true
}

var nodeEngineRe = regexp.MustCompile(`(\d+)(?:\.\d+)*`)

func planNode(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "node"}
	pkg, ok := readPkg(dir)
	if !ok {
		return r
	}
	// The package manager is whichever one left its lockfile, because that is the one whose
	// resolution the repository actually committed to.
	pm, frozen := "npm", []string{"ci", "--no-audit", "--no-fund"}
	switch {
	case exists(dir, "bun.lockb", "bun.lock"):
		pm, frozen = "bun", []string{"install", "--frozen-lockfile"}
	case exists(dir, "pnpm-lock.yaml"):
		pm, frozen = "pnpm", []string{"install", "--frozen-lockfile"}
	case exists(dir, "yarn.lock"):
		pm = "yarn"
		if exists(dir, ".yarnrc.yml") { // Berry renamed the flag
			frozen = []string{"install", "--immutable"}
		} else {
			frozen = []string{"install", "--frozen-lockfile"}
		}
	case exists(dir, "package-lock.json", "npm-shrinkwrap.json"):
	default:
		frozen = []string{"install", "--no-audit", "--no-fund"}
	}
	if strings.HasPrefix(pkg.PackageManager, "pnpm") {
		pm = "pnpm"
	} else if strings.HasPrefix(pkg.PackageManager, "yarn") {
		pm = "yarn"
	}
	r.Setup = []app.RecipeStep{setupStep(append([]string{pm}, frozen...)...)}
	if v := firstLine(dir, ".nvmrc"); v != "" {
		r.Tools = tools("node", v)
	} else if v := firstLine(dir, ".node-version"); v != "" {
		r.Tools = tools("node", v)
	} else if m := nodeEngineRe.FindString(pkg.Engines.Node); m != "" {
		r.Tools = tools("node", m)
	}
	script := func(name string) *app.RecipeStep {
		s := strings.TrimSpace(pkg.Scripts[name])
		if s == "" || strings.Contains(s, "no test specified") {
			return nil
		}
		return step(name, pm, "run", name)
	}
	r.Test, r.Build, r.Lint = script("test"), script("build"), script("lint")
	if r.Build == nil {
		// No build script: a typecheck is the next best compile gate, and a TypeScript
		// repository always has one to give.
		if s := script("typecheck"); s != nil {
			s.Name = "build"
			r.Build = s
		} else if s := script("tsc"); s != nil {
			s.Name = "build"
			r.Build = s
		}
	}
	return r
}

// ---- jvm ----

var mavenReleaseRe = regexp.MustCompile(`<maven\.compiler\.(?:release|source|target)>\s*(?:1\.)?(\d+)\s*</`)

func planMaven(dir string) app.Recipe {
	mvn := local(dir, "mvnw", "mvn")
	setup := setupStep(mvn, "-B", "-ntp", "-q", "-DskipTests", "dependency:go-offline")
	setup.Optional = true // an offline pre-fetch that fails still leaves a build that works
	r := app.Recipe{Ecosystem: "java-maven", Setup: []app.RecipeStep{setup},
		Build: step("build", mvn, "-B", "-ntp", "-q", "-DskipTests", "package"),
		Test:  step("test", mvn, "-B", "-ntp", "test")}
	if v := firstLine(dir, ".java-version"); v != "" {
		r.Tools = tools("java", v)
	} else if m := mavenReleaseRe.FindStringSubmatch(readText(dir, "pom.xml")); m != nil {
		r.Tools = tools("java", m[1])
	}
	return r
}

func planGradle(dir string) app.Recipe {
	g := local(dir, "gradlew", "gradle")
	r := app.Recipe{Ecosystem: "java-gradle",
		Build: step("build", g, "--no-daemon", "-q", "assemble"),
		Test:  step("test", g, "--no-daemon", "test")}
	if v := firstLine(dir, ".java-version"); v != "" {
		r.Tools = tools("java", v)
	}
	return r
}

// ---- dotnet ----

func planDotnet(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "dotnet", Setup: []app.RecipeStep{setupStep("dotnet", "restore")},
		Build: step("build", "dotnet", "build", "--no-restore", "--nologo")}
	if hasGlob(dir, "*[Tt]est*.csproj") || hasGlob(dir, "*/*[Tt]est*.csproj") || hasGlob(dir, "*.sln") {
		r.Test = step("test", "dotnet", "test", "--no-build", "--nologo")
	}
	return r
}

// ---- ruby ----

func planRuby(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "ruby", Setup: []app.RecipeStep{setupStep("bundle", "install", "--jobs", "4", "--retry", "2")}}
	if v := firstLine(dir, ".ruby-version"); v != "" {
		r.Tools = tools("ruby", v)
	}
	switch {
	case exists(dir, "spec"), exists(dir, ".rspec"):
		r.Test = step("test", "bundle", "exec", "rspec")
	case exists(dir, "Rakefile"):
		r.Test = step("test", "bundle", "exec", "rake", "test")
	}
	if exists(dir, ".rubocop.yml") {
		r.Lint = step("lint", "bundle", "exec", "rubocop")
	}
	return r
}

// ---- php ----

func planPHP(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "php", Setup: []app.RecipeStep{setupStep("composer", "install", "--no-interaction", "--no-progress")}}
	if exists(dir, "phpunit.xml", "phpunit.xml.dist") {
		r.Test = step("test", "vendor/bin/phpunit")
	} else if exists(dir, "tests") {
		r.Test = step("test", "vendor/bin/phpunit", "tests")
	}
	return r
}

// ---- elixir ----

func planElixir(dir string) app.Recipe {
	return app.Recipe{Ecosystem: "elixir", Setup: []app.RecipeStep{setupStep("mix", "deps.get")},
		Build: step("build", "mix", "compile"), Test: step("test", "mix", "test")}
}

// ---- dart ----

func planDart(dir string) app.Recipe {
	if strings.Contains(readText(dir, "pubspec.yaml"), "flutter:") {
		return app.Recipe{Ecosystem: "dart", Setup: []app.RecipeStep{setupStep("flutter", "pub", "get")},
			Build: step("build", "flutter", "analyze"), Test: step("test", "flutter", "test")}
	}
	return app.Recipe{Ecosystem: "dart", Setup: []app.RecipeStep{setupStep("dart", "pub", "get")},
		Build: step("build", "dart", "analyze"), Test: step("test", "dart", "test")}
}

// ---- swift ----

func planSwift(dir string) app.Recipe {
	return app.Recipe{Ecosystem: "swift", Build: step("build", "swift", "build"), Test: step("test", "swift", "test")}
}

// ---- cmake ----

func planCMake(dir string) app.Recipe {
	return app.Recipe{Ecosystem: "cmake",
		Setup: []app.RecipeStep{setupStep("cmake", "-S", ".", "-B", "build")},
		Build: step("build", "cmake", "--build", "build", "--parallel"),
		Test:  step("test", "ctest", "--test-dir", "build", "--output-on-failure")}
}

// ---- make ----

func planMake(dir string) app.Recipe {
	r := app.Recipe{Ecosystem: "make"}
	m := makeTargets(dir)
	for _, t := range []struct {
		target string
		step   **app.RecipeStep
	}{{"build", &r.Build}, {"lint", &r.Lint}, {"test", &r.Test}} {
		if m[t.target] {
			*t.step = step(t.target, "make", t.target)
		}
	}
	if r.Test == nil && m["check"] {
		r.Test = step("test", "make", "check")
	}
	if r.Setup == nil && m["deps"] {
		s := setupStep("make", "deps")
		s.Optional = true
		r.Setup = []app.RecipeStep{s}
	}
	return r
}
