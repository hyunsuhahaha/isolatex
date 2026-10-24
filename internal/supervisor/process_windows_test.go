//go:build windows

package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsJobHelper(t *testing.T) {
	mode := os.Getenv("ISOLATEX_JOB_TEST_MODE")
	if mode == "" {
		return
	}
	if mode == "owner" || mode == "owner-before-attach" {
		cmd := jobHelperCommand("tree")
		if mode == "owner-before-attach" {
			if err := ensureOwnerJob(); err != nil {
				os.Exit(34)
			}
			cmd = jobHelperCommand("leaf")
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
			if err := cmd.Start(); err != nil {
				os.Exit(35)
			}
		} else {
			if _, err := startManagedProcess(cmd); err != nil {
				os.Exit(30)
			}
		}
		if err := os.WriteFile(os.Getenv("ROOT_PID_FILE"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			os.Exit(31)
		}
		for {
			if _, err := os.Stat(os.Getenv("OWNER_EXIT_FILE")); err == nil {
				// Deliberately bypass Go cleanup: the OS must close the job.
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if mode == "tree" || mode == "tree-exit" || mode == "wide-tree" {
		count := 1
		if mode == "wide-tree" {
			count = 20
		}
		var pids []string
		for range count {
			child := jobHelperCommand("leaf")
			if err := child.Start(); err != nil {
				os.Exit(32)
			}
			pids = append(pids, strconv.Itoa(child.Process.Pid))
		}
		if err := os.WriteFile(os.Getenv("LEAF_PID_FILE"), []byte(strings.Join(pids, "\n")), 0600); err != nil {
			os.Exit(33)
		}
		if mode == "tree-exit" {
			os.Exit(0)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}

func jobHelperCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsJobHelper$")
	cmd.Env = append(os.Environ(), "ISOLATEX_JOB_TEST_MODE="+mode)
	return cmd
}

func jobTestProcess(t *testing.T, mode string) (*exec.Cmd, *processControl, <-chan struct{}, string) {
	t.Helper()
	leafFile := filepath.Join(t.TempDir(), "leaf.pid")
	cmd := jobHelperCommand(mode)
	cmd.Env = append(cmd.Env, "LEAF_PID_FILE="+leafFile)
	control, err := startManagedProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		control.signal(true)
		control.close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("managed helper did not exit")
		}
	})
	return cmd, control, done, leafFile
}

func jobTestHandle(t *testing.T, pid int) windows.Handle {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Failure cleanup uses a retained process handle, never a reusable PID.
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	})
	return h
}

func jobTestPID(t *testing.T, filename string) int {
	t.Helper()
	pid := 0
	waitFor(t, func() bool {
		data, _ := os.ReadFile(filename)
		pid, _ = strconv.Atoi(string(data))
		return pid > 0
	})
	return pid
}

func requireJobProcessState(t *testing.T, process windows.Handle, stopped bool) {
	t.Helper()
	wait := uint32(0)
	if stopped {
		wait = 3000
	}
	state, err := windows.WaitForSingleObject(process, wait)
	want := uint32(windows.WAIT_TIMEOUT)
	if stopped {
		want = windows.WAIT_OBJECT_0
	}
	if err != nil || state != want {
		t.Fatalf("process stopped=%v: wait=%d want=%d error=%v", stopped, state, want, err)
	}
}

func TestWindowsJobStopsItsTreeAndPreservesAnotherWorker(t *testing.T) {
	root, control, done, leafFile := jobTestProcess(t, "tree")
	rootHandle := jobTestHandle(t, root.Process.Pid)
	leafHandle := jobTestHandle(t, jobTestPID(t, leafFile))
	other, _, _, _ := jobTestProcess(t, "leaf")
	otherHandle := jobTestHandle(t, other.Process.Pid)
	control.signal(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("root survived job stop")
	}
	requireJobProcessState(t, rootHandle, true)
	requireJobProcessState(t, leafHandle, true)
	requireJobProcessState(t, otherHandle, false)
}

