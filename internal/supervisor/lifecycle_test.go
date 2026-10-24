package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestStartCannotOverlapWorkerGenerationsBeforeBind(t *testing.T) {
	s := New(fixture(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	// Hold both real workers before process creation, so a port preflight
	// cannot accidentally serve as the supervisor's lifecycle guard.
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var starts atomic.Int32
	s.start = func(cmd *exec.Cmd, r *config.Resources) (*processControl, error) {
		starts.Add(1)
		<-release
		return startManagedProcessWithResources(cmd, r)
	}
	t.Cleanup(func() { cancel(); unblock(); s.Stop() })
	if err := s.Start(ctx); err != nil {
		unblock()
		t.Fatal(err)
	}
	waitFor(t, func() bool { return starts.Load() == 2 })
	err := s.Start(ctx)
	unblock()
	if err == nil {
		t.Fatal("duplicate Start accepted while the original workers had not bound their ports")
	}
	waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 && s.Pools["isolated"].Ready() == 1 })
	if starts.Load() != 2 {
		t.Fatalf("overlapping generations: %d process starts", starts.Load())
	}
	// Concurrent shutdown callers must all observe completed worker cleanup.
	var stops sync.WaitGroup
	for range 8 {
		stops.Add(1)
		go func() { defer stops.Done(); s.Stop() }()
	}
	stops.Wait()
	for name, p := range s.Pools {
		w := p.Snapshot()[0]
		if w.PID != 0 || w.Ready || w.State != "stopped" {
			t.Fatalf("%s shutdown incomplete: %+v", name, w)
		}
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("stopped supervisor restarted its worker generations")
	}
}

func TestStopWaitsForInFlightWorkerCreation(t *testing.T) {
	s := New(fixture(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var starts atomic.Int32
	s.start = func(cmd *exec.Cmd, r *config.Resources) (*processControl, error) {
		starts.Add(1)
		<-release
		return startManagedProcessWithResources(cmd, r)
	}
	t.Cleanup(func() { unblock(); s.Stop() })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return starts.Load() == 2 })
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	// Observe the shutdown admission boundary without a timing assumption.
	waitFor(t, func() bool {
		s.lifecycle.Lock()
		defer s.lifecycle.Unlock()
		return s.stopped
	})
	select {
	case <-done:
		t.Fatal("Stop returned with worker creation still in flight")
	default:
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start accepted during shutdown")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Stop failed to collect workers created during cancellation")
	}
	for name, p := range s.Pools {
		w := p.Snapshot()[0]
		if w.PID != 0 || w.Ready || w.State != "stopped" {
			t.Fatalf("%s worker remained after Stop: %+v", name, w)
		}
		ln, err := net.Listen("tcp", w.URL.Host)
		if err != nil {
			t.Fatalf("%s worker port leaked after Stop: %v", name, err)
		}
		_ = ln.Close()
	}
}

func TestStopBeforeStartIsTerminal(t *testing.T) {
	s := New(fixture(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	var starts atomic.Int32
	s.start = func(*exec.Cmd, *config.Resources) (*processControl, error) {
		starts.Add(1)
		return nil, context.Canceled
	}
	s.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.Stop() }()
	if err := s.Start(ctx); err == nil {
		t.Fatal("Start accepted after Stop returned")
	}
	if starts.Load() != 0 {
		t.Fatal("process creation attempted after terminal Stop")
	}
}

func TestCanceledStartCreatesNothingAndCanRetry(t *testing.T) {
	c := fixture(t)
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer s.Stop()
	if err := s.Start(ctx); err != context.Canceled {
		t.Fatalf("canceled Start = %v, want context.Canceled", err)
	}
	// A rejected attempt did not consume the one successful start.
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 && s.Pools["isolated"].Ready() == 1 })
}

func TestPortPreflightFailureCanRetryBeforeStop(t *testing.T) {
	c := fixture(t)
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.Bulkheads["normal"].Process.PortStart)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer s.Stop()
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("occupied worker port accepted")
	}
	_ = ln.Close()
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 && s.Pools["isolated"].Ready() == 1 })
}

func TestConcurrentStartAndStopCannotLeaveRunningWorkers(t *testing.T) {
	for range 32 {
		// Static probes also use the lifecycle wait group. Keep this stress
		// case process-free; the real worker boundary is exercised above.
		c := config.Config{Bulkheads: map[string]config.Group{"static": {
			Upstreams: []string{"http://127.0.0.1:1"},
			Readiness: config.Readiness{Path: "/readyz", Timeout: time.Millisecond, Interval: time.Millisecond},
		}}}
		s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx, cancel := context.WithCancel(context.Background())
		var callers sync.WaitGroup
		for range 8 {
			callers.Add(2)
			go func() { defer callers.Done(); _ = s.Start(ctx) }()
			go func() { defer callers.Done(); s.Stop() }()
		}
		callers.Wait()
		cancel()
		s.Stop()
		if err := s.Start(context.Background()); err == nil {
			t.Fatal("concurrent Stop did not close the lifecycle")
		}
	}
}
