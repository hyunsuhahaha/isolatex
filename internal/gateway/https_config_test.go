package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/pool"
)

func TestConfiguredHTTPSUpstreamVerification(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "encrypted upstream") }))
	defer up.Close()
	normal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer normal.Close()
	c, err := config.Decode(strings.NewReader(fmt.Sprintf(`version: 1
bulkheads:
  isolated:
    match: {path_prefix: /api/fault}
    upstreams: [%q]
    max_concurrent: 1
    queue_size: 1
    queue_timeout: 1s
    request_timeout: 5s
  normal:
    match: {path_prefix: /api}
    upstreams: [%q]
    max_concurrent: 1
    queue_size: 1
    queue_timeout: 1s
    request_timeout: 5s
`, up.URL, normal.URL)))
	if err != nil {
		t.Fatal(err)
	}
	pools := map[string]*pool.Pool{}
	for name, b := range c.Bulkheads {
		pools[name] = pool.New(b.Upstreams, true)
	}
	g := New(c, pools, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer g.Close()
	s := httptest.NewServer(g)
	defer s.Close()
	if got := code(t, s.Client(), s.URL+"/api/fault"); got != http.StatusBadGateway {
		t.Fatalf("configured HTTPS accepted an untrusted certificate: %d", got)
	}
	// The fixture CA is trusted only by this transport, without changing the
	// host trust store or providing an insecure verification bypass.
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	g.groups[0].transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	resp, err := s.Client().Get(s.URL + "/api/fault")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || readErr != nil || string(body) != "encrypted upstream" {
		t.Fatalf("configured HTTPS forwarding: status=%d body=%q error=%v", resp.StatusCode, body, readErr)
	}
	if got := code(t, s.Client(), s.URL+"/api/work"); got != http.StatusOK {
		t.Fatalf("mixed HTTP group: %d", got)
	}
}
