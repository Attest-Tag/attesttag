package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"attesttag/internal/app"
)

// Toolchains. The worker image carries a handful of runtimes at one version each, which is the
// wrong answer twice over: a repository that pins Node 18 got 22 and a repository written in
// anything not baked in got nothing at all. mise (github.com/jdx/mise) fixes both — it reads the
// version files repositories already commit (.tool-versions, .nvmrc, .python-version,
// rust-toolchain.toml, .java-version) and fetches what they ask for into a cache. So the image
// stays a base rather than a museum of every language, and pinning stops being a polite fiction.
//
// It is best-effort throughout: a toolchain that will not install leaves whatever the image has,
// and the step that needed it is dropped with its reason rather than failing the job.

// miseTools are the names mise knows that we are willing to install, mapped from the names the
// probes use. Anything else in a recipe's Tools is ignored rather than passed to a fetcher.
var miseTools = map[string]string{
	"node": "node", "python": "python", "java": "java", "ruby": "ruby", "go": "go",
	"rust": "rust", "erlang": "erlang", "elixir": "elixir", "dotnet": "dotnet",
	"php": "php", "bun": "bun", "deno": "deno", "swift": "swift", "dart": "dart",
	"gradle": "gradle", "maven": "maven", "terraform": "terraform",
}

// idiomaticTools is the tool list mise may read out of a repository's own version files.
func idiomaticTools() []string {
	out := make([]string, 0, len(miseTools))
	for _, v := range miseTools {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// provisionTools installs what the recipe pins and puts the results on the workspace's PATH.
func provisionTools(ctx context.Context, ws *Workspace, r *app.Recipe, o Options) error {
	bin := o.MiseBin
	if bin == "" {
		bin = "mise"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil // no mise in this image: the baked toolchains are what there is
	}
	dir := filepath.Join(ws.RepoDir, filepath.FromSlash(nonEmptyStr(r.Workdir, ".")))
	// mise runs in the clone and does what the clone asks: .tool-versions and mise.toml are the
	// repository's files, MISE_YES=1 answers the trust prompt for them, and a backend named in
	// one fetches and runs code. That is the repository's own code by any useful definition, so
	// it runs as the sandbox user like the rest of it — the data and cache directories are made
	// and handed over first, because mise writes into them and HOME.
	dataDir := filepath.Join(o.WorkDir, ".cache", "mise")
	cacheDir := filepath.Join(o.WorkDir, ".cache", "mise-cache")
	os.MkdirAll(dataDir, 0o755)
	os.MkdirAll(cacheDir, 0o755)
	grantSandbox(dataDir, cacheDir)
	env := append(append([]string{}, ws.Env...),
		"MISE_DATA_DIR="+dataDir,
		"MISE_CACHE_DIR="+cacheDir,
		"MISE_YES=1", "MISE_QUIET=1", "MISE_EXPERIMENTAL=1",
		// Recent mise ignores .nvmrc, .python-version, .ruby-version and friends unless it is
		// told which tools may be read from them. Those files are exactly why mise is here —
		// they are what repositories already commit — so every tool we are willing to install
		// is named. Without this line `mise install` quietly does nothing at all.
		"MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS="+strings.Join(idiomaticTools(), ","))
	var wanted []string
	for name, version := range r.Tools {
		tool, ok := miseTools[strings.ToLower(strings.TrimSpace(name))]
		if !ok || !safeVersion(version) {
			continue
		}
		wanted = append(wanted, tool+"@"+strings.TrimSpace(version))
	}
	sort.Strings(wanted)
	// A repository that ships its own version file is authoritative; `mise install` with no
	// arguments is exactly "install what this directory asks for".
	hasVersionFile := exists(dir, ".tool-versions", ".mise.toml", "mise.toml", ".mise/config.toml")
	if !hasVersionFile && len(wanted) == 0 {
		return nil
	}
	cap := 12 * time.Minute
	if left := time.Until(ws.Deadline); left < cap {
		cap = left / 3
	}
	if cap < time.Minute {
		return nil
	}
	if hasVersionFile {
		ws.Reporter.Log("installing the toolchains this repository pins")
		if out, err := runCmd(ctx, cmdSpec{Dir: dir, Argv: []string{bin, "install"}, Env: env, Timeout: cap, Sandbox: true}); err != nil || out.Code != 0 {
			ws.Reporter.Log("mise install: " + lastLines(out.Output, 400))
		}
	}
	for _, w := range wanted {
		ws.Reporter.Log("installing " + w)
		// `use --global` installs it and makes it the active version, which `install` alone does
		// not: a tool that is installed but not active does not appear on bin-paths and so never
		// reaches the PATH the steps run with. Global here means the job's own HOME, which is
		// thrown away with the job — it never writes into the repository.
		out, err := runCmd(ctx, cmdSpec{Dir: dir, Argv: []string{bin, "use", "--global", w}, Env: env, Timeout: cap, Sandbox: true})
		if err != nil || out.Code != 0 {
			ws.Reporter.Log("could not install " + w + ": " + lastLines(out.Output, 300))
		}
	}
	// Whatever ended up installed goes on PATH ahead of the image's own, so `node` in a test
	// script is the version the repository asked for.
	out, err := runCmd(ctx, cmdSpec{Dir: dir, Argv: []string{bin, "bin-paths"}, Env: env, Timeout: time.Minute, Sandbox: true})
	if err != nil || out.Code != 0 {
		return fmt.Errorf("mise bin-paths: %s", lastLines(out.Output, 200))
	}
	var paths []string
	for _, line := range strings.Split(out.Output, "\n") {
		if line = strings.TrimSpace(line); line != "" && filepath.IsAbs(line) {
			paths = append(paths, line)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	ws.Env = withPathPrefix(ws.Env, paths)
	ws.Reporter.Log("toolchains ready: " + strings.Join(shortPaths(paths), ", "))
	return nil
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
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+strings.Join(paths, ":")+":"+strings.TrimPrefix(kv, "PATH="))
			continue
		}
		out = append(out, kv)
	}
	return out
}

func shortPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) >= 3 {
			out = append(out, strings.Join(parts[len(parts)-3:len(parts)-1], "/"))
			continue
		}
		out = append(out, p)
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
