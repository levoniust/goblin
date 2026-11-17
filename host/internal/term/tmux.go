//go:build !windows

package term

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/levoniust/goblin/host/internal/core"
)

// Tmux keeps every chat as a window of one session on Goblin's own tmux server (`tmux -L goblin`), so it never
// mixes with other tmux sessions on the machine. The server is started in its own systemd scope: restarting the
// goblin service must never kill the chats (Switchboard lost every chat that way on 2026-11-08).
type Tmux struct {
	Session string
	Socket  string
	Bin     string
	mu      sync.Mutex
}

func NewBackend(cfg *core.Config) Backend {
	return &Tmux{Session: cfg.TmuxSession, Socket: "goblin-" + cfg.TmuxSession, Bin: "tmux"}
}

const fmtWin = "#{window_id}\t#{window_index}\t#{window_name}\t#{pane_id}\t#{pane_pid}\t#{pane_current_path}\t#{pane_current_command}\t#{window_activity}\t#{pane_dead}"

func (t *Tmux) cmd(args ...string) *exec.Cmd {
	c := exec.Command(t.Bin, append([]string{"-L", t.Socket}, args...)...)
	c.Env = cleanEnv()
	return c
}

func cleanEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") && !strings.HasPrefix(e, "TMUX_PANE=") {
			env = append(env, e)
		}
	}
	return env
}

func (t *Tmux) run(args ...string) (string, error) {
	var out, errb bytes.Buffer
	c := t.cmd(args...)
	c.Stdout, c.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		return "", err
	}
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return out.String(), fmt.Errorf("tmux %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
		}
		return out.String(), nil
	case <-time.After(10 * time.Second):
		c.Process.Kill()
		return "", errors.New("tmux timed out")
	}
}

// Ensure starts the session (and the tmux server, in its own scope) if needed.
func (t *Tmux) Ensure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd("has-session", "-t", "="+t.Session).Run() == nil {
		return nil
	}
	args := []string{t.Bin, "-L", t.Socket, "new-session", "-d", "-s", t.Session, "-n", "shell", "-x", "200", "-y", "50"}
	var c *exec.Cmd
	if sr, err := exec.LookPath("systemd-run"); err == nil && os.Getenv("XDG_RUNTIME_DIR") != "" {
		nb := make([]byte, 4)
		rand.Read(nb)
		unit := "goblin-tmux-" + t.Session + "-" + hex.EncodeToString(nb)
		c = exec.Command(sr, append([]string{"--user", "--scope", "--quiet", "--collect", "--unit", unit}, args...)...)
	} else {
		c = exec.Command(args[0], args[1:]...)
	}
	c.Env = cleanEnv()
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("start tmux: %v %s", err, out)
	}
	t.run("set-option", "-t", "="+t.Session, "history-limit", "50000")
	t.run("set-option", "-t", "="+t.Session, "status", "off")
	return nil
}

func (t *Tmux) Windows() ([]Window, error) {
	out, err := t.run("list-windows", "-t", "="+t.Session, "-F", fmtWin)
	if err != nil {
		return []Window{}, nil
	}
	ws := []Window{}
	for _, line := range strings.Split(out, "\n") {
		p := strings.Split(line, "\t")
		if len(p) < 9 {
			continue
		}
		idx, _ := strconv.Atoi(p[1])
		pid, _ := strconv.Atoi(p[4])
		act, _ := strconv.ParseInt(p[7], 10, 64)
		ws = append(ws, Window{ID: p[0], Index: idx, Name: p[2], Pane: p[3], PID: pid, Cwd: p[5], Command: p[6], Activity: act, Dead: p[8] == "1"})
	}
	return ws, nil
}

func (t *Tmux) NewWindow(name, cwd, command string) (string, string, error) {
	if err := t.Ensure(); err != nil {
		return "", "", err
	}
	out, err := t.run("new-window", "-d", "-P", "-F", "#{window_id}\t#{pane_id}", "-t", "="+t.Session+":", "-n", name, "-c", cwd, command)
	if err != nil {
		return "", "", err
	}
	p := strings.Split(strings.TrimSpace(out), "\t")
	if len(p) != 2 {
		return "", "", errors.New("tmux new-window: unexpected output")
	}
	return p[0], p[1], nil
}

func (t *Tmux) Rename(wid, name string) error {
	_, err := t.run("rename-window", "-t", wid, name)
	return err
}
func (t *Tmux) Kill(wid string) error { _, err := t.run("kill-window", "-t", wid); return err }

