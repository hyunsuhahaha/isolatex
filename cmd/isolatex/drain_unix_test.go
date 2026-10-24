//go:build unix

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"gopkg.in/yaml.v3"
)

// The runtime starts this test executable as an actual managed child. A file
// barrier keeps the request in flight until the parent observes draining.
func TestDrainWorkerHelper(t *testing.T) {
	if os.Getenv("ISOLATEX_DRAIN_WORKER") != "1" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hold") == "1" {
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(os.Getenv("RELEASE_FILE")); err == nil {
					break
				}
				select {
				case <-r.Context().Done():
					return
				case <-tick.C:
				}
			}
		}
		_, _ = fmt.Fprintf(w, "worker=%d", os.Getpid())
	})
	if err := http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), mux); err != nil {
		os.Exit(24)
	}
}

func TestNativeCLIShutdownWithActiveAndQueuedRequests(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "isolatex")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	for _, finish := range []bool{true, false} {
		name := "grace_expires"
		if finish {
			name = "finishes_within_grace"
		}
		t.Run(name, func(t *testing.T) { checkNativeDrain(t, binary, finish) })
	}
}

func checkNativeDrain(t *testing.T, binary string, finish bool) {
	t.Helper()
	dir := t.TempDir()
	logfile, err := os.Create(filepath.Join(dir, "runtime.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = logfile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logfile.Name())
			t.Logf("runtime log:\n%s", data)
		}
	})
	ports := make([]int, 4)
	// Hold all reservations together so the kernel cannot return the same
	// ephemeral port twice. Supervisor still checks the unavoidable bind race.
	var reservations []net.Listener
	for i := range ports {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, reservation := range reservations {
				_ = reservation.Close()
			}
			t.Fatal(err)
		}
		reservations = append(reservations, ln)
		ports[i] = ln.Addr().(*net.TCPAddr).Port
	}
	for _, ln := range reservations {
		_ = ln.Close()
	}
	address := func(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }
	release := filepath.Join(dir, "release")
	grace := 5 * time.Second
	if !finish {
		grace = 250 * time.Millisecond
	}
	c := config.Config{Version: 1, Listen: address(ports[0]), AdminListen: address(ports[1]), MaxConnections: 1024, MaxAdmitted: 4, MaxBodyBytes: 1024, ShutdownGrace: grace, Bulkheads: map[string]config.Group{}}
	for i, name := range []string{"alpha", "beta"} {
		g := config.Group{
			Process:       &config.Process{Command: []string{os.Args[0], "-test.run=^TestDrainWorkerHelper$"}, Workers: 1, PortStart: ports[i+2], Env: map[string]string{"ISOLATEX_DRAIN_WORKER": "1", "RELEASE_FILE": release}},
			MaxConcurrent: 1, QueueSize: 1, QueueTimeout: 15 * time.Second, RequestTimeout: 20 * time.Second,
			Readiness: config.Readiness{Path: "/readyz", Interval: 10 * time.Millisecond, Timeout: time.Second, StartupTimeout: 5 * time.Second},
		}
		g.Match.PathPrefix = "/" + name
		c.Bulkheads[name] = g
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-config", filename)
	cmd.Stdout, cmd.Stderr = logfile, logfile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	var workerPIDs []int
	t.Cleanup(func() {
		if !exited {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			// Failure cleanup only: do not leave known helper children behind
			// if an assertion required forcibly stopping the runtime.
			for _, pid := range workerPIDs {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	tr := &http.Transport{DisableKeepAlives: true, Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	public, admin := "http://"+c.Listen, "http://"+c.AdminListen
	request := func(url string) (int, string, error) {
		resp, err := client.Get(url)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), err
	}
	wait := func(condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal("native runtime condition timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(func() bool { code, _, _ := request(admin + "/readyz"); return code == 200 })
	type groupState struct {
		Active, Queued int
		Workers        []struct{ PID int }
	}
	status := func() map[string]groupState {
		_, body, _ := request(admin + "/status")
		var state struct{ Groups map[string]groupState }
		_ = json.Unmarshal([]byte(body), &state)
		return state.Groups
	}
	initial := status()
	for _, name := range []string{"alpha", "beta"} {
		if len(initial[name].Workers) != 1 || initial[name].Workers[0].PID <= 0 {
			t.Fatalf("missing worker PID for %s: %+v", name, initial)
		}
		workerPIDs = append(workerPIDs, initial[name].Workers[0].PID)
	}
	type result struct {
		code int
		body string
		err  error
	}
	active, queued := make(chan result, 1), make(chan result, 1)
	go func() { code, body, err := request(public + "/alpha?hold=1"); active <- result{code, body, err} }()
	wait(func() bool { return status()["alpha"].Active == 1 })
	go func() { code, body, err := request(public + "/alpha"); queued <- result{code, body, err} }()
	wait(func() bool { return status()["alpha"].Queued == 1 })
	// A different managed process must still serve before shutdown begins.
	if code, _, err := request(public + "/beta"); err != nil || code != 200 {
		t.Fatalf("other group before drain: %d %v", code, err)
	}
	started := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-queued:
		if got.err != nil || got.code != 503 || got.body != "{\"error\":\"draining\"}\n" {
			t.Fatalf("queued request during drain: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request was not rejected promptly")
	}
	if finish {
		// Queue rejection is emitted only after Drain closes the gate. The
		// active worker must still be alive until its request is released.
		if err := syscall.Kill(initial["alpha"].Workers[0].PID, 0); err != nil {
			t.Fatalf("worker stopped before active request completed: %v", err)
		}
		if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-active:
		if finish && (got.err != nil || got.code != 200 || got.body != fmt.Sprintf("worker=%d", initial["alpha"].Workers[0].PID)) {
			t.Fatalf("active request lost within grace: %+v", got)
		}
		if !finish && got.err == nil && got.code == 200 {
			t.Fatalf("unreleased request succeeded after grace: %+v", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("active request survived shutdown grace")
	}
	select {
	case err := <-done:
		exited = true
		if err != nil {
			t.Fatalf("runtime exit: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runtime did not stop after drain")
	}
	elapsed := time.Since(started)
	if elapsed > grace+4*time.Second {
		t.Fatalf("shutdown exceeded grace and worker stop bound: %v", elapsed)
	}
	if !finish && elapsed < grace {
		t.Fatalf("active request aborted before grace: %v < %v", elapsed, grace)
	}
	logs, err := os.ReadFile(logfile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if exhausted := strings.Contains(string(logs), `"msg":"drain_deadline"`); exhausted == finish {
		t.Fatalf("drain deadline log = %v, finished within grace = %v", exhausted, finish)
	}
	if !strings.Contains(string(logs), `"msg":"workers_stopped"`) {
		t.Fatal("runtime exited without stopping supervisor")
	}
	for name, state := range initial {
		if err := syscall.Kill(state.Workers[0].PID, 0); err != syscall.ESRCH {
			t.Fatalf("%s worker still exists after runtime exit: %v", name, err)
		}
	}
	for _, port := range ports {
		ln, err := net.Listen("tcp", address(port))
		if err != nil {
			t.Fatalf("port %d not released: %v", port, err)
		}
		_ = ln.Close()
	}
}
