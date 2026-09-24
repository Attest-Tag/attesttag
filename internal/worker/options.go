// Package worker is the fix-job worker: the process the bot starts in a separate container to
// clone a repository, change it, run its tests, push a branch and open a draft pull request.
// It is deliberately small and dumb about policy — the bot decided what may happen; the worker
// carries it out, reports honestly, and never pushes anywhere but the branch it was given.
package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"attesttag/internal/app"
)

// Options come from the environment the dispatcher set (ATTEST_*) plus a few knobs baked into
// the image. The token is read from the environment only: argv shows in ps and in execution
// metadata.
type Options struct {
	JobID    int64
	BotURL   string
	Token    string
	Mode     string // local | cloudrun
	WorkDir  string
	MaxWall  time.Duration
	QwenBin  string
	MiseBin  string
	KeepWork bool
	Version  string
	// SandboxUID is the user repository code runs as when the worker is root (sandbox_unix.go).
	SandboxUID int
	// GitBase and GitHubAPI redirect the two places a job touches github.com: where it clones
	// from and where it opens the pull request. They are honoured in local mode only — a
	// cloudrun worker always talks to GitHub — and exist so a whole job can be run end to end
	// against local repositories and a stub API without anything leaving the machine.
	GitBase   string
	GitHubAPI string
}

func optionsFromEnv() (Options, error) {
	var o Options
	id, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(app.EnvJobID)), 10, 64)
	if err != nil || id <= 0 {
		return o, errors.New(app.EnvJobID + " is not set")
	}
	o.JobID = id
	o.BotURL = strings.TrimRight(strings.TrimSpace(os.Getenv(app.EnvBotURL)), "/")
	if o.BotURL == "" {
		return o, errors.New(app.EnvBotURL + " is not set")
	}
	o.Token = strings.TrimSpace(os.Getenv(app.EnvJobToken))
	if o.Token == "" {
		return o, errors.New(app.EnvJobToken + " is not set")
	}
	o.Mode = strings.ToLower(env("WORKER_MODE", "local"))
	if o.Mode != "local" && !strings.HasPrefix(o.BotURL, "https://") {
		return o, fmt.Errorf("%s must be https outside local mode", app.EnvBotURL)
	}
	o.WorkDir = os.Getenv("WORKER_DIR")
	if o.WorkDir == "" {
		if st, err := os.Stat("/work"); err == nil && st.IsDir() && writable("/work") {
			o.WorkDir = "/work"
		} else {
			o.WorkDir = filepath.Join(os.TempDir(), "attesttag-worker")
		}
	}
	o.MaxWall = 55 * time.Minute
	if v := os.Getenv("WORKER_MAX_WALL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > time.Minute {
			o.MaxWall = d
		}
	}
	o.QwenBin = env("WORKER_QWEN_BIN", "qwen")
	o.MiseBin = env("WORKER_MISE_BIN", "mise")
	o.KeepWork = os.Getenv("WORKER_KEEP_WORK") == "1"
	if v := os.Getenv("WORKER_SANDBOX_UID"); v != "" {
		uid, err := strconv.Atoi(v)
		if err != nil || uid <= 0 {
			return o, errors.New("WORKER_SANDBOX_UID must be a positive user id")
		}
		o.SandboxUID = uid
	}
	o.Version = env("GIT_COMMIT", "dev")
	if o.Mode == "local" {
		o.GitBase = strings.TrimRight(os.Getenv("WORKER_GIT_BASE"), "/")
		o.GitHubAPI = strings.TrimRight(os.Getenv("WORKER_GITHUB_API"), "/")
	}
	return o, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".w-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
