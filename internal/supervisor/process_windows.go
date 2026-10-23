//go:build windows

package supervisor

import (
	"os/exec"
	"syscall"
)

// Windows is a development fallback: only direct children are terminated.
// Full process-tree lifetime control requires Job Objects, which are not implemented.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
func signalProcess(cmd *exec.Cmd, force bool) { _ = cmd.Process.Kill() }
