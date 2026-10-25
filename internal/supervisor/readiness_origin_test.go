package supervisor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestStaticReadinessRootSlashOrigin(t *testing.T) {
	var wrongPath atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			wrongPath.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	c, err := config.Decode(strings.NewReader(fmt.Sprintf(`version: 1
bulkheads:
  normal:
    match: {path_prefix: /normal}
    upstreams: [%q]
    max_concurrent: 1
    queue_size: 0
    queue_timeout: 1s
    request_timeout: 2s
    readiness: {path: /readyz, interval: 10ms, timeout: 1s}
  slash:
    match: {path_prefix: /slash}
    upstreams: [%q]
    max_concurrent: 1
    queue_size: 0
    queue_timeout: 1s
    request_timeout: 2s
    readiness: {path: /readyz, interval: 10ms, timeout: 1s}
`, up.URL, up.URL+"/")))
	if err != nil {
		t.Fatal(err)
	}
	s := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, func() bool {
		return s.Pools["normal"].Snapshot()[0].HealthChecks >= 5 && s.Pools["slash"].Snapshot()[0].HealthChecks >= 5
	})
	for _, name := range []string{"normal", "slash"} {
		worker := s.Pools[name].Snapshot()[0]
		if !worker.Ready || worker.HealthFailures != 0 {
			t.Fatalf("healthy origin classified incorrectly: group=%s wrong_paths=%d worker=%+v", name, wrongPath.Load(), worker)
		}
	}
	if wrongPath.Load() != 0 {
		t.Fatalf("unexpected readiness paths: %d", wrongPath.Load())
	}
}

func TestReadinessHTTPSRootSlashOrigin(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "/readyz/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "ready")
	}))
	defer up.Close()
	tr := newReadinessTransport()
	defer tr.CloseIdleConnections()
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: roots}
	client := &http.Client{Transport: tr, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, suffix := range []string{"", "/"} {
		original := up.URL + suffix
		origin, err := url.Parse(original)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(readinessRequest(context.Background(), origin, "/readyz/"))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || readErr != nil || string(body) != "ready" || origin.String() != original {
			t.Fatalf("HTTPS readiness: origin=%s status=%d body=%q error=%v", origin, resp.StatusCode, body, readErr)
		}
	}
}