func TestWindowsJobCloseRemovesDescendantAfterRootExit(t *testing.T) {
	_, control, done, leafFile := jobTestProcess(t, "tree-exit")
	leafHandle := jobTestHandle(t, jobTestPID(t, leafFile))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("root did not exit")
	}
	requireJobProcessState(t, leafHandle, false)
	control.close()
	control.close() // Repeated cleanup must not close a reused OS handle.
	requireJobProcessState(t, leafHandle, true)
}

func TestWindowsJobClosesWhenOwnerExitsWithoutCleanup(t *testing.T) {
	for _, abrupt := range []bool{false, true} {
		name := "exit"
		if abrupt {
			name = "terminate"
		}
		t.Run(name, func(t *testing.T) { checkJobOwnerExit(t, abrupt) })
	}
}

func TestWindowsOwnerJobClosesTheSuspendedStartupWindow(t *testing.T) {
	rootFile := filepath.Join(t.TempDir(), "suspended.pid")
	owner := jobHelperCommand("owner-before-attach")
	owner.Env = append(owner.Env, "ROOT_PID_FILE="+rootFile, "OWNER_EXIT_FILE="+filepath.Join(t.TempDir(), "unused"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = owner.Wait(); close(done) }()
	t.Cleanup(func() { _ = owner.Process.Kill(); <-done })
	suspended := jobTestHandle(t, jobTestPID(t, rootFile))
	requireJobProcessState(t, suspended, false)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	requireJobProcessState(t, suspended, true)
}

func TestWindowsManagedCrashReapsDescendantAndPreservesOtherGroup(t *testing.T) {
	c := fixture(t)
	filename := filepath.Join(t.TempDir(), "managed-child.pid")
	g := c.Bulkheads["isolated"]
	g.Process.Env["ISOLATEX_TEST_CHILD_PID_FILE"] = filename
	c.Bulkheads["isolated"] = g
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 && s.Pools["normal"].Ready() == 1 })
	before := s.Pools["isolated"].Snapshot()[0]
	normal := s.Pools["normal"].Snapshot()[0]
	oldChildPID := jobTestPID(t, filename)
	oldChild := jobTestHandle(t, oldChildPID)
	normalHandle := jobTestHandle(t, normal.PID)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	resp, _ := client.Post(before.URL.String()+"/?crash=1", "", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	waitFor(t, func() bool {
		current := s.Pools["isolated"].Snapshot()[0]
		return current.Ready && current.PID != before.PID && current.Restarts > before.Restarts
	})
	requireJobProcessState(t, oldChild, true)
	other := s.Pools["normal"].Snapshot()[0]
	if !other.Ready || other.PID != normal.PID || other.Restarts != normal.Restarts {
		t.Fatalf("other group changed: before=%+v after=%+v", normal, other)
	}
	requireJobProcessState(t, normalHandle, false)
	newChildPID := jobTestPID(t, filename)
	if newChildPID == oldChildPID {
		t.Fatal("replacement worker did not create a new descendant")
	}
	newChild := jobTestHandle(t, newChildPID)
	requireJobProcessState(t, newChild, false)
	s.Stop()
	requireJobProcessState(t, newChild, true)
	requireJobProcessState(t, normalHandle, true)
}

func checkJobOwnerExit(t *testing.T, abrupt bool) {
	t.Helper()
	dir := t.TempDir()
	rootFile, leafFile, exitFile := filepath.Join(dir, "root.pid"), filepath.Join(dir, "leaf.pid"), filepath.Join(dir, "exit")
	owner := jobHelperCommand("owner")
	owner.Env = append(owner.Env, "ROOT_PID_FILE="+rootFile, "LEAF_PID_FILE="+leafFile, "OWNER_EXIT_FILE="+exitFile)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = owner.Wait(); close(done) }()
	t.Cleanup(func() { _ = owner.Process.Kill(); <-done })
	rootHandle := jobTestHandle(t, jobTestPID(t, rootFile))
	leafHandle := jobTestHandle(t, jobTestPID(t, leafFile))
	requireJobProcessState(t, rootHandle, false)
	requireJobProcessState(t, leafHandle, false)
	var err error
	if abrupt {
		err = owner.Process.Kill()
	} else {
		err = os.WriteFile(exitFile, []byte("exit"), 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job owner did not exit")
	}
	requireJobProcessState(t, rootHandle, true)
	requireJobProcessState(t, leafHandle, true)
}
