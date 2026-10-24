package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestStartupCleanupFailureCannotAccumulateReplacementBudgets(t *testing.T) {
	for _, cleanupFailed := range []bool{true, false} {
		t.Run(fmt.Sprintf("cleanup_failed=%t", cleanupFailed), func(t *testing.T) {
			c := fixture(t)
			c.Bulkheads["isolated"].Process.Env["ISOLATEX_TEST_START_FAILURE"] = "1"
			s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
			var starts, waits atomic.Int32
			var lostCauses atomic.Bool
			missing := filepath.Join(t.TempDir(), "missing-worker")
			cleanupErr := errors.New("injected startup resource cleanup failure")
			s.start = func(cmd *exec.Cmd, r *config.Resources) (*processControl, error) {
				if !slices.Contains(cmd.Env, "ISOLATEX_TEST_START_FAILURE=1") || starts.Add(1) != 1 {
					return startManagedProcessWithResources(cmd, r)
				}
				// Exercise a real failed native launch. Inject a teardown failure
				// after cleanup so the test does not deliberately leak OS resources.
				cmd.Path = missing
				control, startErr := startManagedProcessWithResources(cmd, r)
				if startErr == nil {
					control.signal(true)
					_ = cmd.Wait()
					_ = control.close()
					return nil, errors.New("missing executable unexpectedly started")
				}
				var teardownErr error
				if cleanupFailed {
					teardownErr = cleanupErr
				}
				err := fmt.Errorf("native launcher: %w", failedStart(startErr, teardownErr))
				if !errors.Is(err, startErr) || (cleanupFailed && !errors.Is(err, cleanupErr)) {
					lostCauses.Store(true)
					return nil, errors.New("launcher lost original failure causes")
				}
				return nil, err
			}
			s.wait = func(ctx context.Context, _ time.Duration) bool { waits.Add(1); return ctx.Err() == nil }
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			waitFor(t, func() bool {
				w := s.Pools["isolated"].Snapshot()[0]
				return s.Pools["normal"].Ready() == 1 && (w.Ready || w.State == "failed")
			})
			w := s.Pools["isolated"].Snapshot()[0]
			if lostCauses.Load() {
				t.Fatal("wrapped startup error lost a failure cause")
			}
			if cleanupFailed {
				if w.State != "failed" || w.Ready || w.PID != 0 || w.Restarts != 0 || starts.Load() != 1 || waits.Load() != 0 {
					t.Fatalf("startup cleanup failure allowed replacement: worker=%+v starts=%d waits=%d", w, starts.Load(), waits.Load())
				}
			} else if !w.Ready || w.Restarts != 1 || starts.Load() != 2 || waits.Load() != 1 {
				t.Fatalf("clean startup failure did not recover: worker=%+v starts=%d waits=%d", w, starts.Load(), waits.Load())
			}
			normal := s.Pools["normal"].Snapshot()[0]
			resp, err := (&http.Client{Timeout: time.Second}).Get(normal.URL.String() + "/work")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || normal.Restarts != 0 {
				t.Fatalf("startup failure affected protected group: status=%d worker=%+v", resp.StatusCode, normal)
			}
		})
	}
}

func TestCleanupFailureCannotAccumulateReplacementBudgets(t *testing.T) {
	c := fixture(t)
	c.Bulkheads["isolated"].Process.Env["ISOLATEX_TEST_CLEANUP_FAILURE"] = "1"
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var starts, waits atomic.Int32
	cleanup := make(chan struct{})
	s.start = func(cmd *exec.Cmd, r *config.Resources) (*processControl, error) {
		control, err := startManagedProcessWithResources(cmd, r)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(cmd.Env, "ISOLATEX_TEST_CLEANUP_FAILURE=1") {
			return control, nil
		}
		starts.Add(1)
		original := control.close
		control.close = func() error {
			err := original()
			close(cleanup)
			if err != nil {
				return err
			}
			return errors.New("injected resource teardown failure")
		}
		return control, nil
	}
	s.wait = func(context.Context, time.Duration) bool { waits.Add(1); return false }
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["isolated"].Ready() == 1 && s.Pools["normal"].Ready() == 1 })
	normal := s.Pools["normal"].Snapshot()[0]
	client := &http.Client{Timeout: time.Second}
	resp, _ := client.Post(s.Pools["isolated"].Snapshot()[0].URL.String()+"/work?crash=1", "", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	select {
	case <-cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("worker cleanup did not run")
	}
	waitFor(t, func() bool {
		state := s.Pools["isolated"].Snapshot()[0].State
		return state == "failed" || state == "stopped"
	})
	w := s.Pools["isolated"].Snapshot()[0]
	if w.State != "failed" || w.Ready || starts.Load() != 1 || waits.Load() != 0 {
		t.Fatalf("replacement was allowed after unconfirmed resource cleanup: worker=%+v starts=%d waits=%d", w, starts.Load(), waits.Load())
	}
	other := s.Pools["normal"].Snapshot()[0]
	if !other.Ready || other.PID != normal.PID || other.Restarts != normal.Restarts {
		t.Fatalf("cleanup failure affected another group: %+v", other)
	}
	resp, err := client.Get(other.URL.String() + "/work")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("protected worker status: %d", resp.StatusCode)
	}
}
