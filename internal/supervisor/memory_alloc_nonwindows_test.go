//go:build !windows

package supervisor

import "runtime"

func allocateTestMemory(size int) (func(), error) {
	payload := make([]byte, size)
	for i := 0; i < len(payload); i += 4096 {
		payload[i] = 1
	}
	return func() { runtime.KeepAlive(payload) }, nil
}
