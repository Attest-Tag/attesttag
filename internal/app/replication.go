package app

// Litestream, as a child of this process rather than its parent.
//
// It used to be the other way round: deploy/entrypoint.sh ran `litestream replicate -exec` and the
// bot was the child. That could not work once the restore had to wait for the write lease, because
// the restore happened before the bot process existed. Inverting it also makes the shutdown order
// explicit — the database is closed first, and only then is Litestream told to make its final sync
// — instead of leaving it implicit in -exec.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	restoreTimeout  = 5 * time.Minute
	replicaRestarts = 3                // within replicaRestartWindow, before we give up
	replicaWindow   = 60 * time.Second // ...
)

// proc is one run of the Litestream child. Exactly one goroutine calls Wait on it; everyone else
// waits for done to close, so the supervisor and a concurrent Stop cannot both reap the same pid.
type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

type replicator struct {
	bin, conf, dbPath string
	env               []string
	generated         string // the config file we rendered, removed on Stop; empty if the operator brought one

	mu      sync.Mutex
	cur     *proc
	stopped bool

	fatal chan struct{}
	once  sync.Once
}

// newReplicator renders the Litestream configuration for the target this deployment resolved and
// prepares the child that will read it.
//
// The config is generated rather than shipped in the image because the two targets — a GCS
// bucket, an S3-compatible one — differ only in that stanza, and a file baked at build time
// cannot know which one this deployment chose. LITESTREAM_CONFIG still points at a file of the
// operator's own, for the replica shape neither of these renders.
func newReplicator(cfg Config) (*replicator, error) {
	if cfg.Replica == nil {
		return nil, errors.New("no replica target: nothing here runs without a bucket to stream to")
	}
	r := &replicator{
		bin:    cfg.LitestreamBin,
		conf:   cfg.LitestreamConf,
		dbPath: cfg.DBPath,
		// A config file — ours or the operator's — expands these at load time.
		// deploy/entrypoint.sh used to export them; now nothing else does, so they are passed
		// explicitly rather than assumed inherited.
		env: append(os.Environ(),
			"DB_PATH="+cfg.DBPath,
			"LITESTREAM_BUCKET="+cfg.ReplicaBucket,
			"LITESTREAM_PATH="+cfg.ReplicaPath,
		),
		fatal: make(chan struct{}),
	}
	r.env = append(r.env, cfg.Replica.childEnv()...)
	if r.conf == "" {
		// 0600, and no credential in it: the S3 keys reach the child through its environment.
		f, err := os.CreateTemp("", "litestream-*.yml")
		if err != nil {
			return nil, fmt.Errorf("litestream config: %w", err)
		}
		if _, err := f.WriteString(cfg.Replica.litestreamYAML(cfg.DBPath)); err != nil {
			f.Close()
			return nil, fmt.Errorf("litestream config: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("litestream config: %w", err)
		}
		r.conf, r.generated = f.Name(), f.Name()
	}
	return r, nil
}

// Fatal closes when replication cannot be kept running. Continuing to serve without it would mean
// writing to a disk Cloud Run throws away, so the caller must treat this as terminal.
func (r *replicator) Fatal() <-chan struct{} { return r.fatal }

func (r *replicator) markFatal() { r.once.Do(func() { close(r.fatal) }) }

// Restore pulls the newest replica down when there is no local database yet. It must only be
// called while the write lease is held: restoring while another container is still writing is how
// a stale snapshot gets promoted over live data.
func (r *replicator) Restore(ctx context.Context) error {
	if _, err := os.Stat(r.dbPath); err == nil {
		slog.Info("local database present; not restoring", "db", r.dbPath)
		return nil
	}
	slog.Info("restoring the database from its replica", "db", r.dbPath)
	cctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.bin, "restore", "-if-replica-exists", "-config", r.conf, r.dbPath)
	cmd.Env, cmd.Stdout, cmd.Stderr = r.env, os.Stdout, os.Stderr
	setProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("litestream restore: %w", err)
	}
	return nil
}

// Start begins streaming and keeps it running. It returns once the child is up; Run supervises.
func (r *replicator) Start() error {
	cmd := exec.Command(r.bin, "replicate", "-config", r.conf)
	cmd.Env, cmd.Stdout, cmd.Stderr = r.env, os.Stdout, os.Stderr
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("litestream replicate: %w", err)
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	r.mu.Lock()
	r.cur = p
	r.mu.Unlock()
	slog.Info("replicating the database", "pid", cmd.Process.Pid)
	return nil
}

// Run restarts a Litestream that dies under us, and gives up after too many tries in a row.
// It deliberately never restores again: by now the local database is the authoritative copy, and
// restoring over it would replace live data with a snapshot.
func (r *replicator) Run(ctx context.Context) {
	var restarts int
	var windowStart time.Time
	for {
		r.mu.Lock()
		p := r.cur
		r.mu.Unlock()
		if p == nil {
			return
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			return
		}

		r.mu.Lock()
		stopped := r.stopped
		r.mu.Unlock()
		if stopped || ctx.Err() != nil {
			return // we asked it to stop
		}

		now := time.Now()
		if windowStart.IsZero() || now.Sub(windowStart) > replicaWindow {
			windowStart, restarts = now, 0
		}
		restarts++
		if restarts > replicaRestarts {
			slog.Error("litestream keeps failing; the database is no longer being replicated", "err", p.err)
			r.markFatal()
			return
		}
		backoff := time.Duration(1<<uint(2*(restarts-1))) * time.Second // 1s, 4s, 16s
		slog.Error("litestream exited; restarting", "err", p.err, "attempt", restarts, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := r.Start(); err != nil {
			slog.Error("could not restart litestream", "err", err)
			r.markFatal()
			return
		}
	}
}

// Stop asks Litestream to finish its final sync and waits for it. This runs after the database is
// closed, so what it flushes is everything the process ever wrote.
func (r *replicator) Stop(wait time.Duration) error {
	r.mu.Lock()
	p := r.cur
	r.stopped = true
	r.mu.Unlock()
	if r.generated != "" {
		defer os.Remove(r.generated)
	}
	if p == nil || p.cmd.Process == nil {
		return nil
	}
	killProcessGroup(p.cmd, false)
	select {
	case <-p.done:
		slog.Info("litestream finished its final sync")
		return nil
	case <-time.After(wait):
		killProcessGroup(p.cmd, true)
		return errors.New("litestream did not finish its final sync in time; the last writes may not have reached the replica")
	}
}
