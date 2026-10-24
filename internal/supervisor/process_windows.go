//go:build windows

package supervisor

import (
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/hyunsuhahaha/isolatex/internal/config"

	"golang.org/x/sys/windows"
)

var ownerJob struct {
	sync.Mutex
	handle windows.Handle
}

// Keep one non-inheritable root job handle for this process's entire lifetime.
// A child inherits membership atomically at CreateProcess, covering a runtime
// exit between suspended creation and assignment to the per-worker job.
func ensureOwnerJob() error {
	ownerJob.Lock()
	defer ownerJob.Unlock()
	if ownerJob.handle != 0 {
		return nil
	}
	job, err := newLifetimeJob()
	if err != nil {
		return err
	}
	current, err := windows.GetCurrentProcess()
	if err == nil {
		err = windows.AssignProcessToJobObject(job, current)
	}
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("assign runtime owner job: %w", err)
	}
	// Closing this handle while the owner is alive would also terminate the
	// owner. Only OS process teardown closes it; no Go finalizer owns it.
	ownerJob.handle = job
	return nil
}

func newLifetimeJob() (windows.Handle, error) {
	return newWorkerJob(nil)
}

func newWorkerJob(resources *config.Resources) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create lifetime job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if resources != nil && resources.MemoryMaxBytes > 0 {
		limits.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		limits.JobMemoryLimit = uintptr(resources.MemoryMaxBytes)
	}
	if resources != nil && resources.ProcessesMax > 0 {
		limits.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		limits.BasicLimitInformation.ActiveProcessLimit = uint32(resources.ProcessesMax)
	}
	// This API exposes its input as uintptr rather than a typed pointer.
	// Keep the buffer fixed across lazy DLL resolution and the native call.
	var pinned runtime.Pinner
	pinned.Pin(&limits)
	defer pinned.Unpin()
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("configure lifetime job: %w", err)
	}
	if resources != nil && resources.CPUPercent > 0 {
		// Native structure is two DWORDs; the second is the CpuRate union arm.
		const enable, hardCap = 0x1, 0x4
		rate := struct{ ControlFlags, CPURate uint32 }{enable | hardCap, uint32(resources.CPUPercent) * 100}
		var ratePinned runtime.Pinner
		ratePinned.Pin(&rate)
		_, err := windows.SetInformationJobObject(job, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate)))
		ratePinned.Unpin()
		if err != nil {
			_ = windows.CloseHandle(job)
			return 0, fmt.Errorf("configure worker CPU hard cap: %w", err)
		}
	}
	return job, nil
}

// Suspend before the first application instruction so even an immediately
// spawned descendant inherits this worker's job. Never resume on attach failure.
func startManagedProcessWithResources(cmd *exec.Cmd, resources *config.Resources) (*processControl, error) {
	if err := resources.Validate(); err != nil {
		return nil, err
	}
	if err := ensureOwnerJob(); err != nil {
		return nil, err
	}
	job, err := newWorkerJob(resources)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	control := &processControl{
		signal: func(bool) {
			mu.Lock()
			defer mu.Unlock()
			if job != 0 {
				_ = windows.TerminateJobObject(job, 1)
			}
		},
		close: func() error {
			mu.Lock()
			defer mu.Unlock()
			if job != 0 {
				err := windows.CloseHandle(job)
				job = 0
				return err
			}
			return nil
		},
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		control.close()
		return nil, err
	}
	attach := func() error {
		process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if err != nil {
			return fmt.Errorf("open suspended worker: %w", err)
		}
		defer windows.CloseHandle(process)
		if err := windows.AssignProcessToJobObject(job, process); err != nil {
			return fmt.Errorf("assign worker job: %w", err)
		}
		return resumeWorker(uint32(cmd.Process.Pid))
	}
	if err := attach(); err != nil {
		control.signal(true)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		control.close()
		return nil, err
	}
	return control, nil
}

// os/exec closes CreateProcess's primary thread handle before returning. The
// suspended process has not run its loader or created application threads yet.
func resumeWorker(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot worker thread: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("open worker thread: %w", err)
		}
		previous, err := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		if err != nil {
			return fmt.Errorf("resume worker: %w", err)
		}
		if previous != 1 {
			return fmt.Errorf("unexpected worker thread suspend count: %d", previous)
		}
		return nil
	}
	return fmt.Errorf("find suspended worker thread: %w", err)
}
