//go:build linux || windows

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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/gateway"
)

type cpuSample struct {
	PID                          int
	Location, Quota              string
	Flags, Rate, Processes, CPUs uint32
	UsageUsec, Throttled         uint64
	Cycles, WorkUnits            uint64
}

type cpuResult struct {
	Before, After, Child, ChildAfter cpuSample
	Elapsed                          time.Duration
}

var cpuBurnSink atomic.Uint64
var cpuBurnBatches atomic.Uint64

func burnTestCPU() {
	var wg sync.WaitGroup
	deadline := time.Now().Add(1500 * time.Millisecond)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := uint64(1)
			batches := uint64(0)
			for time.Now().Before(deadline) {
				batches++
				for n := 0; n < 4096; n++ {
					x = x*6364136223846793005 + 1
				}
			}
			cpuBurnSink.Add(x)
			cpuBurnBatches.Add(batches)
		}()
	}
	wg.Wait()
}

func TestCPUWorkerHelper(t *testing.T) {
	mode := os.Getenv("ISOLATEX_CPU_HELPER")
	if mode == "" {
		return
	}
	if mode == "child" {
		before, err := sampleTestCPU()
		if err != nil {
			os.Exit(25)
		}
		burnTestCPU()
		after, err := sampleTestCPU()
		if err != nil {
			os.Exit(25)
		}
		data, err := json.Marshal(cpuResult{Before: before, After: after})
		if err != nil || os.WriteFile(os.Getenv("CPU_CHILD_FILE"), data, 0600) != nil {
			os.Exit(25)
		}
		os.Exit(0)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("burn") != "1" {
			w.WriteHeader(200)
			return
		}
		result := cpuResult{}
		started := time.Now()
		var err error
		result.Before, err = sampleTestCPU()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		child := exec.Command(os.Args[0], "-test.run=^TestCPUWorkerHelper$")
		child.Env = append(os.Environ(), "ISOLATEX_CPU_HELPER=child")
		if err := child.Start(); err != nil {
			os.Exit(24)
		}
		// Parent and child burn together, within a finite wall-clock window.
		burnTestCPU()
		if err := child.Wait(); err != nil {
			os.Exit(24)
		}
		result.After, err = sampleTestCPU()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		data, err := os.ReadFile(os.Getenv("CPU_CHILD_FILE"))
		var childResult cpuResult
		if err != nil || json.Unmarshal(data, &childResult) != nil {
			os.Exit(25)
		}
		result.Child, result.ChildAfter = childResult.Before, childResult.After
		result.Elapsed = time.Since(started)
		_ = json.NewEncoder(w).Encode(result)
	})
	if err := http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), mux); err != nil {
		os.Exit(24)
	}
	os.Exit(0)
}

