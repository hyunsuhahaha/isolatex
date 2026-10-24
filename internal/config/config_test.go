package config

import (
	"strconv"
	"strings"
	"testing"
)

const validConfig = `version: 1
bulkheads:
  normal:
    match:
      path_prefix: /api
    upstreams: ["http://127.0.0.1:9100"]
    max_concurrent: 2
    queue_size: 1
    queue_timeout: 100ms
    request_timeout: 2s
`

func TestDecodeStrictValidation(t *testing.T) {
	if _, err := Decode(strings.NewReader(validConfig)); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{
		"unknown":   validConfig + "surprise: true\n",
		"duplicate": validConfig + "version: 1\n",
		"multiple":  validConfig + "---\nversion: 1\n",
		"negative":  strings.Replace(validConfig, "queue_size: 1", "queue_size: -1", 1),
		"duration":  strings.Replace(validConfig, "100ms", "banana", 1),
		"path":      strings.Replace(validConfig, "/api\n", "/api/../fault\n", 1),
		"admin":     validConfig + "admin_listen: '0.0.0.0:9090'\n",
		"mode":      strings.Replace(validConfig, "    upstreams:", "    process: {command: ['./app'], workers: 1, port_start: 9100}\n    upstreams:", 1),
		"origin":    strings.Replace(validConfig, "http://127.0.0.1:9100", "http://user:secret@127.0.0.1:9100", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestAdmissionBudgetReservesEveryBulkhead(t *testing.T) {
	// Each group needs two running and one queued request. A shared cap of
	// three would otherwise allow either group to starve the other completely.
	twoGroups := validConfig + strings.ReplaceAll(strings.ReplaceAll(
		strings.SplitN(validConfig, "bulkheads:\n", 2)[1], "normal:", "isolated:"),
		"path_prefix: /api", "path_prefix: /api/fault")
	for _, tt := range []struct {
		name  string
		input string
		valid bool
	}{
		{"single exact", validConfig + "max_admitted_requests: 3\n", true},
		{"single insufficient", validConfig + "max_admitted_requests: 2\n", false},
		{"two exact", twoGroups + "max_admitted_requests: 6\n", true},
		{"two spare", twoGroups + "max_admitted_requests: 7\n", true},
		{"two overcommitted", twoGroups + "max_admitted_requests: 5\n", false},
		{"default overcommitted", strings.Replace(validConfig, "max_concurrent: 2", "max_concurrent: 256", 1), false},
		{"integer overflow", strings.Replace(validConfig, "max_concurrent: 2", "max_concurrent: "+strconv.Itoa(int(^uint(0)>>1)), 1) + "max_admitted_requests: " + strconv.Itoa(int(^uint(0)>>1)) + "\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.input))
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && (err == nil || !strings.Contains(err.Error(), "max_admitted_requests")) {
				t.Fatalf("expected admission budget error, got %v", err)
			}
		})
	}
}

func TestUpstreamsRequireUsableHostAndPort(t *testing.T) {
	for _, origin := range []string{
		"http://:9100", "http://127.0.0.1:", "http://127.0.0.1:0",
		"http://127.0.0.1:65536", "http://[::1]:65536", "http://localhost:999999999999999999999",
	} {
		t.Run(origin, func(t *testing.T) {
			input := strings.Replace(validConfig, "http://127.0.0.1:9100", origin, 1)
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("unusable upstream accepted")
			}
		})
	}
	for _, origin := range []string{"http://127.0.0.1:1", "http://localhost:65535", "http://[::1]:9100", "http://service.internal:8080/"} {
		t.Run(origin, func(t *testing.T) {
			input := strings.Replace(validConfig, "http://127.0.0.1:9100", origin, 1)
			if _, err := Decode(strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
