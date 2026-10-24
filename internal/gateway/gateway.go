package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/breaker"
	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/pool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type group struct {
	name      string
	cfg       config.Group
	pool      *pool.Pool
	gate      *gate
	transport *http.Transport
	dialer    *requestDialer
	breaker   *breaker.Breaker
	metrics   metrics
}

type Gateway struct {
	cfg      config.Config
	groups   []*group
	draining atomic.Bool
	log      *slog.Logger
	registry *prometheus.Registry
}

// New requires a validated config. The group gates enforce the global bound
// through reserved budgets, without a shared semaphore on the request path.
func New(c config.Config, pools map[string]*pool.Pool, logger *slog.Logger) *Gateway {
	g := &Gateway{cfg: c, log: logger, registry: newRegistry()}
	for name, cfg := range c.Bulkheads {
		dialer := &requestDialer{dial: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
		t := &http.Transport{Proxy: nil, DialContext: dialer.DialContext, MaxIdleConns: cfg.MaxConcurrent, MaxIdleConnsPerHost: cfg.MaxConcurrent, MaxConnsPerHost: cfg.MaxConcurrent, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: cfg.RequestTimeout, MaxResponseHeaderBytes: 64 << 10, DisableCompression: true}
		b := &group{name: name, cfg: cfg, pool: pools[name], gate: newGate(cfg.MaxConcurrent, cfg.QueueSize), transport: t, dialer: dialer, breaker: breaker.New(cfg.CircuitBreaker)}
		b.registerMetrics(g.registry)
		g.groups = append(g.groups, b)
	}
	sort.Slice(g.groups, func(i, j int) bool {
		return len(g.groups[i].cfg.Match.PathPrefix) > len(g.groups[j].cfg.Match.PathPrefix)
	})
	return g
}

func (g *Gateway) Drain() {
	g.draining.Store(true)
	for _, b := range g.groups {
		b.gate.close()
	}
}
func (g *Gateway) Close() {
	g.Drain()
	for _, b := range g.groups {
		b.transport.CloseIdleConnections()
	}
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// Local rejections and routing probes also need a bounded final socket
	// write. net/http resets this after finishing the response, not us before
	// its buffered tail has been written.
	_ = rc.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	// Explicitly reserved probes precede routing and draining rejection so
	// remote balancers can observe why a group is ineligible during shutdown.
	if prefix := g.cfg.RoutingProbePrefix; prefix != "" && (r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/")) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.RawPath != "" || !config.ValidPath(r.URL.Path) {
			fail(w, 400, "invalid_path")
			return
		}
		g.serveRoutingProbe(w, r, strings.TrimPrefix(r.URL.Path, prefix+"/"))
		return
	}
	if g.draining.Load() {
		fail(w, 503, "draining")
		return
	}
	if r.URL.RawPath != "" || !config.ValidPath(r.URL.Path) {
		fail(w, 400, "invalid_path")
		return
	}
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" || strings.Contains(r.Header.Get("Accept"), "text/event-stream") || strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		fail(w, 501, "unsupported_protocol")
		return
	}
	var b *group
	for _, candidate := range g.groups {
		prefix := candidate.cfg.Match.PathPrefix
		if prefix == "/" || r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			b = candidate
			break
		}
	}
	if b == nil {
		fail(w, 404, "no_route")
		return
	}
	started := time.Now()
	outcome := "internal_error"
	defer func() {
		b.metrics.record(outcome, time.Since(started))
		g.log.Debug("request", "group", b.name, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds())
	}()
	if r.ContentLength > g.cfg.MaxBodyBytes {
		outcome = "body_too_large"
		fail(w, 413, outcome)
		return
	}
	if b.breaker.Blocked() {
		outcome = "circuit_open"
		fail(w, 503, outcome)
		return
	}
	if b.pool.Ready() == 0 {
		outcome = "no_ready_worker"
		fail(w, 503, outcome)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.cfg.RequestTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	// Queue timeout replies must still have the same small write grace as
	// upstream timeout replies. Dispatch tightens this to the request budget.
	_ = rc.SetWriteDeadline(deadline.Add(250 * time.Millisecond))
	queueStart := time.Now()
	admissionErr := b.gate.acquire(ctx, b.cfg.QueueTimeout)
	queueOutcome := "assigned"
	if admissionErr != nil {
		queueOutcome = admissionErr.Error()
	}
	b.metrics.queue.WithLabelValues(queueOutcome).Observe(time.Since(queueStart).Seconds())
	if err := admissionErr; err != nil {
		outcome = err.Error()
		switch {
		case errors.Is(err, context.Canceled):
			outcome = "canceled"
			return
		case errors.Is(err, context.DeadlineExceeded):
			outcome = "timeout"
			fail(w, 504, outcome)
		default:
			fail(w, 503, outcome)
		}
		return
	}
	defer b.gate.release()
	if err := contextErrorAt(ctx, time.Now()); err != nil {
		outcome = "canceled"
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = "timeout"
			fail(w, 504, outcome)
		}
		return
	}
	finish, allowed := b.breaker.Allow()
	if !allowed {
		outcome = "circuit_open"
		fail(w, 503, outcome)
		return
	}
	result := breaker.Ignored
	defer func() { finish(result) }()
	target, release := b.pool.Pick()
	if target == nil {
		outcome = "no_ready_worker"
		fail(w, 503, outcome)
		return
	}
	defer release()
	expired := func() bool { return !time.Now().Before(deadline) || errors.Is(ctx.Err(), context.DeadlineExceeded) }
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	r = r.WithContext(context.WithValue(ctx, dialRequestContextKey{}, ctx))
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxBodyBytes)
	tracked := &trackingWriter{ResponseWriter: w}
	outcome = "success"
	result = breaker.Success
	proxy := httputil.ReverseProxy{
		Rewrite:   func(pr *httputil.ProxyRequest) { pr.SetURL(target); pr.SetXForwarded() },
		Transport: b.transport,
		ModifyResponse: func(resp *http.Response) error {
			// A backend can finish concurrently with cancellation. Do not turn a
			// deadline (or a server read deadline canceling the parent) into 200.
			if expired() {
				return context.DeadlineExceeded
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				result = breaker.Ignored
				return errors.New("unsupported stream")
			}
			if resp.StatusCode >= 500 {
				result = breaker.Failure
				outcome = "upstream_5xx"
			} else if resp.StatusCode >= 400 {
				outcome = "upstream_4xx"
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			// An expired socket read deadline can cancel net/http's connection
			// context. Do not reuse that connection for another request.
			if expired() || req.Context().Err() != nil {
				w.Header().Set("Connection", "close")
			}
			// Allow a small error response after the upstream budget expired.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				result = breaker.Ignored
				outcome = "body_too_large"
				fail(w, 413, outcome)
			case expired():
				result = breaker.Failure
				outcome = "timeout"
				fail(w, 504, outcome)
			case errors.Is(req.Context().Err(), context.Canceled):
				result = breaker.Ignored
				outcome = "canceled"
			default:
				if result != breaker.Ignored {
					result = breaker.Failure
				}
				outcome = "upstream_error"
				fail(w, 502, outcome)
			}
		},
	}
	defer func() {
		if v := recover(); v != nil {
			result = breaker.Failure
			outcome = "stream_error"
			if ctx.Err() != nil {
				outcome = "timeout"
				if !expired() && errors.Is(ctx.Err(), context.Canceled) {
					result = breaker.Ignored
					outcome = "canceled"
				}
			}
			if tracked.writeFailed {
				result = breaker.Ignored
			}
			panic(v)
		}
	}()
	proxy.ServeHTTP(tracked, r)
	// Small known-length responses can remain entirely in net/http's buffer.
	// Flush before releasing the pool/gate and completing breaker/metrics so
	// a non-reading client cannot hide a blocked tail outside our deadline.
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		outcome = "stream_error"
		if expired() {
			outcome = "timeout"
		}
		// A downstream write failure is not evidence against a healthy
		// upstream. Keep a backend failure already established by its reply.
		if result == breaker.Success {
			result = breaker.Ignored
		}
	}
	if ctx.Err() != nil && outcome == "success" {
		outcome = "canceled"
		result = breaker.Ignored
		if expired() {
			outcome = "timeout"
			result = breaker.Failure
		}
	}
}