func TestWorkerCPUBudgetContainsDescendantLoad(t *testing.T) {
	parent := os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
	if runtime.GOOS == "linux" && parent == "" {
		t.Skip("requires delegated CPU controller")
	}
	for _, mode := range []string{"unlimited", "cpu_only", "combined"} {
		t.Run(mode, func(t *testing.T) {
			c := fixture(t)
			c.ShutdownGrace = time.Second
			for name, g := range c.Bulkheads {
				g.Process.Command = []string{os.Args[0], "-test.run=^TestCPUWorkerHelper$"}
				g.Process.Env = map[string]string{"ISOLATEX_CPU_HELPER": "worker", "GOMAXPROCS": "4", "CPU_CHILD_FILE": filepath.Join(t.TempDir(), "child.json")}
				g.Readiness.StartupTimeout = 10 * time.Second
				g.Readiness.Timeout = 500 * time.Millisecond
				g.Readiness.Interval = 50 * time.Millisecond
				g.RequestTimeout = 20 * time.Second
				resources := &config.Resources{MemoryMaxBytes: 512 << 20, CgroupParent: parent}
				if name == "isolated" && mode != "unlimited" {
					resources.MemoryMaxBytes = 0
					if runtime.GOOS == "linux" {
						resources.CPUMillicores = 250
					} else {
						resources.CPUPercent = 1
					}
					if mode == "combined" {
						resources.MemoryMaxBytes = 512 << 20
						if runtime.GOOS == "linux" {
							resources.TasksMax = 128
						} else {
							resources.ProcessesMax = 8
						}
					}
				}
				g.Process.Resources = resources
				c.Bulkheads[name] = g
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := New(c, logger)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 && s.Pools["normal"].Ready() == 1 })
			isolated, normal := s.Pools["isolated"].Snapshot()[0], s.Pools["normal"].Snapshot()[0]
			gw := gateway.New(c, s.Pools, logger)
			defer gw.Close()
			server := httptest.NewServer(gw)
			defer server.Close()
			admin := httptest.NewServer(gw.Admin())
			defer admin.Close()
			client := &http.Client{Timeout: 25 * time.Second}
			ctx, cancel := context.WithCancel(context.Background())
			var successes, failures atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				for ctx.Err() == nil {
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/work", nil)
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
			resp, err := client.Post(server.URL+"/api/fault/work?burn=1", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if successes.Load() <= started {
				_ = resp.Body.Close()
				t.Fatal("protected route made no progress during CPU pressure")
			}
			var result cpuResult
			err = json.NewDecoder(resp.Body).Decode(&result)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("CPU pressure response: status=%d result=%+v error=%v", resp.StatusCode, result, err)
			}
			if result.Child.PID <= 0 || result.Child.PID == isolated.PID || result.Before.PID != isolated.PID || result.After.PID != isolated.PID {
				t.Fatalf("missing live descendant: %+v", result)
			}
			if result.After.WorkUnits <= result.Before.WorkUnits || result.ChildAfter.WorkUnits <= result.Child.WorkUnits {
				t.Fatalf("no actual compute work recorded: %+v", result)
			}
			if runtime.GOOS == "linux" && result.After.UsageUsec <= result.Before.UsageUsec {
				t.Fatalf("no kernel CPU work recorded: %+v", result)
			}
			if runtime.GOOS == "linux" {
				quota := "max 100000"
				if mode != "unlimited" {
					quota = "25000 100000"
				}
				if result.Before.Quota != quota || result.Child.Quota != quota || result.After.Quota != quota || result.Child.Location != result.Before.Location {
					t.Fatalf("worker-tree CPU budget mismatch: %+v", result)
				}
				if mode != "unlimited" {
					if result.After.Throttled <= result.Before.Throttled {
						t.Fatalf("no kernel CPU throttling: %+v", result)
					}
					budget := float64(result.Elapsed.Microseconds())*0.25*1.5 + 100000
					if float64(result.After.UsageUsec-result.Before.UsageUsec) > budget {
						t.Fatalf("CPU usage exceeded tree quota: %+v", result)
					}
				}
			} else {
				if result.After.Cycles <= result.Before.Cycles || result.ChildAfter.Cycles <= result.Child.Cycles {
					t.Fatalf("no process CPU cycles recorded: %+v", result)
				}
				// Legacy Job time counters can stay unchanged at very low CPU rates.
				// Require work and high-resolution cycle progress in both processes.
				flags, rate := uint32(0), uint32(0)
				if mode != "unlimited" {
					flags, rate = 5, 100
				}
				if result.Before.Flags != flags || result.Before.Rate != rate || result.Child.Flags != flags || result.Child.Rate != rate || result.After.Processes < 2 {
					t.Fatalf("worker-tree Job CPU budget mismatch: %+v", result)
				}
				if mode != "unlimited" {
					// Cycle-based control and accounting clocks differ. Allow scheduling
					// granularity while checking real aggregate CPU use alongside native flags.
					budget := float64(result.Elapsed.Microseconds())*float64(result.After.CPUs)*0.01*2 + 250000
					if float64(result.After.UsageUsec-result.Before.UsageUsec) > budget {
						t.Fatalf("CPU usage exceeded Job budget: %+v", result)
					}
				}
			}
			waitFor(t, func() bool { return successes.Load() > started })
			// A deliberately tiny CPU quota may legitimately expire readiness probes
			// during load. Once load ends, require recovery in the same generation.
			waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 })
			for name, pid := range map[string]int{"normal": normal.PID, "isolated": isolated.PID} {
				actual := s.Pools[name].Snapshot()[0]
				if !actual.Ready || actual.PID != pid || actual.Restarts != 0 {
					t.Fatalf("CPU pressure affected %s: %+v", name, actual)
				}
			}
			if failures.Load() != 0 {
				t.Fatalf("protected HTTP failures: %d", failures.Load())
			}
			resp, err = admin.Client().Get(admin.URL + "/status")
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Groups map[string]struct {
					Millicores int64 `json:"worker_cpu_millicores"`
					Percent    int64 `json:"worker_cpu_percent"`
				}
			}
			err = json.NewDecoder(resp.Body).Decode(&status)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			resp, err = admin.Client().Get(admin.URL + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			metrics, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			for name, g := range c.Bulkheads {
				expected := g.Process.Resources
				actual := status.Groups[name]
				if actual.Millicores != int64(expected.CPUMillicores) || actual.Percent != int64(expected.CPUPercent) {
					t.Fatalf("CPU status mismatch: %+v", actual)
				}
				for metric, value := range map[string]int64{"isolatex_worker_cpu_millicores_limit": int64(expected.CPUMillicores), "isolatex_worker_cpu_percent_limit": int64(expected.CPUPercent)} {
					line := fmt.Sprintf("%s{group=%q} %d\n", metric, name, value)
					if !strings.Contains(string(metrics), line) {
						t.Fatalf("missing CPU metric: %s", line)
					}
				}
			}
		})
	}
}
