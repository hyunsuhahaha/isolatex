//go:build linux || windows

package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/gateway"
)

func TestWorkerMemoryBudgetContainsAllocationFailure(t *testing.T) {
	parent := ""
	if runtime.GOOS == "linux" {
		parent = os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
		if parent == "" {
			t.Skip("set ISOLATEX_TEST_CGROUP_PARENT to a delegated memory-enabled cgroup v2 parent")
		}
	}
	for _, scenario := range []struct {
		name, query string
		budget      int64
		limited     bool
	}{
		{"unlimited_parent_control", "1", 64 << 20, false},
		{"limited_parent", "1", 64 << 20, true},
		{"unlimited_descendant_control", "tree", 256 << 20, false},
		{"limited_descendants", "tree", 256 << 20, true},
	} {
		limited := scenario.limited
		t.Run(scenario.name, func(t *testing.T) {
			c := fixture(t)
			c.MaxConnections = 1024
			c.Version, c.Listen, c.AdminListen = 1, "127.0.0.1:8080", "127.0.0.1:9090"
			c.MaxAdmitted, c.MaxBodyBytes, c.ShutdownGrace = 8, 1<<20, time.Second
			g := c.Bulkheads["isolated"]
			g.Process.Env["ISOLATEX_TEST_MEMORY"] = "1"
			childFile := filepath.Join(t.TempDir(), "memory-child.pid")
			g.Process.Env["MEMORY_CHILD_PID_FILE"] = childFile
			g.RequestTimeout = 3 * time.Second
			if limited {
				g.Process.Resources = &config.Resources{MemoryMaxBytes: config.ResourceLimit(scenario.budget), CgroupParent: parent}
			}
			c.Bulkheads["isolated"] = g
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := New(c, logger)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 && s.Pools["normal"].Ready() == 1 })
			before := s.Pools["isolated"].Snapshot()[0]
			normal := s.Pools["normal"].Snapshot()[0]
			gw := gateway.New(c, s.Pools, logger)
			defer gw.Close()
			server := httptest.NewServer(gw)
			defer server.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var successes, failures atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				for ctx.Err() == nil {
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+c.Bulkheads["normal"].Match.PathPrefix+"/work", nil)
					resp, err := client.Do(req)
					if err == nil {
						_, err = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
					}
					if ctx.Err() != nil {
						return
					}
					if err != nil || resp.StatusCode != 200 {
						failures.Add(1)
					} else {
						successes.Add(1)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			defer func() { cancel(); <-done }()
			waitFor(t, func() bool { return successes.Load() > 0 })
			started := successes.Load()
			// A failed allocation may kill a worker before any response. POST
			// prevents an automatic GET replay into a replacement generation.
			resp, err := client.Post(server.URL+g.Match.PathPrefix+"/work?memory="+scenario.query, "", nil)
			status := 0
			if resp != nil {
				status = resp.StatusCode
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if limited {
				if err == nil && status == 200 {
					t.Fatal("allocation succeeded beyond worker tree memory budget")
				}
				waitFor(t, func() bool {
					w := s.Pools["isolated"].Snapshot()[0]
					return w.Ready && w.PID != before.PID && w.Restarts > 0
				})
			} else if err != nil || status != 200 {
				t.Fatalf("unlimited allocation control: status=%d error=%v", status, err)
			}
			if scenario.query == "tree" {
				if _, err := os.Stat(childFile); err != nil {
					t.Fatalf("descendant did not execute memory helper: %v", err)
				}
			}
			waitFor(t, func() bool { return successes.Load() > started })
			other := s.Pools["normal"].Snapshot()[0]
			if !other.Ready || other.PID != normal.PID || other.Restarts != 0 || failures.Load() != 0 {
				t.Fatalf("memory fault affected protected group: worker=%+v failures=%d", other, failures.Load())
			}
		})
	}
}
