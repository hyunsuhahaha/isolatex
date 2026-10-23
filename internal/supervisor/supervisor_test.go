package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"isolatex/internal/config"
	"isolatex/internal/gateway"
)

// Launch this test binary as a real managed HTTP worker, without another build.
func TestWorkerHelper(t *testing.T) {
	if os.Getenv("ISOLATEX_TEST_WORKER") != "1" {
		return
	}
	if os.Getenv("FAIL_START") == "1" {
		os.Exit(23)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if filename := os.Getenv("HEALTH_FILE"); filename != "" {
			data, _ := os.ReadFile(filename)
			if string(data) != "healthy" {
				w.WriteHeader(503)
				return
			}
		}
		if os.Getenv("NOT_READY") == "1" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("crash") == "1" {
			os.Exit(23)
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"pid": os.Getpid()})
	})
	if err := http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), mux); err != nil {
		os.Exit(24)
	}
	os.Exit(0)
}

func TestManagedHealthFailureAndRecoveryKeepsPID(t *testing.T) {
	c := fixture(t)
	filename := filepath.Join(t.TempDir(), "health")
	if err := os.WriteFile(filename, []byte("healthy"), 0600); err != nil {
		t.Fatal(err)
	}
	g := c.Bulkheads["isolated"]
	g.Process.Env["HEALTH_FILE"] = filename
	g.Readiness.FailureThreshold = 2
	g.Readiness.SuccessThreshold = 2
	c.Bulkheads["isolated"] = g
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 && s.Pools["isolated"].Ready() == 1 })
	original := s.Pools["isolated"].Snapshot()[0].PID
	if err := os.WriteFile(filename, []byte("unhealthy"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Pools["isolated"].Snapshot()[0].State == "unhealthy" })
	w := s.Pools["isolated"].Snapshot()[0]
	if w.Ready || w.PID != original || w.Restarts != 0 || w.HealthFailures < 2 {
		t.Fatalf("health failure restarted worker: %+v", w)
	}
	if s.Pools["normal"].Ready() != 1 {
		t.Fatal("normal pool affected")
	}
	if err := os.WriteFile(filename, []byte("healthy"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 })
	w = s.Pools["isolated"].Snapshot()[0]
	if w.PID != original || w.Restarts != 0 || w.ConsecutiveSuccesses < 2 {
		t.Fatalf("invalid recovery: %+v", w)
	}
}

func TestStaticUpstreamHealthMonitoring(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(503)
		}
	}))
	defer up.Close()
	g := config.Group{Upstreams: []string{up.URL}, Readiness: config.Readiness{Path: "/readyz", Interval: 10 * time.Millisecond, Timeout: 50 * time.Millisecond, FailureThreshold: 2, SuccessThreshold: 2}}
	s := New(config.Config{Bulkheads: map[string]config.Group{"isolated": g}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 })
	healthy.Store(false)
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 0 })
	healthy.Store(true)
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 })
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for !f() {
		if time.Now().After(until) {
			t.Fatal("worker condition timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fixture(t *testing.T) config.Config {
	t.Helper()
	c := config.Config{MaxAdmitted: 16, MaxBodyBytes: 1024, Bulkheads: map[string]config.Group{}}
	used := map[int]bool{}
	for _, name := range []string{"normal", "isolated"} {
		port := freePort(t)
		for used[port] {
			port = freePort(t)
		}
		used[port] = true
		g := config.Group{Process: &config.Process{Command: []string{os.Args[0], "-test.run=^TestWorkerHelper$"}, Workers: 1, PortStart: port, Env: map[string]string{"ISOLATEX_TEST_WORKER": "1"}}, MaxConcurrent: 2, QueueSize: 2, QueueTimeout: time.Second, RequestTimeout: time.Second, Readiness: config.Readiness{Path: "/readyz", Interval: 10 * time.Millisecond, Timeout: 100 * time.Millisecond, StartupTimeout: time.Second}}
		g.Match.PathPrefix = "/api"
		if name == "isolated" {
			g.Match.PathPrefix = "/api/fault"
		}
		c.Bulkheads[name] = g
	}
	return c
}

func TestManagedCrashIsolationRestartAndStop(t *testing.T) {
	c := fixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(c, logger)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 && s.Pools["isolated"].Ready() == 1 })
	normalPID := s.Pools["normal"].Snapshot()[0].PID
	isolatedPID := s.Pools["isolated"].Snapshot()[0].PID
	g := gateway.New(c, s.Pools, logger)
	defer g.Close()
	server := httptest.NewServer(g)
	defer server.Close()
	resp, err := server.Client().Get(server.URL + "/api/fault?crash=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("crash status=%d", resp.StatusCode)
	}
	resp, err = server.Client().Get(server.URL + "/api/orders")
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ PID int }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 200 || body.PID != normalPID {
		t.Fatalf("normal affected: %d %+v", resp.StatusCode, body)
	}
	waitFor(t, func() bool {
		w := s.Pools["isolated"].Snapshot()[0]
		return w.Ready && w.PID != isolatedPID && w.Restarts == 1
	})
	g.Drain()
	s.Stop()
	for name, p := range s.Pools {
		if p.Ready() != 0 {
			t.Fatalf("%s remained ready", name)
		}
		for _, worker := range p.Snapshot() {
			if worker.PID != 0 || worker.State != "stopped" {
				t.Fatalf("worker not stopped: %+v", worker)
			}
		}
	}
	for _, b := range c.Bulkheads {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", b.Process.PortStart))
		if err != nil {
			t.Fatalf("port leaked: %v", err)
		}
		ln.Close()
	}
}

