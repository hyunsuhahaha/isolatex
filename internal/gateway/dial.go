package gateway

import (
	"context"
	"crypto/tls"
	"net"

	"github.com/hyunsuhahaha/isolatex/internal/requestdial"
)

type requestDialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d *requestDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&requestdial.Dialer{Dial: d.dial}).DialContext(ctx, network, address)
}

func (d *requestDialer) DialTLSContext(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error) {
	return (&requestdial.Dialer{Dial: d.dial}).DialTLSContext(ctx, network, address, config)
}
