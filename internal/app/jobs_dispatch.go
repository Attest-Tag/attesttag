package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A Dispatcher starts the worker for a job somewhere else and can ask after it or stop it.
// There is one per platform that can run a container on demand — cloudrun (jobs_cloudrun.go),
// ecs, aca, k8s, docker — plus local, which runs this binary again as `attesttag worker` for
// development. The launch carries only what the worker needs to call back: the spec and secrets
// never enter the container's environment.
type Dispatcher interface {
	Name() string
	Start(ctx context.Context, j *Job, l JobLaunch) (executionRef string, err error)
	Cancel(ctx context.Context, executionRef string) error
	Status(ctx context.Context, executionRef string) (DispatchStatus, error)
}

// newDispatcherFor builds the dispatcher for one mode. It is separate from the JobRunner because
// two callers need it: dispatching a new job, and the reconciler asking after a job started in a
// mode the configuration has since moved away from.
//
// local is absent on purpose — it holds per-process state (the subprocesses it started), so it
// is constructed once in NewJobRunner and cannot be rebuilt on demand.
func newDispatcherFor(ctx context.Context, cfg Config, mode string) (Dispatcher, error) {
	switch mode {
	case "cloudrun":
		return newCloudRunDispatcher(ctx, cfg)
	case "ecs":
		return newECSDispatcher(cfg)
	case "aca":
		return newACADispatcher(cfg)
	case "k8s":
		return newK8sDispatcher(cfg)
	case "docker":
		return newDockerDispatcher(cfg)
	}
	return nil, fmt.Errorf("no dispatcher for WORKER_MODE=%q", mode)
}

// executionConsoleURL is the platform's own page for an execution, for the Jobs page. Which
// platform it is, is read off the reference rather than passed in, so that a job started before
// a mode change still links to where it actually ran. Kubernetes and Docker have no console to
// link to and return nothing, which the page already handles.
func executionConsoleURL(ref string) string {
	switch {
	case strings.HasPrefix(ref, "arn:"):
		return ecsConsoleURL(ref)
	case strings.HasPrefix(ref, "/subscriptions/"):
		return acaConsoleURL(ref)
	case strings.HasPrefix(ref, "projects/"):
		return cloudRunConsoleURL(ref)
	}
	return ""
}

type JobLaunch struct {
	JobID  int64
	BotURL string
	Token  string
	// Target is the platform-specific execution to start — a Cloud Run job name. A JVM or
	// .NET repository is minutes of cold install away on the base image, so a deployment can
	// keep a second, heavier worker job for those and route to it by ecosystem. Empty means
	// the default worker.
	Target string
}

// DispatchStatus is what the platform says about an execution: unknown means it cannot tell
// (a local process from before a restart), which the reconciler treats with the silence rule.
type DispatchStatus struct {
	State   string // unknown | pending | running | succeeded | failed | cancelled
	Message string
}

func (l JobLaunch) env(mode string) []string {
	return []string{EnvJobID + "=" + fmt.Sprint(l.JobID), EnvBotURL + "=" + l.BotURL, EnvJobToken + "=" + l.Token, "WORKER_MODE=" + mode}
}

// ---- local: a subprocess of this binary ----

type localDispatcher struct {
	bin    string
	logDir string
	mu     sync.Mutex
	procs  map[string]*localProc
}

type localProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func newLocalDispatcher(bin string) *localDispatcher {
	return &localDispatcher{bin: bin, logDir: filepath.Join(os.TempDir(), "attesttag-jobs"), procs: map[string]*localProc{}}
}

func (d *localDispatcher) Name() string { return "local" }

// allowlisted is the environment a local worker inherits: enough to find git and toolchains,
// none of the bot's own secrets (Slack tokens, MASTER_KEY, the shared model key).
func allowlistedEnv() []string {
	var out []string
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "USER", "SHELL", "GOPATH", "GOMODCACHE", "GOCACHE", "SSL_CERT_FILE", "LOG_LEVEL",
		"WORKER_DIR", "WORKER_MAX_WALL", "WORKER_QWEN_BIN", "WORKER_MISE_BIN", "WORKER_KEEP_WORK",
		// Local-mode only, and refused by the worker outside it: where to clone from and which
		// API to open the pull request against. They are how a whole job runs against local
		// repositories, which is what the end-to-end test does.
		"WORKER_GIT_BASE", "WORKER_GITHUB_API"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func (d *localDispatcher) Start(ctx context.Context, j *Job, l JobLaunch) (string, error) {
	if d.bin == "" {
		return "", errors.New("no worker binary")
	}
	if err := os.MkdirAll(d.logDir, 0o700); err != nil {
		return "", err
	}
	logPath := filepath.Join(d.logDir, fmt.Sprintf("job-%d.log", j.ID))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(d.bin, "worker")
	cmd.Env = append(allowlistedEnv(), l.env("local")...)
	cmd.Dir = os.TempDir()
	cmd.Stdout, cmd.Stderr = f, f
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		f.Close()
		return "", err
	}
	ref := fmt.Sprintf("pid:%d", cmd.Process.Pid)
	p := &localProc{cmd: cmd, done: make(chan struct{})}
	d.mu.Lock()
	d.procs[ref] = p
	d.mu.Unlock()
	go func() {
		p.err = cmd.Wait()
		f.Close()
		close(p.done)
		slog.Info("local worker exited", "job", j.ID, "pid", cmd.Process.Pid, "err", p.err, "log", logPath)
	}()
	slog.Info("local worker started", "job", j.ID, "pid", cmd.Process.Pid, "log", logPath)
	return ref, nil
}

func (d *localDispatcher) proc(ref string) *localProc {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.procs[ref]
}

func (d *localDispatcher) Cancel(ctx context.Context, ref string) error {
	p := d.proc(ref)
	if p == nil {
		return nil // not ours (before a restart): nothing to signal
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	killProcessGroup(p.cmd, false)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		killProcessGroup(p.cmd, true)
	}
	return nil
}

func (d *localDispatcher) Status(ctx context.Context, ref string) (DispatchStatus, error) {
	p := d.proc(ref)
	if p == nil {
		return DispatchStatus{State: "unknown", Message: "not started by this process"}, nil
	}
	select {
	case <-p.done:
		if p.err == nil {
			return DispatchStatus{State: "succeeded"}, nil
		}
		return DispatchStatus{State: "failed", Message: p.err.Error()}, nil
	default:
		return DispatchStatus{State: "running"}, nil
	}
}

// ---- helpers shared by dispatchers ----

// shortRef is the last path segment of an execution name, for messages.
func shortRef(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// readAllLimit drains a control-plane response. A megabyte is far more than any of these APIs
// returns and the cap is only here so that a proxy answering with something enormous cannot
// become this process's memory problem.
func readAllLimit(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// apiError turns a non-2xx control-plane response into an error carrying enough of the body to
// name the cause. These are the errors an operator sees in the Jobs page when a deployment is
// misconfigured — "no such task definition", "subnet not found" — so the body is the message.
func apiError(what string, resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		return fmt.Errorf("%s: %s", what, resp.Status)
	}
	return fmt.Errorf("%s: %s: %s", what, resp.Status, msg)
}

// envPairs splits the KEY=VALUE launch environment into ordered pairs, which is the shape every
// container API below wants and the shape JobLaunch.env does not have.
func envPairs(kvs []string) [][2]string {
	out := make([][2]string, 0, len(kvs))
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		out = append(out, [2]string{k, v})
	}
	return out
}
