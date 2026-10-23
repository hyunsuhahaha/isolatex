package config

import (
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
		"path":      strings.Replace(validConfig, "/api\n", "/api/../erp\n", 1),
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
