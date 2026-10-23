// Run against a local, running demo runtime: go run ./bench
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type worker struct {
	PID      int
	Ready    bool
	Restarts int
}
type status struct {
	Groups map[string]struct {
		Active        int
		Queued        int
		Workers       []worker
		MaxConcurrent int `json:"max_concurrent"`
		QueueSize     int `json:"queue_size"`
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	base := flag.String("url", "http://127.0.0.1:8080", "demo gateway")
	admin := flag.String("admin", "http://127.0.0.1:9090", "demo admin origin")
	healthyGroup := flag.String("healthy-group", "alpha", "group whose capacity and workers must remain healthy")
	faultGroup := flag.String("fault-group", "beta", "group receiving injected delays and a process crash")
	healthyPath := flag.String("healthy-path", "/a/work", "healthy demo request path")
	faultPath := flag.String("fault-path", "/b/work", "fault-injection demo request path (without query)")
	faultDelay := flag.Duration("fault-delay", time.Second, "injected demo delay (0..1m)")
	flag.Parse()
	if *healthyGroup == *faultGroup {
		return fmt.Errorf("healthy-group and fault-group must be different")
	}
	if *faultDelay <= 0 || *faultDelay > time.Minute {
		return fmt.Errorf("fault-delay must be positive and at most 1m")
	}
	client := &http.Client{Timeout: 8 * time.Second}
	snapshot := func() (status, error) {
		var s status
		resp, err := client.Get(*admin + "/status")
		if err != nil {
			return s, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return s, fmt.Errorf("status HTTP %d", resp.StatusCode)
		}
		err = json.NewDecoder(resp.Body).Decode(&s)
		return s, err
	}
	before, err := snapshot()
	if err != nil {
		return err
	}
	for _, name := range []string{*healthyGroup, *faultGroup} {
		b, ok := before.Groups[name]
		if !ok || len(b.Workers) == 0 {
			return fmt.Errorf("group %q has no workers", name)
		}
		for _, w := range b.Workers {
			if !w.Ready {
				return fmt.Errorf("%s workers are not all ready", name)
			}
		}
	}
	request := func(method, path string) (int, error) {
		req, err := http.NewRequest(method, *base+path, nil)
		if err != nil {
			return 0, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, err
	}
	counts := map[int]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	faultBudget := before.Groups[*faultGroup]
	if faultBudget.MaxConcurrent < 1 || faultBudget.QueueSize < 0 {
		return fmt.Errorf("fault group has invalid capacity")
	}
	for i := 0; i < faultBudget.MaxConcurrent+faultBudget.QueueSize+4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, err := request(http.MethodGet, *faultPath+"?delay="+faultDelay.String())
			if err != nil {
				code = 0
			}
			mu.Lock()
			counts[code]++
			mu.Unlock()
		}()
	}
	until := time.Now().Add(time.Second)
	for {
		current, err := snapshot()
		if err != nil {
			wg.Wait()
			return err
		}
		if current.Groups[*faultGroup].Active >= faultBudget.MaxConcurrent {
			break
		}
		if time.Now().After(until) {
			wg.Wait()
			return fmt.Errorf("fault group %q did not saturate", *faultGroup)
		}
		time.Sleep(5 * time.Millisecond)
	}
	durations := []time.Duration{}
	for i := 0; i < 40; i++ {
		start := time.Now()
		code, err := request(http.MethodGet, *healthyPath)
		if err != nil || code != 200 {
			wg.Wait()
			return fmt.Errorf("healthy group failed under saturation: HTTP %d, %v", code, err)
		}
		durations = append(durations, time.Since(start))
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	if counts[503] == 0 || counts[200] == 0 || counts[0] != 0 {
		return fmt.Errorf("unexpected fault group load outcomes: %v", counts)
	}
	code, err := request(http.MethodPost, *faultPath+"?crash=1")
	if err != nil || code != 502 {
		return fmt.Errorf("crash did not yield 502: %d %v", code, err)
	}
	oldRestarts := 0
	oldPIDs := map[int]bool{}
	for _, w := range before.Groups[*faultGroup].Workers {
		oldRestarts += w.Restarts
		oldPIDs[w.PID] = true
	}
	until = time.Now().Add(8 * time.Second)
	var after status
	for {
		after, err = snapshot()
		if err != nil {
			return err
		}
		ready, newPID, restarts := true, false, 0
		for _, w := range after.Groups[*faultGroup].Workers {
			ready = ready && w.Ready
			newPID = newPID || !oldPIDs[w.PID]
			restarts += w.Restarts
		}
		if ready && newPID && restarts > oldRestarts {
			break
		}
		if time.Now().After(until) {
			return fmt.Errorf("crashed worker in group %q did not recover", *faultGroup)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(after.Groups[*healthyGroup].Workers) != len(before.Groups[*healthyGroup].Workers) {
		return fmt.Errorf("healthy group worker count changed during crash")
	}
	for i, w := range before.Groups[*healthyGroup].Workers {
		current := after.Groups[*healthyGroup].Workers[i]
		if !current.Ready || current.PID != w.PID {
			return fmt.Errorf("healthy group worker changed during crash")
		}
	}
	code, err = request(http.MethodGet, *healthyPath)
	if err != nil || code != 200 {
		return fmt.Errorf("healthy group failed after crash")
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"result": "passed", "healthy_group": *healthyGroup, "fault_group": *faultGroup, "healthy_requests": len(durations) + 1, "healthy_p95_ms": float64(durations[37].Microseconds()) / 1000, "fault_status_counts": counts, "fault_worker_restarted": true, "healthy_pids_unchanged": true})
}
