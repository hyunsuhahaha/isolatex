package gateway

import (
	"bufio"
	"errors"
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

func TestRequestBodyCircuitAttribution(t *testing.T) {
	for _, mode := range []string{"stalled_client", "stalled_backend", "confirmed_5xx", "early_success"} {
		t.Run(mode, func(t *testing.T) { checkRequestBodyCircuit(t, mode) })
	}
}

func TestMalformedRequestBodyKeepsCircuitClosed(t *testing.T) {
	for name, payload := range map[string]string{
		"invalid_size":       "Z\r\n",
		"invalid_terminator": "1\r\nx!!",
		"invalid_trailer":    "0\r\nbad trailer\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) { checkMalformedRequestBody(t, payload) })
	}
}

func checkMalformedRequestBody(t *testing.T, payload string) {
	t.Helper()
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		_, _ = io.WriteString(w, "ok")
	}), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Minute, MinRequests: 1, FailureRatio: .5, OpenTimeout: time.Minute}
		c.Bulkheads["isolated"] = b
	})
	if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusOK {
		t.Fatalf("warmup: %d", got)
	}
	conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "POST /api/fault HTTP/1.1\r\nHost: test\r\nTransfer-Encoding: chunked\r\n\r\n"+payload); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
	snapshot := g.groups[0].breaker.Snapshot()
	if resp.StatusCode != http.StatusBadRequest || readErr != nil || snapshot.State != "closed" || snapshot.Failures != 0 || snapshot.Requests != 1 {
		t.Fatalf("malformed body: status=%d read=%v breaker=%+v", resp.StatusCode, readErr, snapshot)
	}
	eventually(t, func() bool {
		recorder := httptest.NewRecorder()
		g.Admin().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return strings.Contains(recorder.Body.String(), `isolatex_requests_total{group="isolated",outcome="request_body_error"} 1`)
	})
	if !resp.Close {
		t.Fatal("malformed request connection remains reusable")
	}
	for _, path := range []string{"/api/fault", "/api/work"} {
		if got := code(t, s.Client(), s.URL+path); got != http.StatusOK {
			t.Fatalf("healthy request: path=%s status=%d", path, got)
		}
	}
}

func checkRequestBodyCircuit(t *testing.T, mode string) {
	t.Helper()
	var uploads atomic.Int32
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 0 {
			uploads.Add(1)
			if mode == "confirmed_5xx" || mode == "early_success" {
				rc := http.NewResponseController(w)
				if err := rc.EnableFullDuplex(); err != nil {
					return
				}
				status := http.StatusServiceUnavailable
				if mode == "early_success" {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "x")
				_ = rc.Flush()
			}
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				return
			}
			if mode == "stalled_backend" {
				<-r.Context().Done()
				return
			}
		}
		_, _ = io.WriteString(w, "ok")
	}), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Minute, MinRequests: 1, FailureRatio: .5, OpenTimeout: time.Minute}
		c.Bulkheads["isolated"] = b
	})
	if mode == "early_success" {
		// The response-copy goroutine can observe the request deadline before
		// Transport's body-copy goroutine publishes the socket read timeout.
		s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = delayedBodyReadError{r.Body}
			g.ServeHTTP(w, r)
		}))
		t.Cleanup(s.Close)
	}
	if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusOK {
		t.Fatalf("warmup: %d", got)
	}
	conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Keep the backend-failure controls: completing the upload must not hide
	// a stalled backend, nor may a client stall erase an observed HTTP 5xx.
	payload := "x"
	if mode == "stalled_backend" {
		payload = "xxxx"
	}
	if _, err := fmt.Fprint(conn, "POST /api/fault HTTP/1.1\r\nHost: test\r\nContent-Length: 4\r\n\r\n"+payload); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	streamed := mode == "confirmed_5xx" || mode == "early_success"
	if err != nil && !(streamed && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))) {
		t.Fatal(err)
	}
	expectedStatus := http.StatusGatewayTimeout
	if mode == "confirmed_5xx" {
		expectedStatus = http.StatusServiceUnavailable
	} else if mode == "early_success" {
		expectedStatus = http.StatusOK
	}
	if resp != nil {
		_, readErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if (readErr != nil && !streamed) || resp.StatusCode != expectedStatus {
			t.Fatalf("stalled upload: status=%d read=%v", resp.StatusCode, readErr)
		}
	}
	if uploads.Load() != 1 {
		t.Fatalf("upload did not reach upstream: %d", uploads.Load())
	}
	eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
	snapshot := g.groups[0].breaker.Snapshot()
	expectedState, expectedFailures, expectedRequests := "closed", uint64(0), uint64(1)
	clientTimeout := mode == "stalled_client" || mode == "early_success"
	if !clientTimeout {
		expectedState, expectedFailures, expectedRequests = "open", 1, 2
	}
	if snapshot.State != expectedState || snapshot.Failures != expectedFailures || snapshot.Requests != expectedRequests {
		t.Fatalf("request body/circuit attribution: mode=%s snapshot=%+v", mode, snapshot)
	}
	if clientTimeout {
		eventually(t, func() bool {
			recorder := httptest.NewRecorder()
			g.Admin().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			return strings.Contains(recorder.Body.String(), `isolatex_requests_total{group="isolated",outcome="request_body_timeout"} 1`)
		})
	}
	for _, path := range []string{"/api/fault", "/api/work"} {
		expected := http.StatusOK
		if !clientTimeout && path == "/api/fault" {
			expected = http.StatusServiceUnavailable
		}
		if got := code(t, s.Client(), s.URL+path); got != expected {
			t.Fatalf("healthy request after stalled upload: path=%s status=%d", path, got)
		}
	}
}

type delayedBodyReadError struct{ io.ReadCloser }

func (b delayedBodyReadError) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		time.Sleep(100 * time.Millisecond)
	}
	return n, err
}

// A backend closing its socket after a valid upload remains failure evidence.
func TestBackendResetAfterUploadRemainsFailure(t *testing.T) {
	g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, "ok")
	}), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.CircuitBreaker = &config.CircuitBreaker{Window: time.Minute, MinRequests: 1, FailureRatio: .5, OpenTimeout: time.Minute}
		c.Bulkheads["isolated"] = b
	})
	if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusOK {
		t.Fatalf("warmup: %d", got)
	}
	conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "POST /api/fault HTTP/1.1\r\nHost: test\r\nContent-Length: 4\r\n\r\nxxxx"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
	snapshot := g.groups[0].breaker.Snapshot()
	if resp.StatusCode != http.StatusBadGateway || snapshot.State != "open" || snapshot.Failures != 1 || snapshot.Requests != 2 {
		t.Fatalf("backend reset: status=%d breaker=%+v", resp.StatusCode, snapshot)
	}
	if got := code(t, s.Client(), s.URL+"/api/work"); got != http.StatusOK {
		t.Fatalf("protected group: %d", got)
	}
}
