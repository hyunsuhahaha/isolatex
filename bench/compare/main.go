// Command compare runs matched-resource A/B/C fault-isolation experiments.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

type options struct {
	App, Runtime, Scenario            string
	Warmup, Baseline, Fault, Recovery time.Duration
	HealthyRate, FaultRate, Repeats   int
	Seed                              int64
	Output                            string
}

type phaseResult struct {
	Name            string  `json:"name"`
	DurationSeconds float64 `json:"duration_seconds"`
	Healthy         summary `json:"healthy"`
	Fault           summary `json:"fault"`
}

type trialResult struct {
	Variant                string        `json:"variant"`
	Repeat                 int           `json:"repeat"`
	AppProcesses           int           `json:"app_processes"`
	ExecutionBudget        int           `json:"execution_budget"`
	QueueBudget            int           `json:"queue_budget"`
	ProtectedPIDsUnchanged bool          `json:"protected_pids_unchanged"`
	Phases                 []phaseResult `json:"phases"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	var o options
	flag.StringVar(&o.App, "app", "bin/demo-app"+suffix, "demo-app executable")
	flag.StringVar(&o.Runtime, "runtime", "bin/isolatex"+suffix, "runtime executable")
	flag.StringVar(&o.Scenario, "scenario", "delay", "delay or crash")
	flag.DurationVar(&o.Warmup, "warmup", 2*time.Second, "warmup per trial")
	flag.DurationVar(&o.Baseline, "baseline", 5*time.Second, "baseline per trial")
	flag.DurationVar(&o.Fault, "fault", 5*time.Second, "fault injection per trial")
	flag.DurationVar(&o.Recovery, "recovery", 5*time.Second, "recovery per trial")
	flag.IntVar(&o.HealthyRate, "healthy-rate", 40, "healthy request arrivals per second")
	flag.IntVar(&o.FaultRate, "fault-rate", 40, "fault-domain request arrivals per second")
	flag.IntVar(&o.Repeats, "repeats", 3, "randomized repetitions of A/B/C")
	flag.Int64Var(&o.Seed, "seed", 42, "variant order seed")
	flag.StringVar(&o.Output, "out", "", "JSON result path; empty writes stdout")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q; pass output paths as -out <path>", flag.Args())
	}
	if err := o.validate(); err != nil {
		return err
	}
	var err error
	o.App, err = filepath.Abs(o.App)
	if err != nil {
		return err
	}
	o.Runtime, err = filepath.Abs(o.Runtime)
	if err != nil {
		return err
	}
	appHash, err := fileHash(o.App)
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	runtimeHash, err := fileHash(o.Runtime)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	started := time.Now().UTC()
	results := []trialResult{}
	rng := rand.New(rand.NewSource(o.Seed))
	for repeat := 1; repeat <= o.Repeats; repeat++ {
		order := []string{"A", "B", "C"}
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, variant := range order {
			fmt.Fprintf(os.Stderr, "repeat %d/%d variant %s scenario %s\n", repeat, o.Repeats, variant, o.Scenario)
			result, err := trial(ctx, o, variant, repeat)
			if err != nil {
				return fmt.Errorf("repeat %d variant %s: %w", repeat, variant, err)
			}
			results = append(results, result)
		}
	}
	report := map[string]any{
		"format_version": 1, "started_at": started, "finished_at": time.Now().UTC(),
		"go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"logical_cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0),
		"scenario": o.Scenario, "seed": o.Seed, "healthy_rate": o.HealthyRate, "fault_rate": o.FaultRate,
		"app_sha256": appHash, "runtime_sha256": runtimeHash, "trials": results,
		"scope":         "controlled local HTTP experiment; load generator, gateway and app processes share a host; no CPU, memory or DB isolation claim",
		"load_contract": map[string]any{"max_inflight": 256, "late_drop_after_ms": 100, "demo_max_active_per_worker": 1, "demo_baseline_delay_ms": 5, "fault_delay_ms": 1000, "crash_interval_seconds": 1, "latency_cohort": "requests that started in each phase; in-flight work is not drained between phases"},
		"topologies":    map[string]string{"A": "shared request budget and 4 shared app processes", "B": "split request budgets and 4 shared app processes", "C": "split request budgets and disjoint app processes (2+2)"},
	}
	var out io.Writer = os.Stdout
	if o.Output != "" {
		if err := os.MkdirAll(filepath.Dir(o.Output), 0755); err != nil {
			return err
		}
		f, err := os.Create(o.Output)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func (o options) validate() error {
	if o.Scenario != "delay" && o.Scenario != "crash" {
		return fmt.Errorf("scenario must be delay or crash")
	}
	if o.Repeats < 1 || o.Repeats > 20 || o.HealthyRate < 1 || o.HealthyRate > 5000 || o.FaultRate < 1 || o.FaultRate > 5000 {
		return fmt.Errorf("repeats must be 1..20 and rates 1..5000")
	}
	if o.Warmup < 0 || o.Warmup > 10*time.Minute {
		return fmt.Errorf("warmup must be 0..10m")
	}
	for _, d := range []time.Duration{o.Baseline, o.Fault, o.Recovery} {
		if d < time.Second || d > 10*time.Minute {
			return fmt.Errorf("baseline, fault and recovery must be 1s..10m")
		}
	}
	planned := (o.Warmup + o.Baseline + o.Fault + o.Recovery).Seconds() * float64(o.HealthyRate+o.FaultRate) * float64(o.Repeats) * 3
	if planned > 2_000_000 {
		return fmt.Errorf("experiment exceeds 2 million planned requests; reduce rates, durations or repeats")
	}
	return nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
