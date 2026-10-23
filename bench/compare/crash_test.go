package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCrashInjectionIsNotReplayedAfterResponseConnectionCloses(t *testing.T) {
	var injections atomic.Int32
	methods := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("crash") == "1" {
			methods <- r.Method
			if injections.Add(1) == 1 {
				// Simulate a process disappearing after reading the crash
				// request but before sending any response bytes.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	transport := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	// Retry of replayable requests requires a previously successful connection.
	resp, err := client.Get(server.URL + "/prime")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	s := &samples{statuses: map[int]int{}}
	var pending sync.WaitGroup
	generate(context.Background(), client, server.URL+"/fault", 1, time.Now(), time.Second, "crash", s, make(chan struct{}, 1), &pending)
	pending.Wait()
	if got := injections.Load(); got != 1 {
		t.Fatalf("one scheduled crash executed %d times", got)
	}
	if method := <-methods; method != http.MethodPost {
		t.Fatalf("crash method = %s", method)
	}
	result := s.summarize(time.Second)
	if result.Scheduled != 1 || result.Sent != 1 || result.TransportErrors != 1 || result.Successful != 0 {
		t.Fatalf("crash attempt accounting: %+v", result)
	}
}
