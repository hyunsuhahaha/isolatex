//go:build linux

package supervisor

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"golang.org/x/sys/unix"
)

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
