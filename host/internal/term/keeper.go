package term

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/coder/websocket"
	"github.com/levoniust/goblin/host/internal/core"
)

// The keeper holds chats on machines without tmux (Windows): one long-lived process with a pseudo-terminal per
// chat (ConPTY), a screen emulator with scrollback, and a replay buffer for new viewers. `goblin run` talks to it
// on 127.0.0.1 with a key, so goblin can restart or update without ending chats. Keep it small and stable:
// updating the keeper ends open chats.

// ptyProc is a program running on a pseudo-terminal (ConPTY on Windows, a PTY elsewhere).
type ptyProc interface {
	io.ReadWriter
	Resize(cols, rows int) error
	Pid() int
	Close() error
}

const (
	keeperScrollback = 50000
	replayMax        = 512 << 10
)

type ksess struct {
	mu       sync.Mutex
	idx      int
	id, pane string
	name     string
	cwd      string
	command  string
	proc     ptyProc
	emu      *vt.SafeEmulator
	replay   []byte
	viewers  map[chan []byte]bool
	activity int64
	w, h     int
	dead     bool
}

type Keeper struct {
	mu    sync.Mutex
	key   string
	next  int
	sess  map[string]*ksess // by window id
	start func(cmd, cwd string, env []string, cols, rows int) (ptyProc, error)
}

func keeperKeyPath(home string) string { return filepath.Join(home, "keeper.key") }

// RunKeeper serves the keeper API until ctx ends.
func RunKeeper(ctx context.Context, home string, port int) error {
	kb := make([]byte, 24)
	rand.Read(kb)
	k := &Keeper{key: hex.EncodeToString(kb), sess: map[string]*ksess{}, start: startPty}
	// take the port first: a second keeper (or a stray start) must not overwrite the running keeper's key
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("keeper port %d: %w", port, err)
	}
	if err := core.WritePrivate(keeperKeyPath(home), []byte(k.key)); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: k.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Printf("goblin keeper on 127.0.0.1:%d", port)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (k *Keeper) Handler() http.Handler {
	mx := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Keeper-Key")
			if got == "" {
				got = r.URL.Query().Get("key")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(k.key)) != 1 {
				http.Error(w, "no", 401)
				return
			}
			h(w, r)
		}
	}
	mx.HandleFunc("GET /windows", auth(func(w http.ResponseWriter, r *http.Request) { kjson(w, k.windows()) }))
	mx.HandleFunc("POST /new", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name, Cwd, Command string
			Env                []string
			Cols, Rows         int
		}
		json.NewDecoder(r.Body).Decode(&in)
		wid, pane, err := k.newWindow(in.Name, in.Cwd, in.Command, in.Env, in.Cols, in.Rows)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		kjson(w, map[string]string{"id": wid, "pane": pane})
	}))
	mx.HandleFunc("POST /rename", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID, Name string }
		json.NewDecoder(r.Body).Decode(&in)
		if s := k.get(in.ID); s != nil {
			s.mu.Lock()
			s.name = in.Name
			s.mu.Unlock()
		}
		kjson(w, map[string]bool{"ok": true})
	}))
	mx.HandleFunc("POST /kill", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID string }
		json.NewDecoder(r.Body).Decode(&in)
		k.kill(in.ID)
		kjson(w, map[string]bool{"ok": true})
	}))
	mx.HandleFunc("POST /keys", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Pane    string
			Keys    []string
			Literal string
		}
		json.NewDecoder(r.Body).Decode(&in)
		s := k.get(in.Pane)
		if s == nil {
			http.Error(w, "no such pane", 404)
			return
		}
		if in.Literal != "" {
			s.proc.Write([]byte(in.Literal))
		}
		for _, key := range in.Keys {
			s.proc.Write(KeyBytes(key))
		}
		kjson(w, map[string]bool{"ok": true})
	}))
	mx.HandleFunc("GET /info", auth(func(w http.ResponseWriter, r *http.Request) {
		s := k.get(r.URL.Query().Get("pane"))
		if s == nil {
			http.Error(w, "no such pane", 404)
			return
		}
		kjson(w, s.info())
	}))
	mx.HandleFunc("GET /snapshot", auth(func(w http.ResponseWriter, r *http.Request) {
		s := k.get(r.URL.Query().Get("pane"))
		if s == nil {
			http.Error(w, "no such pane", 404)
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		kjson(w, s.snapshot(n, r.URL.Query().Get("plain") == "1"))
	}))
	mx.HandleFunc("GET /attach", auth(k.attach))
	return mx
}

