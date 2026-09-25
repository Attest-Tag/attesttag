package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Toolchains. The worker image carries a handful of runtimes at one version each, which is the
// wrong answer twice over: a repository that pins Node 18 got 22 and a repository written in
// anything not baked in got nothing at all. mise (github.com/jdx/mise) fixes both — it reads the
// version files repositories already commit (.tool-versions, mise.toml, .nvmrc, .python-version,
// .java-version, .ruby-version) and fetches what they ask for into a cache. So the image stays a
// base rather than a museum of every language, and pinning stops being a polite fiction.
//
// A repository can pin differently in different folders — web/ on Node 20, tools/ on Node 22,
// legacy/ on an old Python — so what goes on PATH is mise's shims, not any one version's bin
// directory: a shim picks the version the folder it runs in pins, for the checks and for every
// command the engine runs, whether it cds there first or not. A folder that pins nothing gets the
// job's defaults (below) or else the image's own.
//
// It is best-effort throughout: a toolchain that will not install leaves whatever the image has,
// and the package's checks say so rather than passing off one version as another.

// miseTools are the names mise knows that we are willing to install, mapped from the names the
// probes use. Anything else in a recipe's Tools is ignored rather than passed to a fetcher.
var miseTools = map[string]string{
	"node": "node", "python": "python", "java": "java", "ruby": "ruby", "go": "go",
	"rust": "rust", "erlang": "erlang", "elixir": "elixir", "dotnet": "dotnet",
	"php": "php", "bun": "bun", "deno": "deno", "swift": "swift", "dart": "dart",
	"gradle": "gradle", "maven": "maven", "terraform": "terraform",
}

