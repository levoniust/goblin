package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
	"rsc.io/qr"
)

// The local page: what this node is doing, for whoever is logged in to this computer. It listens on 127.0.0.1 only and
// needs the token in <home>/ui.token (readable by this user only); `goblin ui` opens the browser with it. Approving
// and removing members also need the Goblin PIN (the design rules 7: local access = computer login + PIN).

//go:embed ui/index.html
var uiFiles embed.FS

const uiCookie = "goblin_ui"

var started = time.Now()

func UITokenPath(home string) string { return filepath.Join(home, "ui.token") }

// UIToken returns this node's local-page token, creating it the first time.
func UIToken(home string) (string, error) {
	if b, err := os.ReadFile(UITokenPath(home)); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	t := hex.EncodeToString(b)
	return t, core.WritePrivate(UITokenPath(home), []byte(t))
}

type uiServer struct {
	s      *Server
	token  string
	port   int
	mu     sync.Mutex
	pinBad []time.Time
}

// ServeUI runs the local page until ctx ends. A port already in use is logged, not fatal.
func (s *Server) ServeUI(ctx context.Context) {
	tok, err := UIToken(s.home)
	if err != nil {
		log.Printf("local page: %v", err)
		return
	}
	u := &uiServer{s: s, token: tok, port: s.cfg.UIPort}
	mx := http.NewServeMux()
	mx.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui", http.StatusFound) })
	mx.HandleFunc("GET /ui", u.page)
	mx.HandleFunc("GET /ui/api/state", u.auth(u.state))
	mx.HandleFunc("GET /ui/api/log", u.auth(u.logTail))
	mx.HandleFunc("GET /ui/api/screen/{sid}", u.auth(u.screen))
	mx.HandleFunc("GET /ui/api/config", u.auth(u.getConfig))
	mx.HandleFunc("POST /ui/api/config/check", u.auth(u.checkConfig))
	mx.HandleFunc("POST /ui/api/config", u.auth(u.saveConfig))
	mx.HandleFunc("POST /ui/api/test", u.auth(func(w http.ResponseWriter, r *http.Request) {
		s.mesh.RequestTest()
		writeJSON(w, map[string]any{"ok": true})
	}))
	mx.HandleFunc("POST /ui/api/member", u.auth(u.member))
	mx.HandleFunc("POST /ui/api/pair", u.auth(u.pair))
	mx.HandleFunc("POST /hook", u.hook)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(u.port))
	if err != nil {
		log.Printf("local page not available: %v (change ui_port in goblin.yaml)", err)
		return
	}
	srv := &http.Server{Handler: mx, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("local page on http://127.0.0.1:%d/ui (open it with `goblin ui`)", u.port)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("local page: %v", err)
	}
}

// hostOK blocks DNS-rebinding: only requests addressed to this loopback port are served.
func (u *uiServer) hostOK(r *http.Request) bool {
	p := strconv.Itoa(u.port)
	return r.Host == "127.0.0.1:"+p || r.Host == "localhost:"+p
}

func (u *uiServer) tokenOK(t string) bool {
	return len(t) == len(u.token) && subtle.ConstantTimeCompare([]byte(t), []byte(u.token)) == 1
}

func (u *uiServer) page(w http.ResponseWriter, r *http.Request) {
	if !u.hostOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if t := r.URL.Query().Get("t"); t != "" {
		if !u.tokenOK(t) {
			http.Error(w, "this link is out of date: run `goblin ui` again", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: u.token, Path: "/ui", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/ui", http.StatusFound) // the token leaves the address bar
		return
	}
	if c, err := r.Cookie(uiCookie); err != nil || !u.tokenOK(c.Value) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("<!doctype html><meta charset=utf-8><title>Goblin</title><body style=\"font-family:sans-serif;padding:2em\">" +
			"<h1>Goblin</h1><p>Open this page with <code>goblin ui</code> (or the Goblin shortcut) on this computer.</p>"))
		return
	}
	b, _ := uiFiles.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
	w.Write(b)
}

// auth: cookie from `goblin ui`, our own host name, and for changes a header other web pages cannot send.
func (u *uiServer) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(uiCookie)
		if !u.hostOK(r) || err != nil || !u.tokenOK(c.Value) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Goblin-UI") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

func (u *uiServer) state(w http.ResponseWriter, r *http.Request) {
	s := u.s
	ws, _ := s.term.Windows()
	chats := make([]map[string]any, 0, len(ws))
	for _, x := range ws {
		chats = append(chats, s.sessionView(x))
	}
	live := s.cfg.Live()
	writeJSON(w, map[string]any{
		"node": map[string]any{"name": s.cfg.Name, "id": s.id.ID, "version": core.Version, "api": core.API, "os": runtime.GOOS + "/" + runtime.GOARCH,
			"port": s.cfg.Port, "data": s.home, "config": core.ConfigPath(s.home), "started": started.UnixMilli(), "pin_set": core.HasPIN(s.home)},
		"network": s.mesh.Status(), "chats": chats, "launchers": live.Launchers, "links": live.Links,
		"prefer_hub": live.PreferHub, "never_hub": live.NeverHub,
	})
}

