package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/pool"
)

type Supervisor struct {
	cfg   config.Config
	Pools map[string]*pool.Pool
	log   *slog.Logger
	// Serialize admission to the entire lifetime, including port preflight and
	// every WaitGroup.Add. Stop permanently closes admission before waiting.
	lifecycle sync.Mutex
	started   bool
	stopped   bool
	wg        sync.WaitGroup
	cancel    context.CancelFunc
	logs      chan logLine
	dropped   atomic.Uint64
	now       func() time.Time
	wait      func(context.Context, time.Duration) bool
	start     func(*exec.Cmd, *config.Resources) (*processControl, error)
}

type logLine struct {
	group  string
	worker int
	text   string
}

// Readiness runs inside the shared supervisor. Do not let a faulty worker's
// headers use net/http's much larger default response-header allowance.
const maxReadinessHeaderBytes = 64 << 10

func New(c config.Config, logger *slog.Logger) *Supervisor {
	s := &Supervisor{
		cfg: c, Pools: map[string]*pool.Pool{}, log: logger,
		logs: make(chan logLine, 256), now: time.Now, wait: pause,
		start: startManagedProcessWithResources,
	}
	for name, g := range c.Bulkheads {
		origins := g.Upstreams
		if g.Process != nil {
			for i := 0; i < g.Process.Workers; i++ {
				origins = append(origins, fmt.Sprintf("http://127.0.0.1:%d", g.Process.PortStart+i))
			}
		}
		s.Pools[name] = pool.New(origins, g.Process == nil && g.Readiness.Path == "")
	}
	return s
}

// Start admits at most one successful lifetime. A canceled context or failed
// port preflight may be retried until Stop is called. Probe all managed ports
// before spawning anything; unrelated processes can still race a worker's bind.
func (s *Supervisor) Start(parent context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.stopped {
		return fmt.Errorf("supervisor is stopped; create a new supervisor for a new lifetime")
	}
	if s.started {
		return fmt.Errorf("supervisor already started")
	}
	if err := parent.Err(); err != nil {
		return err
	}
	for name, g := range s.cfg.Bulkheads {
		if p := g.Process; p != nil {
			for i := 0; i < p.Workers; i++ {
				addr := fmt.Sprintf("127.0.0.1:%d", p.PortStart+i)
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					return fmt.Errorf("%s port preflight: %w", name, err)
				}
				_ = ln.Close()
			}
		}
	}
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.started = true
	// Bounded, best-effort log delivery; a blocked sink never blocks a worker.
	go func() {
		for {
			select {
			case line := <-s.logs:
				s.log.Info("worker_output", "group", line.group, "worker", line.worker, "message", line.text)
			case <-ctx.Done():
				return
			}
		}
	}()
	for name, g := range s.cfg.Bulkheads {
		if g.Process != nil {
			for i := 0; i < g.Process.Workers; i++ {
				s.wg.Add(1)
				go func() { defer s.wg.Done(); s.run(ctx, name, g, i) }()
			}
		} else if g.Readiness.Path != "" {
			for i := range g.Upstreams {
				s.wg.Add(1)
				go func() { defer s.wg.Done(); s.monitorStatic(ctx, name, i, g.Readiness) }()
			}
		}
	}
	return nil
}

// Stop closes the lifetime permanently, even when called before Start. Every
// concurrent caller waits for managed workers and readiness monitors to finish.
func (s *Supervisor) Stop() {
	s.lifecycle.Lock()
	first := !s.stopped
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	s.lifecycle.Unlock()
	s.wg.Wait()
	if first {
		s.log.Info("workers_stopped", "dropped_log_chunks", s.dropped.Load())
	}
}

