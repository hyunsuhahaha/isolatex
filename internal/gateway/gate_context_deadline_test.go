package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireRejectsElapsedContextDeadlineBeforeNotification(t *testing.T) {
	base := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), base.Add(time.Hour))
	defer cancel()
	g := newGate(1, 1)
	g.now = func() time.Time { return base.Add(2 * time.Hour) }
	if err := ctx.Err(); err != nil {
		t.Fatalf("context timer unexpectedly fired: %v", err)
	}
	if err := g.acquire(ctx, 3*time.Hour); err != context.DeadlineExceeded {
		t.Fatalf("acquire = %v, want deadline exceeded", err)
	}
	if a, q := g.snapshot(); a != 0 || q != 0 {
		t.Fatalf("expired request consumed capacity: %d/%d", a, q)
	}
}

func TestReleaseSkipsElapsedContextDeadlineBeforeNotification(t *testing.T) {
	base := time.Now()
	var elapsed atomic.Int64
	g := newGate(1, 2)
	t.Cleanup(g.close)
	g.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	ctx, cancel := context.WithDeadline(context.Background(), base.Add(time.Hour))
	defer cancel()
	_ = g.acquire(context.Background(), 3*time.Hour)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- g.acquire(ctx, 3*time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 1 })
	go func() { second <- g.acquire(context.Background(), 3*time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 2 })
	g.mu.Lock()
	expired, next := g.queue.Front().Value.(*waiter), g.queue.Back().Value.(*waiter)
	elapsed.Store(int64(2 * time.Hour))
	g.releaseLocked()
	badHandoff := expired.granted || !next.granted || g.active != 1 || g.queue.Len() != 0
	g.mu.Unlock()
	if badHandoff {
		t.Fatal("elapsed request deadline blocked a live waiter")
	}
	if err := <-first; err != context.DeadlineExceeded {
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

func TestContextDeadlineContinuesAfterHandoff(t *testing.T) {
	base := time.Now()
	var elapsed atomic.Int64
	g := newGate(1, 1)
	t.Cleanup(g.close)
	g.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	ctx, cancel := context.WithDeadline(context.Background(), base.Add(time.Hour))
	defer cancel()
	_ = g.acquire(context.Background(), 3*time.Hour)
	done := make(chan error, 1)
	go func() { done <- g.acquire(ctx, 3*time.Hour) }()
	eventually(t, func() bool { _, q := g.snapshot(); return q == 1 })
	g.mu.Lock()
	g.releaseLocked()
	elapsed.Store(int64(2 * time.Hour))
	g.mu.Unlock()
	if err := <-done; err != context.DeadlineExceeded {
		t.Fatalf("request deadline stopped at assignment: %v", err)
	}
	if a, q := g.snapshot(); a != 0 || q != 0 {
		t.Fatalf("handoff leaked capacity: %d/%d", a, q)
	}
}
