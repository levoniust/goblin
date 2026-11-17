//go:build !windows

package term

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// startPty runs a command on a PTY (the keeper on non-Windows machines: used by tests, and a fallback without tmux).
func startPty(cmd, cwd string, env []string, cols, rows int) (ptyProc, error) {
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = cwd
	c.Env = append(append(os.Environ(), "TERM=xterm-256color"), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	f, err := pty.StartWithSize(c, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	go c.Wait()
	return &unixPty{f: f, c: c}, nil
}

type unixPty struct {
	f *os.File
	c *exec.Cmd
}

func (p *unixPty) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *unixPty) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *unixPty) Resize(c, r int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(c), Rows: uint16(r)})
}
func (p *unixPty) Pid() int { return p.c.Process.Pid }
func (p *unixPty) Close() error {
	p.c.Process.Kill()
	return p.f.Close()
}
