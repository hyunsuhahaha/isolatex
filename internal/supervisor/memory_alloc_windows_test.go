//go:build windows

package supervisor

import (
	"golang.org/x/sys/windows"
)

func allocateTestMemory(size int) (func(), error) {
	address, err := windows.VirtualAlloc(0, uintptr(size), windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE)
	if err != nil {
		return nil, err
	}
	// JOB_MEMORY accounts committed pages. VirtualAlloc with MEM_COMMIT
	// charges the entire region even before physical pages are touched.
	return func() { _ = windows.VirtualFree(address, 0, windows.MEM_RELEASE) }, nil
}
