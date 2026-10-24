//go:build linux

package supervisor

import (
	"fmt"
	"os/exec"
	"sync"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func startManagedProcessWithResources(cmd *exec.Cmd, resources *config.Resources) (*processControl, error) {
	if err := resources.Validate(); err != nil {
		return nil, err
	}
	configureProcess(cmd)
	var cg *workerCgroup
	if resources != nil {
		var err error
		cg, err = newWorkerCgroup(resources)
		if err != nil {
			return nil, err
		}
		// clone3 places the child in its budget before any application code
		// runs. Never fall back to moving an already-running PID.
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(cg.directory.Fd())
	}
	if err := cmd.Start(); err != nil {
		if cg != nil {
			_ = cg.directory.Close()
			cg.directory = nil
			if cleanupErr := cg.close(); cleanupErr != nil {
				return nil, fmt.Errorf("start worker: %w; cgroup cleanup: %v", err, cleanupErr)
			}
		}
		return nil, err
	}
	if cg != nil {
		_ = cg.directory.Close()
		cg.directory = nil
	}
	var mu sync.Mutex
	closed := false
	return &processControl{
		signal: func(force bool) {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return
			}
			signalProcess(cmd, force)
			if force && cg != nil {
				_ = cg.kill()
			}
		},
		close: func() error {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return nil
			}
			closed = true
			if cg != nil {
				return cg.close()
			}
			return nil
		},
	}, nil
}
