package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"
)

type latency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type httpOutcome struct {
	Count       int      `json:"count"`
	RatioOfSent float64  `json:"ratio_of_sent"`
	LatencyMS   *latency `json:"latency_ms"`
}

type summary struct {
	Scheduled               int                 `json:"scheduled"`
	Sent                    int                 `json:"sent"`
	GeneratorMissed         int                 `json:"generator_missed"`
	GeneratorLateMeanMS     float64             `json:"generator_late_mean_ms"`
	GeneratorLateMaxMS      float64             `json:"generator_late_max_ms"`
	TransportErrors         int                 `json:"transport_errors"`
	Statuses                map[int]int         `json:"statuses"`
	Successful              int                 `json:"successful"`
	SentPerSecond           float64             `json:"sent_per_phase_second"`
	SuccessRatio            float64             `json:"success_ratio_of_sent"`
	SuccessLatencyMS        *latency            `json:"successful_latency_ms,omitempty"`
	HTTPOutcomes            map[int]httpOutcome `json:"http_outcomes"`
	TransportErrorRatio     float64             `json:"transport_error_ratio_of_sent"`
	TransportErrorLatencyMS *latency            `json:"transport_error_latency_ms,omitempty"`
}

type samples struct {
	mu                              sync.Mutex
	scheduled, sent, missed, errors int
	statuses                        map[int]int
	success                         []time.Duration
	byStatus                        map[int][]time.Duration
	transportErrors                 []time.Duration
	lateTotal, lateMax              time.Duration
}

func (s *samples) summarize(duration time.Duration) summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := summary{Scheduled: s.scheduled, Sent: s.sent, GeneratorMissed: s.missed, TransportErrors: s.errors, Statuses: s.statuses, Successful: len(s.success)}
	r.HTTPOutcomes = make(map[int]httpOutcome, len(s.byStatus))
	for status, values := range s.byStatus {
		outcome := httpOutcome{Count: len(values), LatencyMS: summarizeLatency(values)}
		if s.sent > 0 {
			outcome.RatioOfSent = float64(len(values)) / float64(s.sent)
		}
		r.HTTPOutcomes[status] = outcome
	}
	if s.scheduled > 0 {
		r.GeneratorLateMeanMS = float64(s.lateTotal) / float64(s.scheduled) / float64(time.Millisecond)
	}
	r.GeneratorLateMaxMS = float64(s.lateMax) / float64(time.Millisecond)
	if duration > 0 {
		r.SentPerSecond = float64(s.sent) / duration.Seconds()
	}
	if s.sent > 0 {
		r.SuccessRatio = float64(len(s.success)) / float64(s.sent)
		r.TransportErrorRatio = float64(s.errors) / float64(s.sent)
	}
	r.SuccessLatencyMS = summarizeLatency(s.success)
	r.TransportErrorLatencyMS = summarizeLatency(s.transportErrors)
	return r
}

