//go:build !linux && !darwin && !windows

package supervisor

import "os/exec"

func configureProcess(cmd *exec.Cmd)          {}
func signalProcess(cmd *exec.Cmd, force bool) { _ = cmd.Process.Kill() }
