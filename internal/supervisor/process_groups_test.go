//go:build linux || darwin

package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessGroupContainsDescendantsAndPreservesAnotherWorker(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "managed_group"
		if legacy {
			name = "legacy_direct_child_negative_control"
		}
		t.Run(name, func(t *testing.T) { checkProcessGroup(t, legacy) })
	}
}

func checkProcessGroup(t *testing.T, legacy bool) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `sleep 60 & echo $!; wait`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var control *processControl
	if legacy {
		err = cmd.Start()
		control = &processControl{signal: func(bool) { _ = cmd.Process.Kill() }, close: func() error { return nil }}
	} else {
		control, err = startManagedProcess(cmd)
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	child, childStopped := 0, false
	t.Cleanup(func() {
		control.signal(true)
		if child > 0 && !childStopped {
			// The negative control intentionally leaves this known sleep alive.
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("root shell was not reaped")
		}
		control.close()
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("descendant PID missing: %v", scanner.Err())
	}
	child, err = strconv.Atoi(scanner.Text())
	if err != nil || child < 1 {
		t.Fatalf("invalid descendant PID: %q", scanner.Text())
	}
	group, err := syscall.Getpgid(child)
	if err != nil || (!legacy && group != cmd.Process.Pid) || (legacy && group == cmd.Process.Pid) {
		t.Fatalf("unexpected process group: child=%d root=%d group=%d legacy=%v error=%v", child, cmd.Process.Pid, group, legacy, err)
	}
	other := exec.Command("sleep", "60")
	otherControl, err := startManagedProcess(other)
	if err != nil {
		t.Fatal(err)
	}
	otherDone := make(chan struct{})
	go func() { _ = other.Wait(); close(otherDone) }()
	t.Cleanup(func() {
		otherControl.signal(true)
		select {
		case <-otherDone:
		case <-time.After(5 * time.Second):
			_ = other.Process.Kill()
			<-otherDone
			t.Error("other worker was not reaped by group cleanup")
		}
		otherControl.close()
	})
	control.signal(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("root shell survived termination")
	}
	if legacy {
		stopped, err := processGroupDescendantStopped(child)
		if err != nil || stopped {
			t.Fatalf("negative control did not leave a live descendant: stopped=%v error=%v", stopped, err)
		}
		t.Log("direct-child termination left the descendant alive")
	} else {
		waitForGroupDescendantStop(t, child, "group termination")
		childStopped = true
	}
	control.close()
	control.signal(true) // A closed generation must not signal a future group.
	if err := syscall.Kill(other.Process.Pid, 0); err != nil {
		t.Fatalf("another worker was terminated: %v", err)
	}
}

func processGroupDescendantStopped(pid int) (bool, error) {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		_, tail, ok := strings.Cut(string(data), ") ")
		return ok && strings.HasPrefix(tail, "Z "), nil
	}
	// macOS has no /proc; an exited orphan may briefly await launchd reaping.
	output, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(output))
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 && state == "" {
			return true, nil
		}
		return false, fmt.Errorf("read descendant state: %w", err)
	}
	return strings.HasPrefix(state, "Z"), nil
}

func waitForGroupDescendantStop(t *testing.T, pid int, phase string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		stopped, err := processGroupDescendantStopped(pid)
		if err != nil {
			t.Fatalf("%s descendant state: %v", phase, err)
		}
		if stopped {
			return
		}
		if time.Now().After(deadline) {
			group, _ := syscall.Getpgid(pid)
			t.Fatalf("%s: descendant %d in group %d survived termination", phase, pid, group)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagedGroupReapsTermResistantDescendantOnCrashAndStop(t *testing.T) {
	c := fixture(t)
	filename := filepath.Join(t.TempDir(), "resistant-child.pid")
	g := c.Bulkheads["isolated"]
	g.Process.Env["ISOLATEX_TEST_CHILD_PID_FILE"] = filename
	g.Process.Env["ISOLATEX_TEST_CHILD_MODE"] = "ignore-term"
	c.Bulkheads["isolated"] = g
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	children := map[int]bool{}
	t.Cleanup(func() {
		s.Stop()
		for pid := range children {
			if stopped, _ := processGroupDescendantStopped(pid); !stopped {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 && s.Pools["normal"].Ready() == 1 })
	childPID := func(previous, group int) int {
		t.Helper()
		pid := 0
		waitFor(t, func() bool {
			data, _ := os.ReadFile(filename)
			pid, _ = strconv.Atoi(string(data))
			if pid <= 0 || pid == previous {
				return false
			}
			actualGroup, err := syscall.Getpgid(pid)
			return err == nil && actualGroup == group
		})
		children[pid] = true
		return pid
	}
	before := s.Pools["isolated"].Snapshot()[0]
	normal := s.Pools["normal"].Snapshot()[0]
	oldChild := childPID(0, before.PID)
	if pgid, err := syscall.Getpgid(oldChild); err != nil || pgid != before.PID {
		t.Fatalf("descendant outside worker group: %d %v", pgid, err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	resp, _ := client.Post(before.URL.String()+"/?crash=1", "", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	waitFor(t, func() bool {
		current := s.Pools["isolated"].Snapshot()[0]
		return current.Ready && current.PID != before.PID && current.Restarts > before.Restarts
	})
	waitForGroupDescendantStop(t, oldChild, "restart")
	delete(children, oldChild)
	other := s.Pools["normal"].Snapshot()[0]
	if !other.Ready || other.PID != normal.PID || other.Restarts != normal.Restarts {
		t.Fatalf("another group changed: before=%+v after=%+v", normal, other)
	}
	resp, err := client.Get(normal.URL.String() + "/work")
	if err != nil {
		t.Fatal(err)
	}
	var identity map[string]int
	err = json.NewDecoder(resp.Body).Decode(&identity)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || identity["pid"] != normal.PID {
		t.Fatalf("protected worker failed: status=%d identity=%v error=%v", resp.StatusCode, identity, err)
	}
	newChild := childPID(oldChild, s.Pools["isolated"].Snapshot()[0].PID)
	s.Stop()
	waitForGroupDescendantStop(t, newChild, "Stop")
	delete(children, newChild)
}