func (s *Supervisor) run(ctx context.Context, name string, g config.Group, index int) {
	p := s.Pools[name]
	history := []time.Time{}
	delay := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			p.Set(index, false, 0, "stopped")
			return
		}
		cmd := exec.Command(g.Process.Command[0], g.Process.Command[1:]...)
		cmd.Env = workerEnv(g.Process.Env, g.Process.PortStart+index)
		writer := &logWriter{s: s, group: name, index: index}
		cmd.Stdout = writer
		cmd.Stderr = writer
		cmd.WaitDelay = time.Second
		p.Set(index, false, 0, "starting")
		control, err := s.start(cmd, g.Process.Resources)
		if err == nil {
			pid := cmd.Process.Pid
			p.Set(index, false, pid, "starting")
			s.log.Info("worker_started", "group", name, "worker", index, "pid", pid)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			err = s.watch(ctx, cmd, control, done, p, index, g.Readiness)
			// Also clean remaining descendants after a direct child's exit.
			control.signal(true)
			if closeErr := control.close(); closeErr != nil {
				s.log.Error("worker_resource_cleanup_failed", "group", name, "worker", index, "error", closeErr)
				// Never stack another generation's budget on resources whose
				// teardown could not be confirmed. Other slots remain independent.
				p.Set(index, false, 0, "failed")
				return
			}
		} else if errors.Is(err, errResourceCleanup) {
			// Startup can allocate a Job/cgroup before process creation fails.
			// An unconfirmed teardown is terminal even without a running child.
			s.log.Error("worker_resource_cleanup_failed", "group", name, "worker", index, "error", err)
			p.Set(index, false, 0, "failed")
			return
		}
		p.Set(index, false, 0, "backoff")
		if ctx.Err() != nil {
			p.Set(index, false, 0, "stopped")
			return
		}
		s.log.Warn("worker_exited", "group", name, "worker", index, "error", err)
		now := s.now()
		recent := history[:0]
		for _, t := range history {
			if now.Sub(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		history = recent
		// Once every prior restart is outside the budget window, this is a
		// fresh incident rather than another step in the old crash loop.
		if len(history) == 0 {
			delay = 500 * time.Millisecond
		}
		if len(history) >= 5 {
			p.Set(index, false, 0, "failed")
			s.log.Error("restart_budget_exhausted", "group", name, "worker", index)
			return
		}
		if !s.wait(ctx, min(delay+time.Duration(rand.Int64N(int64(delay/2))), 30*time.Second)) {
			p.Set(index, false, 0, "stopped")
			return
		}
		history = append(history, s.now())
		p.Restart(index)
		delay = min(delay*2, 30*time.Second)
	}
}

func (s *Supervisor) watch(ctx context.Context, cmd *exec.Cmd, control *processControl, done <-chan error, p *pool.Pool, index int, r config.Readiness) error {
	tr := newReadinessTransport()
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: r.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	startupCtx, cancelStartup := context.WithTimeout(ctx, r.StartupTimeout)
	defer cancelStartup()
	startupDone := startupCtx.Done()
	tick := time.NewTicker(min(r.Interval, 100*time.Millisecond))
	defer tick.Stop()
	ready := false
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return stopChild(control, done)
		case <-startupDone:
			p.Set(index, false, cmd.Process.Pid, "draining")
			_ = stopChild(control, done)
			return fmt.Errorf("readiness startup timeout")
		case <-tick.C:
			origin := p.Snapshot()[index].URL.String()
			probeCtx := ctx
			if !ready {
				probeCtx = startupCtx
			}
			req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, origin+r.Path, nil)
			resp, err := client.Do(req)
			ok := err == nil && resp.StatusCode == http.StatusOK
			if resp != nil {
				_ = resp.Body.Close()
			}
			if ok {
				select {
				case err := <-done:
					return err
				default:
				}
				if ctx.Err() != nil {
					return stopChild(control, done)
				}
			}
			before, after := p.Health(index, ok, r.Failures(), r.Successes())
			if before != after {
				s.log.Info("worker_health_changed", "pid", cmd.Process.Pid, "ready", after)
			}
			if !ready && after {
				ready = true
				startupDone = nil
				cancelStartup()
				tick.Reset(r.Interval)
				s.log.Info("worker_ready", "pid", cmd.Process.Pid)
			}
		}
	}
}

func (s *Supervisor) monitorStatic(ctx context.Context, name string, index int, r config.Readiness) {
	p := s.Pools[name]
	p.Set(index, false, 0, "checking")
	tr := newReadinessTransport()
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: r.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer p.Set(index, false, 0, "stopped")
	for ctx.Err() == nil {
		origin := p.Snapshot()[index].URL.String()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin+r.Path, nil)
		resp, err := client.Do(req)
		ok := err == nil && resp.StatusCode == 200
		if resp != nil {
			resp.Body.Close()
		}
		before, after := p.Health(index, ok, r.Failures(), r.Successes())
		if before != after {
			s.log.Info("upstream_health_changed", "group", name, "worker", index, "ready", after)
		}
		if !pause(ctx, r.Interval) {
			return
		}
	}
}

func stopChild(control *processControl, done <-chan error) error {
	control.signal(false)
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		control.signal(true)
		return <-done
	}
}

func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func workerEnv(extra map[string]string, port int) []string {
	values := map[string]string{}
	key := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k != "" {
			values[key(k)] = v
		}
	}
	for k, v := range extra {
		values[key(k)] = v
	}
	values["PORT"] = strconv.Itoa(port)
	values["HOST"] = "127.0.0.1"
	result := make([]string, 0, len(values))
	for k, v := range values {
		result = append(result, k+"="+v)
	}
	return result
}

type logWriter struct {
	s     *Supervisor
	group string
	index int
}

func (w *logWriter) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		size := min(len(data), 4096)
		text := strings.TrimSpace(string(data[:size]))
		data = data[size:]
		if text == "" {
			continue
		}
		select {
		case w.s.logs <- logLine{w.group, w.index, text}:
		default:
			w.s.dropped.Add(1)
		}
	}
	return n, nil
}
