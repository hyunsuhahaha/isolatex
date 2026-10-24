package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/gateway"
	"github.com/hyunsuhahaha/isolatex/internal/pool"
)

func TestSmokeChecksRuntimeIdentityBeforeInjectingFaults(t *testing.T) {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binary := filepath.Join(t.TempDir(), "bench"+suffix)
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build smoke driver: %v\n%s", err, output)
	}
	for _, mode := range []string{"unverified_control", "wrong_runtime", "verified_runtime"} {
		t.Run(mode, func(t *testing.T) {
			var appRequests, crashes atomic.Int32
			var delayedDone atomic.Bool
			var drainObservations atomic.Int32
			pools := map[string]*pool.Pool{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				appRequests.Add(1)
				if r.URL.Query().Get("crash") == "1" {
					if drainObservations.Load() < 2 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					count := crashes.Add(1)
					// Model a replacement in the control plane without terminating
					// the test process that owns this HTTP fixture.
					pools["beta"].Set(0, true, 202+int(count), "ready")
					pools["beta"].Restart(0)
					w.WriteHeader(502)
					return
				}
				if delay, err := time.ParseDuration(r.URL.Query().Get("delay")); err == nil {
					time.Sleep(delay)
					delayedDone.Store(true)
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer upstream.Close()
			c := config.Config{Version: 1, Listen: "127.0.0.1:18080", AdminListen: "127.0.0.1:19090", MaxConnections: 1024, MaxAdmitted: 3, MaxBodyBytes: 1024, ShutdownGrace: time.Second, Bulkheads: map[string]config.Group{}}
			for i, name := range []string{"alpha", "beta"} {
				g := config.Group{Upstreams: []string{upstream.URL}, MaxConcurrent: 1, QueueTimeout: time.Second, RequestTimeout: 2 * time.Second}
				g.Match.PathPrefix = []string{"/a", "/b"}[i]
				if name == "alpha" {
					g.QueueSize = 1
				}
				c.Bulkheads[name] = g
				pools[name] = pool.New(g.Upstreams, true)
				pools[name].Set(0, true, 101*(i+1), "ready")
			}
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			g := gateway.New(c, pools, slog.New(slog.NewTextHandler(io.Discard, nil)))
			defer g.Close()
			adminHandler := g.Admin()
			admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status" || !delayedDone.Load() {
					adminHandler.ServeHTTP(w, r)
					return
				}
				// Receiving response EOF need not synchronize the server's final
				// slot accounting. Force a non-drained observation before recovery.
				recorder := httptest.NewRecorder()
				adminHandler.ServeHTTP(recorder, r)
				var current status
				if err := json.Unmarshal(recorder.Body.Bytes(), &current); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if drainObservations.Add(1) == 1 {
					group := current.Groups["beta"]
					group.Active = 1
					current.Groups["beta"] = group
				}
				_ = json.NewEncoder(w).Encode(current)
			}))
			public := httptest.NewServer(g)
			defer public.Close()
			defer admin.Close()
			args := []string{"-url", public.URL, "-admin", admin.URL}
			if mode != "unverified_control" {
				expected := os.Getpid()
				if mode == "wrong_runtime" {
					expected++
				}
				args = append(args, "-expected-runtime-pid", strconv.Itoa(expected))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
			if mode == "wrong_runtime" {
				if err == nil || !strings.Contains(string(output), "runtime identity mismatch") {
					t.Fatalf("wrong runtime was not rejected: %v\n%s", err, output)
				}
				if appRequests.Load() != 0 || crashes.Load() != 0 {
					t.Fatalf("wrong runtime received application requests: all=%d crashes=%d", appRequests.Load(), crashes.Load())
				}
				return
			}
			if err != nil {
				t.Fatalf("smoke failed: %v\n%s", err, output)
			}
			var result struct {
				Result     string `json:"result"`
				RuntimePID int    `json:"runtime_pid"`
			}
			if err := json.Unmarshal(output, &result); err != nil || result.Result != "passed" || crashes.Load() != 1 {
				t.Fatalf("injection fixture did not complete: result=%+v crashes=%d error=%v", result, crashes.Load(), err)
			}
			if mode == "verified_runtime" && result.RuntimePID != os.Getpid() {
				t.Fatalf("reported runtime PID = %d", result.RuntimePID)
			}
		})
	}
}
