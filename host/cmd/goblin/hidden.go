package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// goblin hidden [--low] -- PROGRAM ARGS: runs a program with no window and waits for it (2026-11-17: nothing of
// Goblin's, or the model servers it starts, may sit on the desktop). --low = below-normal priority. Use it from
// goblinw.exe (the windowless build) in Scheduled Tasks.
func cmdHidden(args []string) error {
	low := false
	for len(args) > 0 && args[0] != "--" {
		if args[0] == "--low" {
			low = true
		}
		args = args[1:]
	}
	if len(args) < 2 {
		return errors.New("usage: goblin hidden [--low] -- PROGRAM [ARGS...]")
	}
	c := exec.Command(args[1], args[2:]...)
	hideWindow(c, low)
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return fmt.Errorf("%s: %w", args[1], err)
	}
	return nil
}
