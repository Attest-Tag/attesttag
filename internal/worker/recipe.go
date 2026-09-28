package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"attesttag/internal/app"

	"gopkg.in/yaml.v3"
)

// How the worker learns to build and test a repository it has never seen. Three sources, in
// order of how much the answer is worth trusting:
//
//  1. .attest/recipe.yaml committed in the repository — the people who own the code said so;
//  2. the recipe an admin set on the repository connection in the console;
//  3. detection from the marker files in the clone.
//
// The first two are merged over the third rather than replacing it, so a repository that only
// wants to correct the test command still gets the toolchains and the dependency install worked
// out for it. What comes out is one app.Recipe, which travels into the engine's brief, the pull
// request body, the Slack report and back onto the connection for next time.

// recipeFiles are looked for in the repository root, in order.
var recipeFiles = []string{".attest/recipe.yaml", ".attest/recipe.yml", ".attest/recipe.json", ".attest.yaml"}

// resolveRecipe produces the recipe this job will run. It never returns nil: a repository
// nothing matched comes back with Source none and Why saying so, which is what the pull request
// prints instead of pretending a suite passed.
func resolveRecipe(root string, conn *app.Recipe, legacyTestCmd string, hints []string) *app.Recipe {
	base := detectRecipe(root, hints)
	if r, err := readRepoRecipe(root); err != nil {
		base.Why = strings.TrimSpace(base.Why + " (" + filepath.Base(recipeFiles[0]) + " ignored: " + err.Error() + ")")
	} else if r != nil {
		return finish(mergeRecipe(base, r, app.RecipeSourceRepoFile), root)
	}
	if over := connectionRecipe(conn, legacyTestCmd); over != nil {
		return finish(mergeRecipe(base, over, app.RecipeSourceConnection), root)
	}
	return finish(base, root)
}

// mergeRecipe lays an authored recipe over a detected one: every field the author set wins, and
// everything they left out keeps what detection worked out.
func mergeRecipe(base, over *app.Recipe, source string) *app.Recipe {
	out := *base
	out.Source = source
	if over.Ecosystem != "" {
		out.Ecosystem = over.Ecosystem
	}
	if over.Workdir != "" {
		out.Workdir = over.Workdir
	}
	if len(over.Tools) > 0 {
		if out.Tools == nil {
			out.Tools = map[string]string{}
		}
		for k, v := range over.Tools {
			out.Tools[k] = v
		}
	}
	if len(over.Setup) > 0 {
		out.Setup = over.Setup
	}
	for _, f := range []struct{ dst, src **app.RecipeStep }{{&out.Build, &over.Build}, {&out.Lint, &over.Lint}, {&out.Test, &over.Test}} {
		if *f.src != nil {
			*f.dst = *f.src
		}
	}
	if len(over.Services) > 0 {
		out.Services = over.Services
	}
	out.Why = ""
	return &out
}

// finish is the shape pass over any recipe, whoever wrote it: directories must exist and be
// inside the clone, and commands must be programs rather than shell lines. It deliberately does
// NOT decide whether a program is installed — pruneMissing does that later, because the
// toolchains a repository pins are fetched in between, and a step dropped before mise ran would
// be a step dropped for a program that was about to exist.
func finish(r *app.Recipe, root string) *app.Recipe {
	if r == nil {
		r = &app.Recipe{Source: app.RecipeSourceNone}
	}
	if r.Workdir == "" {
		r.Workdir = "."
	}
	var dropped []string
	for i := range r.Setup {
		r.Setup[i].Name = nonEmptyStr(r.Setup[i].Name, "install")
	}
	r.Setup = keepValid(r.Setup, root, r.Workdir, &dropped, nil)
	for _, f := range []struct {
		label string
		step  **app.RecipeStep
	}{{"build", &r.Build}, {"lint", &r.Lint}, {"test", &r.Test}} {
		st := *f.step
		if st == nil {
			continue
		}
		st.Name = nonEmptyStr(st.Name, f.label)
		if kept := keepValid([]app.RecipeStep{*st}, root, r.Workdir, &dropped, nil); len(kept) == 0 {
			*f.step = nil
		} else {
			*f.step = &kept[0]
		}
	}
	noteWhy(r, dropped)
	return r
}

