package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCrashTrialRejectsAnAppThatIgnoresTheFault(t *testing.T) {
	dir := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	build := func(name, source string) string {
		t.Helper()
		path := filepath.Join(dir, name+suffix)
		cmd := exec.Command("go", "build", "-o", path, source)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, output)
		}
		return path
	}
	o := options{
		App: build("no-crash-app", "./testdata/no-crash-app"), Runtime: build("isolatex", "../../cmd/isolatex"),
		Scenario: "crash", Baseline: time.Second, Fault: time.Second, Recovery: time.Second,
		HealthyRate: 1, FaultRate: 1, Repeats: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := trial(ctx, o, "C", 1)
	if err == nil || !strings.Contains(err.Error(), "crash injection was not verified") {
		t.Fatalf("unobserved fault accepted or wrong failure: %v", err)
	}
}
