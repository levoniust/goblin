//go:build !linux && !windows

package term

// LowerPriority: not done on this system yet.
func LowerPriority(pid int, level string) {}