func TestPortConflictSpawnsNothing(t *testing.T) {
	c := fixture(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.Bulkheads["normal"].Process.PortStart))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err == nil {
		defer s.Stop()
		t.Fatal("port conflict accepted")
	}
	for _, p := range s.Pools {
		if p.Snapshot()[0].PID != 0 {
			t.Fatal("spawned before preflight completed")
		}
	}
}

func TestStopDuringFailedStartup(t *testing.T) {
	c := fixture(t)
	for _, g := range c.Bulkheads {
		g.Process.Command = []string{"isolatex-intentionally-missing-command"}
	}
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Pools["normal"].Snapshot()[0].State == "backoff" })
	s.Stop()
	for _, p := range s.Pools {
		w := p.Snapshot()[0]
		if w.State != "stopped" || w.Restarts != 0 {
			t.Fatalf("restarted on shutdown: %+v", w)
		}
	}
}

func TestWorkerEnvironment(t *testing.T) {
	t.Setenv("PORT", "1234")
	env := workerEnv(map[string]string{"APP_POOL": "isolated", "custom_key": "present"}, 9876)
	ports := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "PORT=") {
			ports++
			if entry != "PORT="+strconv.Itoa(9876) {
				t.Fatal(entry)
			}
		}
	}
	if ports != 1 {
		t.Fatalf("PORT entries=%d", ports)
	}
}

func TestStartupDeadlineBoundsSlowReadiness(t *testing.T) {
	c := fixture(t)
	delete(c.Bulkheads, "isolated")
	b := c.Bulkheads["normal"]
	b.Process.Env["NOT_READY"] = "1"
	b.Readiness.StartupTimeout = 250 * time.Millisecond
	b.Readiness.Timeout = 5 * time.Second
	c.Bulkheads["normal"] = b
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	start := time.Now()
	waitFor(t, func() bool { return s.Pools["normal"].Snapshot()[0].State == "backoff" })
	if time.Since(start) > 2*time.Second {
		t.Fatal("probe exceeded startup deadline")
	}
	if s.Pools["normal"].Ready() != 0 {
		t.Fatal("unready process received traffic")
	}
}

func TestCrashLoopExhaustsRestartBudget(t *testing.T) {
	c := fixture(t)
	delete(c.Bulkheads, "isolated")
	c.Bulkheads["normal"].Process.Env["FAIL_START"] = "1"
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	deadline := time.Now().Add(40 * time.Second)
	for {
		w := s.Pools["normal"].Snapshot()[0]
		if w.State == "failed" {
			if w.Restarts != 5 || w.Ready || w.PID != 0 {
				t.Fatalf("invalid exhausted state: %+v", w)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart loop did not stop: %+v", w)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
