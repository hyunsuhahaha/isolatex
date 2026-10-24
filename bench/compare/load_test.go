package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSummaryKeepsFastRejectionsOutOfSuccessQuantiles(t *testing.T) {
	s := samples{scheduled: 7, sent: 6, missed: 1, errors: 1, statuses: map[int]int{200: 3, 503: 2}, success: []time.Duration{100 * time.Millisecond, time.Millisecond, 10 * time.Millisecond}, byStatus: map[int][]time.Duration{200: {100 * time.Millisecond, time.Millisecond, 10 * time.Millisecond}, 503: {time.Microsecond, 2 * time.Microsecond}}, transportErrors: []time.Duration{time.Second}}
	r := s.summarize(2 * time.Second)
	if r.SuccessLatencyMS == nil || r.SuccessLatencyMS.P50 != 10 || r.SuccessLatencyMS.P95 != 100 || r.SuccessLatencyMS.P99 != 100 {
		t.Fatalf("successful quantiles: %+v", r.SuccessLatencyMS)
	}
	if r.Scheduled != r.Sent+r.GeneratorMissed || r.Successful != 3 || r.SuccessRatio != .5 || r.SentPerSecond != 3 {
		t.Fatalf("cohort accounting: %+v", r)
	}
	if r.TransportErrors+r.Statuses[200]+r.Statuses[503] != r.Sent {
		t.Fatal("outcomes do not account for sent requests")
	}
	if got := r.HTTPOutcomes[503]; got.Count != 2 || got.RatioOfSent != 2.0/6 || got.LatencyMS.P95 != .002 {
		t.Fatalf("503 cohort: %+v", got)
	}
	if r.TransportErrorRatio != 1.0/6 || r.TransportErrorLatencyMS.P95 != 1000 {
		t.Fatalf("transport failure cohort: %+v", r)
	}
}

// A response with 200 headers and a truncated body must remain a transport
// failure, rather than improving the success or HTTP-status latency cohorts.
func TestGeneratorSeparatesHTTPOutcomesAndIncompleteBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/success":
			w.WriteHeader(200)
		case "/reject":
			w.WriteHeader(503)
		case "/failure":
			w.WriteHeader(500)
		case "/truncated":
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
		}
		_, _ = io.WriteString(w, "short")
	}))
	defer server.Close()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	s := &samples{statuses: map[int]int{}}
	var pending sync.WaitGroup
	for _, path := range []string{"/success", "/reject", "/failure", "/truncated"} {
		generate(context.Background(), client, server.URL+path, 1, time.Now(), time.Millisecond, "", s, make(chan struct{}, 1), &pending)
	}
	pending.Wait()
	r := s.summarize(time.Second)
	if r.Sent != 4 || r.Successful != 1 || r.SuccessRatio != .25 || r.TransportErrors != 1 || r.TransportErrorRatio != .25 || r.TransportErrorLatencyMS == nil {
		t.Fatalf("outcome accounting: %+v", r)
	}
	if len(r.HTTPOutcomes) != 3 {
		t.Fatalf("HTTP cohorts: %+v", r.HTTPOutcomes)
	}
	for _, status := range []int{200, 503, 500} {
		got := r.HTTPOutcomes[status]
		if r.Statuses[status] != 1 || got.Count != 1 || got.RatioOfSent != .25 || got.LatencyMS == nil {
			t.Fatalf("status %d: %+v", status, got)
		}
	}
}

func TestNoSuccessHasNoInventedLatency(t *testing.T) {
	s := samples{scheduled: 1, sent: 1, statuses: map[int]int{503: 1}}
	if got := s.summarize(time.Second); got.SuccessLatencyMS != nil || got.SuccessRatio != 0 {
		t.Fatalf("all-rejected cohort: %+v", got)
	}
}

func TestExperimentRejectsUnboundedOrEmptyPlans(t *testing.T) {
	valid := options{Scenario: "delay", Baseline: time.Second, Fault: time.Second, Recovery: time.Second, HealthyRate: 40, FaultRate: 40, Repeats: 1}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*options){
		func(o *options) { o.Baseline = 0 },
		func(o *options) { o.FaultRate = 0 },
		func(o *options) { o.Repeats = 0 },
		func(o *options) { o.Scenario = "unknown" },
		func(o *options) { o.HealthyRate = 5000; o.FaultRate = 5000; o.Fault = 10 * time.Minute; o.Repeats = 20 },
	} {
		o := valid
		mutate(&o)
		if err := o.validate(); err == nil {
			t.Fatal("invalid experiment accepted")
		}
	}
}
