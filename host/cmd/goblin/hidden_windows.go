package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func hideWindow(c *exec.Cmd, low bool) {
	flags := uint32(windows.CREATE_NO_WINDOW)
	if low {
		flags |= windows.BELOW_NORMAL_PRIORITY_CLASS
	}
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
}