func (t *Tmux) SendKeys(pane string, keys []string, literal string) error {
	if literal != "" {
		if _, err := t.run("send-keys", "-t", pane, "-l", literal); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if _, err := t.run("send-keys", "-t", pane, k); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tmux) Capture(pane string, lines int) (string, error) {
	return t.run("capture-pane", "-p", "-J", "-t", pane, "-S", "-"+strconv.Itoa(lines))
}

func (t *Tmux) Info(pane string) (*PaneInfo, error) {
	out, err := t.run("display-message", "-p", "-t", pane, "#{pane_width}\t#{pane_height}\t#{history_size}\t#{alternate_on}")
	if err != nil {
		return nil, err
	}
	p := strings.Split(strings.TrimSpace(out), "\t")
	if len(p) < 4 {
		return nil, errors.New("pane gone")
	}
	w, _ := strconv.Atoi(p[0])
	h, _ := strconv.Atoi(p[1])
	hist, _ := strconv.Atoi(p[2])
	return &PaneInfo{W: w, H: h, Hist: hist, Alt: p[3] == "1"}, nil
}

func (t *Tmux) Snapshot(pane string, lines int) ([]string, error) {
	out, err := t.run("capture-pane", "-p", "-e", "-J", "-t", pane, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return nil, err
	}
	rows := strings.Split(out, "\n")
	for len(rows) > 0 && strings.TrimSpace(stripSGR(rows[len(rows)-1])) == "" {
		rows = rows[:len(rows)-1]
	}
	return rows, nil
}

// CommandFor: the shell command a new chat runs. Falls back to a shell when the program exits. GOBLIN_KIND lets
// Goblin tell launchers apart later; Claude runs without the alternate screen so its output scrolls into history.
func (t *Tmux) CommandFor(l core.Launcher, resume bool) string {
	cmd := l.Command
	if resume && l.Resume != "" {
		cmd = l.Resume
	}
	if cmd == "" {
		return "env GOBLIN_KIND=shell bash -l"
	}
	env := "GOBLIN_KIND=" + l.Kind + " "
	if l.Claude {
		env += "CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN=1 "
	}
	return "bash -ic '" + env + cmd + "; exec bash'"
}

// DetectKind looks for GOBLIN_KIND or a launcher's process text in the pane's process tree.
func (t *Tmux) DetectKind(pid int, ls []core.Launcher) string {
	for _, p := range append([]int{pid}, descendants(pid, 4)...) {
		env, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p), "environ"))
		for _, kv := range bytes.Split(env, []byte{0}) {
			if k, ok := bytes.CutPrefix(kv, []byte("GOBLIN_KIND=")); ok {
				for _, l := range ls {
					if l.Kind == string(k) && l.Kind != "shell" {
						return l.Kind
					}
				}
			}
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p), "cmdline"))
		args := strings.Split(string(cmdline), "\x00")
		for _, l := range ls {
			for i, a := range args {
				if i > 2 {
					break
				}
				if (l.Process != "" && strings.Contains(a, l.Process)) || (l.Kind == "claude" && filepath.Base(a) == "claude") {
					return l.Kind
				}
			}
		}
	}
	return "shell"
}

func descendants(pid, depth int) []int {
	var out []int
	level := []int{pid}
	for d := 0; d < depth && len(level) > 0; d++ {
		var next []int
		for _, p := range level {
			b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", p, p))
			if err != nil {
				continue
			}
			for _, f := range strings.Fields(string(b)) {
				if n, err := strconv.Atoi(f); err == nil {
					next = append(next, n)
				}
			}
		}
		out = append(out, next...)
		level = next
	}
	return out
}

// Cleanup removes view sessions left over from a crash.
func (t *Tmux) Cleanup() {
	out, _ := t.run("list-sessions", "-F", "#{session_name}")
	for _, n := range strings.Fields(out) {
		if strings.HasPrefix(n, "gv-") {
			t.run("kill-session", "-t", "="+n)
		}
	}
}

// Attach: a grouped view session (own current window) attached on a PTY. Modes other than "live" never resize
// the real window (ignore-size), so the phone doesn't squash the PC's view.
func (t *Tmux) Attach(w Window, mode string, cols, rows int) (Attachment, error) {
	nb := make([]byte, 4)
	rand.Read(nb)
	view := "gv-" + hex.EncodeToString(nb)
	if _, err := t.run("new-session", "-d", "-s", view, "-t", "="+t.Session); err != nil {
		return nil, err
	}
	t.run("set-option", "-t", view, "status", "off")
	t.run("select-window", "-t", view+":"+w.ID)
	args := []string{"-L", t.Socket, "attach-session", "-t", "=" + view}
	if mode != "live" {
		args = []string{"-L", t.Socket, "attach-session", "-f", "ignore-size", "-t", "=" + view}
	}
	c := exec.Command(t.Bin, args...)
	c.Env = append(cleanEnv(), "TERM=xterm-256color", "COLORTERM=truecolor")
	f, err := pty.StartWithSize(c, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		t.run("kill-session", "-t", "="+view)
		return nil, err
	}
	return &ptyAttach{t: t, view: view, f: f, c: c}, nil
}

type ptyAttach struct {
	t    *Tmux
	view string
	f    *os.File
	c    *exec.Cmd
	once sync.Once
}

func (a *ptyAttach) Read() ([]byte, error) {
	buf := make([]byte, 64*1024)
	n, err := a.f.Read(buf)
	if n > 0 {
		return buf[:n], nil
	}
	if err == nil {
		err = io.EOF
	}
	return nil, err
}

func (a *ptyAttach) Write(b []byte) error {
	_, err := a.f.Write(b)
	return err
}

func (a *ptyAttach) Resize(cols, rows int) {
	pty.Setsize(a.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (a *ptyAttach) Close() error {
	a.once.Do(func() {
		a.c.Process.Kill()
		a.c.Wait()
		a.f.Close()
		a.t.run("kill-session", "-t", "="+a.view)
	})
	return nil
}
