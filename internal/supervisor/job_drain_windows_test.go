//go:build windows

package supervisor

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsJobCloseConfirmsTreeExitBeforeReturning(t *testing.T) {
	other, _, _, _ := jobTestProcess(t, "leaf")
	otherHandle := jobTestHandle(t, other.Process.Pid)
	for attempt := 0; attempt < 8; attempt++ {
		t.Run(strconv.Itoa(attempt), func(t *testing.T) {
			root, control, _, leafFile := jobTestProcess(t, "tree")
			rootHandle := jobTestHandle(t, root.Process.Pid)
			leafHandle := jobTestHandle(t, jobTestPID(t, leafFile))
			if err := control.close(); err != nil {
				t.Fatal(err)
			}
			requireJobProcessState(t, otherHandle, false)
			// Zero timeout is deliberate: a successful cleanup must establish
			// the boundary before a replacement gets another resource budget.
			for _, handle := range []windows.Handle{rootHandle, leafHandle} {
				state, err := windows.WaitForSingleObject(handle, 0)
				if err != nil || state != windows.WAIT_OBJECT_0 {
					t.Fatalf("cleanup returned before the worker tree exited: wait=%d error=%v", state, err)
				}
			}
		})
	}
}

func TestWindowsJobCloseWaitsForWideTree(t *testing.T) {
	root, control, _, filename := jobTestProcess(t, "wide-tree")
	handles := []windows.Handle{jobTestHandle(t, root.Process.Pid)}
	var pids []string
	waitFor(t, func() bool {
		data, _ := os.ReadFile(filename)
		pids = strings.Fields(string(data))
		return len(pids) == 20
	})
	for _, text := range pids {
		pid, err := strconv.Atoi(text)
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, jobTestHandle(t, pid))
	}
	if err := control.close(); err != nil {
		t.Fatal(err)
	}
	for _, h := range handles {
		state, err := windows.WaitForSingleObject(h, 0)
		if err != nil || state != windows.WAIT_OBJECT_0 {
			t.Fatalf("wide tree member still exiting: wait=%d error=%v", state, err)
		}
	}
}
