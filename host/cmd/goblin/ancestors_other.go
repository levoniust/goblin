//go:build !windows

package main

import (
	"fmt"
	"os"
	"strings"
)

// ancestors: this process and its parents (nearest first) from /proc (Linux); elsewhere just this process.
func ancestors() []int {
	var out []int
	p := os.Getpid()
	for i := 0; i < 24 && p > 1; i++ {
		out = append(out, p)
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p))
		if err != nil {
			break
		}
		s := string(b)
		f := strings.Fields(s[strings.LastIndex(s, ")")+1:]) // state ppid ...
		if len(f) < 2 {
			break
		}
		fmt.Sscan(f[1], &p)
	}
	return out
}
