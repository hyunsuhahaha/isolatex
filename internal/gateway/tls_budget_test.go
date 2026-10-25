package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/pool"
)

func TestRequestDeadlineReclaimsTLSHandshake(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 300 * time.Millisecond
		c.Bulkheads["isolated"] = b
	})
	address, active, started := stalledTLSServer(t)
	g.groups[0].pool = pool.New([]string{"https://" + address}, true)
	for cycle := range 2 {
		if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusGatewayTimeout {
			t.Fatalf("cycle %d: status=%d", cycle, got)
		}
		select {
		case <-started:
		default:
			t.Fatal("upstream did not receive TLS ClientHello")
		}
		until := time.Now().Add(time.Second)
		for active.Load() != 0 && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}
		if active.Load() != 0 {
			t.Fatalf("request finished with pending TLS sockets: %d", active.Load())
		}
		if got := code(t, s.Client(), s.URL+"/api/work"); got != http.StatusOK {
			t.Fatalf("protected route: %d", got)
		}
		eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
	}
}

func TestClientCancellationReclaimsTLSHandshake(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 10 * time.Second
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Minute, MinRequests: 1, FailureRatio: 1, OpenTimeout: time.Minute}
		c.Bulkheads["isolated"] = b
	})
	address, active, started := stalledTLSServer(t)
	g.groups[0].pool = pool.New([]string{"https://" + address}, true)
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/api/fault", nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			resp, err := s.Client().Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("TLS handshake did not start")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("client cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("client cancellation did not return")
		}
		until := time.Now().Add(time.Second)
		for active.Load() != 0 && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}
		if active.Load() != 0 {
			t.Fatalf("canceled request retained TLS socket: %d", active.Load())
		}
		eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
		if snapshot := g.groups[0].breaker.Snapshot(); snapshot.State != "closed" || snapshot.Failures != 0 {
			t.Fatalf("client cancellation penalized upstream: %+v", snapshot)
		}
		if got := code(t, s.Client(), s.URL+"/api/work"); got != http.StatusOK {
			t.Fatalf("protected route: %d", got)
		}
	}
}

func TestTLSHandshakeVerificationAndReuse(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	b := g.groups[0]
	b.pool = pool.New([]string{up.URL}, true)
	// The custom TLS dial path must keep ordinary certificate verification.
	if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusBadGateway {
		t.Fatalf("untrusted certificate accepted: %d", got)
	}
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	config := &tls.Config{RootCAs: roots}
	b.transport.TLSClientConfig = config
	var connections atomic.Int32
	// Count fresh TCP dials after the failed verification has completed.
	baseDial := b.dialer.dial
	b.dialer.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		connections.Add(1)
		return baseDial(ctx, network, address)
	}
	for range 2 {
		if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusOK {
			t.Fatalf("trusted TLS upstream: %d", got)
		}
	}
	if connections.Load() != 1 || config.ServerName != "" {
		t.Fatalf("TLS reuse/config isolation: dials=%d server_name=%q", connections.Load(), config.ServerName)
	}
}

// Accept a real TLS ClientHello but never send a ServerHello. Count sockets
// until the peer closes, without replacing dial or TLS functions with mocks.
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
