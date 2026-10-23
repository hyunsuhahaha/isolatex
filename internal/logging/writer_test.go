package logging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type stalledWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	output  bytes.Buffer
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.output.Write(p)
}

func TestStalledDestinationBoundsQueueAndShutdown(t *testing.T) {
	sink := &stalledWriter{entered: make(chan struct{}), release: make(chan struct{})}
	w := NewWriter(sink, 2, 64)
	t.Cleanup(func() { close(sink.release); _ = w.Close(context.Background()) })
	_, _ = io.WriteString(w, "first\n")
	<-sink.entered
	_, _ = io.WriteString(w, "second\n")
	_, _ = io.WriteString(w, "third\n")
	var producers sync.WaitGroup
	for i := 0; i < 100; i++ {
		producers.Add(1)
		go func() { defer producers.Done(); _, _ = io.WriteString(w, "dropped\n") }()
	}
	producers.Wait()
	if got := w.Dropped(); got != 100 {
		t.Fatalf("drops = %d", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := w.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked shutdown = %v", err)
	}
	_, _ = io.WriteString(w, "after close\n")
	if got := w.Dropped(); got != 101 {
		t.Fatalf("post-close drops = %d", got)
	}
}

func TestQueuedRecordOwnsBufferAndOversizeDoesNotConsumeCapacity(t *testing.T) {
	sink := &stalledWriter{entered: make(chan struct{}), release: make(chan struct{})}
	w := NewWriter(sink, 1, 8)
	_, _ = io.WriteString(w, "start\n")
	<-sink.entered
	_, _ = io.WriteString(w, "too large for limit")
	record := []byte("kept\n")
	_, _ = w.Write(record)
	copy(record, "oops\n")
	close(sink.release)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.output.String(); got != "start\nkept\n" {
		t.Fatalf("queued output = %q", got)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("oversized drops = %d", got)
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDestinationErrorsAreCounted(t *testing.T) {
	w := NewWriter(failingWriter{}, 1, 64)
	_, _ = io.WriteString(w, "lost\n")
	_ = w.Close(context.Background())
	if got := w.Dropped(); got != 1 {
		t.Fatalf("failed delivery drops = %d", got)
	}
}
