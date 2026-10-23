package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"isolatex/internal/config"
	"isolatex/internal/pool"
)

func testGateway(t *testing.T, handler http.Handler, options ...func(*config.Config)) (*Gateway, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	c := config.Config{Version: 1, Listen: "127.0.0.1:8080", AdminListen: "127.0.0.1:9090", MaxAdmitted: 4, MaxBodyBytes: 32, ShutdownGrace: time.Second, Bulkheads: map[string]config.Group{}}
	for _, name := range []string{"normal", "external"} {
		b := config.Group{MaxConcurrent: 1, QueueSize: 1, QueueTimeout: time.Second, RequestTimeout: 2 * time.Second, Upstreams: []string{up.URL}}
		b.Match.PathPrefix = "/api"
		if name == "external" {
			b.Match.PathPrefix = "/api/erp"
		}
		c.Bulkheads[name] = b
	}
	for _, option := range options {
		option(&c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	g := New(c, map[string]*pool.Pool{"normal": pool.New([]string{up.URL}, true), "external": pool.New([]string{up.URL}, true)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(g.Close)
	s := httptest.NewServer(g)
	t.Cleanup(s.Close)
	return g, s
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func code(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestSaturatedERPDoesNotBlockNormal(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	defer close(release)
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/erp" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(200)
	}))
	results := make(chan int, 2)
	get := func() {
		resp, err := s.Client().Get(s.URL + "/api/erp")
		if err != nil {
			results <- 0
			return
		}
		resp.Body.Close()
		results <- resp.StatusCode
	}
	go get()
	<-entered
	go get()
	external := g.groups[0]
	eventually(t, func() bool { a, q := external.gate.snapshot(); return a == 1 && q == 1 })
	if got := code(t, s.Client(), s.URL+"/api/erp"); got != 503 {
		t.Fatalf("overflow=%d", got)
	}
	if got := code(t, s.Client(), s.URL+"/api/orders"); got != 200 {
		t.Fatalf("normal=%d", got)
	}
	release <- struct{}{}
	<-entered
	release <- struct{}{}
	for i := 0; i < 2; i++ {
		if got := <-results; got != 200 {
			t.Fatalf("admitted=%d", got)
		}
	}
	eventually(t, func() bool { a, q := external.gate.snapshot(); return a == 0 && q == 0 })
}

func TestRoutingInputAndHeaders(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Forwarded") != "" || strings.Contains(r.Header.Get("X-Forwarded-For"), "spoof") {
			t.Error("untrusted forwarding headers survived")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	for path, want := range map[string]int{"/api/erp-old": 200, "/unmatched": 404, "/api/../erp": 400, "/api//erp": 400, "/api/%65rp": 400, "/api/a%20b": 400} {
		if got := code(t, s.Client(), s.URL+path); got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	stats, err := g.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var external float64
	for _, family := range stats {
		if family.GetName() == "isolatex_requests_total" {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "group" && label.GetValue() == "external" {
						external += metric.GetCounter().GetValue()
					}
				}
			}
		}
	}
	if external != 0 {
		t.Fatal("erp-old routed to external")
	}
	req, _ := http.NewRequest("POST", s.URL+"/api", strings.NewReader(strings.Repeat("x", 33)))
	req.Header.Set("X-Forwarded-For", "spoof")
	req.Header.Set("Forwarded", "spoof")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatal(resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", s.URL+"/api", nil)
	req.Header.Set("X-Forwarded-For", "spoof")
	req.Header.Set("Forwarded", "spoof")
	resp, err = s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestTimeoutReleasesCapacity(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	g.groups[0].cfg.RequestTimeout = 40 * time.Millisecond
	if got := code(t, s.Client(), s.URL+"/api/erp"); got != 504 {
		t.Fatalf("timeout=%d", got)
	}
	a, q := g.groups[0].gate.snapshot()
	if a != 0 || q != 0 {
		t.Fatalf("leak: %d %d", a, q)
	}
}

func TestQueueFIFOAndCancellationRace(t *testing.T) {
	g := newGate(1, 2)
	if err := g.acquire(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- g.acquire(context.Background(), time.Second) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 1 })
	go func() { second <- g.acquire(context.Background(), time.Second) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 2 })
	g.release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
		t.Fatal("FIFO violated")
	default:
	}
	g.release()
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	g.release()
	for i := 0; i < 1000; i++ {
		if err := g.acquire(context.Background(), time.Second); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			err := g.acquire(ctx, time.Second)
			if err == nil {
				g.release()
			}
			done <- err
		}()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); g.release() }()
		wg.Wait()
		<-done
		a, q := g.snapshot()
		if a != 0 || q != 0 {
			t.Fatalf("iteration %d leaked %d/%d", i, a, q)
		}
	}
}

func TestQueueTimeoutAndDrain(t *testing.T) {
	g := newGate(1, 2)
	_ = g.acquire(context.Background(), time.Second)
	if err := g.acquire(context.Background(), time.Millisecond); err != errQueueTimeout {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.acquire(context.Background(), time.Second) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 1 })
	g.close()
	if err := <-done; err != errDraining {
		t.Fatal(err)
	}
	g.release()
	a, q := g.snapshot()
	if a != 0 || q != 0 {
		t.Fatalf("drain leak %d/%d", a, q)
	}
}

func TestChunkedBodyLimitAndClientCancelReleaseSlots(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("wait") {
			<-r.Context().Done()
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		w.WriteHeader(200)
	}))
	// An unknown Content-Length must still be bounded while streaming.
	reader, writer := io.Pipe()
	go func() { _, _ = io.WriteString(writer, strings.Repeat("x", 128)); _ = writer.Close() }()
	req, _ := http.NewRequest("POST", s.URL+"/api/erp", reader)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("chunked overflow=%d", resp.StatusCode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ = http.NewRequestWithContext(ctx, "GET", s.URL+"/api/erp?wait", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := s.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	eventually(t, func() bool { a, _ := g.groups[0].gate.snapshot(); return a == 1 })
	cancel()
	<-done
	eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
	if got := code(t, s.Client(), s.URL+"/api/erp"); got != 200 {
		t.Fatal(got)
	}
}

func TestNoReadyWorkerAndAdminDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/erp" {
			return
		}
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := s.Client().Get(s.URL + "/api/erp")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	if got := code(t, s.Client(), s.URL+"/api/orders"); got != 200 {
		t.Fatalf("normal affected by active ERP request: %d", got)
	}
	release <- struct{}{}
	<-done
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	if got := code(t, admin.Client(), admin.URL+"/readyz"); got != 200 {
		t.Fatal(got)
	}
	g.groups[0].pool.Set(0, false, 0, "failed")
	if got := code(t, s.Client(), s.URL+"/api/erp"); got != 503 {
		t.Fatal(got)
	}
	if got := code(t, admin.Client(), admin.URL+"/readyz"); got != 503 {
		t.Fatal(got)
	}
	if got := code(t, admin.Client(), admin.URL+"/metrics"); got != 200 {
		t.Fatal(got)
	}
	g.Drain()
	if got := code(t, s.Client(), s.URL+"/api/orders"); got != 503 {
		t.Fatal(got)
	}
	if got := code(t, admin.Client(), admin.URL+"/healthz"); got != 200 {
		t.Fatal(got)
	}
}
