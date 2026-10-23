package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackoffResetsAfterRestartHistoryExpires(t *testing.T) {
	c := fixture(t)
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	started := time.Now()
	var elapsed atomic.Int64
	s.now = func() time.Time { return started.Add(time.Duration(elapsed.Load())) }
	waits := make(chan time.Duration, 4)
	s.wait = func(ctx context.Context, d time.Duration) bool {
		select {
		case waits <- d:
			return ctx.Err() == nil
		case <-ctx.Done():
			return false
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool { return s.Pools["normal"].Ready() == 1 && s.Pools["isolated"].Ready() == 1 })
	healthyPID := s.Pools["normal"].Snapshot()[0].PID
	origin := s.Pools["isolated"].Snapshot()[0].URL.String()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	crash := func() time.Duration {
		t.Helper()
		oldPID := s.Pools["isolated"].Snapshot()[0].PID
		// POST is deliberate: a replayable GET may be retried after EOF by
		// the client's transport while the replacement is binding its port.
		req, _ := http.NewRequest(http.MethodPost, origin+"/?crash=1", nil)
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("crash request did not disconnect")
		}
		var delay time.Duration
		select {
		case delay = <-waits:
		case <-time.After(5 * time.Second):
			t.Fatal("no restart delay observed")
		}
		waitFor(t, func() bool { w := s.Pools["isolated"].Snapshot()[0]; return w.Ready && w.PID != oldPID })
		if w := s.Pools["normal"].Snapshot()[0]; !w.Ready || w.PID != healthyPID || w.Restarts != 0 {
			t.Fatalf("restart affected healthy group: %+v", w)
		}
		return delay
	}
	if d := crash(); d < 500*time.Millisecond || d >= 750*time.Millisecond {
		t.Fatalf("initial delay = %v", d)
	}
	if d := crash(); d < time.Second || d >= 1500*time.Millisecond {
		t.Fatalf("recent crash did not retain exponential backoff: %v", d)
	}
	elapsed.Store(int64(2 * time.Minute))
	if d := crash(); d < 500*time.Millisecond || d >= 750*time.Millisecond {
		t.Fatalf("old incidents still increased backoff after history expiry: %v", d)
	}
}
