package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/pool"
	"github.com/hyunsuhahaha/isolatex/internal/requestdial"
)

func TestRequestDeadlineReclaimsPendingUpstreamDials(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 80 * time.Millisecond
		c.Bulkheads["isolated"] = b
	})
	var urls []string
	for range 4 {
		up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(up.Close)
		urls = append(urls, up.URL)
	}
	b := g.groups[0]
	b.pool = pool.New(urls, true)
	dials := stallUpstreamDials(t, b)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	for cycle := range 4 {
		if got := code(t, client, s.URL+"/api/fault"); got != http.StatusGatewayTimeout {
			t.Fatalf("cycle %d: status=%d", cycle, got)
		}
		if got := code(t, client, s.URL+"/api/work"); got != http.StatusOK {
			t.Fatalf("protected route: %d", got)
		}
	}
	deadline := time.Now().Add(time.Second)
	for dials.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if dials.active.Load() != 0 || dials.peak.Load() == 0 {
		t.Fatalf("request budgets released with pending sockets: active=%d peak=%d", dials.active.Load(), dials.peak.Load())
	}
	for _, group := range g.groups {
		eventually(t, func() bool { a, q := group.gate.snapshot(); return a == 0 && q == 0 })
	}
}

func TestDialPreservesTransportCancellation(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	ctx, cancelTransport := context.WithCancel(requestdial.Bind(context.Background(), requestCtx))
	defer cancelTransport()
	started := make(chan struct{})
	dialer := &requestDialer{dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { _, err := dialer.DialContext(ctx, "tcp", "unused"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	cancelTransport()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || requestCtx.Err() != nil {
			t.Fatalf("transport cancellation was lost: error=%v request=%v", err, requestCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("transport cancellation did not stop dial")
	}
}

type stalledDials struct {
	active, peak atomic.Int32
	started      chan struct{}
}

func stallUpstreamDials(t *testing.T, b *group) *stalledDials {
	t.Helper()
	dials := &stalledDials{started: make(chan struct{}, 8)}
	baseDial := b.dialer.dial
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	b.dialer.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := baseDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		n := dials.active.Add(1)
		for old := dials.peak.Load(); n > old && !dials.peak.CompareAndSwap(old, n); old = dials.peak.Load() {
		}
		dials.started <- struct{}{}
		defer dials.active.Add(-1)
		defer conn.Close()
		// Hold a real TCP socket at the unfinished-dial boundary. The standard
		// transport detaches cancellation; the gateway must restore it.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stop:
			return nil, context.Canceled
		}
	}
	return dials
}

func TestClientCancellationReclaimsPendingUpstreamDial(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 5 * time.Second
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Minute, MinRequests: 1, FailureRatio: 1, OpenTimeout: time.Minute}
		c.Bulkheads["isolated"] = b
	})
	dials := stallUpstreamDials(t, g.groups[0])
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
	case <-dials.started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream dial did not start")
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
	for dials.active.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if dials.active.Load() != 0 {
		t.Fatal("client cancellation left an upstream dial running")
	}
	eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
	if state := g.groups[0].breaker.Snapshot().State; state != "closed" {
		t.Fatalf("client cancellation tripped the upstream circuit: %s", state)
	}
	if got := code(t, s.Client(), s.URL+"/api/work"); got != http.StatusOK {
		t.Fatalf("protected route: %d", got)
	}
}

func TestTimeoutReplyClosesCanceledConnection(t *testing.T) {
	_, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fault" {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "ok")
	}), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 40 * time.Millisecond
		c.Bulkheads["isolated"] = b
	})
	for cycle := range 4 {
		resp, err := s.Client().Get(s.URL + "/api/fault")
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusGatewayTimeout || !resp.Close {
			t.Fatalf("cycle %d: timeout response status=%d close=%t read=%v", cycle, resp.StatusCode, resp.Close, readErr)
		}
		if got := code(t, s.Client(), s.URL+"/api/work"); got != http.StatusOK {
			t.Fatalf("healthy request after timeout: %d", got)
		}
	}
}
