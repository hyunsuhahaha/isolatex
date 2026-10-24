//go:build linux

package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"golang.org/x/sys/unix"
)

func TestMemoryBudgetFailedExecReleasesCgroup(t *testing.T) {
	parent := os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
	if parent == "" {
		t.Skip("set ISOLATEX_TEST_CGROUP_PARENT to a delegated cgroup")
	}
	before, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing-worker"))
	control, err := startManagedProcessWithResources(cmd, &config.Resources{MemoryMaxBytes: 64 << 20, CgroupParent: parent})
	if err == nil {
		control.signal(true)
		_ = cmd.Wait()
		_ = control.close()
		t.Fatal("missing executable unexpectedly started")
	}
	if control != nil || !errors.Is(err, os.ErrNotExist) || errors.Is(err, errResourceCleanup) {
		t.Fatalf("clean failed exec must preserve a retryable exec error: control=%v error=%v", control, err)
	}
	after, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("failed exec left a worker cgroup: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatalf("failed exec changed cgroup contents: before=%v after=%v", before, after)
		}
	}
}

func TestMemoryBudgetRejectsOrdinaryDirectoryBeforeWorkerStarts(t *testing.T) {
	parent := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	cmd := exec.Command("sh", "-c", `printf started > "$1"`, "isolatex-marker", marker)
	control, err := startManagedProcessWithResources(cmd, &config.Resources{MemoryMaxBytes: 64 << 20, CgroupParent: parent})
	if err == nil {
		control.signal(true)
		_ = cmd.Wait()
		_ = control.close()
		t.Fatal("memory budget silently ignored on ordinary filesystem")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker executed despite unenforced budget: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed preflight left files: %v %+v", err, entries)
	}
}

func TestMemoryCgroupContainsEscapedDescendant(t *testing.T) {
	parent := os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
	if parent == "" {
		t.Skip("requires a delegated cgroup v2 parent")
	}
	resources := &config.Resources{MemoryMaxBytes: 64 << 20, CgroupParent: parent}
	cmd := exec.Command("sh", "-c", `setsid sh -c 'echo "$$"; exec sleep 60' & wait`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	control, err := startManagedProcessWithResources(cmd, resources)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	childFD := -1
	t.Cleanup(func() {
		control.signal(true)
		// Retain process identity for failure cleanup, even if a broken
		// implementation leaves this session outside the parent's group.
		if childFD >= 0 {
			_ = unix.PidfdSendSignal(childFD, unix.SIGKILL, nil, 0)
			_ = unix.Close(childFD)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("root not reaped")
		}
		if err := control.close(); err != nil {
			t.Error(err)
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("missing escaped child PID: %v", scanner.Err())
	}
	child, err := strconv.Atoi(scanner.Text())
	if err != nil || child < 1 {
		t.Fatalf("bad child PID: %q", scanner.Text())
	}
	childFD, err = unix.PidfdOpen(child, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { pgid, err := unix.Getpgid(child); return err == nil && pgid == child })
	rootGroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	childGroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", child))
	if err != nil || !bytes.Equal(rootGroup, childGroup) {
		t.Fatalf("descendant escaped cgroup too: root=%q child=%q error=%v", rootGroup, childGroup, err)
	}
	other := exec.Command("sleep", "60")
	otherControl, err := startManagedProcessWithResources(other, resources)
	if err != nil {
		t.Fatal(err)
	}
	otherDone := make(chan struct{})
	go func() { _ = other.Wait(); close(otherDone) }()
	t.Cleanup(func() {
		otherControl.signal(true)
		<-otherDone
		if err := otherControl.close(); err != nil {
			t.Error(err)
		}
	})
	control.signal(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("root survived force stop")
	}
	waitForGroupDescendantStop(t, child, "cgroup force stop after setsid")
	if err := control.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-otherDone:
		t.Fatal("another worker cgroup was killed")
	default:
	}
}

// LockOSThread creates kernel tasks rather than merely Go goroutines.
func TestTaskThreadHelper(t *testing.T) {
	if os.Getenv("ISOLATEX_THREAD_HELPER") != "1" {
		return
	}
	fmt.Println("thread-helper-started")
	membership, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		os.Exit(25)
	}
	fmt.Printf("cgroup:%s", membership)
	release := make(chan struct{})
	started := make(chan struct{})
	for i := 0; i < 64; i++ {
		go func() { runtime.LockOSThread(); started <- struct{}{}; <-release; runtime.UnlockOSThread() }()
		<-started
	}
	fmt.Println("thread-helper-completed")
	close(release)
	os.Exit(0)
}

func TestTasksBudgetContainsThreadCreation(t *testing.T) {
	parent := os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
	if parent == "" {
		t.Skip("requires delegated pids controller")
	}
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTaskThreadHelper$")
			cmd.Env = append(os.Environ(), "ISOLATEX_THREAD_HELPER=1", "GOMAXPROCS=2")
			var resources *config.Resources
			if limited {
				resources = &config.Resources{TasksMax: 16, CgroupParent: parent}
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			control, err := startManagedProcessWithResources(cmd, resources)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				control.signal(true)
				if err := control.close(); err != nil {
					t.Error(err)
				}
			}()
			err = cmd.Wait()
			if ctx.Err() != nil {
				t.Fatal("thread helper timed out")
			}
			text := output.String()
			if !strings.Contains(text, "thread-helper-started") {
				t.Fatalf("helper never executed: %v", err)
			}
			if limited {
				if err == nil || strings.Contains(text, "thread-helper-completed") {
					t.Fatalf("thread budget not enforced: %v", err)
				}
				var path string
				for _, line := range strings.Split(text, "\n") {
					if strings.HasPrefix(line, "cgroup:0::") {
						path = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "cgroup:0::"))
					}
				}
				if path == "" {
					t.Fatal("helper did not report cgroup")
				}
				events, readErr := os.ReadFile(filepath.Join(path, "pids.events"))
				fields := strings.Fields(string(events))
				if readErr != nil || len(fields) != 2 || fields[0] != "max" || fields[1] == "0" {
					t.Fatalf("no kernel pids refusal: %q error=%v", events, readErr)
				}
				memory, readErr := os.ReadFile(filepath.Join(path, "memory.max"))
				if readErr == nil && strings.TrimSpace(string(memory)) != "max" {
					t.Fatalf("count-only budget imposed memory limit: %q", memory)
				}
			} else if err != nil || !strings.Contains(text, "thread-helper-completed") {
				t.Fatalf("unlimited control failed: error=%v output=%s", err, text)
			}
		})
	}
}

