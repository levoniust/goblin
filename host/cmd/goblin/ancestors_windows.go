package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ancestors: this process and its parents (nearest first), so the host can tell which chat a hook runs in when
// GOBLIN_CHAT is missing (chats started by a keeper older than 0.2.1).
func ancestors() []int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	parent := map[uint32]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parent[e.ProcessID] = e.ParentProcessID
	}
	var out []int
	p := uint32(os.Getpid())
	for i := 0; i < 24 && p != 0; i++ {
		out = append(out, int(p))
		p = parent[p]
	}
	return out
}
