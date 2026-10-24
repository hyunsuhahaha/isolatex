//go:build !windows

package supervisor

import (
	"os/exec"
	"sync"
)

func startManagedProcess(cmd *exec.Cmd) (*processControl, error) {
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	closed := false
	return &processControl{
		signal: func(force bool) {
			mu.Lock()
			defer mu.Unlock()
			if !closed {
				signalProcess(cmd, force)
			}
		},
		close: func() {
			mu.Lock()
			defer mu.Unlock()
			closed = true
		},
	}, nil
}
