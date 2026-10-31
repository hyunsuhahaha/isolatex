package config

import (
	"strings"
	"testing"
)

func TestIntegerSettingsRejectCoercion(t *testing.T) {
	for name, input := range map[string]string{
		"version":     strings.Replace(validConfig, "version: 1", "version: 1.9", 1),
		"admitted":    validConfig + "max_admitted_requests: 256.9\n",
		"body":        validConfig + "max_body_bytes: 1024.9\n",
		"concurrency": strings.Replace(validConfig, "max_concurrent: 2", "max_concurrent: 2.9", 1),
		"queue":       strings.Replace(validConfig, "queue_size: 1", "queue_size: -0.9", 1),
		"health":      validConfig + "    readiness: {failure_threshold: 0.9}\n",
		"breaker":     validConfig + "    circuit_breaker: {window: 1s, min_requests: 1.9, failure_ratio: 0.5, open_timeout: 1s}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("silently coerced fractional setting")
			}
		})
	}
}

func TestIntegerSettingsScalarTypesAndAliases(t *testing.T) {
	managed := strings.Replace(validConfig, `upstreams: ["http://127.0.0.1:9100"]`, `process: {command: [worker], workers: 1, port_start: 9100}
    readiness: {path: /readyz, interval: 1s, timeout: 100ms, startup_timeout: 2s}`, 1)
	for _, field := range []string{"workers: 1", "port_start: 9100"} {
		for _, value := range []string{"1.9", "1e1", `"1"`, "true", "null", "9223372036854775808"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				input := strings.Replace(managed, field, strings.Split(field, ":")[0]+": "+value, 1)
				if _, err := Decode(strings.NewReader(input)); err == nil {
					t.Fatal("invalid process integer accepted")
				}
			})
		}
	}
	for _, value := range []string{"1.9", "1e1", `"1"`, "true", "null", "9223372036854775808"} {
		input := strings.Replace(validConfig, "queue_size: 1", "queue_size: "+value, 1)
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid queue integer accepted: %s", value)
		}
	}
	for _, input := range []string{
		strings.Replace(validConfig, "max_concurrent: 2", "max_concurrent: &slots 2.9", 1) + "max_admitted_requests: *slots\n",
		strings.Replace(validConfig, "queue_size: 1", "<<: {queue_size: 1.9}", 1),
		strings.Replace(validConfig, "queue_size: 1", "<<: [{queue_size: 1.9}]", 1),
		validConfig + "    readiness: {success_threshold: -0.9}\n",
	} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatal("aliased or merged fractional integer accepted")
		}
	}
	for _, input := range []string{
		validConfig,
		managed,
		strings.Replace(validConfig, "max_concurrent: 2", "max_concurrent: &slots 2", 1) + "max_admitted_requests: 3\n",
		strings.Replace(validConfig, "queue_size: 1", "queue_size: &size 1", 1) + "max_body_bytes: *size\n",
		strings.Replace(validConfig, "queue_size: 1", "<<: {queue_size: 1}", 1),
		validConfig + "    circuit_breaker: {window: 1s, min_requests: 1, failure_ratio: 0.5, open_timeout: 1s}\n",
	} {
		if _, err := Decode(strings.NewReader(input)); err != nil {
			t.Fatalf("valid integer/duration/ratio rejected: %v", err)
		}
	}
}
