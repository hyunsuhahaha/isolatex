//go:build linux

package supervisor

import (
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
			return nil, failedStart(err, cg.close())
		}
		return nil, err
	}
	if cg != nil {
		_ = cg.directory.Close()
		cg.directory = nil
	}
	var mu sync.Mutex
	closed := false
	var cleanupResult error
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
				return cleanupResult
			}
			closed = true
			if cg != nil {
				cleanupResult = cg.close()
			} else {
				signalProcess(cmd, true)
				cleanupResult = waitLinuxProcessGroup(cmd.Process.Pid)
			}
			return cleanupResult
		},
	}, nil
}
