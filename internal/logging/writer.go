// Package logging keeps a stalled log destination off runtime control paths.
package logging

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
)

// Writer delivers bounded records asynchronously. Write never waits for the
// destination. A full queue, oversized record, failed destination write, or write
// after Close increments Dropped. Delivery is best effort, including shutdown.
type Writer struct {
	mu             sync.Mutex
	closed         bool
	queue          chan []byte
	done           chan struct{}
	maxRecordBytes int
	dropped        atomic.Uint64
}

func NewWriter(destination io.Writer, capacity, maxRecordBytes int) *Writer {
	if capacity < 1 || maxRecordBytes < 1 {
		panic("logging: capacity and record size must be positive")
	}
	w := &Writer{queue: make(chan []byte, capacity), done: make(chan struct{}), maxRecordBytes: maxRecordBytes}
	go func() {
		defer close(w.done)
		for record := range w.queue {
			if n, err := destination.Write(record); err != nil || n != len(record) {
				w.dropped.Add(1)
			}
		}
	}()
	return w
}

func (w *Writer) Write(record []byte) (int, error) {
	n := len(record)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || n > w.maxRecordBytes || len(w.queue) == cap(w.queue) {
		w.dropped.Add(1)
		return n, nil
	}
	// io.Writer does not own its caller's buffer after Write returns.
	w.queue <- append([]byte(nil), record...)
	return n, nil
}

func (w *Writer) Dropped() uint64 { return w.dropped.Load() }

// Close stops new records and drains until ctx expires. A blocked destination
// may leave one delivery goroutine until it resumes; it cannot block shutdown
// beyond ctx. Concurrent or repeated Close calls are safe.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
