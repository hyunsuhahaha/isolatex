package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"isolatex/internal/config"
)

func fixture() (*Breaker, *time.Time) {
	now := time.Unix(1000, 0)
	b := New(&config.CircuitBreaker{Window: 10 * time.Second, MinRequests: 4, FailureRatio: .5, OpenTimeout: time.Second})
	b.now = func() time.Time { return now }
	return b, &now
}

func complete(t *testing.T, b *Breaker, result Result) {
	t.Helper()
	finish, ok := b.Allow()
	if !ok {
		t.Fatal("unexpectedly blocked")
	}
	finish(result)
}

func TestMinimumVolumeAndFailureRatio(t *testing.T) {
	b, _ := fixture()
	complete(t, b, Failure)
	complete(t, b, Success)
	complete(t, b, Success)
	if b.Blocked() {
		t.Fatal("opened before minimum volume")
	}
	complete(t, b, Success)
	complete(t, b, Failure)
	if b.Blocked() {
		t.Fatal("opened below failure threshold")
	}
	complete(t, b, Failure)
	if !b.Blocked() || b.Snapshot().State != "open" {
		t.Fatal("did not open at threshold")
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("Open admitted a request")
	}
}

func TestStaleResultsAndHalfOpenRecovery(t *testing.T) {
	b, now := fixture()
	b.cfg.MinRequests = 1
	stale, _ := b.Allow()
	complete(t, b, Failure)
	*now = now.Add(time.Second)
	probe, ok := b.Allow()
	if !ok {
		t.Fatal("did not allow recovery probe")
	}
	stale(Success)
	if s := b.Snapshot(); s.State != "half_open" || !s.ProbeInFlight {
		t.Fatalf("stale result affected probe: %+v", s)
	}
	probe(Ignored)
	if b.Blocked() {
		t.Fatal("canceled probe permanently blocked recovery")
	}
	probe, ok = b.Allow()
	if !ok {
		t.Fatal("replacement probe refused")
	}
	probe(Failure)
	if !b.Blocked() {
		t.Fatal("failed probe did not reopen")
	}
	*now = now.Add(time.Second)
	probe, ok = b.Allow()
	if !ok {
		t.Fatal("next probe refused")
	}
	probe(Success)
	probe(Failure)
	if s := b.Snapshot(); s.State != "closed" || s.Requests != 0 {
		t.Fatalf("successful probe did not reset: %+v", s)
	}
}

func TestOnlyOneHalfOpenProbeUnderConcurrency(t *testing.T) {
	b, now := fixture()
	b.cfg.MinRequests = 1
	complete(t, b, Failure)
	*now = now.Add(time.Second)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	finishes := make(chan func(Result), 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if finish, ok := b.Allow(); ok {
				admitted.Add(1)
				finishes <- finish
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("probes=%d", admitted.Load())
	}
	(<-finishes)(Success)
	if b.Blocked() {
		t.Fatal("successful recovery stayed blocked")
	}
}

func TestWindowExpiryAndIgnoredAdmissions(t *testing.T) {
	b, now := fixture()
	complete(t, b, Failure)
	complete(t, b, Failure)
	complete(t, b, Ignored)
	if b.Snapshot().Requests != 2 {
		t.Fatal("counted a local rejection or canceled request")
	}
	*now = now.Add(10 * time.Second)
	if b.Snapshot().Requests != 0 {
		t.Fatal("old failures did not expire")
	}
	complete(t, b, Success)
	if b.Blocked() || b.Snapshot().Requests != 1 {
		t.Fatal("expired failures affected new window")
	}
}
