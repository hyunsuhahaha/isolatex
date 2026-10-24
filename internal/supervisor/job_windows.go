//go:build windows

package supervisor

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var isProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// Only use this for a per-worker job, never the lifetime owner job containing us.
func newWorkerControl(job windows.Handle) *processControl {
	var mu sync.Mutex
	var handles []windows.Handle
	var requested bool
	var requestErr, cleanupResult error
	terminate := func() {
		if !requested {
			requested = true
			// Retain handles before asking Windows to kill the tree: accounting
			// may reach zero before those processes finish exiting.
			handles, requestErr = workerJobHandles(job)
		}
		requestErr = errors.Join(requestErr, windows.TerminateJobObject(job, 1))
	}
	return &processControl{
		signal: func(bool) {
			mu.Lock()
			defer mu.Unlock()
			if job != 0 {
				terminate()
			}
		},
		close: func() error {
			mu.Lock()
			defer mu.Unlock()
			if job == 0 {
				return cleanupResult
			}
			terminate()
			drainErr := waitWorkerJob(job, handles, time.Second)
			for _, h := range handles {
				_ = windows.CloseHandle(h)
			}
			handles = nil
			cleanupResult = errors.Join(requestErr, drainErr, windows.CloseHandle(job))
			job = 0
			return cleanupResult
		},
	}
}

// Bounded snapshot including nested jobs. Membership is checked on the retained
// process handle, so an exited/reused PID cannot make us wait on an outsider.
func workerJobHandles(job windows.Handle) ([]windows.Handle, error) {
	var handles []windows.Handle
	fail := func(err error) ([]windows.Handle, error) { return handles, err }
	for capacity := 16; capacity <= 65536; capacity *= 2 {
		// Two DWORDs followed by ULONG_PTR process IDs (8-byte header).
		buf := make([]byte, 8+capacity*int(unsafe.Sizeof(uintptr(0))))
		var pinned runtime.Pinner
		pinned.Pin(&buf[0])
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf)), nil)
		pinned.Unpin()
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			continue
		}
		if err != nil {
			return fail(fmt.Errorf("snapshot worker job: %w", err))
		}
		header := (*struct{ Assigned, Listed uint32 })(unsafe.Pointer(&buf[0]))
		if header.Assigned > uint32(capacity) || header.Listed < header.Assigned {
			continue
		}
		if header.Listed > uint32(capacity) {
			return fail(fmt.Errorf("invalid worker job process list"))
		}
		ids := unsafe.Slice((*uintptr)(unsafe.Pointer(&buf[8])), int(header.Listed))
		for _, pid := range ids {
			h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				continue
			} // already gone
			if err != nil {
				return fail(fmt.Errorf("open worker job member: %w", err))
			}
			var member uint32
			var memberPinned runtime.Pinner
			memberPinned.Pin(&member)
			ok, _, callErr := isProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&member)))
			memberPinned.Unpin()
			if ok == 0 {
				_ = windows.CloseHandle(h)
				return fail(fmt.Errorf("verify worker job membership: %w", callErr))
			}
			if member == 0 {
				_ = windows.CloseHandle(h)
				continue
			}
			handles = append(handles, h)
		}
		return handles, nil
	}
	return fail(fmt.Errorf("worker job snapshot exceeds 65536 processes"))
}

func waitWorkerJob(job windows.Handle, handles []windows.Handle, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, h := range handles {
		remaining := max(0, time.Until(deadline))
		state, err := windows.WaitForSingleObject(h, uint32(remaining/time.Millisecond))
		if err != nil {
			return fmt.Errorf("wait worker job member: %w", err)
		}
		if state != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("worker job member did not exit before cleanup deadline")
		}
	}
	accounting := struct {
		User, Kernel, PeriodUser, PeriodKernel int64
		Faults, Processes, Active, Terminated  uint32
	}{}
	var pinned runtime.Pinner
	pinned.Pin(&accounting)
	defer pinned.Unpin()
	for {
		if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return fmt.Errorf("query worker job termination: %w", err)
		}
		if accounting.Active == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("worker job still has %d active processes after termination", accounting.Active)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
