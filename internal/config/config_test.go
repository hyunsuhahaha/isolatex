package config

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

func TestMemoryResourceValidation(t *testing.T) {
	for _, r := range []*Resources{{}, {MemoryMaxBytes: -1}, {MemoryMaxBytes: 64 << 20, CgroupParent: "relative/parent"}} {
		if err := r.Validate(); err == nil {
			t.Fatalf("invalid resource budget accepted: %+v", r)
		}
	}
	r := &Resources{MemoryMaxBytes: 64 << 20}
	if runtime.GOOS == "linux" {
		r.CgroupParent = "/sys/fs/cgroup/isolatex"
	}
	err := r.Validate()
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil {
		t.Fatal("unsupported memory limit silently accepted")
	}
	if runtime.GOOS == "windows" {
		r.CgroupParent = "/sys/fs/cgroup/isolatex"
		if err := r.Validate(); err == nil {
			t.Fatal("Linux cgroup option silently ignored on Windows")
		}
	}
}

func TestDecodeValidatesMemoryResources(t *testing.T) {
	input := strings.Replace(validConfig, `    upstreams: ["http://127.0.0.1:9100"]`, `    process:
      command: ["./app"]
      workers: 1
      port_start: 9100
      resources:
        memory_max_bytes: 67108864`, 1) + `    readiness:
      path: /readyz
      interval: 1s
      timeout: 100ms
      startup_timeout: 5s
`
	if runtime.GOOS == "linux" {
		input = strings.Replace(input, "memory_max_bytes: 67108864", "memory_max_bytes: 67108864\n        cgroup_parent: /sys/fs/cgroup/isolatex", 1)
	}
	_, err := Decode(strings.NewReader(input))
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil {
		t.Fatal("unsupported memory limit accepted through Decode")
	}
	for _, invalid := range []string{
		strings.Replace(input, "memory_max_bytes: 67108864", "memory_max_bytes: 0", 1),
		strings.Replace(input, "memory_max_bytes:", "memory_max_bytez:", 1),
	} {
		if _, err := Decode(strings.NewReader(invalid)); err == nil {
			t.Fatal("invalid resource setting passed Decode")
		}
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

func TestDecodeValidatesTaskResources(t *testing.T) {
	base := strings.Replace(validConfig, `    upstreams: ["http://127.0.0.1:9100"]`, `    process:
      command: ["./app"]
      workers: 1
      port_start: 9100
      resources: {LIMIT}
    readiness:
      path: /readyz
      interval: 1s
      timeout: 100ms
      startup_timeout: 5s`, 1)
	for _, tt := range []struct {
		setting  string
		platform string
	}{
		{"tasks_max: 32, cgroup_parent: /sys/fs/cgroup/isolatex", "linux"},
		{"processes_max: 4", "windows"},
		{"tasks_max: 32, memory_max_bytes: 67108864, cgroup_parent: /sys/fs/cgroup/isolatex", "linux"},
		{"processes_max: 4, memory_max_bytes: 67108864", "windows"},

		{"cpu_millicores: 250, cgroup_parent: /sys/fs/cgroup/isolatex", "linux"},
		{"cpu_millicores: 1000, tasks_max: 128, memory_max_bytes: 268435456, cgroup_parent: /sys/fs/cgroup/isolatex", "linux"},
		{"cpu_percent: 5", "windows"},
		{"cpu_percent: 5, processes_max: 8, memory_max_bytes: 268435456", "windows"},
		{"cpu_millicores: -1", ""}, {"cpu_millicores: 9, cgroup_parent: /sys/fs/cgroup/isolatex", ""},
		{"cpu_millicores: 92233720368547759, cgroup_parent: /sys/fs/cgroup/isolatex", ""},
		{"cpu_percent: -1", ""}, {"cpu_percent: 101", ""},
		{"cpu_percent: 0", ""}, {"cpu_millicores: 0", ""},
		{"cpu_millicores: 250, cpu_percent: 5, cgroup_parent: /sys/fs/cgroup/isolatex", ""},
		{"cpu_millicore: 250", ""},
		{"tasks_max: -1", ""}, {"processes_max: -1", ""},
		{"processes_max: 4294967296", ""}, {"tasks_max: 0", ""},
		{"tasks_max: 32", ""},
		{"tasks_max: 32, cgroup_parent: relative", ""},
		{"tasks_max: 32, processes_max: 4, cgroup_parent: /sys/fs/cgroup/isolatex", ""},
		{"task_max: 32", ""},
	} {
		t.Run(tt.setting, func(t *testing.T) {
			_, err := Decode(strings.NewReader(strings.Replace(base, "LIMIT", tt.setting, 1)))
			if (err == nil) != (runtime.GOOS == tt.platform) {
				t.Fatalf("platform=%s error=%v", runtime.GOOS, err)
			}
		})
	}
}

func TestResourceLimitsRequireIntegerScalars(t *testing.T) {
	for _, field := range []string{"memory_max_bytes", "tasks_max", "processes_max", "cpu_millicores", "cpu_percent"} {
		for _, value := range []string{"0.9", "10.0", "true", `"10"`, ".nan", ".inf"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				var resources Resources
				decoder := yaml.NewDecoder(strings.NewReader(field + ": " + value + "\n"))
				decoder.KnownFields(true)
				if err := decoder.Decode(&resources); err == nil {
					t.Fatalf("noninteger resource limit accepted: %s=%s", field, value)
				}
			})
		}
	}
	for _, input := range []string{
		"cpu_millicores: &limit 250\ntasks_max: *limit\n",
		"<<: &limits {cpu_millicores: 250}\ntasks_max: 128\n",
	} {
		var resources Resources
		decoder := yaml.NewDecoder(strings.NewReader(input))
		decoder.KnownFields(true)
		if err := decoder.Decode(&resources); err != nil {
			t.Fatalf("integer aliases/merge rejected: %v", err)
		}
	}
	for _, input := range []string{
		"cpu_millicores: &limit 0.9\ntasks_max: *limit\n",
		"<<: &limits {cpu_millicores: 0.9}\nmemory_max_bytes: 67108864\n",
	} {
		var resources Resources
		decoder := yaml.NewDecoder(strings.NewReader(input))
		decoder.KnownFields(true)
		if err := decoder.Decode(&resources); err == nil {
			t.Fatal("noninteger alias/merge accepted")
		}
	}
}
