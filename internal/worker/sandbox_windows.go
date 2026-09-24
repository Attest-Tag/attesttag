//go:build windows

package worker

import "os/exec"

func configureSandbox(o Options) error { return nil }
func applySandbox(cmd *exec.Cmd)       {}
func grantSandbox(paths ...string)     {}
