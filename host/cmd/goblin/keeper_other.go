//go:build !windows

package main

import "fmt"

func runKeeper(home string) error {
	return fmt.Errorf("the keeper is only needed on Windows (Linux and Mac use tmux)")
}
