//go:build !windows && !linux

package supervisor

import (
	"fmt"
	"os/exec"
	"sync"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func startManagedProcessWithResources(cmd *exec.Cmd, resources *config.Resources) (*processControl, error) {
	if resources != nil {
		return nil, fmt.Errorf("managed memory limits are unsupported on this platform")
	}
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
		close: func() error {
			mu.Lock()
			defer mu.Unlock()
			closed = true
			return nil
		},
	}, nil
}
