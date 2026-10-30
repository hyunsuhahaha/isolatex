package gateway

import (
	"errors"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
)

var errInformationalLimit = errors.New("upstream informational response budget exceeded")

const maxInformationalResponses = 8
const maxInformationalHeaderBytes = 64 << 10

// ReverseProxy installs a fresh ClientTrace before invoking this transport.
// Transport resets its header limit after each delivered 1xx, so the proxy
// must bound the number and aggregate decoded header size itself.
type informationalTransport struct{ base http.RoundTripper }

func (t informationalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(req.Context())
	if trace == nil {
		return nil, errors.New("proxy informational trace missing")
	}
	previous := trace.Got1xxResponse
	var mu sync.Mutex
	count, remaining := 0, maxInformationalHeaderBytes
	// Replace the proxy-owned hook before starting I/O. WithClientTrace's
	// composition discards the new hook's error when an older hook exists.
	trace.Got1xxResponse = func(code int, header textproto.MIMEHeader) error {
		mu.Lock()
		defer mu.Unlock()
		count++
		if count > maxInformationalResponses {
			return errInformationalLimit
		}
		remaining -= 32 // bounded allowance for the status line and blank line
		for name, values := range header {
			for _, value := range values {
				remaining -= len(name) + len(value) + 4 // colon, space, CRLF
				if remaining < 0 {
					return errInformationalLimit
				}
			}
		}
		if remaining < 0 {
			return errInformationalLimit
		}
		if previous != nil {
			return previous(code, header)
		}
		return nil
	}
	return t.base.RoundTrip(req)
}
