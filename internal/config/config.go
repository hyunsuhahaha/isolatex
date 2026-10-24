package config

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version       int              `yaml:"version"`
	Listen        string           `yaml:"listen"`
	AdminListen   string           `yaml:"admin_listen"`
	MaxAdmitted   int              `yaml:"max_admitted_requests"`
	MaxBodyBytes  int64            `yaml:"max_body_bytes"`
	ShutdownGrace time.Duration    `yaml:"shutdown_grace"`
	Bulkheads     map[string]Group `yaml:"bulkheads"`
}

type Group struct {
	Match struct {
		PathPrefix string `yaml:"path_prefix"`
	} `yaml:"match"`
	Process        *Process        `yaml:"process"`
	Upstreams      []string        `yaml:"upstreams"`
	MaxConcurrent  int             `yaml:"max_concurrent"`
	QueueSize      int             `yaml:"queue_size"`
	QueueTimeout   time.Duration   `yaml:"queue_timeout"`
	RequestTimeout time.Duration   `yaml:"request_timeout"`
	Readiness      Readiness       `yaml:"readiness"`
	CircuitBreaker *CircuitBreaker `yaml:"circuit_breaker"`
}

type Process struct {
	Command   []string          `yaml:"command"`
	Workers   int               `yaml:"workers"`
	PortStart int               `yaml:"port_start"`
	Env       map[string]string `yaml:"env"`
	Resources *Resources        `yaml:"resources"`
}

// Resources apply to one managed worker and its descendants, not the group.
// TasksMax counts Linux threads; ProcessesMax counts Windows processes.
// CPU uses Linux core bandwidth or Windows Job-relative percent, respectively.
type Resources struct {
	MemoryMaxBytes ResourceLimit `yaml:"memory_max_bytes"`
	CPUMillicores  ResourceLimit `yaml:"cpu_millicores"`
	CPUPercent     ResourceLimit `yaml:"cpu_percent"`
	TasksMax       ResourceLimit `yaml:"tasks_max"`
	ProcessesMax   ResourceLimit `yaml:"processes_max"`
	CgroupParent   string        `yaml:"cgroup_parent"`
}

// ResourceLimit rejects YAML float-to-integer coercion, which can silently
// turn a fractional positive limit into zero (disabled). Node-level decoding
// preserves the enclosing decoder's strict fields and YAML alias handling.
type ResourceLimit int64

func (limit *ResourceLimit) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag != "!!int" {
		return fmt.Errorf("resource limit at line %d must be a YAML integer", node.Line)
	}
	var value int64
	if err := node.Decode(&value); err != nil {
		return err
	}
	*limit = ResourceLimit(value)
	return nil
}

func (r *Resources) Validate() error {
	if r == nil {
		return nil
	}
	if r.MemoryMaxBytes < 0 || uint64(r.MemoryMaxBytes) > uint64(^uintptr(0)) || r.TasksMax < 0 || r.ProcessesMax < 0 || r.ProcessesMax > math.MaxUint32 {
		return fmt.Errorf("resources limits must be nonnegative and representable")
	}
	if r.CPUMillicores < 0 || (r.CPUMillicores > 0 && r.CPUMillicores < 10) || r.CPUMillicores > math.MaxInt64/100 || r.CPUPercent < 0 || r.CPUPercent > 100 {
		return fmt.Errorf("resources requires cpu_millicores >= 10 when enabled, representable quota, and cpu_percent in 0..100")
	}
	if r.MemoryMaxBytes == 0 && r.TasksMax == 0 && r.ProcessesMax == 0 && r.CPUMillicores == 0 && r.CPUPercent == 0 {
		return fmt.Errorf("resources requires at least one positive limit")
	}
	switch runtime.GOOS {
	case "windows":
		if r.CPUMillicores != 0 {
			return fmt.Errorf("resources.cpu_millicores is Linux-only; cpu_percent uses Windows Job CPU rate")
		}
		if r.TasksMax != 0 {
			return fmt.Errorf("resources.tasks_max is Linux-only; processes_max counts Windows processes")
		}
		if r.CgroupParent != "" {
			return fmt.Errorf("resources.cgroup_parent is Linux-only")
		}
	case "linux":
		if r.CPUPercent != 0 {
			return fmt.Errorf("resources.cpu_percent is Windows-only; cpu_millicores uses Linux core bandwidth")
		}
		if r.ProcessesMax != 0 {
			return fmt.Errorf("resources.processes_max is Windows-only; tasks_max counts Linux threads")
		}
		if !filepath.IsAbs(r.CgroupParent) || filepath.Clean(r.CgroupParent) != r.CgroupParent {
			return fmt.Errorf("resources requires an absolute clean cgroup_parent on Linux")
		}
	default:
		return fmt.Errorf("managed resource limits are unsupported on %s", runtime.GOOS)
	}
	return nil
}

type Readiness struct {
	Path             string        `yaml:"path"`
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	StartupTimeout   time.Duration `yaml:"startup_timeout"`
	FailureThreshold int           `yaml:"failure_threshold"`
	SuccessThreshold int           `yaml:"success_threshold"`
}

type CircuitBreaker struct {
	Window       time.Duration `yaml:"window"`
	MinRequests  int           `yaml:"min_requests"`
	FailureRatio float64       `yaml:"failure_ratio"`
	OpenTimeout  time.Duration `yaml:"open_timeout"`
}

func (r Readiness) Failures() int {
	if r.FailureThreshold == 0 {
		return 3
	}
	return r.FailureThreshold
}
func (r Readiness) Successes() int {
	if r.SuccessThreshold == 0 {
		return 2
	}
	return r.SuccessThreshold
}

