package ingress

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestCloseInterruptsAcceptAtCapacityWithoutClosingAcceptedConnection(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := New(raw, 1)
	defer l.Close()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if l.Active() != 1 {
		t.Fatal("accepted connection not accounted")
	}
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	_ = l.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close stranded Accept waiting for capacity")
	}
	// Closing the listener must leave an accepted request available to drain.
	_ = accepted.SetDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err := accepted.Read(data[:]); err != nil || data[0] != 'x' {
		t.Fatalf("accepted connection closed before drain: %v", err)
	}
	_ = accepted.Close()
	_ = accepted.Close()
	if l.Active() != 0 {
		t.Fatal("double Close leaked or duplicated a connection permit")
	}
}
