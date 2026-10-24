//go:build windows

package supervisor

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var queryTestProcessCycles = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryProcessCycleTime")

func sampleTestCPU() (cpuSample, error) {
	result := cpuSample{PID: os.Getpid(), CPUs: windows.GetActiveProcessorCount(0xffff), WorkUnits: cpuBurnBatches.Load()}
	rate := struct{ Flags, Rate uint32 }{}
	// Native LARGE_INTEGER fields precede four DWORDs in the accounting buffer.
	accounting := struct {
		User, Kernel, PeriodUser, PeriodKernel int64
		Faults, Processes, Active, Terminated  uint32
	}{}
	var cycles uint64
	var pinned runtime.Pinner
	pinned.Pin(&cycles)
	pinned.Pin(&rate)
	pinned.Pin(&accounting)
	defer pinned.Unpin()
	if err := windows.QueryInformationJobObject(0, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate)), nil); err != nil {
		return result, err
	}
	if err := windows.QueryInformationJobObject(0, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
		return result, err
	}
	if ok, _, err := queryTestProcessCycles.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&cycles))); ok == 0 {
		return result, err
	}
	result.Cycles = cycles
	result.Flags, result.Rate, result.Processes = rate.Flags, rate.Rate, accounting.Processes
	result.UsageUsec = uint64(accounting.User+accounting.Kernel) / 10
	return result, nil
}
