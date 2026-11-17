//go:build !windows

package mesh

import "syscall"

// reuseControl lets several goblin instances (tests) share the beacon port and allows broadcast sends.
func reuseControl(network, address string, c syscall.RawConn) error {
	var err error
	c.Control(func(fd uintptr) {
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		setReusePort(int(fd))
		err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	})
	return err
}