// pruneMissing drops the steps whose program nothing on this PATH provides, naming the program.
// It runs after the toolchains are installed and against the environment the steps will actually
// run in, not the worker's own — which is the difference between "this repository needs Ruby"
// and "this repository's suite failed".
func pruneMissing(r *app.Recipe, root string, env []string) {
	if r == nil {
		return
	}
	path := envValue(env, "PATH")
	var dropped []string
	r.Setup = keepValid(r.Setup, root, r.Workdir, &dropped, &path)
	for _, step := range []**app.RecipeStep{&r.Build, &r.Lint, &r.Test} {
		if *step == nil {
			continue
		}
		if kept := keepValid([]app.RecipeStep{**step}, root, r.Workdir, &dropped, &path); len(kept) == 0 {
			*step = nil
		}
	}
	noteWhy(r, dropped)
}

// noteWhy is the sentence the pull request and the Slack report print when a recipe could not do
// everything it meant to, or could do nothing at all.
func noteWhy(r *app.Recipe, dropped []string) {
	switch {
	case len(dropped) > 0 && r.Why == "":
		r.Why = strings.Join(dropped, "; ")
	case len(dropped) > 0:
		r.Why += "; " + strings.Join(dropped, "; ")
	case !r.Runnable() && r.Why == "":
		r.Why = "no build or test command was found for this repository, and none is set on the connection or in .attest/recipe.yaml"
	}
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return os.Getenv(key)
}

// keepValid drops the steps that cannot run and says why. A nil path means the question of
// whether the program exists is not being asked yet.
func keepValid(steps []app.RecipeStep, root, workdir string, dropped *[]string, path *string) []app.RecipeStep {
	out := steps[:0:0]
	for _, s := range steps {
		if err := validateStep(&s, root, workdir); err != nil {
			*dropped = append(*dropped, fmt.Sprintf("%q was not run: %s", s.String(), err))
			continue
		}
		if path != nil {
			if tool := stepMissing(&s, root, workdir, *path); tool != "" {
				*dropped = append(*dropped, fmt.Sprintf("%q needs %s, which is not installed in the worker", s.String(), tool))
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// validateStep is the shape check every step passes, including one a repository committed. A
// recipe file is not a new trust boundary — the repository's own test code already runs here —
// but a step still may not name a shell, escape the clone, or arrive empty.
func validateStep(s *app.RecipeStep, root, workdir string) error {
	if len(s.Argv) == 0 || strings.TrimSpace(s.Argv[0]) == "" {
		return fmt.Errorf("the command is empty")
	}
	for _, a := range s.Argv {
		if strings.ContainsAny(a, "\n\r\x00") {
			return fmt.Errorf("the command contains a newline")
		}
	}
	if strings.ContainsAny(s.Argv[0], "|&;<>`$(){}*?") {
		return fmt.Errorf("the command must be a program, not a shell line")
	}
	dir := s.Dir
	if dir == "" {
		dir = workdir
	}
	abs := filepath.Join(root, filepath.FromSlash(dir))
	if rel, err := filepath.Rel(root, abs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("its directory is outside the repository")
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("the directory %s does not exist", dir)
	}
	return nil
}

// stepMissing names the program a step starts with when nothing on PATH provides it and the
// repository does not ship it. A path with a separator (./gradlew, .venv/bin/python) is made by
// the clone or the install step, so it is looked for relative to where the step runs.
func stepMissing(s *app.RecipeStep, root, workdir, path string) string {
	tool := s.Argv[0]
	dir := s.Dir
	if dir == "" {
		dir = workdir
	}
	if strings.ContainsAny(tool, "/\\") {
		// Made by an install step that has not run yet: present or not, it is not a missing
		// toolchain, so it is never dropped here.
		return ""
	}
	// Looked up against the PATH the step will run with, which — after mise has installed what
	// the repository pins — is not the worker's own.
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(d, tool)); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return ""
		}
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), tool)); err == nil {
		return ""
	}
	return tool
}

