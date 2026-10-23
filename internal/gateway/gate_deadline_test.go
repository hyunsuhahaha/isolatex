package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestReleaseSkipsExpiredWaiterWithoutSchedulerHelp(t *testing.T) {
	g := newGate(1, 2)
	t.Cleanup(g.close)
	var elapsed atomic.Int64
	base := time.Now()
	g.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	if err := g.acquire(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- g.acquire(context.Background(), time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 1 })
	go func() { second <- g.acquire(context.Background(), 3*time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 2 })

	// Advancing the gate clock leaves both real timers asleep. Correctness
	// must not depend on the expired waiter's goroutine being scheduled.
	g.mu.Lock()
	expired := g.queue.Front().Value.(*waiter)
	next := g.queue.Back().Value.(*waiter)
	elapsed.Store(int64(2 * time.Hour))
	g.releaseLocked()
	badHandoff := expired.granted || !next.granted || g.active != 1 || g.queue.Len() != 0
	g.mu.Unlock()
	if badHandoff {
		t.Fatal("expired waiter received the slot or blocked the next live waiter")
	}
	if err := <-first; err != errQueueTimeout {
		t.Fatalf("expired waiter = %v", err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	g.release()
	if a, q := g.snapshot(); a != 0 || q != 0 {
		t.Fatalf("leak: %d/%d", a, q)
	}
}

func TestReleaseSkipsCanceledWaiterWithoutSchedulerHelp(t *testing.T) {
	g := newGate(1, 2)
	t.Cleanup(g.close)
	_ = g.acquire(context.Background(), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- g.acquire(ctx, time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 1 })
	go func() { second <- g.acquire(context.Background(), time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 2 })
	g.mu.Lock()
	canceled := g.queue.Front().Value.(*waiter)
	next := g.queue.Back().Value.(*waiter)
	cancel()
	g.releaseLocked()
	badHandoff := canceled.granted || !next.granted || g.queue.Len() != 0
	g.mu.Unlock()
	if badHandoff {
		t.Fatal("canceled waiter received the slot or blocked the next live waiter")
	}
	if err := <-first; err != context.Canceled {
		t.Fatalf("canceled waiter = %v", err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	g.release()
}

func TestQueueDeadlineEndsAtHandoff(t *testing.T) {
	g := newGate(1, 1)
	t.Cleanup(g.close)
	var elapsed atomic.Int64
	base := time.Now()
	g.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	_ = g.acquire(context.Background(), time.Hour)
	done := make(chan error, 1)
	go func() { done <- g.acquire(context.Background(), time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 1 })
	g.mu.Lock()
	g.releaseLocked()                   // Slot assigned before the queue deadline.
	elapsed.Store(int64(2 * time.Hour)) // Acquirer cannot resume until unlock.
	g.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("queue deadline continued after assignment: %v", err)
	}
	g.release()
}
