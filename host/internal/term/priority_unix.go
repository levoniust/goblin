//go:build linux

package term

import "syscall"

// LowerPriority puts a chat's process and everything under it behind the computer's user (2026-11-17: LLM work
// takes a back seat): CPU nice and idle-class disk I/O. Programs the chat starts later inherit both.
func LowerPriority(pid int, level string) {
	nice, io := 0, false
	switch level {
	case "below_normal":
		nice, io = 10, true
	case "idle":
		nice, io = 19, true
	default:
		return
	}
	for _, p := range append([]int{pid}, descendants(pid, 6)...) {
		syscall.Setpriority(syscall.PRIO_PROCESS, p, nice)
		if io {
			// ioprio_set(IOPRIO_WHO_PROCESS, p, IOPRIO_CLASS_IDLE<<13)
			syscall.Syscall(syscall.SYS_IOPRIO_SET, 1, uintptr(p), uintptr(3<<13))
		}
	}
}
