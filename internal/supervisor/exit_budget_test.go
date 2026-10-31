package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"testing"
	"time"
)

func TestStopChildBoundsUnconfirmedExit(t *testing.T) {
	done := make(chan error, 1)
	signals := make(chan bool, 4)
	control := &processControl{signal: func(force bool) { signals <- force }}
	result := make(chan error, 1)
	go func() { result <- stopChild(control, done) }()
	// Release the old unbounded implementation too: no leaked test goroutine.
	defer func() { done <- nil }()
	select {
	case err := <-result:
		if !errors.Is(err, errResourceCleanup) {
			t.Fatalf("unconfirmed exit lost cleanup classification: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("force termination left exit confirmation unbounded")
	}
	if <-signals || !<-signals {
		t.Fatal("expected graceful then forced termination")
	}
}

func TestStopChildPreservesConfirmedExit(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprint(forced), func(t *testing.T) {
			done := make(chan error, 1)
			exitErr := errors.New("confirmed native exit")
			control := &processControl{signal: func(force bool) {
				if force == forced {
					done <- exitErr
				}
			}}
			if err := stopChild(control, done); !errors.Is(err, exitErr) || errors.Is(err, errResourceCleanup) {
				t.Fatalf("confirmed exit misclassified: %v", err)
			}
		})
	}
}

func TestUnconfirmedWorkerExitFailsSlotWithoutBlockingStop(t *testing.T) {
	for _, startup := range []bool{true, false} {
		t.Run(fmt.Sprintf("startup=%t", startup), func(t *testing.T) {
			c := fixture(t)
			g := c.Bulkheads["isolated"]
			g.Process.Env["ISOLATEX_EXIT_BUDGET_TEST"] = "1"
			if startup {
				g.Process.Env["NOT_READY"] = "1"
				g.Readiness.StartupTimeout = 250 * time.Millisecond
			}
			c.Bulkheads["isolated"] = g
			var logs exitBudgetLogs
			s := New(c, slog.New(slog.NewJSONHandler(&logs, nil)))
			native := make(chan *processControl, 1)
			var starts, waits atomic.Int32
			s.start = func(cmd *exec.Cmd, r *config.Resources) (*processControl, error) {
				control, err := startManagedProcessWithResources(cmd, r)
				if err != nil || !slices.Contains(cmd.Env, "ISOLATEX_EXIT_BUDGET_TEST=1") {
					return control, err
				}
				starts.Add(1)
				native <- control
				// Real worker launch and Wait; fault injection suppresses termination
				// requests until close. It does not pretend to reproduce a kernel hang.
				return &processControl{signal: func(bool) {}, close: func() error {
					control.signal(true)
					return control.close()
				}}, nil
			}
			s.wait = func(ctx context.Context, _ time.Duration) bool { waits.Add(1); return ctx.Err() == nil }
			var control *processControl
			t.Cleanup(func() {
				if control != nil {
					control.signal(true)
				}
				s.Stop()
			})
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case control = <-native:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not launch")
			}

			waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 })
			normal := s.Pools["normal"].Snapshot()[0]
			if startup {
				waitFor(t, func() bool { return s.Pools["isolated"].Snapshot()[0].State == "draining" })
				resp, err := (&http.Client{Timeout: time.Second}).Get(normal.URL.String() + "/work")
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("protected HTTP status: %d", resp.StatusCode)
				}
				waitFor(t, func() bool { return s.Pools["isolated"].Snapshot()[0].State == "failed" })
				other := s.Pools["normal"].Snapshot()[0]
				if !other.Ready || other.PID != normal.PID || other.Restarts != 0 {
					t.Fatalf("protected worker changed: %+v", other)
				}
			} else {
				waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 })
				stopped := make(chan struct{})
				go func() { s.Stop(); close(stopped) }()
				select {
				case <-stopped:
				case <-time.After(7 * time.Second):
					t.Fatal("unconfirmed worker exit blocked Supervisor.Stop")
				}
			}
			s.Stop()
			if text := logs.text(); !strings.Contains(text, `"msg":"worker_shutdown_incomplete"`) || strings.Contains(text, `"msg":"workers_stopped"`) {
				t.Fatalf("shutdown log claimed cleanup completion: %s", text)
			}
			w := s.Pools["isolated"].Snapshot()[0]
			if w.Ready || w.PID != 0 || w.State != "failed" || w.Restarts != 0 || starts.Load() != 1 || waits.Load() != 0 {
				t.Fatalf("unconfirmed exit allowed replacement or claimed clean stop: worker=%+v starts=%d waits=%d", w, starts.Load(), waits.Load())
			}
		})
	}
}

type exitBudgetLogs struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *exitBudgetLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}
func (l *exitBudgetLogs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}
