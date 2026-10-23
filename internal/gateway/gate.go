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
	ready    chan struct{}
	granted  bool
	ctx      context.Context
	deadline time.Time
	err      error
}

// A release hands its slot directly to the oldest waiter under the same lock.
// Cancellation that races with that handoff must release the granted slot.
type gate struct {
	mu                      sync.Mutex
	limit, capacity, active int
	queue                   list.List
	closed                  bool
	done                    chan struct{}
	now                     func() time.Time
}

func newGate(limit, capacity int) *gate {
	return &gate{limit: limit, capacity: capacity, done: make(chan struct{}), now: time.Now}
}

func (g *gate) acquire(ctx context.Context, timeout time.Duration) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return errDraining
	}
	if err := contextErrorAt(ctx, g.now()); err != nil {
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
	w := &waiter{ready: make(chan struct{}), ctx: ctx, deadline: g.now().Add(timeout)}
	e := g.queue.PushBack(w)
	g.mu.Unlock()
	timer := time.NewTimer(max(0, w.deadline.Sub(g.now())))
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
	// Assignment under the lock ends queue waiting. If both the timer and
	// handoff became observable before this goroutine resumed, use the
	// committed handoff rather than select's random choice of ready cases.
	if w.granted {
		err = nil
	} else if w.err != nil {
		err = w.err
	}
	if contextErr := contextErrorAt(ctx, g.now()); contextErr != nil {
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
	for !g.closed && g.queue.Len() != 0 {
		e := g.queue.Front()
		g.queue.Remove(e)
		w := e.Value.(*waiter)
		// Do not hand capacity to a dead waiter and depend on its goroutine
		// running to give it back. Continue directly to the oldest live one.
		if err := contextErrorAt(w.ctx, g.now()); err != nil {
			w.err = err
		} else if !g.now().Before(w.deadline) {
			w.err = errQueueTimeout
		}
		if w.err != nil {
			close(w.ready)
			continue
		}
		w.granted = true
		close(w.ready)
		return
	}
	g.active--
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

// Deadline cancellation is delivered by a timer goroutine. Admission must also
// honor the deadline while that goroutine is waiting to run.
func contextErrorAt(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !now.Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
