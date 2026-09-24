//go:build windows

package app

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd, force bool) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