type trackingWriter struct {
	http.ResponseWriter
	writeFailed bool
}

func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *trackingWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		w.writeFailed = true
	}
	return n, err
}

func (g *Gateway) Admin() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/routingz/{group}", func(w http.ResponseWriter, r *http.Request) {
		g.serveRoutingProbe(w, r, r.PathValue("group"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if g.draining.Load() {
			fail(w, 503, "draining")
			return
		}
		for _, b := range g.groups {
			if b.pool.Ready() == 0 {
				fail(w, 503, "no_ready_worker")
				return
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /readyz/{group}", func(w http.ResponseWriter, r *http.Request) {
		for _, b := range g.groups {
			if b.name != r.PathValue("group") {
				continue
			}
			if g.draining.Load() {
				fail(w, 503, "draining")
			} else if b.pool.Ready() == 0 {
				fail(w, 503, "no_ready_worker")
			} else {
				w.WriteHeader(200)
				_, _ = w.Write([]byte("ready\n"))
			}
			return
		}
		fail(w, 404, "unknown_group")
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		groups := map[string]any{}
		for _, b := range g.groups {
			a, q := b.gate.snapshot()
			groups[b.name] = map[string]any{"active": a, "queued": q, "max_concurrent": b.cfg.MaxConcurrent, "queue_size": b.cfg.QueueSize, "worker_memory_max_bytes": b.workerMemoryLimit(), "worker_tasks_max": b.workerResources().TasksMax, "worker_cpu_millicores": b.workerResources().CPUMillicores, "worker_cpu_percent": b.workerResources().CPUPercent, "worker_processes_max": b.workerResources().ProcessesMax, "workers": b.pool.Snapshot(), "circuit_breaker": b.breaker.Snapshot()}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runtime_pid": os.Getpid(), "draining": g.draining.Load(), "groups": groups})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(g.registry, promhttp.HandlerOpts{}))
	return mux
}