// ---- the repository's own recipe ----

// readRepoRecipe reads .attest/recipe.yaml (or .yml/.json) from the clone. Missing is not an
// error; malformed is, and the reason reaches the pull request so whoever wrote it finds out.
func readRepoRecipe(root string) (*app.Recipe, error) {
	for _, name := range recipeFiles {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		if len(raw) > 64<<10 {
			return nil, fmt.Errorf("%s is over 64 KB", name)
		}
		var f recipeFile
		if err := yaml.Unmarshal(raw, &f); err != nil { // yaml.v3 reads JSON too
			return nil, fmt.Errorf("%s is not valid YAML: %v", name, err)
		}
		r, err := f.recipe()
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		return r, nil
	}
	return nil, nil
}

// recipeFile is the on-disk shape, which is friendlier than app.Recipe: a step may be written
// as a plain string ("go test ./...") or as a mapping with a directory and a timeout.
type recipeFile struct {
	Ecosystem string            `yaml:"ecosystem"`
	Workdir   string            `yaml:"workdir"`
	Tools     map[string]string `yaml:"tools"`
	Setup     []stepFile        `yaml:"setup"`
	Build     *stepFile         `yaml:"build"`
	Lint      *stepFile         `yaml:"lint"`
	Test      *stepFile         `yaml:"test"`
	Services  []string          `yaml:"services"`
}

type stepFile struct {
	Run      string   `yaml:"run"`
	Argv     []string `yaml:"argv"`
	Dir      string   `yaml:"dir"`
	TimeoutS int      `yaml:"timeout_s"`
	Optional bool     `yaml:"optional"`
	plain    string
}

func (s *stepFile) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.plain = n.Value
		return nil
	}
	type raw stepFile
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*s = stepFile(r)
	return nil
}

func (s *stepFile) step(name string) (*app.RecipeStep, error) {
	if s == nil {
		return nil, nil
	}
	argv := s.Argv
	if len(argv) == 0 {
		line := strings.TrimSpace(nonEmptyStr(s.plain, s.Run))
		if line == "" {
			return nil, nil
		}
		var err error
		if argv, err = app.SplitCommand(line); err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
	}
	if len(argv) == 0 {
		return nil, nil
	}
	return &app.RecipeStep{Name: name, Argv: argv, Dir: s.Dir, TimeoutS: s.TimeoutS, Optional: s.Optional}, nil
}

func (f recipeFile) recipe() (*app.Recipe, error) {
	r := &app.Recipe{Ecosystem: f.Ecosystem, Workdir: f.Workdir, Tools: f.Tools, Services: f.Services}
	for i := range f.Setup {
		s, err := f.Setup[i].step(fmt.Sprintf("install %d", i+1))
		if err != nil {
			return nil, err
		}
		if s != nil {
			r.Setup = append(r.Setup, *s)
		}
	}
	var err error
	if r.Build, err = f.Build.step("build"); err != nil {
		return nil, err
	}
	if r.Lint, err = f.Lint.step("lint"); err != nil {
		return nil, err
	}
	if r.Test, err = f.Test.step("test"); err != nil {
		return nil, err
	}
	return r, nil
}

// ---- the connection's recipe ----

// connectionRecipe is what an admin set in the console: a structured recipe, or the older
// single test command, which still means exactly what it used to. A recipe an earlier job
// worked out and the bot remembered is not a decision — it is about whatever package that job
// was about — so it never overrides what this job detects. Current bots do not send one; this
// is for the ones that still do.
func connectionRecipe(c *app.Recipe, legacyTestCmd string) *app.Recipe {
	if c.SetByAdmin() && (c.Runnable() || len(c.Setup) > 0 || len(c.Tools) > 0 || c.Workdir != "") {
		out := *c
		return &out
	}
	line := strings.TrimSpace(legacyTestCmd)
	if line == "" {
		return nil
	}
	argv, err := app.SplitCommand(line)
	if err != nil || len(argv) == 0 {
		return nil
	}
	return &app.Recipe{Test: &app.RecipeStep{Name: "test", Argv: argv}}
}

func nonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// ---- detection ----

// detectRecipe works the repository out from its marker files: which package directory the job
// is about, which ecosystems live there, and what each one's install, build, lint and test
// commands are. A repository can be more than one — a Go service with a TypeScript console is
// the common case — so every ecosystem found contributes its install step, and the primary one
// contributes the commands.
func detectRecipe(root string, hints []string) *app.Recipe {
	workdir := pickWorkdir(root, hints)
	if workdir != "." && len(ecosystemsIn(filepath.Join(root, filepath.FromSlash(workdir)))) == 0 {
		workdir = "." // the package directory declares nothing; try the root
	}
	r := detectAt(root, workdir)
	if r.Source == app.RecipeSourceNone {
		if pkgs := packageDirs(root, 3); len(pkgs) > 1 {
			r.Why = fmt.Sprintf("this looks like a monorepo (%s) and nothing said which package the change is in; name a file in the brief, or set workdir in .attest/recipe.yaml",
				strings.Join(pkgs, ", "))
		}
	}
	return r
}

// detectAt works out one package directory: which ecosystems it declares, what each installs,
// and the primary one's build, lint and test commands.
func detectAt(root, workdir string) *app.Recipe {
	dir := filepath.Join(root, filepath.FromSlash(workdir))
	found := ecosystemsIn(dir)
	r := &app.Recipe{Source: app.RecipeSourceDetected, Workdir: workdir, Tools: map[string]string{}}
	if len(found) == 0 {
		r.Source = app.RecipeSourceNone
		r.Why = "no marker file for any ecosystem the worker knows (" + strings.Join(probeNames(), ", ") + ")"
		return r
	}
	// Every ecosystem present installs; the first contributes the commands and names the recipe.
	for _, p := range found {
		plan := p.plan(dir)
		r.Setup = append(r.Setup, plan.Setup...)
		for k, v := range plan.Tools {
			r.Tools[k] = v
		}
	}
	primary := found[0].plan(dir)
	r.Ecosystem, r.Build, r.Lint, r.Test = primary.Ecosystem, primary.Build, primary.Lint, primary.Test
	if r.Ecosystem == "" {
		r.Ecosystem = found[0].name
	}
	// A Makefile that names the target wins: a repository with `make test` has said how it is
	// tested more plainly than any marker file, and the install steps above still apply.
	if m := makeTargets(dir); len(m) > 0 {
		for _, t := range []struct {
			target string
			step   **app.RecipeStep
		}{{"build", &r.Build}, {"lint", &r.Lint}, {"test", &r.Test}} {
			if m[t.target] {
				*t.step = &app.RecipeStep{Name: t.target, Argv: []string{"make", t.target}}
			}
		}
	}
	if len(r.Tools) == 0 {
		r.Tools = nil
	}
	return r
}

// pickWorkdir is what makes a monorepo work: the package the job is about is the nearest
// directory at or above the files the brief pointed at that declares an ecosystem. With no
// hints, the root when it declares one, else the only child that does.
func pickWorkdir(root string, hints []string) string {
	for _, h := range hints {
		if dir := packageOf(root, h); dir != "" {
			return dir
		}
	}
	if len(ecosystemsIn(root)) > 0 {
		return "."
	}
	// A repository whose root declares nothing is usually a monorepo whose packages sit a level
	// or two down — dart-lang/http keeps them under pkgs/, and plenty of repositories keep them
	// under packages/ or services/. One candidate is the answer; several with no hint to choose
	// between them is not, and the root is the honest place to stand.
	if found := packageDirs(root, 3); len(found) == 1 {
		return found[0]
	}
	return "."
}

