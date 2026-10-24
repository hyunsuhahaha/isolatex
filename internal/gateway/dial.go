package gateway

import (
	"context"
	"net"
)

type dialRequestContextKey struct{}

type requestDialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d *requestDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	requestCtx, ok := ctx.Value(dialRequestContextKey{}).(context.Context)
	if !ok {
		return d.dial(ctx, network, address)
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	// net/http intentionally detaches dial cancellation for connection reuse.
	// Keep its own cancellation too (e.g. CloseIdleConnections), but do not
	// let this generation of connection work outlive the original request.
	var cancel context.CancelFunc
	if deadline, ok := requestCtx.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	stop := context.AfterFunc(requestCtx, cancel)
	defer func() { stop(); cancel() }()
	return d.dial(ctx, network, address)
}
