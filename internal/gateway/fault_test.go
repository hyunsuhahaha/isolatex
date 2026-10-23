package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"isolatex/internal/config"
)

func TestCircuitOpensFailsFastAndRecovers(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	var calls atomic.Int32
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/erp" && failing.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}), func(c *config.Config) {
		b := c.Bulkheads["external"]
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Second, MinRequests: 2, FailureRatio: 1, OpenTimeout: 40 * time.Millisecond}
		c.Bulkheads["external"] = b
	})
	for i := 0; i < 2; i++ {
		if got := code(t, s.Client(), s.URL+"/api/erp"); got != 503 {
			t.Fatal(got)
		}
	}
	if state := g.groups[0].breaker.Snapshot().State; state != "open" {
		t.Fatalf("state=%s", state)
	}
	before := calls.Load()
	resp, err := s.Client().Get(s.URL + "/api/erp")
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 128)
	n, _ := resp.Body.Read(data)
	resp.Body.Close()
	if !strings.Contains(string(data[:n]), "circuit_open") || calls.Load() != before {
		t.Fatal("Open request reached backend")
	}
	if got := code(t, s.Client(), s.URL+"/api/orders"); got != 200 {
		t.Fatal("normal affected")
	}
	failing.Store(false)
	time.Sleep(45 * time.Millisecond)
	if got := code(t, s.Client(), s.URL+"/api/erp"); got != 200 {
		t.Fatal(got)
	}
	if state := g.groups[0].breaker.Snapshot().State; state != "closed" {
		t.Fatalf("recovery state=%s", state)
	}
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	resp, err = admin.Client().Get(admin.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	families, err := g.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, family := range families {
		seen[family.GetName()] = true
		if family.GetName() == "isolatex_request_duration_seconds" {
			for _, m := range family.Metric {
				h := m.GetHistogram()
				if h.GetSampleCount() == 0 || len(h.Bucket) == 0 {
					t.Fatal("histogram observations missing")
				}
			}
		}
	}
	for _, name := range []string{"isolatex_requests_total", "isolatex_rejected_requests_total", "isolatex_request_duration_seconds", "isolatex_queue_duration_seconds", "isolatex_breaker_state"} {
		if !seen[name] {
			t.Errorf("metric %s missing", name)
		}
	}
}

func TestLocalRejectionsDoNotTripCircuit(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	defer close(release)
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}), func(c *config.Config) {
		b := c.Bulkheads["external"]
		b.QueueSize = 0
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Second, MinRequests: 1, FailureRatio: .5, OpenTimeout: time.Second}
		c.Bulkheads["external"] = b
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := s.Client().Get(s.URL + "/api/erp")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	for i := 0; i < 4; i++ {
		if got := code(t, s.Client(), s.URL+"/api/erp"); got != 503 {
			t.Fatal(got)
		}
	}
	if s := g.groups[0].breaker.Snapshot(); s.Requests != 0 || s.State != "closed" {
		t.Fatalf("local rejects counted as failures: %+v", s)
	}
	release <- struct{}{}
	<-done
}