func TestTasksBudgetMissingControllerDoesNotExecuteWorker(t *testing.T) {
	parent := os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
	if parent == "" {
		t.Skip("requires delegation")
	}
	// Do not enable pids for this subtree; the private worker must fail closed.
	undelegated, err := os.MkdirTemp(parent, "isolatex-test-nopids-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Remove(undelegated); err != nil {
			t.Error(err)
		}
	}()
	marker := filepath.Join(t.TempDir(), "executed")
	cmd := exec.Command("sh", "-c", `printf started > "$1"`, "isolatex-marker", marker)
	control, err := startManagedProcessWithResources(cmd, &config.Resources{TasksMax: 32, CgroupParent: undelegated})
	if err == nil {
		control.signal(true)
		_ = cmd.Wait()
		_ = control.close()
		t.Fatal("missing pids controller silently ignored")
	}
	if !strings.Contains(err.Error(), "pids.max") {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker executed without budget: %v", err)
	}
	entries, err := os.ReadDir(undelegated)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("failed start leaked cgroup %s", entry.Name())
		}
	}
}

func TestCPUBudgetMissingControllerDoesNotExecuteWorker(t *testing.T) {
	parent := os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
	if parent == "" {
		t.Skip("requires delegation")
	}
	// Do not enable cpu for this subtree; the private worker must fail closed.
	undelegated, err := os.MkdirTemp(parent, "isolatex-test-nocpu-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Remove(undelegated); err != nil {
			t.Error(err)
		}
	}()
	marker := filepath.Join(t.TempDir(), "executed")
	cmd := exec.Command("sh", "-c", `printf started > "$1"`, "isolatex-marker", marker)
	control, err := startManagedProcessWithResources(cmd, &config.Resources{CPUMillicores: 250, CgroupParent: undelegated})
	if err == nil {
		control.signal(true)
		_ = cmd.Wait()
		_ = control.close()
		t.Fatal("missing cpu controller silently ignored")
	}
	if !strings.Contains(err.Error(), "cpu.max") {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker executed without budget: %v", err)
	}
	entries, err := os.ReadDir(undelegated)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("failed start leaked cgroup %s", entry.Name())
		}
	}
}
