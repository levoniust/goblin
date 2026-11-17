//go:build !windows

package main

import (
	"os"
	"os/exec"
)

func hideWindow(c *exec.Cmd, low bool) {
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if low {
		c.Args = append([]string{"nice", "-n", "10"}, c.Args...)
		c.Path, _ = exec.LookPath("nice")
	}
}
