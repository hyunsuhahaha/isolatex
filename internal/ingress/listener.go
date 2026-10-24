// Package ingress bounds accepted connections before HTTP headers or routes exist.
package ingress

import (
	"net"
	"sync"
	"sync/atomic"
)

// Listener applies backpressure before Accept when all permits are occupied.
// Connections in the kernel's listen backlog are outside this process budget.
type Listener struct {
	net.Listener
	permits  chan struct{}
	done     chan struct{}
	once     sync.Once
	closeErr error
	active   atomic.Int64
}

func New(listener net.Listener, limit int) *Listener {
	if limit < 1 {
		panic("ingress connection limit must be positive")
	}
	return &Listener{Listener: listener, permits: make(chan struct{}, limit), done: make(chan struct{})}
}

func (l *Listener) Active() int64 { return l.active.Load() }

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case l.permits <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.permits
		return nil, err
	}
	l.active.Add(1)
	return &connection{Conn: conn, release: func() { l.active.Add(-1); <-l.permits }}, nil
}

// Close interrupts both an OS Accept and an Accept waiting for a permit.
// Accepted connections remain owned by the HTTP server for graceful draining.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done); l.closeErr = l.Listener.Close() })
	return l.closeErr
}

type connection struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *connection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
