package supervisor

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/requestdial"
)

// Bind at RoundTrip entry so http.Client.Timeout, startup deadlines, and
// supervisor cancellation all cover TCP and TLS work detached by Transport.
type readinessTransport struct{ *http.Transport }

func (t readinessTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.Transport.RoundTrip(r.WithContext(requestdial.Bind(r.Context(), r.Context())))
}

func newReadinessTransport() readinessTransport {
	d := &requestdial.Dialer{Dial: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
	t := &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: maxReadinessHeaderBytes, DialContext: d.DialContext}
	t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return d.DialTLSContext(ctx, network, address, t.TLSClientConfig)
	}
	return readinessTransport{t}
}
