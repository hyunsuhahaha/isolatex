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
		Active  int
		Queued  int
		Workers []worker
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
	flag.Parse()
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
	for _, name := range []string{"normal", "external"} {
		b, ok := before.Groups[name]
		if !ok || len(b.Workers) == 0 {
			return fmt.Errorf("smoke needs normal/external demo groups")
		}
		for _, w := range b.Workers {
			if !w.Ready {
				return fmt.Errorf("%s workers are not all ready", name)
			}
		}
	}
	get := func(path string) (int, error) {
		resp, err := client.Get(*base + path)
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
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, err := get("/api/erp?delay=2s")
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
		if current.Groups["external"].Active >= 2 {
			break
		}
		if time.Now().After(until) {
			wg.Wait()
			return fmt.Errorf("ERP did not saturate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	durations := []time.Duration{}
	for i := 0; i < 40; i++ {
		start := time.Now()
		code, err := get("/api/orders")
		if err != nil || code != 200 {
			wg.Wait()
			return fmt.Errorf("normal failed under ERP saturation: HTTP %d, %v", code, err)
		}
		durations = append(durations, time.Since(start))
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	if counts[503] == 0 || counts[200] == 0 || counts[0] != 0 {
		return fmt.Errorf("unexpected ERP load outcomes: %v", counts)
	}
	code, err := get("/api/erp?crash=1")
	if err != nil || code != 502 {
		return fmt.Errorf("crash did not yield 502: %d %v", code, err)
	}
	oldRestarts := 0
	oldPIDs := map[int]bool{}
	for _, w := range before.Groups["external"].Workers {
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
		for _, w := range after.Groups["external"].Workers {
			ready = ready && w.Ready
			newPID = newPID || !oldPIDs[w.PID]
			restarts += w.Restarts
		}
		if ready && newPID && restarts > oldRestarts {
			break
		}
		if time.Now().After(until) {
			return fmt.Errorf("crashed ERP worker did not recover")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, w := range before.Groups["normal"].Workers {
		current := after.Groups["normal"].Workers[i]
		if !current.Ready || current.PID != w.PID {
			return fmt.Errorf("normal worker changed during ERP crash")
		}
	}
	code, err = get("/api/orders")
	if err != nil || code != 200 {
		return fmt.Errorf("normal failed after ERP crash")
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"result": "passed", "normal_requests": len(durations) + 1, "normal_p95_ms": float64(durations[37].Microseconds()) / 1000, "erp_status_counts": counts, "erp_worker_restarted": true, "normal_pids_unchanged": true})
}
