package main

import (
	"testing"
	"time"
)

func TestSummaryKeepsFastRejectionsOutOfSuccessQuantiles(t *testing.T) {
	s := samples{scheduled: 7, sent: 6, missed: 1, errors: 1, statuses: map[int]int{200: 3, 503: 2}, success: []time.Duration{100 * time.Millisecond, time.Millisecond, 10 * time.Millisecond}}
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
