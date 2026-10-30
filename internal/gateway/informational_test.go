package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestInformationalResponseBudget(t *testing.T) {
	for _, tt := range []struct {
		name                              string
		count, padding, forwarded, status int
	}{
		{"normal", 3, 16, 3, 200},
		{"count_boundary", 8, 16, 8, 200},
		{"count_exceeded", 16, 16, 8, 502},
		{"bytes_exceeded", 4, 24 << 10, 2, 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/fault" {
					w.Header().Set("X-Hint", strings.Repeat("h", tt.padding))
					for range tt.count {
						w.WriteHeader(http.StatusEarlyHints)
					}
					w.Header().Del("X-Hint")
				}
				_, _ = io.WriteString(w, "ok")
			}), func(c *config.Config) {
				b := c.Bulkheads["isolated"]
				b.CircuitBreaker = &config.CircuitBreaker{Window: time.Minute, MinRequests: 1, FailureRatio: 1, OpenTimeout: time.Minute}
				c.Bulkheads["isolated"] = b
			})
			var observed atomic.Int32
			// An existing caller hook must neither bypass limits nor run twice.
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error { observed.Add(1); return nil }})
				g.ServeHTTP(w, r.WithContext(ctx))
			}))
			defer s.Close()
			conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, err = io.WriteString(conn, "GET /api/fault HTTP/1.1\r\nHost: test\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			forwarded := 0
			for {
				resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if resp.StatusCode < 200 {
					forwarded++
					continue
				}
				if resp.StatusCode != tt.status || forwarded != tt.forwarded || int(observed.Load()) != tt.forwarded {
					t.Fatalf("informational budget: final=%d forwarded=%d hooks=%d", resp.StatusCode, forwarded, observed.Load())
				}
				break
			}
			eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
			snapshot := g.groups[0].breaker.Snapshot()
			expectedFailures := uint64(0)
			expectedState := "closed"
			if tt.status == 502 {
				expectedFailures = 1
				expectedState = "open"
				eventually(t, func() bool {
					recorder := httptest.NewRecorder()
					g.Admin().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
					return strings.Contains(recorder.Body.String(), `isolatex_requests_total{group="isolated",outcome="upstream_informational_limit"} 1`)
				})
			}
			if snapshot.Failures != expectedFailures || snapshot.Requests != 1 || snapshot.State != expectedState || g.groups[0].pool.Snapshot()[0].Active != 0 {
				t.Fatalf("breaker: %+v", snapshot)
			}
			if got := code(t, s.Client(), s.URL+"/api/work"); got != 200 {
				t.Fatalf("protected route: %d", got)
			}
		})
	}
}
