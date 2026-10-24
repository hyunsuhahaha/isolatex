package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/supervisor"
	"gopkg.in/yaml.v3"
)

type fixture struct {
	owner         *supervisor.Supervisor
	cmd           *exec.Cmd
	done          chan struct{}
	waitErr       error
	log           *os.File
	dir, origin   string
	listeners     []net.Listener
	runtimeConfig config.Config
}

func group(prefix string, concurrent, queue int) config.Group {
	g := config.Group{MaxConcurrent: concurrent, QueueSize: queue, QueueTimeout: 100 * time.Millisecond, RequestTimeout: 2 * time.Second}
	g.Match.PathPrefix = prefix
	return g
}

func launch(ctx context.Context, o options, variant string) (_ *fixture, err error) {
	f := &fixture{}
	defer func() {
		if err != nil {
			f.close()
		}
	}()
	f.dir, err = os.MkdirTemp("", "isolatex-compare-")
	if err != nil {
		return nil, err
	}
	// Hold all six ports until their owners are about to start. The remaining
	// close/bind race is the same documented local-host race as the runtime.
	workerPort := 0
	for attempt := 0; attempt < 100; attempt++ {
		base, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			return nil, e
		}
		start := base.Addr().(*net.TCPAddr).Port
		batch := []net.Listener{base}
		if start <= 65532 {
			for i := 1; i < 4; i++ {
				ln, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", start+i))
				if e != nil {
					break
				}
				batch = append(batch, ln)
			}
		}
		if len(batch) == 4 {
			f.listeners = batch
			workerPort = start
			break
		}
		for _, ln := range batch {
			_ = ln.Close()
		}
	}
	if workerPort == 0 {
		return nil, fmt.Errorf("cannot reserve four contiguous worker ports")
	}
	for i := 0; i < 2; i++ {
		ln, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			return nil, e
		}
		f.listeners = append(f.listeners, ln)
	}
	public, admin := f.listeners[4].Addr().String(), f.listeners[5].Addr().String()
	base := config.Config{Version: 1, Listen: public, AdminListen: admin, MaxConnections: 1024, MaxAdmitted: 12, MaxBodyBytes: 1 << 20, ShutdownGrace: 2 * time.Second}
	ownerConfig := base
	ownerConfig.Bulkheads = map[string]config.Group{}
	managed := func(name string, start, count int) {
		g := group("/"+name, count, count*2)
		g.Process = &config.Process{Command: []string{o.App}, Workers: count, PortStart: start, Env: map[string]string{"APP_POOL": name, "DEMO_MAX_ACTIVE": "1", "DEMO_DELAY": "5ms"}}
		g.Readiness = config.Readiness{Path: "/readyz", Interval: 100 * time.Millisecond, Timeout: 200 * time.Millisecond, StartupTimeout: 10 * time.Second}
		ownerConfig.Bulkheads[name] = g
	}
	if variant == "C" {
		managed("healthy", workerPort, 2)
		managed("fault", workerPort+2, 2)
	} else {
		managed("shared", workerPort, 4)
	}
	if err = ownerConfig.Validate(); err != nil {
		return nil, err
	}
	origins := make([]string, 4)
	for i := range origins {
		origins[i] = fmt.Sprintf("http://127.0.0.1:%d", workerPort+i)
	}
	runtimeConfig := base
	runtimeConfig.Bulkheads = map[string]config.Group{}
	if variant == "A" {
		g := group("/", 4, 8)
		g.Upstreams = origins
		runtimeConfig.Bulkheads["shared"] = g
	} else {
		for _, name := range []string{"healthy", "fault"} {
			g := group("/"+name, 2, 4)
			g.Upstreams = origins
			if variant == "C" {
				if name == "healthy" {
					g.Upstreams = origins[:2]
				} else {
					g.Upstreams = origins[2:]
				}
			}
			runtimeConfig.Bulkheads[name] = g
		}
	}
	data, err := yaml.Marshal(runtimeConfig)
	if err != nil {
		return nil, err
	}
	if _, err = config.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	f.runtimeConfig = runtimeConfig
	configPath := filepath.Join(f.dir, "runtime.yaml")
	if err = os.WriteFile(configPath, data, 0600); err != nil {
		return nil, err
	}
	for _, ln := range f.listeners[:4] {
		_ = ln.Close()
	}
	f.owner = supervisor.New(ownerConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err = f.owner.Start(ctx); err != nil {
		return nil, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = waitReady(readyCtx, func() bool {
		for _, p := range f.owner.Pools {
			if p.Ready() != len(p.Snapshot()) {
				return false
			}
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("app startup: %w", err)
	}
	f.log, err = os.Create(filepath.Join(f.dir, "runtime.log"))
	if err != nil {
		return nil, err
	}
	for _, ln := range f.listeners[4:] {
		_ = ln.Close()
	}
	f.cmd = exec.Command(o.Runtime, "-config", configPath)
	f.cmd.Stdout, f.cmd.Stderr = f.log, f.log
	if err = f.cmd.Start(); err != nil {
		return nil, err
	}
	f.done = make(chan struct{})
	go func() { f.waitErr = f.cmd.Wait(); close(f.done) }()
	f.origin = "http://" + public
	client := &http.Client{Timeout: 250 * time.Millisecond}
	defer client.CloseIdleConnections()
	if err = waitReady(readyCtx, func() bool {
		select {
		case <-f.done:
			return false
		default:
		}
		resp, e := client.Get("http://" + admin + "/readyz")
		if e != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	}); err != nil {
		log, _ := os.ReadFile(filepath.Join(f.dir, "runtime.log"))
		return nil, fmt.Errorf("gateway startup: %w: %s", err, log)
	}
	return f, nil
}

func waitReady(ctx context.Context, check func() bool) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if check() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (f *fixture) protectedPIDs(variant string) []int {
	name := "shared"
	if variant == "C" {
		name = "healthy"
	}
	var pids []int
	for _, w := range f.owner.Pools[name].Snapshot() {
		pids = append(pids, w.PID)
	}
	return pids
}

func (f *fixture) faultRestarts(variant string) int {
	name := "shared"
	if variant == "C" {
		name = "fault"
	}
	total := 0
	for _, w := range f.owner.Pools[name].Snapshot() {
		total += w.Restarts
	}
	return total
}

func (f *fixture) close() {
	if f.done != nil {
		if runtime.GOOS == "windows" {
			_ = f.cmd.Process.Kill()
		} else {
			_ = f.cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-f.done:
		case <-time.After(3 * time.Second):
			_ = f.cmd.Process.Kill()
			<-f.done
		}
	}
	if f.owner != nil {
		f.owner.Stop()
	}
	for _, ln := range f.listeners {
		_ = ln.Close()
	}
	if f.log != nil {
		_ = f.log.Close()
	}
	if f.dir != "" {
		_ = os.RemoveAll(f.dir)
	} // Private directory returned by MkdirTemp.
}
