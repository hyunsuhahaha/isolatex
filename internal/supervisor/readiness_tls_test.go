package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestStaticReadinessReclaimsStalledTLS(t *testing.T) {
	address, active, _ := stalledTLSServer(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer good.Close()
	c := config.Config{Bulkheads: map[string]config.Group{}}
	for name, origin := range map[string]string{"normal": good.URL, "isolated": "https://" + address} {
		c.Bulkheads[name] = config.Group{Upstreams: []string{origin}, Readiness: config.Readiness{Path: "/readyz", Interval: 20 * time.Millisecond, Timeout: 100 * time.Millisecond}}
	}
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["isolated"].Snapshot()[0].HealthFailures >= 5 })
	bad := s.Pools["isolated"].Snapshot()[0]
	normal := s.Pools["normal"].Snapshot()[0]
	if active.Load() > 2 || bad.Ready || !normal.Ready || normal.HealthChecks < 5 {
		t.Fatalf("readiness TLS retention: sockets=%d bad=%+v normal=%+v", active.Load(), bad, normal)
	}
	s.Stop()
	until := time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 {
		t.Fatalf("stopped monitor retained TLS sockets: %d", active.Load())
	}
}

func TestStaticReadinessStopCancelsTLSBeforeProbeTimeout(t *testing.T) {
	address, active, started := stalledTLSServer(t)
	c := config.Config{Bulkheads: map[string]config.Group{
		"isolated": {Upstreams: []string{"https://" + address}, Readiness: config.Readiness{Path: "/readyz", Interval: time.Second, Timeout: 10 * time.Second}},
	}}
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("readiness TLS handshake did not start")
	}
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for the 10 second probe timeout")
	}
	until := time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 || s.Pools["isolated"].Snapshot()[0].State != "stopped" {
		t.Fatalf("Stop retained readiness resources: active=%d worker=%+v", active.Load(), s.Pools["isolated"].Snapshot()[0])
	}
}

func stalledTLSServer(t *testing.T) (string, *atomic.Int32, <-chan struct{}) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	started := make(chan struct{}, 8)
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(acceptDone)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			active.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer active.Add(-1)
				defer conn.Close()
				buf := make([]byte, 4096)
				if n, _ := conn.Read(buf); n > 0 {
					started <- struct{}{}
				}
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-acceptDone
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return l.Addr().String(), &active, started
}
