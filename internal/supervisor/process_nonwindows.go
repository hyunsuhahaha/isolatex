//go:build !windows

package supervisor

import "os/exec"

func startManagedProcess(cmd *exec.Cmd) (*processControl, error) {
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processControl{signal: func(force bool) { signalProcess(cmd, force) }, close: func() {}}, nil
}
