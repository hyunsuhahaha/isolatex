package supervisor

import "os/exec"

// Each worker generation owns its process lifetime control. signal stops that
// generation; close releases platform resources and disables future signals.
type processControl struct {
	signal func(force bool)
	close  func() error
}

func startManagedProcess(cmd *exec.Cmd) (*processControl, error) {
	return startManagedProcessWithResources(cmd, nil)
}