func summarizeLatency(samples []time.Duration) *latency {
	if len(samples) == 0 {
		return nil
	}
	values := append([]time.Duration(nil), samples...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	quantile := func(p float64) float64 {
		return float64(values[int(math.Ceil(float64(len(values))*p))-1]) / float64(time.Millisecond)
	}
	return &latency{P50: quantile(.50), P95: quantile(.95), P99: quantile(.99)}
}

type collectedPhase struct {
	name           string
	duration       time.Duration
	healthy, fault *samples
}

func trial(ctx context.Context, o options, variant string, repeat int) (trialResult, error) {
	f, err := launch(ctx, o, variant)
	if err != nil {
		return trialResult{}, err
	}
	defer f.close()
	appProcesses, executionBudget, queueBudget := 0, 0, 0
	for _, p := range f.owner.Pools {
		appProcesses += len(p.Snapshot())
	}
	for _, g := range f.runtimeConfig.Bulkheads {
		executionBudget += g.MaxConcurrent
		queueBudget += g.QueueSize
	}
	if appProcesses != 4 || executionBudget != 4 || queueBudget != 8 {
		return trialResult{}, fmt.Errorf("unmatched topology resources: apps=%d active=%d queued=%d", appProcesses, executionBudget, queueBudget)
	}
	before := f.protectedPIDs(variant)
	beforeRestarts := f.faultRestarts(variant)
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 256, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 256, IdleConnTimeout: 10 * time.Second}
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
	defer transport.CloseIdleConnections()
	var pending sync.WaitGroup
	capacity := make(chan struct{}, 256)
	var phases []collectedPhase
	for _, p := range []struct {
		name     string
		duration time.Duration
	}{{"warmup", o.Warmup}, {"baseline", o.Baseline}, {"fault", o.Fault}, {"recovery", o.Recovery}} {
		if p.duration == 0 {
			continue
		}
		collected := collectedPhase{name: p.name, duration: p.duration, healthy: &samples{statuses: map[int]int{}}, fault: &samples{statuses: map[int]int{}}}
		phases = append(phases, collected)
		start := time.Now()
		var schedules sync.WaitGroup
		schedules.Add(2)
		go func() {
			defer schedules.Done()
			generate(ctx, client, f.origin+"/healthy/work", o.HealthyRate, start, p.duration, "", collected.healthy, capacity, &pending)
		}()
		go func() {
			defer schedules.Done()
			scenario := ""
			if p.name == "fault" {
				scenario = o.Scenario
			}
			generate(ctx, client, f.origin+"/fault/work", o.FaultRate, start, p.duration, scenario, collected.fault, capacity, &pending)
		}()
		schedules.Wait()
		// Do not wait for in-flight requests here: recovery must observe work
		// left behind by the fault phase, rather than silently draining it.
		if wait := time.Until(start.Add(p.duration)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
		if ctx.Err() != nil {
			pending.Wait()
			return trialResult{}, ctx.Err()
		}
		select {
		case <-f.done:
			pending.Wait()
			return trialResult{}, fmt.Errorf("gateway exited during experiment: %v", f.waitErr)
		default:
		}
	}
	pending.Wait()
	restarts := f.faultRestarts(variant) - beforeRestarts
	if o.Scenario == "crash" && restarts == 0 {
		return trialResult{}, fmt.Errorf("no fault worker restart observed; crash injection was not verified (reduce load or increase fault/recovery duration)")
	}
	result := trialResult{Variant: variant, Repeat: repeat, AppProcesses: appProcesses, ExecutionBudget: executionBudget, QueueBudget: queueBudget, ProtectedPIDsUnchanged: reflect.DeepEqual(before, f.protectedPIDs(variant)), FaultWorkerRestarts: restarts}
	for _, p := range phases {
		result.Phases = append(result.Phases, phaseResult{Name: p.name, DurationSeconds: p.duration.Seconds(), Healthy: p.healthy.summarize(p.duration), Fault: p.fault.summarize(p.duration)})
	}
	return result, nil
}

func generate(ctx context.Context, client *http.Client, path string, rate int, start time.Time, duration time.Duration, scenario string, samples *samples, capacity chan struct{}, pending *sync.WaitGroup) {
	count := (int64(duration)*int64(rate) + int64(time.Second) - 1) / int64(time.Second)
	for i := int64(0); i < count; i++ {
		due := start.Add(time.Duration(i * int64(time.Second) / int64(rate)))
		if wait := time.Until(due); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		late := max(0, time.Since(due))
		samples.mu.Lock()
		samples.scheduled++
		samples.lateTotal += late
		samples.lateMax = max(samples.lateMax, late)
		samples.mu.Unlock()
		// Report generator overload rather than reducing offered load invisibly.
		if late > 100*time.Millisecond {
			samples.mu.Lock()
			samples.missed++
			samples.mu.Unlock()
			continue
		}
		select {
		case capacity <- struct{}{}:
		default:
			samples.mu.Lock()
			samples.missed++
			samples.mu.Unlock()
			continue
		}
		target := path
		method := http.MethodGet
		if scenario == "delay" {
			target += "?delay=1s"
		}
		if scenario == "crash" && i%int64(rate) == 0 {
			target += "?crash=1"
			// The worker may exit after reading this request. POST prevents
			// automatic replay on a reused connection with no response.
			method = http.MethodPost
		}
		samples.mu.Lock()
		samples.sent++
		samples.mu.Unlock()
		pending.Add(1)
		go func() {
			defer pending.Done()
			defer func() { <-capacity }()
			started := time.Now()
			req, err := http.NewRequestWithContext(ctx, method, target, nil)
			var resp *http.Response
			if err == nil {
				resp, err = client.Do(req)
			}
			if resp != nil {
				_, bodyErr := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err == nil {
					err = bodyErr
				}
			}
			elapsed := time.Since(started)
			samples.mu.Lock()
			defer samples.mu.Unlock()
			if err != nil {
				samples.errors++
				samples.transportErrors = append(samples.transportErrors, elapsed)
				return
			}
			samples.statuses[resp.StatusCode]++
			if samples.byStatus == nil {
				samples.byStatus = make(map[int][]time.Duration)
			}
			samples.byStatus[resp.StatusCode] = append(samples.byStatus[resp.StatusCode], elapsed)
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				samples.success = append(samples.success, elapsed)
			}
		}()
	}
}
