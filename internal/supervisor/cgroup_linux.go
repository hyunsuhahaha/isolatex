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
			return nil, fmt.Errorf("%w; remove new cgroup: %v", err, removeErr)
		}
		return nil, err
	}
	// A missing controller/interface is a startup error. Memory includes the
	// descendant hierarchy; disable swap and make cgroup OOM a worker-wide event.
	for _, setting := range []struct{ name, value string }{
		{"memory.max", strconv.FormatInt(resources.MemoryMaxBytes, 10)},
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"},
	} {
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
