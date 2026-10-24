package gateway

import (
	"github.com/hyunsuhahaha/isolatex/internal/config"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type metrics struct {
	requests *prometheus.CounterVec
	rejected *prometheus.CounterVec
	duration *prometheus.HistogramVec
	queue    *prometheus.HistogramVec
}

func (b *group) workerResources() config.Resources {
	if b.cfg.Process != nil && b.cfg.Process.Resources != nil {
		return *b.cfg.Process.Resources
	}
	return config.Resources{}
}

func (b *group) workerMemoryLimit() int64 { return b.workerResources().MemoryMaxBytes }

func newRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}

// RegisterLogDrops installs runtime-wide log delivery accounting before Serve.
func (g *Gateway) RegisterLogDrops(dropped func() uint64) {
	g.registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "isolatex_dropped_log_records_total",
		Help: "Runtime log records dropped due to capacity, size, delivery failure, or shutdown.",
	}, func() float64 { return float64(dropped()) }))
}

func (b *group) registerMetrics(r *prometheus.Registry) {
	labels := prometheus.Labels{"group": b.name}
	b.metrics = metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "isolatex_requests_total", Help: "Completed classified requests by final outcome.", ConstLabels: labels}, []string{"outcome"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "isolatex_rejected_requests_total", Help: "Requests rejected locally before upstream dispatch.", ConstLabels: labels}, []string{"reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "isolatex_request_duration_seconds", Help: "Classified request duration including queue wait and response transfer.", ConstLabels: labels, Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10, 30, 60}}, []string{"outcome"}),
		queue:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "isolatex_queue_duration_seconds", Help: "Time spent acquiring an execution slot, including immediate admissions and rejections.", ConstLabels: labels, Buckets: []float64{.0001, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10}}, []string{"outcome"}),
	}
	r.MustRegister(b.metrics.requests, b.metrics.rejected, b.metrics.duration, b.metrics.queue)
	gauge := func(name, help string, f func() float64) {
		r.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help, ConstLabels: labels}, f))
	}
	gauge("isolatex_active_requests", "Requests holding an execution slot.", func() float64 { a, _ := b.gate.snapshot(); return float64(a) })
	gauge("isolatex_queued_requests", "Requests waiting for an execution slot.", func() float64 { _, q := b.gate.snapshot(); return float64(q) })
	gauge("isolatex_ready_workers", "Workers eligible for new requests.", func() float64 { return float64(b.pool.Ready()) })
	gauge("isolatex_breaker_state", "Circuit breaker state: 0=Closed, 1=Open, 2=Half-open.", func() float64 {
		switch b.breaker.Snapshot().State {
		case "open":
			return 1
		case "half_open":
			return 2
		default:
			return 0
		}
	})
	gauge("isolatex_concurrent_limit", "Configured group execution limit.", func() float64 { return float64(b.cfg.MaxConcurrent) })
	gauge("isolatex_queue_limit", "Configured group queue capacity.", func() float64 { return float64(b.cfg.QueueSize) })
	gauge("isolatex_worker_memory_limit_bytes", "Configured per-worker and descendant memory budget; zero disables. Accounting is platform-specific.", func() float64 { return float64(b.workerMemoryLimit()) })
	gauge("isolatex_worker_tasks_limit", "Configured per-worker Linux task limit including descendant threads; zero disables.", func() float64 { return float64(b.workerResources().TasksMax) })
	gauge("isolatex_worker_processes_limit", "Configured per-worker Windows active process limit including descendants; zero disables.", func() float64 { return float64(b.workerResources().ProcessesMax) })
	counter := func(name, help string, f func() float64) {
		r.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help, ConstLabels: labels}, f))
	}
	counter("isolatex_worker_restarts_total", "Managed worker restarts.", func() float64 {
		n := 0
		for _, w := range b.pool.Snapshot() {
			n += w.Restarts
		}
		return float64(n)
	})
	counter("isolatex_health_checks_total", "Worker readiness checks performed.", func() float64 {
		var n uint64
		for _, w := range b.pool.Snapshot() {
			n += w.HealthChecks
		}
		return float64(n)
	})
	counter("isolatex_health_check_failures_total", "Failed worker readiness checks.", func() float64 {
		var n uint64
		for _, w := range b.pool.Snapshot() {
			n += w.HealthFailures
		}
		return float64(n)
	})
	counter("isolatex_breaker_transitions_total", "Circuit breaker state transitions.", func() float64 { return float64(b.breaker.Snapshot().Transitions) })
}

func (m *metrics) record(outcome string, d time.Duration) {
	m.requests.WithLabelValues(outcome).Inc()
	m.duration.WithLabelValues(outcome).Observe(d.Seconds())
	switch outcome {
	case "queue_full", "queue_timeout", "no_ready_worker", "circuit_open", "draining":
		m.rejected.WithLabelValues(outcome).Inc()
	}
}