func (u *uiServer) logTail(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"text": core.TailFile(core.LogPath(u.s.home, "goblin.log"), 64<<10)})
}

func (u *uiServer) screen(w http.ResponseWriter, r *http.Request) {
	win, ok := u.s.windowFor(r.PathValue("sid"))
	if !ok {
		http.Error(w, "no such chat", 404)
		return
	}
	text, _ := u.s.term.Capture(win.Pane, 400)
	writeJSON(w, map[string]any{"name": win.Name, "text": text})
}

func (u *uiServer) getConfig(w http.ResponseWriter, r *http.Request) {
	b, _ := os.ReadFile(core.ConfigPath(u.s.home))
	writeJSON(w, map[string]any{"path": core.ConfigPath(u.s.home), "text": string(b)})
}

func (u *uiServer) checkConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if readBody(r, &in) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	writeJSON(w, map[string]any{"problems": core.CheckConfig([]byte(in.Text))})
}

// saveConfig writes goblin.yaml only when it has no problems; the reload watcher then applies it.
func (u *uiServer) saveConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if readBody(r, &in) != nil || len(in.Text) > 256<<10 {
		http.Error(w, "bad request", 400)
		return
	}
	if p := core.CheckConfig([]byte(in.Text)); len(p) > 0 {
		writeJSON(w, map[string]any{"ok": false, "problems": p})
		return
	}
	if err := core.WritePrivate(core.ConfigPath(u.s.home), []byte(in.Text)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	log.Printf("goblin.yaml saved from the local page")
	writeJSON(w, map[string]any{"ok": true})
}

func (u *uiServer) pinOK(pin string) bool {
	u.mu.Lock()
	now := time.Now()
	var recent []time.Time
	for _, t := range u.pinBad {
		if now.Sub(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	u.pinBad = recent
	locked := len(recent) >= 5
	u.mu.Unlock()
	if locked {
		return false
	}
	if core.CheckPIN(u.s.home, pin) {
		return true
	}
	u.mu.Lock()
	u.pinBad = append(u.pinBad, now)
	u.mu.Unlock()
	time.Sleep(time.Second)
	return false
}

// member: approve or dismiss a waiting host/client, or remove a member. All need the PIN.
func (u *uiServer) member(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Op  string `json:"op"` // approve | dismiss | remove
		ID  string `json:"id"`
		PIN string `json:"pin"`
	}
	if readBody(r, &in) != nil || in.ID == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if !u.pinOK(in.PIN) {
		writeJSON(w, map[string]any{"ok": false, "error": "wrong PIN (5 wrong tries lock this for 10 minutes)"})
		return
	}
	s := u.s
	switch in.Op {
	case "approve":
		var p *struct{ Kind, Name, Pub, Pin string }
		for _, x := range s.mesh.Status().Pending {
			if x.ID == in.ID {
				p = &struct{ Kind, Name, Pub, Pin string }{x.Kind, x.Name, x.Pub, x.Pin}
			}
		}
		if p == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "nothing is waiting with that id"})
			return
		}
		e := core.Event{T: "add", Kind: p.Kind, ID: in.ID, Pub: p.Pub, Name: p.Name, Pin: p.Pin, TS: time.Now().UnixMilli(), By: s.id.ID}
		e.Sig = s.id.Sign(e.Text(s.log.NetID()))
		n := s.mesh.MergeEvents([]core.Event{e})
		log.Printf("local page: approved %s %q (%d event)", p.Kind, p.Name, n)
	case "dismiss":
		s.mesh.Dismiss(in.ID)
	case "remove":
		if in.ID == s.id.ID {
			writeJSON(w, map[string]any{"ok": false, "error": "this computer can't remove itself; remove it from another one"})
			return
		}
		m, ok := s.log.Get(in.ID)
		if !ok {
			writeJSON(w, map[string]any{"ok": false, "error": "no such member"})
			return
		}
		tmp := &core.Log{}
		if err := tmp.Adopt(s.log.NetID(), s.log.All()); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		before := len(tmp.All())
		if err := tmp.RemoveMember(s.id, in.ID); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.mesh.MergeEvents(tmp.All()[before:])
		log.Printf("local page: removed %s %q", m.Kind, m.Name)
	default:
		http.Error(w, "bad request", 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// pair: a one-time pairing link + QR for a new phone (the same as `goblin pair`). Needs the PIN.
func (u *uiServer) pair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PIN string `json:"pin"`
	}
	if readBody(r, &in) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if !u.pinOK(in.PIN) {
		writeJSON(w, map[string]any{"ok": false, "error": "wrong PIN (5 wrong tries lock this for 10 minutes)"})
		return
	}
	s := u.s
	link, err := PairLink(s.home, s.cfg, s.id, s.log.NetID())
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := map[string]any{"ok": true, "link": link}
	if c, err := qr.Encode(link, qr.L); err == nil {
		c.Scale = 6
		out["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(c.PNG())
	}
	log.Printf("local page: pairing link made (valid 10 minutes, one use)")
	writeJSON(w, out)
}
