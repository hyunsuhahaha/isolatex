package gateway

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

var (
	errFull         = errors.New("queue_full")
	errQueueTimeout = errors.New("queue_timeout")
	errDraining     = errors.New("draining")
)

type waiter struct {
	ready   chan struct{}
	granted bool
}

// A release hands its slot directly to the oldest waiter under the same lock.
// Cancellation that races with that handoff must release the granted slot.
type gate struct {
	mu                      sync.Mutex
	limit, capacity, active int
	queue                   list.List
	closed                  bool
	done                    chan struct{}
}

func newGate(limit, capacity int) *gate {
	return &gate{limit: limit, capacity: capacity, done: make(chan struct{})}
}

func (g *gate) acquire(ctx context.Context, timeout time.Duration) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return errDraining
	}
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return err
	}
	if g.active < g.limit && g.queue.Len() == 0 {
		g.active++
		g.mu.Unlock()
		return nil
	}
	if g.queue.Len() >= g.capacity {
		g.mu.Unlock()
		return errFull
	}
	w := &waiter{ready: make(chan struct{})}
	e := g.queue.PushBack(w)
	g.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case <-w.ready:
		// Fall through to check cancellation and draining under the lock.
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errQueueTimeout
	case <-g.done:
		err = errDraining
	}
	g.mu.Lock()
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	if g.closed {
		err = errDraining
	}
	if !w.granted {
		g.queue.Remove(e)
	}
	if err != nil && w.granted {
		g.releaseLocked()
	}
	g.mu.Unlock()
	return err
}

func (g *gate) releaseLocked() {
	if e := g.queue.Front(); e != nil && !g.closed {
		g.queue.Remove(e)
		w := e.Value.(*waiter)
		w.granted = true
		close(w.ready)
	} else {
		g.active--
	}
}

func (g *gate) release() { g.mu.Lock(); defer g.mu.Unlock(); g.releaseLocked() }
func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.closed = true
		close(g.done)
	}
}
func (g *gate) snapshot() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active, g.queue.Len()
}
