//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"golang.org/x/sys/unix"
)

type workerCgroup struct {
	path      string
	directory *os.File
}

func newWorkerCgroup(resources *config.Resources) (*workerCgroup, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(resources.CgroupParent, &fs); err != nil {
		return nil, fmt.Errorf("cgroup parent: %w", err)
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, fmt.Errorf("cgroup_parent must be on cgroup v2")
	}
	path, err := os.MkdirTemp(resources.CgroupParent, "isolatex-worker-")
	if err != nil {
		return nil, fmt.Errorf("create worker cgroup: %w", err)
	}
	cg := &workerCgroup{path: path}
	fail := func(err error) (*workerCgroup, error) {
		// This newly-created cgroup has never contained a process. Only remove
		// our empty directory; never delete an operator's parent or files.
		if removeErr := os.Remove(path); removeErr != nil {
			return nil, failedStart(err, fmt.Errorf("remove new cgroup: %w", removeErr))
		}
		return nil, err
	}
	// Only require controllers explicitly requested by the operator.
	type setting struct{ name, value string }
	var settings []setting
	if resources.MemoryMaxBytes > 0 {
		settings = append(settings, setting{"memory.max", strconv.FormatInt(int64(resources.MemoryMaxBytes), 10)},
			setting{"memory.swap.max", "0"}, setting{"memory.oom.group", "1"})
	}
	if resources.TasksMax > 0 {
		settings = append(settings, setting{"pids.max", strconv.FormatInt(int64(resources.TasksMax), 10)})
	}
	if resources.CPUMillicores > 0 {
		// Fixed 100ms period: 1000 millicores means one full logical CPU.
		settings = append(settings, setting{"cpu.max", strconv.FormatInt(int64(resources.CPUMillicores)*100, 10) + " 100000"})
	}
	for _, setting := range settings {
		if err := os.WriteFile(filepath.Join(path, setting.name), []byte(setting.value), 0600); err != nil {
			return fail(fmt.Errorf("configure %s: %w", setting.name, err))
		}
	}
	if _, err := os.Stat(filepath.Join(path, "cgroup.kill")); err != nil {
		return fail(fmt.Errorf("cgroup.kill required: %w", err))
	}
	cg.directory, err = os.Open(path)
	if err != nil {
		return fail(fmt.Errorf("open worker cgroup: %w", err))
	}
	return cg, nil
}

func (cg *workerCgroup) kill() error {
	return os.WriteFile(filepath.Join(cg.path, "cgroup.kill"), []byte("1"), 0600)
}

func (cg *workerCgroup) close() error {
	if cg.directory != nil {
		_ = cg.directory.Close()
		cg.directory = nil
	}
	if err := cg.kill(); err != nil {
		return fmt.Errorf("kill worker cgroup: %w", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if err := os.Remove(cg.path); err == nil {
			return nil
		} else if !errors.Is(err, unix.EBUSY) && !errors.Is(err, unix.ENOTEMPTY) {
			return fmt.Errorf("remove worker cgroup: %w", err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("worker cgroup did not become removable: %s", cg.path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
