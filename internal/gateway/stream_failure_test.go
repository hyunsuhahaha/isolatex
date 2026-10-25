package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestClientResetPreservesBackendFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			payload := strings.Repeat("x", 8<<20)
			g, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/stream") {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					w.WriteHeader(status)
					_, _ = io.WriteString(w, payload)
					return
				}
				_, _ = io.WriteString(w, "ok")
			}), func(c *config.Config) {
				b := c.Bulkheads["isolated"]
				b.CircuitBreaker = &config.CircuitBreaker{Window: time.Minute, MinRequests: 1, FailureRatio: .5, OpenTimeout: time.Minute}
				c.Bulkheads["isolated"] = b
			})
			var progress atomic.Pointer[time.Time]
			s := httptest.NewUnstartedServer(g)
			s.Listener = smallWriteListener{s.Listener, &progress}
			closed := make(chan struct{}, 4)
			s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			s.Start()
			t.Cleanup(s.Close)
			if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusOK {
				t.Fatalf("warmup: %d", got)
			}
			conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(conn, "GET /api/fault/stream HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != status {
				t.Fatalf("backend status: %d", resp.StatusCode)
			}
			// Abort a real TCP response after its status is observed, before
			// consuming the body. SO_LINGER=0 forces a reset instead of a drain.
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("reset request did not finish")
			}
			eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
			snapshot := g.groups[0].breaker.Snapshot()
			state, requests, failures := "closed", uint64(1), uint64(0)
			if status >= 500 {
				state, requests, failures = "open", 2, 1
			}
			if snapshot.State != state || snapshot.Requests != requests || snapshot.Failures != failures {
				t.Fatalf("backend %d then client reset: %+v", status, snapshot)
			}
			eventually(t, func() bool {
				recorder := httptest.NewRecorder()
				g.Admin().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				for _, outcome := range []string{"stream_error", "canceled", "timeout"} {
					if strings.Contains(recorder.Body.String(), `isolatex_requests_total{group="isolated",outcome="`+outcome+`"} 1`) {
						return true
					}
				}
				return false
			})
			if got := code(t, s.Client(), s.URL+"/api/work"); got != http.StatusOK {
				t.Fatalf("protected group: %d", got)
			}
		})
	}
}
