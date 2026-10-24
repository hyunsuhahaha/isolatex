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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/gateway"
)

type spawnResult struct{ Started, Blocked int }

// A bounded three-generation tree: worker -> branch -> at most 48 leaves.
// This is deliberately finite even when kernel enforcement is broken.
func TestTaskTreeHelper(t *testing.T) {
	mode := os.Getenv("ISOLATEX_TASK_HELPER")
	if mode == "" {
		return
	}
	if mode == "leaf" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if mode == "branch" {
		if err := os.WriteFile(os.Getenv("TASK_BRANCH_FILE"), []byte("started"), 0600); err != nil {
			os.Exit(25)
		}
		var children []*exec.Cmd
		result := spawnResult{}
		for i := 0; i < 48; i++ {
			var child *exec.Cmd
			if runtime.GOOS == "linux" {
				child = exec.Command("sleep", "60")
			} else {
				child = exec.Command(os.Args[0], "-test.run=^TestTaskTreeHelper$")
				child.Env = append(os.Environ(), "ISOLATEX_TASK_HELPER=leaf")
			}
			if err := child.Start(); err != nil {
				result.Blocked++
				break
			}
			result.Started++
			children = append(children, child)
		}
		// Leave the budget occupied while the protected route receives traffic.
		time.Sleep(200 * time.Millisecond)
		for _, child := range children {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		os.Exit(0)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("spawn") == "1" {
			child := exec.Command(os.Args[0], "-test.run=^TestTaskTreeHelper$")
			child.Env = append(os.Environ(), "ISOLATEX_TASK_HELPER=branch")
			data, err := child.Output()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(200)
	})
	if err := http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), mux); err != nil {
		os.Exit(24)
	}
	os.Exit(0)
}

func TestWorkerTaskBudgetContainsDescendantSpawns(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("resource limits unsupported")
	}
	parent := os.Getenv("ISOLATEX_TEST_CGROUP_PARENT")
	if runtime.GOOS == "linux" && parent == "" {
		t.Skip("requires delegated pids controller")
	}
	for _, mode := range []string{"unlimited", "count_only", "count_and_memory"} {
		t.Run(mode, func(t *testing.T) {
			c := fixture(t)
			c.ShutdownGrace = time.Second
			for name, g := range c.Bulkheads {
				g.Process.Command = []string{os.Args[0], "-test.run=^TestTaskTreeHelper$"}
				g.Process.Env = map[string]string{"ISOLATEX_TASK_HELPER": "worker", "GOMAXPROCS": "2", "TASK_BRANCH_FILE": filepath.Join(t.TempDir(), name+"-branch")}
				g.RequestTimeout = 10 * time.Second
				g.Readiness.StartupTimeout = 5 * time.Second
				if mode != "unlimited" {
					resources := &config.Resources{CgroupParent: parent}
					if runtime.GOOS == "linux" {
						resources.TasksMax = 32
					} else {
						resources.ProcessesMax = 4
					}
					if mode == "count_and_memory" {
						resources.MemoryMaxBytes = 512 << 20
					}
					// Both groups have equal limits, but their budgets must be independent.
					g.Process.Resources = resources
				}
				c.Bulkheads[name] = g
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := New(c, logger)
			refusals := observeTaskRefusals(t, s)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 && s.Pools["isolated"].Ready() == 1 })
			normal, isolated := s.Pools["normal"].Snapshot()[0], s.Pools["isolated"].Snapshot()[0]
			gw := gateway.New(c, s.Pools, logger)
			defer gw.Close()
			server := httptest.NewServer(gw)
			defer server.Close()
			admin := httptest.NewServer(gw.Admin())
			defer admin.Close()
			statusResponse, err := admin.Client().Get(admin.URL + "/status")
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Groups map[string]struct {
					Tasks     int64 `json:"worker_tasks_max"`
					Processes int64 `json:"worker_processes_max"`
				}
			}
			err = json.NewDecoder(statusResponse.Body).Decode(&status)
			_ = statusResponse.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			metricsResponse, err := admin.Client().Get(admin.URL + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			metricData, err := io.ReadAll(metricsResponse.Body)
			_ = metricsResponse.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			for name, g := range c.Bulkheads {
				var tasks, processes int64
				if g.Process.Resources != nil {
					tasks = g.Process.Resources.TasksMax
					processes = g.Process.Resources.ProcessesMax
				}
				got := status.Groups[name]
				if got.Tasks != tasks || got.Processes != processes {
					t.Fatalf("status for %s: %+v", name, got)
				}
				for metric, value := range map[string]int64{"isolatex_worker_tasks_limit": tasks, "isolatex_worker_processes_limit": processes} {
					line := fmt.Sprintf("%s{group=%q} %d\n", metric, name, value)
					if !strings.Contains(string(metricData), line) {
						t.Fatalf("missing metric: %s", line)
					}
				}
			}
			client := &http.Client{Timeout: 12 * time.Second}
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
			// Repeated pressure must release capacity even when the Go runtime
			// treats inability to create a thread as fatal and needs a replacement.
			for attempt := 0; attempt < 2; attempt++ {
				before := successes.Load()
				rejectedBefore := refusals()
				generation := s.Pools["isolated"].Snapshot()[0].PID
				marker := c.Bulkheads["isolated"].Process.Env["TASK_BRANCH_FILE"]
				if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				resp, err := client.Post(server.URL+"/api/fault/work?spawn=1", "", nil)
				if err != nil {
					t.Fatal(err)
				}
				var result spawnResult
				decodeErr := json.NewDecoder(resp.Body).Decode(&result)
				_ = resp.Body.Close()
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("descendant branch did not execute: %v", err)
				}
				if mode == "unlimited" || runtime.GOOS == "windows" || resp.StatusCode == 200 {
					if decodeErr != nil || resp.StatusCode != 200 {
						t.Fatalf("spawn response: status=%d result=%+v error=%v", resp.StatusCode, result, decodeErr)
					}
					if mode == "unlimited" {
						if result.Started != 48 || result.Blocked != 0 {
							t.Fatalf("unlimited control: %+v", result)
						}
					} else if result.Started < 1 || result.Started >= 48 || result.Blocked != 1 {
						t.Fatalf("descendant budget not enforced: %+v", result)
					}
				} else if resp.StatusCode != 500 && resp.StatusCode != 502 {
					t.Fatalf("unexpected pressure response: %d", resp.StatusCode)
				}
				if runtime.GOOS == "linux" && mode != "unlimited" {
					// HTTP failure alone is insufficient evidence: require the kernel's
					// actual pids-controller refusal, retained across cgroup cleanup.
					waitFor(t, func() bool { return refusals() > rejectedBefore })
				}
				waitFor(t, func() bool {
					worker := s.Pools["isolated"].Snapshot()[0]
					return worker.Ready && (resp.StatusCode != 502 || worker.PID != generation)
				})
				waitFor(t, func() bool { return successes.Load() > before })
			}
			actual := s.Pools["normal"].Snapshot()[0]
			if !actual.Ready || actual.PID != normal.PID || actual.Restarts != 0 {
				t.Fatalf("spawn pressure affected protected worker: %+v", actual)
			}
			actual = s.Pools["isolated"].Snapshot()[0]
			if !actual.Ready {
				t.Fatalf("isolated worker did not recover: %+v", actual)
			}
			if runtime.GOOS == "windows" || mode == "unlimited" {
				if actual.PID != isolated.PID || actual.Restarts != 0 {
					t.Fatalf("handled refusal restarted worker: %+v", actual)
				}
			}

			if failures.Load() != 0 {
				t.Fatalf("protected HTTP failures: %d", failures.Load())
			}
		})
	}
}

