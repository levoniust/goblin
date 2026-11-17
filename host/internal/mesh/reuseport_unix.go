//go:build !windows

package mesh

import (
	"runtime"
	"syscall"
)

// setReusePort sets SO_REUSEPORT (0xf on Linux, 0x200 on the BSDs and macOS).
func setReusePort(fd int) {
	opt := 0x200
	if runtime.GOOS == "linux" {
		opt = 0xf
	}
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, opt, 1)
}