// idiomaticTools is the tool list mise may read out of a repository's own version files. Go and
// Rust are left out: GOTOOLCHAIN=auto and rustup's proxies already switch per folder from go.mod
// and rust-toolchain.toml, and mise reading those files too put a second copy of each toolchain
// ahead of the one that knew how.
func idiomaticTools() []string {
	out := make([]string, 0, len(miseTools))
	for _, v := range miseTools {
		if v != "go" && v != "rust" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// toolchains is mise for one job.
type toolchains struct {
	bin, data, shims, repo string
	// basePath is the job's PATH before the shims went in front of it: what mise itself runs
	// with, since a shim calling mise calling a shim is a loop.
	basePath string
	// settings are what mise reads both when it installs and when a shim runs. They must agree:
	// otherwise `mise install` fetches one version and the shim resolves another.
	settings []string
	// active is whether the shims are on the job's PATH, which only happens once there is
	// something to shim.
	active bool
}

// newToolchains is nil when the image has no mise, and the baked toolchains are what there is.
func newToolchains(ws *Workspace, o Options) *toolchains {
	bin := nonEmptyStr(o.MiseBin, "mise")
	if _, err := exec.LookPath(bin); err != nil {
		return nil
	}
	data := filepath.Join(o.WorkDir, ".cache", "mise")
	cache := filepath.Join(o.WorkDir, ".cache", "mise-cache")
	os.MkdirAll(data, 0o755)
	os.MkdirAll(cache, 0o755)
	// mise runs as the sandbox user — a backend named in a repository's mise.toml fetches and runs
	// code, which is the repository's own code by any useful definition — and writes into both.
	grantSandbox(data, cache)
	return &toolchains{bin: bin, data: data, shims: filepath.Join(data, "shims"), repo: ws.RepoDir,
		basePath: envValue(ws.Env, "PATH"),
		settings: []string{
			"MISE_DATA_DIR=" + data, "MISE_CACHE_DIR=" + cache,
			// The clone's own configs are trusted without a prompt nobody can answer, and nothing
			// above the job directory is read at all.
			"MISE_TRUSTED_CONFIG_PATHS=" + ws.RepoDir, "MISE_CEILING_PATHS=" + ws.JobDir,
			// Recent mise ignores .nvmrc, .python-version and friends unless told which tools may
			// be read from them. Those files are exactly why mise is here.
			"MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS=" + strings.Join(idiomaticTools(), ","),
			"MISE_EXPERIMENTAL=1", "MISE_QUIET=1",
			// Never compile a runtime inside a job: a Python 3.5 or a Node 0.x with no prebuilt
			// build fails in a second instead of spending the job's twelve minutes finding out.
			"MISE_PYTHON_COMPILE=0", "MISE_NODE_COMPILE=0",
		}}
}

// installEnv is what mise runs with when it installs: the job's environment without the shims,
// the shared settings, and yes to every prompt.
func (t *toolchains) installEnv(ws *Workspace) []string {
	env := withEnv(ws.Env, "PATH="+t.basePath)
	return withEnv(env, append(append([]string{}, t.settings...), "MISE_YES=1")...)
}

// activate puts the shims in front of the job's PATH, with the settings a shim resolves by, once
// there is anything to shim. From here every step and the engine get the version their folder
// pins. A shim never installs: what could be installed was, before any code ran.
func (t *toolchains) activate(ws *Workspace) {
	if entries, err := os.ReadDir(t.shims); err != nil || len(entries) == 0 {
		return
	}
	env := withEnv(ws.Env, "PATH="+t.shims+string(os.PathListSeparator)+t.basePath)
	ws.Env = withEnv(env, append(append([]string{}, t.settings...),
		"MISE_NOT_FOUND_AUTO_INSTALL=0", "MISE_AUTO_INSTALL=0", "MISE_EXEC_AUTO_INSTALL=0")...)
	t.active = true
}

// run is one mise command in a folder, as the sandbox user, capped at until.
func (t *toolchains) run(ctx context.Context, ws *Workspace, dir string, until time.Time, args ...string) (procOut, error) {
	return runCmd(ctx, cmdSpec{Dir: dir, Argv: append([]string{t.bin}, args...), Env: t.installEnv(ws), Timeout: time.Until(until), Sandbox: true})
}

// provision installs what one package's folder pins and records what its commands will run on.
// For the primary it first makes the job's defaults: the tools an admin's or the repository's
// recipe names, and the floors of the primary's manifest ranges. Those apply wherever a folder
// pins nothing itself.
func (t *toolchains) provision(ctx context.Context, ws *Workspace, p *pkgRun, until time.Time) {
	if time.Until(until) < 30*time.Second {
		p.note("there was no time left to install the toolchains " + folderName(p.Dir) + " pins; it ran on the image's")
		return
	}
	dir := filepath.Join(ws.RepoDir, filepath.FromSlash(p.Dir))
	if p.Primary && p.Recipe != nil {
		for _, w := range wantedTools(p.Recipe.Tools) {
			ws.Reporter.Log("installing " + w)
			// `use --global` installs it and makes it the default, which `install` alone does not.
			// Global here means the job's own HOME, which is thrown away with the job.
			if out, err := t.run(ctx, ws, dir, until, "use", "--global", w); err != nil || out.Code != 0 {
				ws.Reporter.Log("could not install " + w + ": " + lastLines(out.Output, 300))
				p.note("could not install " + strings.Replace(w, "@", " ", 1) + ", which the recipe asks for; commands ran on the image's")
			}
		}
	}
	ws.Reporter.Log("installing the toolchains " + folderName(p.Dir) + " pins")
	if out, err := t.run(ctx, ws, dir, until, "install"); err != nil || out.Code != 0 {
		ws.Reporter.Log("mise install: " + lastLines(out.Output, 400))
	}
	listed, err := t.list(ctx, ws, dir, until)
	if err != nil {
		ws.Reporter.Log("mise ls: " + err.Error())
	}
	for _, tl := range listed {
		src := t.sourceLabel(tl.Source.Path)
		switch {
		case tl.Installed:
			p.Tools = append(p.Tools, fmt.Sprintf("%s %s (%s)", tl.Tool, tl.Version, src))
		case tl.Tool == "python":
			t.nearestPython(ctx, ws, p, dir, tl.Requested, src, until)
		default:
			p.note(fmt.Sprintf("%s pins %s %s, which could not be installed; commands there ran on the image's %s", src, tl.Tool, tl.Requested, tl.Tool))
		}
	}
	// A restored cache carries installs but not the shims (they point at the mise binary, outside
	// the cache), and a fresh install may have added a program. Cheap either way.
	if out, err := t.run(ctx, ws, dir, until, "reshim"); err != nil || out.Code != 0 {
		ws.Reporter.Log("mise reshim: " + lastLines(out.Output, 200))
	}
}

// nearestPython runs a folder whose pinned Python mise could not install on the nearest one uv
// can: that version when it exists at all, oldestPython when the pin is older than anything that
// does. uv fetches it from builds pinned by hash, into the job cache, and the package's own
// commands get it first on PATH — and in UV_PYTHON, since uv reads .python-version itself and
// refuses anything below 3.6 outright.
func (t *toolchains) nearestPython(ctx context.Context, ws *Workspace, p *pkgRun, dir, requested, src string, until time.Time) {
	want := majorMinor(requested)
	older := want == "" || versionLess(want, oldestPython)
	if older {
		want = oldestPython
	}
	env := withEnv(ws.Env, "PATH="+t.basePath)
	out, err := runCmd(ctx, cmdSpec{Dir: dir, Argv: []string{"uv", "python", "install", want}, Env: env, Timeout: time.Until(until), Sandbox: true})
	if err == nil && out.Code == 0 {
		out, err = runCmd(ctx, cmdSpec{Dir: dir, Argv: []string{"uv", "python", "find", want}, Env: env, Timeout: time.Minute, Sandbox: true})
	}
	interp := strings.TrimSpace(lastLines(out.Output, 400))
	if i := strings.LastIndex(interp, "\n"); i >= 0 {
		interp = interp[i+1:]
	}
	if err != nil || out.Code != 0 || !filepath.IsAbs(interp) {
		ws.Reporter.Log("uv python install " + want + ": " + lastLines(out.Output, 300))
		p.note(fmt.Sprintf("%s pins Python %s, which could not be installed; its checks ran on the image's Python", src, requested))
		return
	}
	version := want
	if v, err := runCmd(ctx, cmdSpec{Dir: dir, Argv: []string{interp, "--version"}, Env: env, Timeout: time.Minute, Sandbox: true}); err == nil && v.Code == 0 {
		version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v.Output), "Python"))
	}
	p.PathPrefix = append(p.PathPrefix, filepath.Dir(interp))
	p.Prefix = append(p.Prefix, "UV_PYTHON="+want)
	p.Tools = append(p.Tools, fmt.Sprintf("python %s (%s asks for %s)", version, src, requested))
	if older {
		p.OldPython = version
		p.note(fmt.Sprintf("%s pins Python %s, which the worker cannot provide (nothing older than %s can be obtained); its checks ran on Python %s, and a failure caused only by that difference is not the change's",
			src, requested, oldestPython, version))
	}
}

