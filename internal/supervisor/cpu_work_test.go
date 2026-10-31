//go:build linux || windows

package supervisor

import (
	"fmt"
	"testing"
	"time"
)

// Supplement Job accounting with independent actual-compute evidence. The
// finite fixture runs four CPU goroutines per process. At 1% on <=64 logical
// CPUs the tree's allowance is <=0.64 CPU, well below its eight runnable loops.
// This deliberately loose 75% ceiling detects effectively uncapped work even
// when legacy Job time counters stay frozen. It is not a CPU percentage meter.
func cpuWorkReduction(reference, limited cpuResult) (ratio float64, checked bool, err error) {
	if reference.After.CPUs == 0 || limited.After.CPUs != reference.After.CPUs {
		return 0, false, fmt.Errorf("CPU work comparison requires a stable nonzero processor count")
	}
	if reference.After.CPUs > 64 {
		return 0, false, nil
	}
	workRate := func(result cpuResult) (float64, error) {
		if result.Elapsed <= 0 || result.After.WorkUnits <= result.Before.WorkUnits || result.ChildAfter.WorkUnits <= result.Child.WorkUnits {
			return 0, fmt.Errorf("CPU work comparison requires positive elapsed time and increasing parent/child counters")
		}
		work := float64(result.After.WorkUnits-result.Before.WorkUnits) + float64(result.ChildAfter.WorkUnits-result.Child.WorkUnits)
		return work / result.Elapsed.Seconds(), nil
	}
	referenceRate, err := workRate(reference)
	if err != nil {
		return 0, false, err
	}
	limitedRate, err := workRate(limited)
	if err != nil {
		return 0, false, err
	}
	ratio = limitedRate / referenceRate
	if ratio > 0.75 {
		return ratio, true, fmt.Errorf("CPU work reduction not demonstrated: limited/reference=%.4f exceeds 0.75; inspect enforcement and baseline host contention", ratio)
	}
	return ratio, true, nil
}

func TestCPUWorkReductionRejectsFrozenAccountingFalsePositive(t *testing.T) {
	reference := cpuResult{
		Before:     cpuSample{PID: 1, Cycles: 10},
		After:      cpuSample{PID: 1, CPUs: 12, Processes: 2, Cycles: 100, WorkUnits: 1000000},
		Child:      cpuSample{PID: 2, Cycles: 10},
		ChildAfter: cpuSample{PID: 2, Cycles: 100, WorkUnits: 1000000},
		Elapsed:    1500 * time.Millisecond,
	}
	// Native flags can look correct and Job time can stay frozen while work
	// proceeds at the uncapped rate. That must not be a successful contrast.
	limited := reference
	limited.Before.Flags, limited.Before.Rate = 5, 100
	limited.After.Flags, limited.After.Rate = 5, 100
	limited.Child.Flags, limited.Child.Rate = 5, 100
	if _, checked, err := cpuWorkReduction(reference, limited); err == nil || !checked {
		t.Fatalf("unreduced work with zero accounting passed: checked=%v error=%v", checked, err)
	}
	// Real work may be nonzero even when accounting remains zero. A much lower
	// compute rate independently demonstrates the contrast, not an exact quota.
	limited.After.WorkUnits, limited.ChildAfter.WorkUnits = 10000, 10000
	if ratio, checked, err := cpuWorkReduction(reference, limited); err != nil || !checked || ratio >= 0.75 {
		t.Fatalf("reduced work rejected: ratio=%v checked=%v error=%v", ratio, checked, err)
	}
	// Normalize for wall time rather than comparing raw batch totals.
	limited = reference
	limited.Elapsed *= 4
	if _, checked, err := cpuWorkReduction(reference, limited); err != nil || !checked {
		t.Fatal(checked, err)
	}
	limited.Elapsed = 0
	if _, _, err := cpuWorkReduction(reference, limited); err == nil {
		t.Fatal("zero elapsed accepted")
	}
	limited = reference
	limited.ChildAfter.WorkUnits = limited.Child.WorkUnits
	if _, _, err := cpuWorkReduction(reference, limited); err == nil {
		t.Fatal("non-progressing child accepted")
	}
	limited = reference
	limited.After.CPUs++
	if _, _, err := cpuWorkReduction(reference, limited); err == nil {
		t.Fatal("changed CPU count accepted")
	}
	reference.After.CPUs, limited.After.CPUs = 65, 65
	if _, checked, err := cpuWorkReduction(reference, limited); err != nil || checked {
		t.Fatal("large host contrast claimed", checked, err)
	}
}
