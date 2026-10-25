package config

import (
	"strings"
	"testing"
)

func TestHTTPSUpstreamOrigins(t *testing.T) {
	for _, origin := range []string{"https://example.com:443", "https://127.0.0.1:9443/", "https://[::1]:8443"} {
		input := strings.Replace(validConfig, "http://127.0.0.1:9100", origin, 1)
		if _, err := Decode(strings.NewReader(input)); err != nil {
			t.Fatalf("HTTPS origin %q rejected: %v", origin, err)
		}
	}
	for _, origin := range []string{"https://example.com", "https://:443", "https://example.com:0", "https://example.com:65536", "https://user:pass@example.com:443", "https://example.com:443/api", "https://example.com:443?q=x", "https://example.com:443#x", "ftp://example.com:443"} {
		input := strings.Replace(validConfig, "http://127.0.0.1:9100", origin, 1)
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid upstream origin %q accepted", origin)
		}
	}
}
