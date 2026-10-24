package gateway

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/ingress"
)

type smallWriteListener struct{ net.Listener }

func (l smallWriteListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		_ = c.(*net.TCPConn).SetWriteBuffer(1024)
	}
	return c, err
}

func TestBufferedResponseBudgetReclaimsNonReadingConnection(t *testing.T) {
	checkNonReadingConnection(t, "/api/fault", false)
}

func TestLocalResponseBudgetReclaimsNonReadingConnection(t *testing.T) {
	for _, path := range []string{"/not-configured", "/_isolatex/route/normal", "/api/fault"} {
		t.Run(path, func(t *testing.T) { checkNonReadingConnection(t, path, true) })
	}
}

func TestResponseBudgetResetsForKeepAlive(t *testing.T) {
	_, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}), func(c *config.Config) {
		c.RoutingProbePrefix = "/_isolatex/route"
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 200 * time.Millisecond
		c.Bulkheads["isolated"] = b
	})
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 1}}
	defer client.CloseIdleConnections()
	for i, path := range []string{"/api/fault", "/_isolatex/route/normal", "/api/fault"} {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		reused := false
		req, err := http.NewRequest(http.MethodGet, s.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		}))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || (i > 0 && !reused) {
			t.Fatalf("request %d: status=%d reused=%t read=%v", i, resp.StatusCode, reused, readErr)
		}
	}
}

func checkNonReadingConnection(t *testing.T, path string, local bool) {
	t.Helper()
	payload := strings.Repeat("x", 1024)
	var calls atomic.Int32
	g, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fault" {
			calls.Add(1)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = io.WriteString(w, payload)
	}), func(c *config.Config) {
		c.RoutingProbePrefix = "/_isolatex/route"
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 200 * time.Millisecond
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Second, MinRequests: 1, FailureRatio: 1, OpenTimeout: time.Minute}
		c.Bulkheads["isolated"] = b
	})
	if local && path == "/api/fault" {
		g.groups[0].pool.Set(0, false, 0, "failed")
	}
	s := httptest.NewUnstartedServer(g)
	limited := ingress.New(smallWriteListener{s.Listener}, 1)
	s.Listener = limited
	s.Config.ReadHeaderTimeout = 5 * time.Second
	s.Config.IdleTimeout = 5 * time.Second
	closed := make(chan struct{}, 8)
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}}
	defer client.CloseIdleConnections()
	for cycle := 0; cycle < 3; cycle++ {
		bad, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bad.Close() })
		_ = bad.(*net.TCPConn).SetReadBuffer(1024)
		_ = bad.SetWriteDeadline(time.Now().Add(2 * time.Second))
		// Each known-length response fits the HTTP response buffer. Without
		// an explicit flush its final socket write outlives the handler.
		_, _ = io.WriteString(bad, strings.Repeat("GET "+path+" HTTP/1.1\r\nHost: test\r\n\r\n", 4096))
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatalf("cycle %d: non-reading connection survived its response budget; accepted=%d upstream_calls=%d", cycle, limited.Active(), calls.Load())
		}
		_ = bad.Close()
		if limited.Active() != 0 {
			t.Fatalf("connection permit leaked: %d", limited.Active())
		}
		resp, err := client.Get(s.URL + "/api/work")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("healthy route after connection recovery: %d", resp.StatusCode)
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("healthy connection not collected")
		}
		if state := g.groups[0].breaker.Snapshot().State; state != "closed" {
			t.Fatalf("slow client opened upstream circuit: %s", state)
		}
		for _, b := range g.groups {
			active, queued := b.gate.snapshot()
			if active != 0 || queued != 0 {
				t.Fatalf("%s leaked request slots: %d/%d", b.name, active, queued)
			}
		}
	}
	if !local && calls.Load() < 4 {
		t.Fatalf("test did not exercise pipelined buffered responses: %d", calls.Load())
	}
	if !local {
		families, err := g.registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var writeFailures float64
		for _, family := range families {
			if family.GetName() != "isolatex_requests_total" {
				continue
			}
			for _, metric := range family.Metric {
				var group, outcome string
				for _, label := range metric.Label {
					if label.GetName() == "group" {
						group = label.GetValue()
					}
					if label.GetName() == "outcome" {
						outcome = label.GetValue()
					}
				}
				if group == "isolated" && (outcome == "stream_error" || outcome == "timeout") {
					writeFailures += metric.GetCounter().GetValue()
				}
			}
		}
		if writeFailures < 3 {
			t.Fatalf("buffered write failures disappeared from outcomes: %v", writeFailures)
		}
	}
}
