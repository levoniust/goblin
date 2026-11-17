package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/levoniust/goblin/host/internal/term"
)

// Chat status and alerts (2026-11-15/16: notifications + a real working/idle status). Harnesses report through
// `goblin hook` (Claude Code hooks, Pi/Kilo extensions, anything else) to the local page port; this keeps each chat's
// status (working, permission, question, done) and a short alert history the app shows and notifies about.

// HookEvent is what `goblin hook` sends, already normalised to one of: working, permission, question, done, ended.
type HookEvent struct {
	Chat    string        `json:"chat"` // pane id: tmux %N or the Windows keeper's %N
	Kind    string        `json:"kind"` // launcher kind (GOBLIN_KIND)
	Harness string        `json:"harness"`
	Event   string        `json:"event"`
	Title   string        `json:"title,omitempty"`
	Detail  string        `json:"detail,omitempty"`
	Options []AlertOption `json:"options,omitempty"`
	PIDs    []int         `json:"pids,omitempty"` // the hook's process and its parents, when Chat is unknown
}

type AlertOption struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// Alert matches the app's Alert (Switchboard 1.x shape): type permission | question | done.
type Alert struct {
	ID       int           `json:"id"`
	TS       float64       `json:"ts"` // seconds
	Pane     string        `json:"pane"`
	Window   string        `json:"window,omitempty"`
	Name     string        `json:"session_name,omitempty"`
	Kind     string        `json:"kind,omitempty"`
	Type     string        `json:"type"`
	Title    string        `json:"title"`
	Body     string        `json:"body"`
	State    string        `json:"state"` // open | answered | closed
	Answered string        `json:"answered,omitempty"`
	Options  []AlertOption `json:"options"`
	Host     string        `json:"host"`
}

type chatState struct {
	Status string // working | permission | question | done
	Last   string
	Alert  int // open alert id, 0 = none
	At     time.Time
}

type alertStore struct {
	mu     sync.Mutex
	path   string
	next   int
	list   []*Alert // newest last, capped
	chats  map[string]*chatState
	keep   int
	onSave func()
}

func newAlertStore(home string) *alertStore {
	a := &alertStore{path: filepath.Join(home, "alerts.json"), chats: map[string]*chatState{}, keep: 200}
	if b, err := os.ReadFile(a.path); err == nil {
		json.Unmarshal(b, &a.list)
		for _, x := range a.list {
			if x.ID > a.next {
				a.next = x.ID
			}
			if x.State == "open" { // after a restart nobody is waiting on these any more
				x.State = "closed"
			}
		}
	}
	return a
}

func (a *alertStore) saveLocked() {
	b, _ := json.Marshal(a.list)
	tmp := a.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, a.path)
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// apply records one hook event; it returns the new alert (if any) and whether anything changed.
func (a *alertStore) apply(e HookEvent, w term.Window, host string) (*Alert, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cs := a.chats[e.Chat]
	if cs == nil {
		cs = &chatState{}
		a.chats[e.Chat] = cs
	}
	closeOpen := func() {
		if cs.Alert == 0 {
			return
		}
		for _, x := range a.list {
			if x.ID == cs.Alert && x.State == "open" {
				x.State = "closed"
			}
		}
		cs.Alert = 0
	}
	before := *cs
	cs.At = time.Now()
	switch e.Event {
	case "working":
		closeOpen()
		cs.Status = "working"
		if e.Detail != "" {
			cs.Last = clip(e.Detail, 160)
		}
		return nil, before.Status != cs.Status || before.Alert != cs.Alert
	case "ended":
		closeOpen()
		delete(a.chats, e.Chat)
		a.saveLocked()
		return nil, true
	case "permission", "question", "done":
	default:
		return nil, false
	}
	if e.Event == cs.Status && (cs.Alert != 0 || e.Event == "done") { // reported twice (hook + notification)
		return nil, false
	}
	closeOpen()
	a.next++
	al := &Alert{ID: a.next, TS: float64(time.Now().UnixMilli()) / 1000, Pane: e.Chat, Window: w.ID, Name: w.Name, Kind: e.Kind,
		Type: e.Event, Title: clip(e.Title, 120), Body: clip(e.Detail, 900), State: "open", Options: e.Options, Host: host}
	if al.Options == nil {
		al.Options = []AlertOption{}
	}
	if al.Title == "" {
		al.Title = map[string]string{"permission": "Needs permission", "question": "Has a question", "done": "Finished"}[e.Event]
	}
	cs.Status, cs.Alert = e.Event, al.ID
	if e.Event == "done" {
		cs.Alert = 0 // nothing to answer: a finished chat has no open alert
		al.State = "closed"
	}
	if al.Body != "" {
		cs.Last = clip(al.Body, 160)
	}
	a.list = append(a.list, al)
	if len(a.list) > a.keep {
		a.list = a.list[len(a.list)-a.keep:]
	}
	a.saveLocked()
	return al, true
}