// packageOf is the package a path in the repository belongs to: the nearest directory at or
// above it that declares an ecosystem — the path itself when it names such a directory — or ""
// when nothing up to the root does. A path that is empty, absolute or climbs out of the clone
// belongs to nothing. A deleted file still belongs to the package around where it was.
func packageOf(root, rel string) string {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		return ""
	}
	dir := filepath.FromSlash(strings.TrimSuffix(rel, "/"))
	if st, err := os.Stat(filepath.Join(root, dir)); err != nil || !st.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		abs := filepath.Join(root, dir)
		if r, err := filepath.Rel(root, abs); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return ""
		}
		if len(ecosystemsIn(abs)) > 0 {
			return filepath.ToSlash(dir)
		}
		if dir == "." || dir == string(filepath.Separator) {
			return ""
		}
		dir = filepath.Dir(dir)
	}
}

// isAPackage is whether a package directory is one worth checking on its own: none of its path
// is documentation, examples, tooling or a dependency tree (notAPackage, skipDir), or hidden.
func isAPackage(dir string) bool {
	if dir == "." {
		return true
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || notAPackage[seg] || skipDir[seg] {
			return false
		}
	}
	return true
}

// notAPackage are directories that carry their own tooling but are never what a repository is.
// jq is a C project whose only marker file anywhere is docs/Pipfile: without this, packageDirs
// found exactly one candidate, pickWorkdir stood in docs/, and a job would have compiled the
// documentation's Python and reported it as the project's build — green, and about nothing the
// change touched. A directory here is skipped only when looking for the package a change belongs
// to; the repository map the engine reads still shows it, because reading the docs is useful even
// when building them is not.
var notAPackage = map[string]bool{
	"docs": true, "doc": true, "documentation": true, "website": true, "site": true, "www": true,
	"examples": true, "example": true, "samples": true, "sample": true, "demo": true, "demos": true,
	"benchmarks": true, "benchmark": true, "scripts": true, "tools": true, "contrib": true,
	"third_party": true, "thirdparty": true, "testdata": true, "fixtures": true,
}

// packageDirs lists the directories within depth that declare an ecosystem, skipping the ones
// nothing useful lives in and the ones that are never the project itself. It stops descending once a directory declares one: a package's own
// subdirectories are that package, not siblings of it.
func packageDirs(root string, depth int) []string {
	var out []string
	var walk func(dir string, left int)
	walk = func(dir string, left int) {
		if left == 0 || len(out) > 8 {
			return
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || skipDir[e.Name()] || notAPackage[e.Name()] {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			if len(ecosystemsIn(sub)) > 0 {
				rel, err := filepath.Rel(root, sub)
				if err == nil {
					out = append(out, filepath.ToSlash(rel))
				}
				continue
			}
			walk(sub, left-1)
		}
	}
	walk(root, depth)
	sort.Strings(out)
	return out
}

func probeNames() []string {
	out := make([]string, 0, len(probes))
	for _, p := range probes {
		out = append(out, p.name)
	}
	return out
}

// ecosystemsIn lists the ecosystems whose marker files sit in one directory, most specific
// first (a lockfile beats a manifest, a manifest beats a stray source file).
func ecosystemsIn(dir string) []probe {
	var out []probe
	for _, p := range probes {
		if p.match(dir) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank < out[j].rank })
	return out
}

// A probe is one ecosystem: how to recognise it and what to run.
type probe struct {
	name    string
	rank    int      // lower wins when a directory declares several
	markers []string // any of these, glob-matched
	when    func(dir string) bool
	plan    func(dir string) app.Recipe
}

func (p probe) match(dir string) bool {
	if p.when != nil && !p.when(dir) {
		return false
	}
	for _, m := range p.markers {
		if strings.ContainsAny(m, "*?") {
			if hits, _ := filepath.Glob(filepath.Join(dir, m)); len(hits) > 0 {
				return true
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

func step(name string, argv ...string) *app.RecipeStep {
	return &app.RecipeStep{Name: name, Argv: argv}
}

func setupStep(argv ...string) app.RecipeStep {
	return app.RecipeStep{Name: "install", Argv: argv}
}
