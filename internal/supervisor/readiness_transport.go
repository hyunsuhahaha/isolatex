package supervisor

import (
	"context"
	"net"
	"net/http"
	"net/url"
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

// Config validates origins with an empty or root path. Replace that path,
// rather than concatenate it, and leave the pool's shared URL unchanged.
func readinessRequest(ctx context.Context, origin *url.URL, path string) *http.Request {
	target := *origin
	target.Path = path
	target.RawPath = ""
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	return req
}
