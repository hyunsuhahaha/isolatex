//go:build unix

package gateway

import "syscall"

func setTestReceiveBuffer(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1024)
}
