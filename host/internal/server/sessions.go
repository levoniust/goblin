package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
	"github.com/levoniust/goblin/host/internal/term"
)

// metaStore remembers how each chat was started, so a reboot can offer Resume (the design rules 8).
type Meta struct {
	Kind    string `json:"kind"`
	Cwd     string `json:"cwd"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

type metaStore struct {
	mu     sync.Mutex
	path   string
	Live   map[string]Meta `json:"live"`   // pane -> how it started
	Resume []Meta          `json:"resume"` // chats that were open before the last reboot
}

func loadMeta(path string) *metaStore {
	m := &metaStore{path: path, Live: map[string]Meta{}}
	core.ReadJSON(path, m)
	if m.Live == nil {
		m.Live = map[string]Meta{}
	}
	return m
}

func (m *metaStore) save() { core.WriteJSON(m.path, m) }

// prepareSessions: if the chats are gone (reboot), what was open becomes the Resume list; then make sure the
// session exists and drop leftover view sessions.
func (s *Server) prepareSessions() {
	ws, _ := s.term.Windows()
	s.meta.mu.Lock()
	if len(ws) == 0 && len(s.meta.Live) > 0 {
		for _, m := range s.meta.Live {
			if m.Kind != "shell" {
				s.meta.Resume = append(s.meta.Resume, m)
			}
		}
		sort.Slice(s.meta.Resume, func(i, j int) bool { return s.meta.Resume[i].Created < s.meta.Resume[j].Created })
		if len(s.meta.Resume) > 20 {
			s.meta.Resume = s.meta.Resume[len(s.meta.Resume)-20:]
		}
		s.meta.Live = map[string]Meta{}
		log.Printf("%d chats from before the restart can be resumed", len(s.meta.Resume))
	}
	s.meta.save()
	s.meta.mu.Unlock()
	if err := s.term.Ensure(); err != nil {
		log.Printf("sessions: %v", err)
	}
	s.term.Cleanup()
}

func (s *Server) launcher(kind string) (core.Launcher, bool) {
	for _, l := range s.cfg.Live().Launchers {
		if l.Kind == kind {
			return l, true
		}
	}
	return core.Launcher{}, false
}

func (s *Server) windowFor(sid string) (term.Window, bool) {
	ws, _ := s.term.Windows()
	for _, w := range ws {
		if w.ID == sid || w.Pane == sid || strconv.Itoa(w.Index) == sid {
			return w, true
		}
	}
	return term.Window{}, false
}

func (s *Server) sessionView(w term.Window) map[string]any {
	s.meta.mu.Lock()
	m, ok := s.meta.Live[w.Pane]
	s.meta.mu.Unlock()
	kind := m.Kind
	if !ok || kind == "" {
		kind = s.term.DetectKind(w.PID, s.cfg.Live().Launchers)
	}
	status, last, pending := s.alerts.view(w.Pane)
	if status == "" {
		status = "idle"
		if kind == "shell" && busyShell(w.Command) { // a plain shell running a program
			status = "working"
		}
	}
	var pend any
	if pending != nil {
		pend = pending
	}
	return map[string]any{"id": w.ID, "index": w.Index, "name": w.Name, "pane": w.Pane, "cwd": w.Cwd, "command": w.Command,
		"kind": kind, "model": nil, "status": status, "last": last, "activity": w.Activity, "updated": nil, "pending": pend,
		"host": s.id.ID}
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	ws, _ := s.term.Windows()
	out := make([]map[string]any, 0, len(ws))
	alive := map[string]bool{}
	for _, x := range ws {
		out = append(out, s.sessionView(x))
		alive[x.Pane] = true
	}
	s.alerts.forget(alive)
	writeJSON(w, out)
}

// busyShell: the program in front of a plain shell chat is not the shell itself (tmux pane_current_command;
// the Windows keeper reports the starting command, so Windows shells stay idle).
func busyShell(cmd string) bool {
	c := strings.ToLower(filepath.Base(strings.TrimSpace(cmd)))
	c = strings.TrimSuffix(c, ".exe")
	switch c {
	case "", ".", "bash", "zsh", "sh", "fish", "dash", "powershell", "pwsh", "cmd", "tmux", "login":
		return false
	}
	return !strings.HasPrefix(c, "powershell.exe") && !strings.HasPrefix(c, "env ")
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (s *Server) safePath(p string) (string, bool) {
	if p == "" {
		p = s.cfg.Live().Roots[0]
	}
	if strings.HasPrefix(p, "~") {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, p[1:])
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	for _, root := range s.cfg.Live().Roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if inside(real, rr) {
			return real, true
		}
	}
	return "", false
}

func inside(p, root string) bool {
	np, nr := filepath.Clean(p), filepath.Clean(root)
	if os.PathSeparator == '\\' {
		np, nr = strings.ToLower(np), strings.ToLower(nr)
	}
	return np == nr || strings.HasPrefix(np, strings.TrimRight(nr, string(os.PathSeparator))+string(os.PathSeparator))
}

func (s *Server) start(kind, cwd, name string, resume bool) (string, string, error) {
	l, _ := s.launcher(kind)
	wid, pane, err := s.term.NewWindow(name, cwd, s.term.CommandFor(l, resume))
	if err != nil {
		return "", "", err
	}
	s.meta.mu.Lock()
	s.meta.Live[pane] = Meta{Kind: kind, Cwd: cwd, Name: name, Created: time.Now().UnixMilli()}
	s.meta.save()
	s.meta.mu.Unlock()
	s.ev.broadcast(map[string]any{"type": "sessions"})
	return wid, pane, nil
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string `json:"kind"`
		Cwd  string `json:"cwd"`
		Name string `json:"name"`
	}
	if readBody(r, &in) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if in.Kind == "" {
		in.Kind = "claude"
	}
	l, ok := s.launcher(in.Kind)
	if !ok {
		http.Error(w, "unknown kind", 400)
		return
	}
	cwd, ok := s.safePath(in.Cwd)
	if !ok {
		http.Error(w, "outside the allowed folders", 403)
		return
	}
	name := core.CleanName(in.Name)
	if name == "" {
		name = l.Name
	}
	wid, pane, err := s.start(l.Kind, cwd, name, false)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.lowerChat(pane)
	writeJSON(w, map[string]any{"id": wid, "pane": pane})
}

func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowFor(r.PathValue("sid"))
	var in struct {
		Name string `json:"name"`
	}
	if !ok || readBody(r, &in) != nil {
		http.Error(w, "no such session", 404)
		return
	}
	s.term.Rename(win.ID, core.CleanName(in.Name))
	s.ev.broadcast(map[string]any{"type": "sessions"})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) killSession(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowFor(r.PathValue("sid"))
	if !ok {
		http.Error(w, "no such session", 404)
		return
	}
	s.term.Kill(win.ID)
	s.meta.mu.Lock()
	delete(s.meta.Live, win.Pane)
	s.meta.save()
	s.meta.mu.Unlock()
	s.ev.broadcast(map[string]any{"type": "sessions"})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) sendKeys(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowFor(r.PathValue("sid"))
	var in struct {
		Keys []string `json:"keys"`
		Text string   `json:"text"`
	}
	if !ok || readBody(r, &in) != nil {
		http.Error(w, "no such session", 404)
		return
	}
	var keys []string
	for _, k := range in.Keys {
		if len(k) > 0 && len(k) < 20 {
			keys = append(keys, k)
		}
	}
	if err := s.term.SendKeys(win.Pane, keys, in.Text); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) screen(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowFor(r.PathValue("sid"))
	if !ok {
		http.Error(w, "no such session", 404)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 || n > 10000 {
		n = 60
	}
	text, _ := s.term.Capture(win.Pane, n)
	opts := [][2]string{}
	opts = append(opts, term.ParseOptions(text)...)
	writeJSON(w, map[string]any{"text": text, "options": opts})
}

func (s *Server) listResume(w http.ResponseWriter, r *http.Request) {
	s.meta.mu.Lock()
	defer s.meta.mu.Unlock()
	out := make([]map[string]any, 0, len(s.meta.Resume))
	for i, m := range s.meta.Resume {
		out = append(out, map[string]any{"index": i, "kind": m.Kind, "cwd": m.Cwd, "name": m.Name, "created": m.Created})
	}
	writeJSON(w, out)
}

// resume restarts chats from before the reboot with each launcher's resume command. {"index": n} or {"all": true};
// {"dismiss": true} forgets the list.
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Index   *int `json:"index"`
		All     bool `json:"all"`
		Dismiss bool `json:"dismiss"`
	}
	if readBody(r, &in) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.meta.mu.Lock()
	var pick []Meta
	var keep []Meta
	for i, m := range s.meta.Resume {
		if in.All || in.Dismiss || (in.Index != nil && *in.Index == i) {
			pick = append(pick, m)
		} else {
			keep = append(keep, m)
		}
	}
	s.meta.Resume = keep
	s.meta.save()
	s.meta.mu.Unlock()
	started := 0
	if !in.Dismiss {
		for _, m := range pick {
			cwd, ok := s.safePath(m.Cwd)
			if !ok {
				cwd = s.cfg.Live().Roots[0]
			}
			if _, _, err := s.start(m.Kind, cwd, m.Name, true); err == nil {
				started++
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true, "started": started})
}

func (s *Server) listDir(w http.ResponseWriter, r *http.Request) {
	p, ok := s.safePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "outside the allowed folders", 403)
		return
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir() != ents[j].IsDir() {
			return ents[i].IsDir()
		}
		return strings.ToLower(ents[i].Name()) < strings.ToLower(ents[j].Name())
	})
	items := []map[string]any{}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		items = append(items, map[string]any{"name": e.Name(), "dir": e.IsDir()})
		if len(items) >= 500 {
			break
		}
	}
	writeJSON(w, map[string]any{"path": p, "roots": s.cfg.Live().Roots, "items": items})
}

// lowerChat gives a chat (and what it runs) the chat_priority from goblin.yaml.
func (s *Server) lowerChat(pane string) {
	ws, _ := s.term.Windows()
	for _, w := range ws {
		if (pane == "" || w.Pane == pane) && w.PID > 0 {
			term.LowerPriority(w.PID, s.cfg.ChatPriority)
		}
	}
}

// LowerAllChats: at startup, chats that were already running get the chat_priority too.
func (s *Server) LowerAllChats() { s.lowerChat("") }
