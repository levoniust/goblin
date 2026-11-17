//go:build windows

package term

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/UserExistsError/conpty"
)

// startPty runs a command on a ConPTY.
func startPty(cmd, cwd string, env []string, cols, rows int) (ptyProc, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, errors.New("ConPTY needs Windows 10 1809 or newer")
	}
	opts := []conpty.ConPtyOption{conpty.ConPtyDimensions(cols, rows), conpty.ConPtyEnv(append(os.Environ(), env...))}
	if cwd != "" {
		opts = append(opts, conpty.ConPtyWorkDir(cwd))
	}
	c, err := conpty.Start(cmd, opts...)
	if err != nil {
		return nil, err
	}
	p := &winPty{c: c}
	go func() { c.Wait(context.Background()); p.Close() }()
	return p, nil
}

// winPty closes its ConPTY exactly once: kill and the exit watcher both close it, and a second close would free
// Windows handles that may already belong to something else (it took the keeper down once).
type winPty struct {
	c    *conpty.ConPty
	once sync.Once
	err  error
}

func (p *winPty) Read(b []byte) (int, error)  { return p.c.Read(b) }
func (p *winPty) Write(b []byte) (int, error) { return p.c.Write(b) }
func (p *winPty) Resize(c, r int) error       { return p.c.Resize(c, r) }
func (p *winPty) Pid() int                    { return p.c.Pid() }
func (p *winPty) Close() error {
	p.once.Do(func() { p.err = p.c.Close() })
	return p.err
}
