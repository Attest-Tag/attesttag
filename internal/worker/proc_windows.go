//go:build windows

package worker

import (
	"os"
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd, force bool) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}

func lookupEnv(k string) (string, bool) { return os.LookupEnv(k) }

func environ() []string { return os.Environ() }
