package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/breaker"
	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func readRouting(t *testing.T, url string, want int) RoutingStatus {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s = %d, want %d", url, resp.StatusCode, want)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("routing observation can be cached")
	}
	var status RoutingStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestRoutingProbeSeesSaturationWithoutChangingReadinessOrAdmission(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	g, public := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fault" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(200)
	}), func(c *config.Config) {
		c.RoutingProbePrefix = "/_isolatex/route"
		for name, b := range c.Bulkheads {
			b.RequestTimeout, b.QueueTimeout = 10*time.Second, 10*time.Second
			c.Bulkheads[name] = b
		}
	})
	t.Cleanup(unblock)
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	base := public.URL + "/_isolatex/route/"
	s := readRouting(t, base+"isolated", 200)
	if !s.Eligible || s.FreeConcurrency != 1 || s.FreeQueue != 1 || s.ReadyWorkers != 1 {
		t.Fatalf("initial routing state: %+v", s)
	}
	finished := make(chan struct{}, 2)
	for range 2 {
		go func() {
			resp, err := public.Client().Get(public.URL + "/api/fault")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			finished <- struct{}{}
		}()
	}
	eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 1 && q == 1 })
	for range 10 {
		s = readRouting(t, base+"isolated", 503)
		if s.Eligible || s.Reason != "saturated" || s.Active != 1 || s.Queued != 1 || s.FreeConcurrency != 0 || s.FreeQueue != 0 {
			t.Fatalf("saturated routing state: %+v", s)
		}
	}
	readRouting(t, admin.URL+"/routingz/isolated", 503)
	if got := code(t, admin.Client(), admin.URL+"/readyz/isolated"); got != 200 {
		t.Fatal("saturation changed worker readiness")
	}
	if s := readRouting(t, base+"normal", 200); !s.Eligible {
		t.Fatal("another group was withdrawn")
	}
	if got := code(t, public.Client(), public.URL+"/api/work"); got != 200 {
		t.Fatal("another group's request was blocked")
	}
	unblock()
	<-finished
	<-finished
	s = readRouting(t, base+"isolated", 200)
	if s.Active != 0 || s.Queued != 0 || s.FreeConcurrency != 1 || s.FreeQueue != 1 {
		t.Fatalf("capacity did not recover: %+v", s)
	}
	g.groups[0].pool.Set(0, false, 0, "failed")
	if s := readRouting(t, base+"isolated", 503); s.Reason != "no_ready_worker" {
		t.Fatalf("worker failure: %+v", s)
	}
	readRouting(t, base+"normal", 200)
	g.Drain()
	if s := readRouting(t, base+"normal", 503); s.Reason != "draining" {
		t.Fatalf("drain: %+v", s)
	}
}

func TestRoutingProbeCircuitAndExposureContract(t *testing.T) {
	g, public := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "app") }))
	if got := code(t, public.Client(), public.URL+"/_isolatex/route/normal"); got != 404 {
		t.Fatal("public probes enabled by default")
	}
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	g.groups[0].breaker = breaker.New(&config.CircuitBreaker{Window: time.Second, MinRequests: 1, FailureRatio: 1, OpenTimeout: time.Minute})
	finish, ok := g.groups[0].breaker.Allow()
	if !ok {
		t.Fatal("initial breaker admission")
	}
	finish(breaker.Failure)
	if s := readRouting(t, admin.URL+"/routingz/isolated", 503); s.Reason != "circuit_open" {
		t.Fatalf("breaker: %+v", s)
	}
	readRouting(t, admin.URL+"/routingz/normal", 200)
	resp, err := admin.Client().Head(admin.URL + "/routingz/normal")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(data) != 0 {
		t.Fatal("HEAD probe contract")
	}
	req, _ := http.NewRequest(http.MethodPost, admin.URL+"/routingz/normal", nil)
	resp, err = admin.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Fatal("probe accepts mutation methods")
	}
	if code(t, admin.Client(), admin.URL+"/routingz/unknown") != 404 {
		t.Fatal("unknown routing group accepted")
	}
	resp, err = admin.Client().Get(admin.URL + "/routingz/normal")
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, secret := range []string{"pid", "http://", "upstream", "command"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("probe leaks %s: %s", secret, data)
		}
	}
}

func TestPublicRoutingProbeReservesOnlyConfiguredPrefix(t *testing.T) {
	_, public := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "app") }), func(c *config.Config) {
		c.RoutingProbePrefix = "/_isolatex/route"
		b := c.Bulkheads["normal"]
		b.Match.PathPrefix = "/"
		c.Bulkheads["normal"] = b
	})
	readRouting(t, public.URL+"/_isolatex/route/normal", 200)
	for path, want := range map[string]int{"/_isolatex/route/unknown": 404, "/_isolatex/route/%6eormal": 400, "/_isolatex/route-old/normal": 200, "/normal": 200} {
		if got := code(t, public.Client(), public.URL+path); got != want {
			t.Fatalf("%s = %d, want %d", path, got, want)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, public.URL+"/_isolatex/route/normal", nil)
	resp, err := public.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatal("public probe accepted a mutation method")
	}
}

func TestRoutingObservationDoesNotConsumeHalfOpenProbe(t *testing.T) {
	g, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	b := breaker.New(&config.CircuitBreaker{Window: time.Second, MinRequests: 1, FailureRatio: 1, OpenTimeout: time.Millisecond})
	g.groups[0].breaker = b
	finish, ok := b.Allow()
	if !ok {
		t.Fatal("initial admission")
	}
	finish(breaker.Failure)
	eventually(t, func() bool { return !b.Blocked() })
	for range 3 {
		readRouting(t, admin.URL+"/routingz/isolated", 200)
	}
	if s := b.Snapshot(); s.State != "open" || s.ProbeInFlight {
		t.Fatalf("observation consumed the recovery probe: %+v", s)
	}
	finish, ok = b.Allow()
	if !ok {
		t.Fatal("real recovery probe was blocked")
	}
	if s := readRouting(t, admin.URL+"/routingz/isolated", 503); s.Reason != "circuit_open" {
		t.Fatalf("busy half-open probe: %+v", s)
	}
	finish(breaker.Ignored)
	readRouting(t, admin.URL+"/routingz/isolated", 200)
}
