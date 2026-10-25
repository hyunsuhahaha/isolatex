package requestdial

import (
	"context"
	"crypto/tls"
	"net"
)

type requestContextKey struct{}

// Bind preserves the originating request through Transport's detached dial context.
// Call this at RoundTrip entry, after http.Client has applied its timeout.
func Bind(ctx, requestCtx context.Context) context.Context {
	return context.WithValue(ctx, requestContextKey{}, requestCtx)
}

type Dialer struct {
	Dial func(context.Context, string, string) (net.Conn, error)
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := requestDialContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.Dial(ctx, network, address)
}

// DialTLSContext owns both TCP connection establishment and TLS negotiation.
// Letting Transport perform TLS after DialContext returns would put the
// handshake back on its detached context and lose the original request budget.
func (d *Dialer) DialTLSContext(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error) {
	ctx, cancel := requestDialContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	if config.ServerName == "" {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		config.ServerName = host
	}
	conn, err := d.Dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func requestDialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	requestCtx, ok := ctx.Value(requestContextKey{}).(context.Context)
	if !ok {
		return ctx, func() {}
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
	if requestCtx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}