func Load(filename string) (Config, error) {
	f, err := os.Open(filename)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	return Decode(f)
}

func Decode(r io.Reader) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", AdminListen: "127.0.0.1:9090", MaxAdmitted: 256, MaxBodyBytes: 1 << 20, ShutdownGrace: 10 * time.Second}
	data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return c, err
	}
	if len(data) > 1<<20 {
		return c, fmt.Errorf("configuration exceeds 1 MiB")
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("expected one YAML document")
	}
	return c, c.Validate()
}

var groupName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1")
	}
	if c.MaxAdmitted < 1 || c.MaxBodyBytes < 1 || c.ShutdownGrace <= 0 {
		return fmt.Errorf("global limits and shutdown_grace must be positive")
	}
	ports := map[int]string{}
	for label, addr := range map[string]string{"listen": c.Listen, "admin_listen": c.AdminListen} {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		ip := net.ParseIP(host)
		n, err := strconv.Atoi(port)
		if ip == nil || err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s requires an explicit IP and port 1..65535", label)
		}
		if label == "admin_listen" && !ip.IsLoopback() {
			return fmt.Errorf("admin_listen must use a loopback IP")
		}
		if owner := ports[n]; owner != "" {
			return fmt.Errorf("port %d overlaps %s", n, owner)
		}
		ports[n] = label
	}
	if len(c.Bulkheads) == 0 {
		return fmt.Errorf("at least one bulkhead is required")
	}
	prefixes := map[string]bool{}
	remaining := c.MaxAdmitted
	for name, g := range c.Bulkheads {
		if !groupName.MatchString(name) {
			return fmt.Errorf("invalid group name %q", name)
		}
		prefix := g.Match.PathPrefix
		if !ValidPath(prefix) || (prefix != "/" && strings.HasSuffix(prefix, "/")) || prefixes[prefix] {
			return fmt.Errorf("%s: invalid or duplicate path_prefix", name)
		}
		prefixes[prefix] = true
		if g.MaxConcurrent < 1 || g.QueueSize < 0 || g.QueueTimeout <= 0 || g.RequestTimeout <= 0 {
			return fmt.Errorf("%s: invalid request/queue limits", name)
		}
		// Reserve each group's entire active + queued budget at startup. A
		// smaller shared runtime semaphore would let a slow group consume the
		// slots promised to another. Subtract instead of summing to avoid int
		// overflow for untrusted configuration values.
		if g.MaxConcurrent > remaining || g.QueueSize > remaining-g.MaxConcurrent {
			return fmt.Errorf("%s: sum of bulkhead max_concurrent + queue_size must not exceed max_admitted_requests (%d); remaining capacity is %d", name, c.MaxAdmitted, remaining)
		}
		remaining -= g.MaxConcurrent
		remaining -= g.QueueSize
		if b := g.CircuitBreaker; b != nil {
			if b.Window < time.Millisecond || b.MinRequests < 1 || math.IsNaN(b.FailureRatio) || math.IsInf(b.FailureRatio, 0) || b.FailureRatio <= 0 || b.FailureRatio > 1 || b.OpenTimeout <= 0 {
				return fmt.Errorf("%s: invalid circuit_breaker window, minimum requests, failure ratio or open timeout", name)
			}
		}
		if g.Readiness.FailureThreshold < 0 || g.Readiness.SuccessThreshold < 0 {
			return fmt.Errorf("%s: health thresholds cannot be negative", name)
		}
		if (g.Process == nil) == (len(g.Upstreams) == 0) {
			return fmt.Errorf("%s: choose exactly one of process or upstreams", name)
		}
		for _, raw := range g.Upstreams {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
				return fmt.Errorf("%s: upstream must be an http origin without credentials, path or query", name)
			}
			host, port, err := net.SplitHostPort(u.Host)
			n, portErr := strconv.Atoi(port)
			if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 {
				return fmt.Errorf("%s: upstream requires a host and explicit port 1..65535", name)
			}
		}
		if g.Process == nil && g.Readiness.Path != "" && (!ValidPath(g.Readiness.Path) || g.Readiness.Interval <= 0 || g.Readiness.Timeout <= 0) {
			return fmt.Errorf("%s: static readiness requires path and positive interval/timeout", name)
		}
		if p := g.Process; p != nil {
			if err := p.Resources.Validate(); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if len(p.Command) == 0 || strings.TrimSpace(p.Command[0]) == "" || p.Workers < 1 || p.PortStart < 1 || p.Workers > 65536-p.PortStart {
				return fmt.Errorf("%s: invalid process command, workers or ports", name)
			}
			for k, v := range p.Env {
				if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || strings.EqualFold(k, "PORT") || strings.EqualFold(k, "HOST") {
					return fmt.Errorf("%s: invalid or reserved environment key %q", name, k)
				}
			}
			for i := 0; i < p.Workers; i++ {
				port := p.PortStart + i
				if owner := ports[port]; owner != "" {
					return fmt.Errorf("%s: port %d overlaps %s", name, port, owner)
				}
				ports[port] = name
			}
			r := g.Readiness
			if !ValidPath(r.Path) || r.Interval <= 0 || r.Timeout <= 0 || r.StartupTimeout <= 0 {
				return fmt.Errorf("%s: process requires readiness path and positive timings", name)
			}
		}
	}
	return nil
}

// Use the same conservative path interpretation for classification and forwarding.
// Encoded paths are deliberately excluded from this initial protocol contract.
func ValidPath(p string) bool {
	for _, ch := range p {
		if ch < 0x21 || ch > 0x7e {
			return false
		}
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "%\\?#;\x00\r\n") || strings.Contains(p, "//") {
		return false
	}
	clean := path.Clean(p)
	return p == clean || (p != "/" && p == clean+"/")
}
