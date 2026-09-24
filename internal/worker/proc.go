package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Every subprocess — git, installers, tests, the engine — runs through runCmd: its own process
// group (so a cancel or timeout ends the whole tree), a per-command timeout, a capped capture,
// and only the environment it was given. The repository token and the model key are never in
// that environment unless a step passes them in on purpose.

type cmdSpec struct {
	Dir     string
	Argv    []string // Argv[0] is the program; used when Shell is empty
	Shell   string   // a shell command line, run with sh -c
	Env     []string
	Timeout time.Duration
	Stdin   string
	// Capture caps; zero means the defaults (4 KB head, 60 KB tail).
	Head, Tail int
	// OnLine receives each stdout line as it arrives, for engines that stream JSON.
	OnLine func(line string)
	// Sandbox runs the command as the sandbox user when one is configured: the parts of a job
	// that execute a repository's own code — installs, tests, the engine — must not be able to
	// read the worker's environment or the token git carries.
	Sandbox bool
}

type procOut struct {
	Code     int
	Output   string // combined stdout+stderr, head+tail
	TimedOut bool
	Duration time.Duration
}

var errTimeout = errors.New("timed out")

func runCmd(ctx context.Context, spec cmdSpec) (procOut, error) {
	var out procOut
	if spec.Timeout <= 0 {
		spec.Timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	var cmd *exec.Cmd
	if spec.Shell != "" {
		cmd = exec.CommandContext(cctx, "/bin/sh", "-c", spec.Shell)
	} else {
		if len(spec.Argv) == 0 {
			return out, errors.New("empty command")
		}
		cmd = exec.CommandContext(cctx, spec.Argv[0], spec.Argv[1:]...)
		// exec.Command resolves a bare program name against the PATH of *this* process, not
		// against cmd.Env — so a toolchain mise installed for the job and put on the job's PATH
		// was found only if the worker's own image happened to have one by the same name, and
		// quietly ignored otherwise. A repository pinning Node 20 got the image's Node 22 and
		// nothing said so. Resolve it against the environment the command is actually given.
		if prog, ok := lookPathIn(spec.Argv[0], spec.Env); ok {
			cmd.Path, cmd.Err = prog, nil
		}
	}
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	head, tail := spec.Head, spec.Tail
	if head <= 0 {
		head = 4 << 10
	}
	if tail <= 0 {
		tail = 60 << 10
	}
	buf := newTailBuffer(head, tail)
	if spec.OnLine != nil {
		lw := &lineWriter{fn: spec.OnLine, mirror: buf}
		cmd.Stdout = lw
		defer lw.flush()
	} else {
		cmd.Stdout = buf
	}
	cmd.Stderr = buf
	setProcessGroup(cmd)
	if spec.Sandbox {
		applySandbox(cmd)
	}
	cmd.Cancel = func() error { killProcessGroup(cmd, false); return nil }
	cmd.WaitDelay = 10 * time.Second
	start := time.Now()
	err := cmd.Run()
	out.Duration = time.Since(start)
	out.Output = buf.String()
	if cmd.ProcessState != nil {
		out.Code = cmd.ProcessState.ExitCode()
	}
	if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		out.TimedOut = true
		if out.Code == 0 {
			out.Code = -1
		}
		return out, errTimeout
	}
	if ctx.Err() != nil {
		if out.Code == 0 {
			out.Code = -1
		}
		return out, context.Cause(ctx)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, nil // a non-zero exit is a result, not a failure to run
		}
		return out, err
	}
	return out, nil
}

// lookPathIn finds a program on the PATH of the environment a command will be given. A name
// with a separator is left alone: the OS resolves it against the command's own directory, which
// is how .venv/bin/python and ./gradlew are meant to work.
func lookPathIn(name string, env []string) (string, bool) {
	if name == "" || strings.ContainsRune(name, os.PathSeparator) {
		return "", false
	}
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	if path == "" {
		return "", false
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// lineWriter splits a stream into lines for OnLine and mirrors everything into the capture.
type lineWriter struct {
	fn     func(string)
	mirror *tailBuffer
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mirror.Write(p)
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.fn(line)
		}
	}
	if len(w.buf) > 4<<20 { // a line that never ends is not a line
		w.buf = w.buf[len(w.buf)-1<<20:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if line := strings.TrimSpace(string(w.buf)); line != "" {
		w.fn(line)
	}
	w.buf = nil
}

// baseEnv is what every subprocess gets: a PATH, a private HOME and TMPDIR inside the job dir,
// caches inside the work dir, and the flags that keep tools quiet and non-interactive.
func baseEnv(jobDir, workDir string) []string {
	path := env("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	for _, extra := range []string{"/usr/local/go/bin", "/usr/local/cargo/bin"} {
		if !strings.Contains(path, extra) {
			path += ":" + extra
		}
	}
	cache := filepath.Join(workDir, ".cache")
	e := []string{
		"PATH=" + path,
		"HOME=" + filepath.Join(jobDir, "home"),
		"TMPDIR=" + filepath.Join(jobDir, "tmp"),
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TERM=dumb", "CI=1", "NO_COLOR=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ASKPASS=/bin/true",
		"PYTHONDONTWRITEBYTECODE=1", "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1",
		"UV_CACHE_DIR=" + filepath.Join(cache, "uv"), "UV_NO_PROGRESS=1",
		"npm_config_cache=" + filepath.Join(cache, "npm"), "npm_config_fund=false", "npm_config_audit=false", "npm_config_update_notifier=false",
		"GOMODCACHE=" + filepath.Join(cache, "go", "mod"), "GOCACHE=" + filepath.Join(cache, "go", "build"), "GOFLAGS=-buildvcs=false", "GOTOOLCHAIN=auto",
		"CARGO_HOME=" + filepath.Join(cache, "cargo"), "CARGO_TERM_COLOR=never", "CARGO_INCREMENTAL=0",
	}
	for _, k := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR", "GOPATH", "GOROOT", "RUSTUP_HOME", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY",
		"JAVA_HOME", "DOTNET_CLI_TELEMETRY_OPTOUT", "MISE_DATA_DIR"} {
		if v, ok := lookupEnv(k); ok {
			e = append(e, k+"="+v)
		}
	}
	// Service endpoints a sidecar published (ATTEST_SERVICE_POSTGRES=...), which is how a suite
	// that needs a database finds one. Nothing else from the worker's own environment crosses.
	for _, kv := range environ() {
		if strings.HasPrefix(kv, "ATTEST_SERVICE_") {
			e = append(e, kv)
		}
	}
	// Caches for the toolchains the image does not bake in, alongside the ones it does.
	for k, sub := range map[string]string{
		"GRADLE_USER_HOME": "gradle", "MAVEN_OPTS_DIR": "maven", "COMPOSER_HOME": "composer",
		"BUNDLE_PATH": "bundle", "NUGET_PACKAGES": "nuget", "PUB_CACHE": "pub",
	} {
		e = append(e, k+"="+filepath.Join(cache, sub))
	}
	// CARGO_HOME above is a cache and holds no toolchain; rustup finds those under RUSTUP_HOME,
	// which the image sets. Off the image nothing does, and rustup would look inside the job's
	// own HOME and report no toolchain at all — so point it at the real one when it is there.
	if _, ok := lookupEnv("RUSTUP_HOME"); !ok {
		if home, ok := lookupEnv("HOME"); ok {
			if st, err := os.Stat(filepath.Join(home, ".rustup")); err == nil && st.IsDir() {
				e = append(e, "RUSTUP_HOME="+filepath.Join(home, ".rustup"))
			}
		}
	}
	return e
}