func kjson(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (k *Keeper) get(idOrPane string) *ksess {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s, ok := k.sess[idOrPane]; ok {
		return s
	}
	for _, s := range k.sess {
		if s.pane == idOrPane || strconv.Itoa(s.idx) == idOrPane {
			return s
		}
	}
	return nil
}

func (k *Keeper) windows() []Window {
	k.mu.Lock()
	list := make([]*ksess, 0, len(k.sess))
	for _, s := range k.sess {
		list = append(list, s)
	}
	k.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].idx < list[j].idx })
	out := []Window{}
	for _, s := range list {
		s.mu.Lock()
		out = append(out, Window{ID: s.id, Index: s.idx, Name: s.name, Pane: s.pane, PID: s.proc.Pid(), Cwd: s.cwd,
			Command: s.command, Activity: s.activity, Dead: s.dead})
		s.mu.Unlock()
	}
	return out
}

func (k *Keeper) newWindow(name, cwd, command string, env []string, cols, rows int) (string, string, error) {
	if cols <= 0 {
		cols, rows = 120, 40
	}
	// the chat's number is taken before it starts, so it can know its own id (GOBLIN_CHAT, used by `goblin hook`)
	k.mu.Lock()
	k.next++
	n := k.next
	k.mu.Unlock()
	env = append(append([]string{}, env...), "GOBLIN_CHAT=%"+strconv.Itoa(n))
	p, err := k.start(command, cwd, env, cols, rows)
	if err != nil {
		return "", "", err
	}
	k.mu.Lock()
	s := &ksess{idx: n, id: "@" + strconv.Itoa(n), pane: "%" + strconv.Itoa(n), name: name, cwd: cwd, command: command,
		proc: p, emu: vt.NewSafeEmulator(cols, rows), viewers: map[chan []byte]bool{}, activity: time.Now().Unix(), w: cols, h: rows}
	s.emu.SetScrollbackSize(keeperScrollback)
	k.sess[s.id] = s
	k.mu.Unlock()
	go s.pumpOut(func() { k.remove(s.id) })
	go s.pumpReplies()
	return s.id, s.pane, nil
}

func (k *Keeper) kill(id string) {
	if s := k.get(id); s != nil {
		s.proc.Close()
		k.remove(s.id)
	}
}

func (k *Keeper) remove(id string) {
	k.mu.Lock()
	s := k.sess[id]
	delete(k.sess, id)
	k.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		s.dead = true
		for ch := range s.viewers {
			close(ch)
		}
		s.viewers = map[chan []byte]bool{}
		s.mu.Unlock()
	}
}

// pumpOut feeds program output to the emulator, the replay buffer and every viewer.
func (s *ksess) pumpOut(done func()) {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.proc.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			s.emu.Write(data)
			s.mu.Lock()
			s.activity = time.Now().Unix()
			s.replay = append(s.replay, data...)
			if len(s.replay) > replayMax {
				s.replay = append([]byte(nil), s.replay[len(s.replay)-replayMax/2:]...)
			}
			for ch := range s.viewers {
				select {
				case ch <- data:
				default: // a slow viewer drops output; it resyncs from the screen
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			done()
			return
		}
	}
}

// pumpReplies sends the emulator's answers (cursor position reports etc.) back to the program.
func (s *ksess) pumpReplies() {
	buf := make([]byte, 4096)
	for {
		n, err := s.emu.Read(buf)
		if n > 0 {
			s.proc.Write(buf[:n])
		}
		if err != nil {
			return
		}
		s.mu.Lock()
		dead := s.dead
		s.mu.Unlock()
		if dead {
			return
		}
	}
}

