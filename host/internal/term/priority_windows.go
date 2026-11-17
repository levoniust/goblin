//go:build windows

package term

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// LowerPriority puts a chat's process and everything under it behind the computer's user (2026-11-17: LLM work
// takes a back seat). Windows hands a below-normal or idle priority class on to every program the chat starts.
func LowerPriority(pid int, level string) {
	var class uint32
	switch level {
	case "below_normal":
		class = windows.BELOW_NORMAL_PRIORITY_CLASS
	case "idle":
		class = windows.IDLE_PRIORITY_CLASS
	default:
		return
	}
	for _, p := range winTree(uint32(pid)) {
		h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p)
		if err != nil {
			continue
		}
		windows.SetPriorityClass(h, class)
		windows.CloseHandle(h)
	}
}

// winTree: pid and all its descendants.
func winTree(root uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return []uint32{root}
	}
	defer windows.CloseHandle(snap)
	kids := map[uint32][]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		kids[e.ParentProcessID] = append(kids[e.ParentProcessID], e.ProcessID)
	}
	out, level := []uint32{root}, []uint32{root}
	for d := 0; d < 8 && len(level) > 0; d++ {
		var next []uint32
		for _, p := range level {
			next = append(next, kids[p]...)
		}
		out = append(out, next...)
		level = next
	}
	return out
}
