package supervisor

import (
	"errors"
	"fmt"
	"os/exec"
)

var errResourceCleanup = errors.New("worker resource cleanup unconfirmed")

// A failed launch is retryable only if its partially allocated budget was
// released. Preserve both causes through wrappers for the supervisor.
func failedStart(startErr, cleanupErr error) error {
	if cleanupErr == nil {
		return startErr
	}
	return errors.Join(startErr, fmt.Errorf("%w: %w", errResourceCleanup, cleanupErr))
}

// Each worker generation owns its process lifetime control. signal stops that
// generation; close releases platform resources and disables future signals.
type processControl struct {
	signal func(force bool)
	close  func() error
}

func startManagedProcess(cmd *exec.Cmd) (*processControl, error) {
	return startManagedProcessWithResources(cmd, nil)
}