func (s *ksess) info() PaneInfo {
	return PaneInfo{W: s.emu.Width(), H: s.emu.Height(), Hist: s.emu.ScrollbackLen(), Alt: s.emu.IsAltScreen()}
}

// snapshot: the last n lines of scrollback + screen, with colours (or plain), trailing blank lines dropped.
func (s *ksess) snapshot(n int, plain bool) []string {
	if n <= 0 {
		n = 2000
	}
	var lines []string
	sb := s.emu.Scrollback()
	if sb != nil {
		all := sb.Lines()
		if len(all) > n {
			all = all[len(all)-n:]
		}
		for _, l := range all {
			if plain {
				lines = append(lines, strings.TrimRight(l.String(), " "))
			} else {
				lines = append(lines, l.Render())
			}
		}
	}
	screen := s.emu.Render()
	for _, l := range strings.Split(screen, "\n") {
		if plain {
			l = strings.TrimRight(stripSGR(l), " ")
		}
		lines = append(lines, strings.TrimRight(l, "\r"))
	}
	for len(lines) > 0 && strings.TrimSpace(stripSGR(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// attach: websocket viewer. Server -> client: replay, then live output (binary). Client -> server: key bytes
// (binary) or {"resize":[c,r]} (text; resizes the chat).
func (k *Keeper) attach(w http.ResponseWriter, r *http.Request) {
	s := k.get(r.URL.Query().Get("pane"))
	if s == nil {
		http.Error(w, "no such pane", 404)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan []byte, 256)
	s.mu.Lock()
	replay := append([]byte(nil), s.replay...)
	s.viewers[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.viewers[ch] {
			delete(s.viewers, ch)
		}
		s.mu.Unlock()
		c.Close(websocket.StatusNormalClosure, "")
	}()
	go func() {
		defer cancel()
		if len(replay) > 0 && c.Write(ctx, websocket.MessageBinary, replay) != nil {
			return
		}
		for data := range ch {
			if c.Write(ctx, websocket.MessageBinary, data) != nil {
				return
			}
		}
	}()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			s.proc.Write(data)
			continue
		}
		var m struct {
			Resize []int `json:"resize"`
		}
		if json.Unmarshal(data, &m) == nil && len(m.Resize) == 2 && m.Resize[0] > 0 && m.Resize[1] > 0 {
			s.proc.Resize(m.Resize[0], m.Resize[1])
			s.emu.Resize(m.Resize[0], m.Resize[1])
		}
	}
}

// KeyBytes turns tmux key names (what the app sends) into terminal input.
func KeyBytes(k string) []byte {
	switch k {
	case "Enter":
		return []byte("\r")
	case "Escape":
		return []byte("\x1b")
	case "Tab":
		return []byte("\t")
	case "BTab":
		return []byte("\x1b[Z")
	case "BSpace":
		return []byte("\x7f")
	case "Space":
		return []byte(" ")
	case "Up":
		return []byte("\x1b[A")
	case "Down":
		return []byte("\x1b[B")
	case "Right":
		return []byte("\x1b[C")
	case "Left":
		return []byte("\x1b[D")
	case "Home":
		return []byte("\x1b[H")
	case "End":
		return []byte("\x1b[F")
	case "PageUp", "PPage":
		return []byte("\x1b[5~")
	case "PageDown", "NPage":
		return []byte("\x1b[6~")
	case "DC", "Delete":
		return []byte("\x1b[3~")
	}
	if len(k) == 3 && (strings.HasPrefix(k, "C-") || strings.HasPrefix(k, "c-")) {
		c := k[2] | 0x20
		if c >= 'a' && c <= 'z' {
			return []byte{c - 'a' + 1}
		}
	}
	if strings.HasPrefix(k, "M-") && len(k) > 2 {
		return append([]byte{0x1b}, k[2:]...)
	}
	if len(k) == 1 {
		return []byte(k)
	}
	return nil
}