// Keep kernel evidence after the supervisor removes an exited generation's
// cgroup. This wrapper observes the real launcher and real resource cleanup.
func observeTaskRefusals(t *testing.T, s *Supervisor) func() int64 {
	if runtime.GOOS != "linux" {
		return func() int64 { return 0 }
	}
	var mu sync.Mutex
	paths := map[string]bool{}
	var closed int64
	read := func(path string) int64 {
		data, err := os.ReadFile(filepath.Join(path, "pids.events"))
		if err != nil {
			t.Errorf("read pids evidence: %v", err)
			return 0
		}
		fields := strings.Fields(string(data))
		if len(fields) != 2 || fields[0] != "max" {
			t.Errorf("bad pids evidence: %q", data)
			return 0
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Error(err)
		}
		return value
	}
	launch := s.start
	s.start = func(cmd *exec.Cmd, resources *config.Resources) (*processControl, error) {
		control, err := launch(cmd, resources)
		if err != nil || resources == nil || resources.TasksMax == 0 {
			return control, err
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", cmd.Process.Pid))
		if err != nil {
			t.Errorf("read worker cgroup: %v", err)
			return control, nil
		}
		var path string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "0::") {
				path = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
			}
		}
		if path == "" {
			t.Error("worker has no cgroup v2 membership")
			return control, nil
		}
		mu.Lock()
		paths[path] = true
		mu.Unlock()
		closeWorker := control.close
		control.close = func() error {
			mu.Lock()
			if paths[path] {
				closed += read(path)
				delete(paths, path)
			}
			mu.Unlock()
			return closeWorker()
		}
		return control, nil
	}
	return func() int64 {
		mu.Lock()
		defer mu.Unlock()
		total := closed
		for path := range paths {
			total += read(path)
		}
		return total
	}
}
