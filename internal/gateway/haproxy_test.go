package gateway

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/supervisor"
)

// Optional locally; CI explicitly installs HAProxy and requires this test.
func TestHAProxyIndependentGroupRouting(t *testing.T) {
	binary := os.Getenv("ISOLATEX_TEST_HAPROXY")
	if binary == "" {
		t.Skip("set ISOLATEX_TEST_HAPROXY to run the real load balancer integration")
	}
	type node struct {
		gateway       *Gateway
		public        *httptest.Server
		alphaHealthy  *atomic.Bool
		probeFallback *atomic.Bool
	}
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	makeNode := func(name string, alphaWorkers int) node {
		c := config.Config{Version: 1, Listen: "127.0.0.1:8080", AdminListen: "127.0.0.1:9090", MaxConnections: 1024, MaxAdmitted: 4, MaxBodyBytes: 1024, ShutdownGrace: time.Second, RoutingProbePrefix: "/_isolatex/route", Bulkheads: map[string]config.Group{}}
		healthy := &atomic.Bool{}
		healthy.Store(true)
		for _, groupName := range []string{"alpha", "beta"} {
			workers := 1
			if groupName == "alpha" {
				workers = alphaWorkers
			}
			g := config.Group{MaxConcurrent: workers, QueueTimeout: 15 * time.Second, RequestTimeout: 20 * time.Second, Readiness: config.Readiness{Path: "/readyz", Interval: 20 * time.Millisecond, Timeout: 200 * time.Millisecond, FailureThreshold: 1, SuccessThreshold: 1}}
			g.Match.PathPrefix = "/b"
			if groupName == "alpha" {
				g.Match.PathPrefix = "/a"
				if name == "A" {
					g.QueueSize = 1
				}
			}
			for range workers {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/readyz" {
						if groupName == "alpha" && !healthy.Load() {
							w.WriteHeader(503)
						}
						return
					}
					if name == "A" && groupName == "alpha" && r.URL.Query().Get("hold") == "1" {
						select {
						case <-release:
						case <-r.Context().Done():
						}
					}
					_, _ = io.WriteString(w, name)
				}))
				t.Cleanup(up.Close)
				g.Upstreams = append(g.Upstreams, up.URL)
			}
			c.Bulkheads[groupName] = g
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		s := supervisor.New(c, logger)
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Stop)
		g := New(c, s.Pools, logger)
		t.Cleanup(g.Close)
		fallback := &atomic.Bool{}
		public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate an absent probe configuration falling through to a
			// catch-all app that returns 200 for unknown paths.
			if fallback.Load() && strings.HasPrefix(r.URL.Path, "/_isolatex/route/") {
				_, _ = io.WriteString(w, "app fallback")
				return
			}
			g.ServeHTTP(w, r)
		}))
		t.Cleanup(public.Close)
		eventually(t, func() bool { return s.Pools["alpha"].Ready() == alphaWorkers && s.Pools["beta"].Ready() == 1 })
		return node{g, public, healthy, fallback}
	}
	a, b := makeNode("A", 1), makeNode("B", 2)
	// Run this cleanup before the servers: failed tests must release held work.
	t.Cleanup(unblock)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bind := ln.Addr().String()
	_ = ln.Close()
	dir, err := os.MkdirTemp("", "ix-haproxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "stats.sock")
	template, err := os.ReadFile("../../examples/haproxy/haproxy.cfg")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(template), "global\n", "global\n    stats socket "+socket+" level user\n", 1)
	filename := filepath.Join(dir, "haproxy.cfg")
	if err := os.WriteFile(filename, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "ISOLATEX_LB_BIND="+bind, "ISOLATEX_NODE_A="+strings.TrimPrefix(a.public.URL, "http://"), "ISOLATEX_NODE_B="+strings.TrimPrefix(b.public.URL, "http://"))
	check := exec.Command(binary, "-c", "-f", filename)
	check.Env = env
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("invalid shipped HAProxy template: %v\n%s", err, out)
	}
	logfile, err := os.Create(filepath.Join(dir, "haproxy.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-db", "-f", filename)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, logfile, logfile
	if err := cmd.Start(); err != nil {
		_ = logfile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logfile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logfile.Name())
			t.Logf("HAProxy log:\n%s", data)
		}
	})
	states := func() map[string]string {
		out := map[string]string{}
		conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
		if err != nil {
			return out
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(conn, "show stat\n")
		r := csv.NewReader(conn)
		r.FieldsPerRecord = -1
		rows, err := r.ReadAll()
		if err != nil || len(rows) == 0 {
			return out
		}
		status := -1
		for i, name := range rows[0] {
			if name == "status" {
				status = i
			}
		}
		for _, row := range rows[1:] {
			if status >= 0 && len(row) > status {
				out[row[0]+"/"+row[1]] = row[status]
			}
		}
		return out
	}
	waitState := func(alphaA, alphaB string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			s := states()
			if s["alpha/node_a"] == alphaA && s["alpha/node_b"] == alphaB && s["beta/node_a"] == "UP" && s["beta/node_b"] == "UP" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("routing state did not converge: %+v", s)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	assertRoutes := func(path, want string) {
		t.Helper()
		seen := map[string]bool{}
		for range 12 {
			resp, err := client.Post("http://"+bind+path, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s: %d %s", path, resp.StatusCode, data)
			}
			seen[string(data)] = true
		}
		for _, name := range []string{"A", "B"} {
			if seen[name] != strings.Contains(want, name) {
				t.Fatalf("%s routed to %+v, want %s", path, seen, want)
			}
		}
	}
	waitState("UP", "UP")
	assertRoutes("/a", "AB")
	assertRoutes("/b", "AB")
	for _, path := range []string{"/a-old", "/_isolatex/route/alpha", "/unknown"} {
		if got := code(t, client, "http://"+bind+path); got != 404 {
			t.Fatalf("frontend path boundary %s = %d", path, got)
		}
	}
	held := make(chan error, 2)
	for range 2 {
		go func() {
			resp, err := a.public.Client().Post(a.public.URL+"/a?hold=1", "", nil)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					err = fmt.Errorf("held request: %d", resp.StatusCode)
				}
			}
			held <- err
		}()
	}
	eventually(t, func() bool {
		for _, g := range a.gateway.groups {
			if g.name == "alpha" {
				active, queued := g.gate.snapshot()
				return active == 1 && queued == 1
			}
		}
		return false
	})
	readRouting(t, a.public.URL+"/_isolatex/route/alpha", 503)
	waitState("DOWN", "UP")
	assertRoutes("/a", "B")
	assertRoutes("/b", "AB")
	unblock()
	for range 2 {
		if err := <-held; err != nil {
			t.Fatal(err)
		}
	}
	waitState("UP", "UP")
	assertRoutes("/a", "AB")
	a.alphaHealthy.Store(false)
	waitState("DOWN", "UP")
	assertRoutes("/a", "B")
	assertRoutes("/b", "AB")
	b.alphaHealthy.Store(false)
	waitState("DOWN", "DOWN")
	if got := code(t, client, "http://"+bind+"/a"); got != 503 {
		t.Fatalf("all alpha nodes down = %d", got)
	}
	assertRoutes("/b", "AB")
	a.alphaHealthy.Store(true)
	waitState("UP", "DOWN")
	assertRoutes("/a", "A")
	assertRoutes("/b", "AB")
	b.probeFallback.Store(true)
	b.alphaHealthy.Store(true)
	eventually(t, func() bool {
		s := states()
		return s["alpha/node_a"] == "UP" && s["alpha/node_b"] == "DOWN" && s["beta/node_a"] == "UP" && s["beta/node_b"] == "DOWN"
	})
	assertRoutes("/a", "A")
	assertRoutes("/b", "A")
}