// miseTool is one line of `mise ls --current --json`: a tool a folder resolves, and whether it
// is there.
type miseTool struct {
	Tool      string `json:"-"`
	Version   string `json:"version"`
	Requested string `json:"requested_version"`
	Installed bool   `json:"installed"`
	Source    struct {
		Type string `json:"type"`
		Path string `json:"path"`
	} `json:"source"`
}

func (t *toolchains) list(ctx context.Context, ws *Workspace, dir string, until time.Time) ([]miseTool, error) {
	out, err := runCmd(ctx, cmdSpec{Dir: dir, Argv: []string{t.bin, "ls", "--current", "--json"}, Env: t.installEnv(ws), Timeout: min(time.Minute, time.Until(until)), Head: 1 << 20, Sandbox: true})
	if err != nil || out.Code != 0 {
		return nil, fmt.Errorf("%v %s", err, lastLines(out.Output, 200))
	}
	return parseMiseList(out.Output)
}

// parseMiseList reads `mise ls --current --json`: {"node": [{"version": …, "installed": …}]},
// sorted by tool. Anything before the opening brace (a warning mise printed) is skipped.
func parseMiseList(raw string) ([]miseTool, error) {
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	var m map[string][]miseTool
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	var out []miseTool
	for tool, versions := range m {
		for _, v := range versions {
			v.Tool = tool
			if v.Requested == "" {
				v.Requested = v.Version
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out, nil
}

// sourceLabel says where a pin came from in the words a reviewer uses: the file in the
// repository, or the job's defaults for one that came from the recipe or a manifest's range.
func (t *toolchains) sourceLabel(path string) string {
	if rel, err := filepath.Rel(t.repo, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && path != "" {
		return filepath.ToSlash(rel)
	}
	return "the job's default"
}

// wantedTools are the recipe's tools as mise names them, version-checked, in a stable order.
func wantedTools(tools map[string]string) []string {
	var wanted []string
	for name, version := range tools {
		tool, ok := miseTools[strings.ToLower(strings.TrimSpace(name))]
		if !ok || !safeVersion(version) {
			continue
		}
		wanted = append(wanted, tool+"@"+strings.TrimSpace(version))
	}
	sort.Strings(wanted)
	return wanted
}

func folderName(dir string) string {
	if dir == "" || dir == "." {
		return "the repository root"
	}
	return dir + "/"
}

// safeVersion keeps a version string to what a version can look like, so nothing from a
// repository's own file reaches a command line as an option or a path.
func safeVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 32 || strings.HasPrefix(v, "-") {
		return false
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func withPathPrefix(env, paths []string) []string {
	if len(paths) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+strings.Join(paths, string(os.PathListSeparator))+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ---- services ----

// servicesMissing names the services a recipe asks for that this worker cannot provide. A
// sidecar sets ATTEST_SERVICE_<NAME> to its connection string; without one the honest answer is
// that the suite cannot run here, which is a different sentence from "no tests were found" and
// belongs in the pull request as such.
func servicesMissing(services []string, ws *Workspace) string {
	var missing []string
	for _, s := range services {
		name := strings.ToUpper(strings.NewReplacer(":", "_", "-", "_", ".", "_", "/", "_").Replace(strings.TrimSpace(s)))
		if name == "" {
			continue
		}
		bare := name
		if i := strings.Index(name, "_"); i > 0 {
			bare = name[:i]
		}
		if serviceEnv(ws.Env, "ATTEST_SERVICE_"+name) || serviceEnv(ws.Env, "ATTEST_SERVICE_"+bare) {
			continue
		}
		missing = append(missing, s)
	}
	return strings.Join(missing, ", ")
}

func serviceEnv(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") && len(kv) > len(key)+1 {
			return true
		}
	}
	return os.Getenv(key) != ""
}