func (a *alertStore) get(id int) *Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.list {
		if x.ID == id {
			c := *x
			return &c
		}
	}
	return nil
}

func (a *alertStore) all() []Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Alert, 0, len(a.list))
	for i := len(a.list) - 1; i >= 0; i-- { // newest first
		out = append(out, *a.list[i])
	}
	return out
}

// view: the status fields of a chat for /api/sessions (status, last, pending alert).
func (a *alertStore) view(pane string) (status, last string, pending *Alert) {
	a.mu.Lock()
	cs := a.chats[pane]
	if cs == nil {
		a.mu.Unlock()
		return "", "", nil
	}
	status, last, id := cs.Status, cs.Last, cs.Alert
	a.mu.Unlock()
	if id != 0 {
		pending = a.get(id)
	}
	return status, last, pending
}

// forget drops chats that no longer exist.
func (a *alertStore) forget(alive map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for p := range a.chats {
		if !alive[p] {
			delete(a.chats, p)
		}
	}
}

func (a *alertStore) markAnswered(id int, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.list {
		if x.ID == id {
			x.State, x.Answered = "answered", key
		}
	}
	for _, cs := range a.chats {
		if cs.Alert == id {
			cs.Alert, cs.Status = 0, "working"
		}
	}
	a.saveLocked()
}

// ---- HTTP

// hook: POST /hook on the local page port, header X-Goblin-Hook = the ui.token (only this computer's user can read it).
func (u *uiServer) hook(w http.ResponseWriter, r *http.Request) {
	if !u.hostOK(r) || !u.tokenOK(r.Header.Get("X-Goblin-Hook")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var e HookEvent
	if err := readBody(r, &e); err != nil || (e.Chat == "" && len(e.PIDs) == 0) {
		http.Error(w, "bad hook event", 400)
		return
	}
	u.s.hookEvent(e)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) hookEvent(e HookEvent) {
	var win term.Window
	if ws, err := s.term.Windows(); err == nil {
		found := false
		for _, x := range ws {
			if x.Pane == e.Chat || (e.Chat == "" && x.PID > 0 && slices.Contains(e.PIDs, x.PID)) {
				win, found = x, true
				e.Chat = x.Pane
			}
		}
		if !found {
			return // not one of this host's chats
		}
	}
	al, changed := s.alerts.apply(e, win, s.id.ID)
	if al != nil {
		s.ev.broadcast(map[string]any{"type": "alert", "alert": al})
	}
	if changed {
		s.ev.broadcast(map[string]any{"type": "status", "pane": e.Chat})
	}
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.alerts.all()) }

func (s *Server) getAlert(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("aid"))
	if a := s.alerts.get(id); a != nil {
		writeJSON(w, a)
		return
	}
	http.NotFound(w, r)
}

// answerAlert presses the chosen key in the chat (a numbered choice in the harness's prompt).
func (s *Server) answerAlert(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("aid"))
	var in struct {
		Key string `json:"key"`
	}
	readBody(r, &in)
	a := s.alerts.get(id)
	if a == nil || a.State != "open" || in.Key == "" || len(in.Key) > 8 {
		http.Error(w, "nothing to answer", http.StatusConflict)
		return
	}
	if err := s.term.SendKeys(a.Pane, nil, in.Key); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.alerts.markAnswered(id, in.Key)
	s.ev.broadcast(map[string]any{"type": "status", "pane": a.Pane})
	writeJSON(w, map[string]any{"ok": true, "answered": in.Key})
}
