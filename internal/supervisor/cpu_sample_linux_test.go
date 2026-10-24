//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func sampleTestCPU() (cpuSample, error) {
	result := cpuSample{PID: os.Getpid()}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return result, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			result.Location = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
		}
	}
	if result.Location == "" {
		return result, fmt.Errorf("no cgroup v2 membership")
	}
	data, err = os.ReadFile(filepath.Join(result.Location, "cpu.max"))
	if err != nil {
		return result, err
	}
	result.Quota = strings.TrimSpace(string(data))
	data, err = os.ReadFile(filepath.Join(result.Location, "cpu.stat"))
	if err != nil {
		return result, err
	}
	fields := strings.Fields(string(data))
	for i := 0; i+1 < len(fields); i += 2 {
		value, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return result, err
		}
		switch fields[i] {
		case "usage_usec":
			result.UsageUsec = value
		case "nr_throttled":
			result.Throttled = value
		}
	}
	return result, nil
}
