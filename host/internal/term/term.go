// Package term holds the chats: tmux on Linux/Mac, the goblin keeper (ConPTY) on Windows.
package term

import (
	"strings"

	"github.com/levoniust/goblin/host/internal/core"
)

// Window is one chat.
type Window struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Pane     string `json:"pane"`
	PID      int    `json:"pid"`
	Cwd      string `json:"cwd"`
	Command  string `json:"command"`
	Activity int64  `json:"activity"`
	Dead     bool   `json:"dead"`
}

// PaneInfo: size, scrollback length and full-screen flag.
type PaneInfo struct {
	W, H, Hist int
	Alt        bool
}

// Attachment is a live terminal view of one chat.
type Attachment interface {
	Read() ([]byte, error) // terminal output; io.EOF at the end
	Write([]byte) error    // key bytes
	Resize(cols, rows int)
	Close() error
}

// Backend is what holds the chats on this machine.
type Backend interface {
	Ensure() error
	Windows() ([]Window, error)
	NewWindow(name, cwd, command string) (wid, pane string, err error)
	Rename(wid, name string) error
	Kill(wid string) error
	SendKeys(pane string, keys []string, literal string) error
	Capture(pane string, lines int) (string, error)
	Info(pane string) (*PaneInfo, error)
	Snapshot(pane string, lines int) ([]string, error) // scrollback + screen with colours, one string per line
	Attach(w Window, mode string, cols, rows int) (Attachment, error)
	CommandFor(l core.Launcher, resume bool) string
	DetectKind(pid int, ls []core.Launcher) string
	Cleanup()
}

// Reader proto 2: seed with the deep scrollback, then keep in sync with window diffs (same as Switchboard).
const (
	DeepCapture = 40000
	FullCap     = 60000
)

// Diff is the smallest update turning the client's copy (prev) into new: lines = lines[drop:][:keep] + tail.
type Diff struct {
	Drop int      `json:"drop"`
	Keep int      `json:"keep"`
	Tail []string `json:"tail"`
	Full bool     `json:"full"`
}

// ScreenDiff: scrolled = how many lines left the top since prev.
func ScreenDiff(prev, cur []string, scrolled int) Diff {
	if prev == nil {
		return Diff{Tail: cur, Full: true}
	}
	drop := scrolled
	if drop < 0 || drop > len(prev) {
		drop = len(prev)
	}
	base := prev[drop:]
	keep := 0
	for keep < len(base) && keep < len(cur) && base[keep] == cur[keep] {
		keep++
	}
	return Diff{Drop: drop, Keep: keep, Tail: append([]string{}, cur[keep:]...), Full: keep == 0 && drop == len(prev)}
}

// FullKeepLen: how many head lines of the client's full list survive a window diff.
func FullKeepLen(fullLen, prevLen int, d Diff) int {
	return fullLen - (prevLen - d.Drop - d.Keep)
}

// ParseOptions finds the numbered choices of a dialog at the bottom of a captured Claude Code screen.
func ParseOptions(screen string) [][2]string {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	type opt struct{ d, label string }
	var found []opt
	for _, l := range lines {
		s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "❯"))
		if len(s) > 3 && s[0] >= '0' && s[0] <= '9' && s[1] == '.' && s[2] == ' ' {
			found = append(found, opt{s[:1], strings.TrimSpace(s[3:])})
		}
	}
	start := -1
	for i, f := range found {
		if f.d == "1" {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	var out [][2]string
	want := 1
	for _, f := range found[start:] {
		if f.d == string(rune('0'+want)) {
			out = append(out, [2]string{f.d, f.label})
			want++
		}
	}
	return out
}

// stripSGR removes colour escapes (CSI sequences) from a line.
func stripSGR(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
