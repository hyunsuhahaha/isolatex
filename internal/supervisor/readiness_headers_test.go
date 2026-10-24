package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestManagedReadinessRejectsOversizedHeadersWithoutRestart(t *testing.T) {
	c := fixture(t)
	filename := filepath.Join(t.TempDir(), "header")
	writeHeader := func(value string) {
		t.Helper()
		if err := os.WriteFile(filename, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeHeader("small")
	g := c.Bulkheads["isolated"]
	g.Process.Env["HEALTH_HEADER_FILE"] = filename
	g.Readiness.FailureThreshold, g.Readiness.SuccessThreshold = 2, 2
	c.Bulkheads["isolated"] = g
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 && s.Pools["normal"].Ready() == 1 })
	before := s.Pools["isolated"].Snapshot()[0]
	normal := s.Pools["normal"].Snapshot()[0]
	// Model a writer paused after truncation: these completed checks still
	// see a valid small response and cannot count toward header rejection.
	writeHeader("")
	waitFor(t, func() bool { return s.Pools["isolated"].Snapshot()[0].HealthChecks >= before.HealthChecks+5 })
	writeHeader(strings.Repeat("x", 1<<20))
	// Probes can complete while WriteFile is truncating/replacing the fixture
	// data. A total check count does not prove consecutive oversized failures.
	waitFor(t, func() bool {
		w := s.Pools["isolated"].Snapshot()[0]
		return w.ConsecutiveFailures >= 2 && w.HealthFailures >= before.HealthFailures+2 && s.Pools["normal"].Snapshot()[0].HealthChecks > normal.HealthChecks
	})
	bad := s.Pools["isolated"].Snapshot()[0]
	if bad.Ready || bad.HealthFailures < 2 || bad.PID != before.PID || bad.Restarts != 0 {
		t.Fatalf("oversized readiness response accepted or restarted worker: %+v", bad)
	}
	other := s.Pools["normal"].Snapshot()[0]
	if !other.Ready || other.PID != normal.PID || other.Restarts != normal.Restarts || other.HealthChecks <= normal.HealthChecks {
		t.Fatalf("other group stopped progressing: before=%+v after=%+v", normal, other)
	}
	writeHeader("small")
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 })
	recovered := s.Pools["isolated"].Snapshot()[0]
	if recovered.PID != before.PID || recovered.Restarts != 0 {
		t.Fatalf("recovery changed process: %+v", recovered)
	}
}

func TestStaticReadinessRejectsOversizedHeadersAndRecovers(t *testing.T) {
	var oversized atomic.Bool
	largeHeader := strings.Repeat("x", 1<<20)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if oversized.Load() {
			w.Header().Set("X-Readiness", largeHeader)
		}
		w.WriteHeader(200)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer good.Close()
	c := config.Config{Bulkheads: map[string]config.Group{}}
	for name, origin := range map[string]string{"normal": good.URL, "isolated": bad.URL} {
		c.Bulkheads[name] = config.Group{Upstreams: []string{origin}, Readiness: config.Readiness{Path: "/readyz", Interval: 10 * time.Millisecond, Timeout: time.Second, FailureThreshold: 2, SuccessThreshold: 2}}
	}
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 && s.Pools["normal"].Ready() == 1 })
	before := s.Pools["isolated"].Snapshot()[0]
	normalChecks := s.Pools["normal"].Snapshot()[0].HealthChecks
	oversized.Store(true)
	waitFor(t, func() bool { return s.Pools["isolated"].Snapshot()[0].HealthChecks >= before.HealthChecks+5 })
	failed := s.Pools["isolated"].Snapshot()[0]
	if failed.Ready || failed.HealthFailures < 2 {
		t.Fatalf("oversized readiness response accepted: %+v", failed)
	}
	if other := s.Pools["normal"].Snapshot()[0]; !other.Ready || other.HealthChecks <= normalChecks {
		t.Fatalf("other monitor stopped progressing: %+v", other)
	}
	oversized.Store(false)
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 })
}
