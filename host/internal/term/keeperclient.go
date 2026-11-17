package term

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/levoniust/goblin/host/internal/core"
)

// KeeperBackend: chats live in the goblin keeper (Windows). goblin restarts never end them.
type KeeperBackend struct {
	Home string
	Port int
	c    *http.Client
}

func NewKeeperBackend(home string, port int) *KeeperBackend {
	return &KeeperBackend{Home: home, Port: port, c: &http.Client{Timeout: 10 * time.Second}}
}

func (k *KeeperBackend) key() string {
	b, _ := os.ReadFile(keeperKeyPath(k.Home))
	return strings.TrimSpace(string(b))
}

func (k *KeeperBackend) base() string { return "http://127.0.0.1:" + strconv.Itoa(k.Port) }

func (k *KeeperBackend) call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, k.base()+path, rd)
	req.Header.Set("X-Keeper-Key", k.key())
	resp, err := k.c.Do(req)
	if err != nil {
		return fmt.Errorf("keeper not running: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("keeper %s: %d %s", path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (k *KeeperBackend) Ensure() error { return k.call("GET", "/windows", nil, nil) }

func (k *KeeperBackend) Windows() ([]Window, error) {
	var ws []Window
	if err := k.call("GET", "/windows", nil, &ws); err != nil {
		return []Window{}, err
	}
	return ws, nil
}

func (k *KeeperBackend) NewWindow(name, cwd, command string) (string, string, error) {
	var r struct{ ID, Pane string }
	err := k.call("POST", "/new", map[string]any{"name": name, "cwd": cwd, "command": command, "cols": 120, "rows": 40}, &r)
	return r.ID, r.Pane, err
}

func (k *KeeperBackend) Rename(wid, name string) error {
	return k.call("POST", "/rename", map[string]string{"id": wid, "name": name}, nil)
}
func (k *KeeperBackend) Kill(wid string) error {
	return k.call("POST", "/kill", map[string]string{"id": wid}, nil)
}

func (k *KeeperBackend) SendKeys(pane string, keys []string, literal string) error {
	return k.call("POST", "/keys", map[string]any{"pane": pane, "keys": keys, "literal": literal}, nil)
}

func (k *KeeperBackend) snapshot(pane string, lines int, plain bool) ([]string, error) {
	var out []string
	q := url.Values{"pane": {pane}, "lines": {strconv.Itoa(lines)}}
	if plain {
		q.Set("plain", "1")
	}
	err := k.call("GET", "/snapshot?"+q.Encode(), nil, &out)
	return out, err
}

func (k *KeeperBackend) Capture(pane string, lines int) (string, error) {
	l, err := k.snapshot(pane, lines, true)
	return strings.Join(l, "\n"), err
}

func (k *KeeperBackend) Snapshot(pane string, lines int) ([]string, error) {
	return k.snapshot(pane, lines, false)
}

func (k *KeeperBackend) Info(pane string) (*PaneInfo, error) {
	var i PaneInfo
	if err := k.call("GET", "/info?pane="+url.QueryEscape(pane), nil, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// CommandFor on Windows: PowerShell, with the launcher's variables set inside it.
func (k *KeeperBackend) CommandFor(l core.Launcher, resume bool) string {
	cmd := l.Command
	if resume && l.Resume != "" {
		cmd = l.Resume
	}
	if cmd == "" {
		return "powershell.exe -NoLogo"
	}
	env := "$env:GOBLIN_KIND='" + l.Kind + "'; "
	if l.Claude {
		env += "$env:CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN='1'; "
	}
	return `powershell.exe -NoLogo -NoExit -Command "` + env + cmd + `"`
}

func (k *KeeperBackend) DetectKind(pid int, ls []core.Launcher) string { return "shell" }
func (k *KeeperBackend) Cleanup()                                      {}

func (k *KeeperBackend) Attach(w Window, mode string, cols, rows int) (Attachment, error) {
	ctx, cancel := context.WithCancel(context.Background())
	u := "ws://127.0.0.1:" + strconv.Itoa(k.Port) + "/attach?pane=" + url.QueryEscape(w.Pane) + "&key=" + url.QueryEscape(k.key())
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	c.SetReadLimit(8 << 20)
	a := &keeperAttach{c: c, ctx: ctx, cancel: cancel, live: mode == "live"}
	if a.live {
		a.Resize(cols, rows)
	}
	return a, nil
}

type keeperAttach struct {
	c      *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	live   bool
	once   sync.Once
}

func (a *keeperAttach) Read() ([]byte, error) {
	for {
		typ, data, err := a.c.Read(a.ctx)
		if err != nil {
			return nil, io.EOF
		}
		if typ == websocket.MessageBinary {
			return data, nil
		}
	}
}

func (a *keeperAttach) Write(b []byte) error { return a.c.Write(a.ctx, websocket.MessageBinary, b) }

func (a *keeperAttach) Resize(cols, rows int) {
	if !a.live {
		return // fixed/reader never resize the real window
	}
	b, _ := json.Marshal(map[string]any{"resize": []int{cols, rows}})
	a.c.Write(a.ctx, websocket.MessageText, b)
}

func (a *keeperAttach) Close() error {
	a.once.Do(func() { a.c.Close(websocket.StatusNormalClosure, ""); a.cancel() })
	return nil
}
