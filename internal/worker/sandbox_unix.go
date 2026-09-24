//go:build !windows

package worker

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// The privilege split. The worker runs as root inside its own container and drops to the
// sandbox user for every subprocess that executes a repository's code. That user owns the job
// directory and nothing else: it cannot read /proc/<worker>/environ (the job token), it never
// sees the environment git clone and push are given (the repository token), and what it can
// write is what git will commit. The engine's model key is per job and budget-capped, and is
// the one secret that has to be in its environment.

var sandbox struct {
	uid, gid uint32
	active   bool
}

// configureSandbox sets the split up from WORKER_SANDBOX_UID. Outside the container — a
// developer's machine, the tests — the worker is not root and everything runs as one user; in
// every container mode that is refused unless WORKER_ALLOW_SHARED_UID=1 says it is meant.
//
// The test was `Mode == "cloudrun"` while Cloud Run was the only container this ran in. It is
// every mode but local now, which is what it always meant: ecs, aca, k8s and docker run the
// same image, as root, and letting a repository's own build and tests run as the worker itself
// is exactly as wrong there.
func configureSandbox(o Options) error {
	sandbox.active = false
	if o.SandboxUID <= 0 {
		if o.Mode != "local" && os.Getenv("WORKER_ALLOW_SHARED_UID") != "1" {
			return errors.New("WORKER_SANDBOX_UID is not set: repository code would run as the worker itself (set it, or WORKER_ALLOW_SHARED_UID=1 to accept that)")
		}
		return nil
	}
	if os.Geteuid() != 0 {
		if o.Mode != "local" && os.Getenv("WORKER_ALLOW_SHARED_UID") != "1" {
			return fmt.Errorf("WORKER_SANDBOX_UID=%d is set but the worker is not root, so it cannot drop to it", o.SandboxUID)
		}
		slog.Warn("sandbox user ignored: the worker is not root", "uid", o.SandboxUID)
		return nil
	}
	sandbox.uid, sandbox.gid, sandbox.active = uint32(o.SandboxUID), uint32(o.SandboxUID), true
	slog.Info("repository code runs as the sandbox user", "uid", o.SandboxUID)
	return nil
}

func applySandbox(cmd *exec.Cmd) {
	if !sandbox.active {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: sandbox.uid, Gid: sandbox.gid, NoSetGroups: true}
}

// grantSandbox hands directories to the sandbox user, so the code running as it can read what
// the worker wrote there (the engine's settings and brief) and write what the tests and the
// engine produce. A no-op when there is no split.
func grantSandbox(paths ...string) {
	if !sandbox.active {
		return
	}
	for _, root := range paths {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil {
				os.Lchown(p, int(sandbox.uid), int(sandbox.gid))
			}
			return nil
		})
	}
}
