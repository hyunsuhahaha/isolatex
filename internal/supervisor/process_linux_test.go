//go:build linux

package supervisor

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLinuxStopTerminatesDescendants(t *testing.T) {
	cmd := exec.Command("sh", "-c", `sleep 60 & echo $!; wait`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	control, err := startManagedProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.signal(true); control.close() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("child PID missing")
	}
	child, err := strconv.Atoi(scanner.Text())
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(child)
	if err != nil || pgid != cmd.Process.Pid {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("child escaped process group: %d %v", pgid, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	_ = stopChild(control, done)
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(child) + "/stat")
		if os.IsNotExist(err) {
			return
		}
		// A dead descendant may briefly remain a zombie until adopted/reaped.
		if err == nil {
			_, tail, ok := strings.Cut(string(data), ") ")
			if ok && strings.HasPrefix(tail, "Z ") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived group termination", child)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLinuxCloseReclaimsProcessGroupWithoutPriorSignal(t *testing.T) {
	cmd := exec.Command("sh", "-c", `sleep 60 & echo $!; wait`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	control, err := startManagedProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	child := 0
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// Keep the unreaped root PID reserved until group cleanup finishes;
		// never signal a descendant PID that init may already have recycled.
		_ = control.close()
		_ = cmd.Wait()
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("child PID missing: %v", scanner.Err())
	}
	child, err = strconv.Atoi(scanner.Text())
	if err != nil || child < 1 {
		t.Fatal("invalid child PID")
	}
	if err := control.close(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{cmd.Process.Pid, child} {
		stopped, err := processGroupDescendantStopped(pid)
		if err != nil || !stopped {
			t.Fatalf("close returned while process %d remained live: stopped=%v error=%v", pid, stopped, err)
		}
	}
	if err := control.close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}
