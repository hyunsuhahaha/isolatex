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
	configureProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { signalProcess(cmd, true) })
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
	_ = stopChild(cmd, done)
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
