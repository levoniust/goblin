// Package server is the HTTPS API every host serves: the client API (Switchboard proto-2 shapes plus Goblin's
// network endpoints), the host-to-host mesh API and the relay.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
	"github.com/levoniust/goblin/host/internal/mesh"
	"github.com/levoniust/goblin/host/internal/term"
)

type Server struct {
	cfg    *core.Config
	home   string
	id     *core.Identity
	log    *core.Log
	verify *core.Verifier
	mesh   *mesh.Mesh
	term   term.Backend
	mux    *http.ServeMux

	ev       *events
	meta     *metaStore
	pairMu   sync.Mutex
	pairFail []time.Time
	alerts   *alertStore
}

type ctxKey int

const memberKey ctxKey = 1

func New(cfg *core.Config, home string, id *core.Identity, lg *core.Log, m *mesh.Mesh, tb term.Backend) *Server {
	s := &Server{cfg: cfg, home: home, id: id, log: lg, verify: core.NewVerifier(id.ID, lg), mesh: m, term: tb, mux: http.NewServeMux()}
	s.ev = newEvents(s)
	s.meta = loadMeta(filepath.Join(home, "sessions.json"))
	s.alerts = newAlertStore(home)
	m.ClientTester = s.ev.speedTest
	m.OnChange = s.onMeshChange
	s.routes()
	return s
}

// SettingsChanged tells clients to re-read /api/info (launchers, links) and peers to re-read our hub preference.
func (s *Server) SettingsChanged() {
	s.ev.broadcast(map[string]any{"type": "info"})
	s.mesh.HelloNow()
}

func (s *Server) onMeshChange(what string) {
	s.ev.broadcast(map[string]any{"type": "network", "what": what})
	if what == "members" {
		s.ev.dropRemoved()
	}
}

func (s *Server) routes() {
	mx := s.mux
	mx.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"ok": true}) })
	mx.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": s.id.ID, "net": s.log.NetID(), "t": time.Now().UnixMilli()})
	})
	mx.HandleFunc("POST /api/pair", s.pair)
	mx.HandleFunc("POST /api/join", s.join)

	c := s.client
	mx.HandleFunc("GET /api/info", c(s.info))
	mx.HandleFunc("GET /api/speedtest", c(s.speedtest))
	mx.HandleFunc("GET /api/network", c(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mesh.Status()) }))
	mx.HandleFunc("POST /api/network/test", c(func(w http.ResponseWriter, r *http.Request) {
		s.mesh.RequestTest()
		writeJSON(w, map[string]any{"ok": true})
	}))
	mx.HandleFunc("GET /api/log", c(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"net": s.log.NetID(), "events": s.log.All()})
	}))
	mx.HandleFunc("POST /api/log", c(s.postLog))
	mx.HandleFunc("POST /api/pending/{id}/dismiss", c(func(w http.ResponseWriter, r *http.Request) {
		s.mesh.Dismiss(r.PathValue("id"))
		writeJSON(w, map[string]any{"ok": true})
	}))
	mx.HandleFunc("GET /api/launchers", c(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.cfg.Live().Launchers) }))
	mx.HandleFunc("GET /api/sessions", c(s.listSessions))
	mx.HandleFunc("POST /api/sessions", c(s.createSession))
	mx.HandleFunc("POST /api/sessions/{sid}/rename", c(s.renameSession))
	mx.HandleFunc("DELETE /api/sessions/{sid}", c(s.killSession))
	mx.HandleFunc("POST /api/sessions/{sid}/keys", c(s.sendKeys))
	mx.HandleFunc("GET /api/sessions/{sid}/screen", c(s.screen))
	mx.HandleFunc("GET /api/sessions/{sid}/term", c(s.terminal))
	mx.HandleFunc("GET /api/resume", c(s.listResume))
	mx.HandleFunc("POST /api/resume", c(s.resume))
	mx.HandleFunc("GET /api/fs", c(s.listDir))
	mx.HandleFunc("GET /api/events", c(s.ev.serve))
	mx.HandleFunc("POST /api/upload", c(s.upload))
	mx.HandleFunc("GET /api/file", c(s.file))
	mx.HandleFunc("GET /api/alerts", c(s.listAlerts))
	mx.HandleFunc("GET /api/alerts/{aid}", c(s.getAlert))
	mx.HandleFunc("POST /api/alerts/{aid}/answer", c(s.answerAlert))
	mx.HandleFunc("/api/link/{name}/{rest...}", c(s.linkProxy))
	mx.HandleFunc("/relay/{host}/{rest...}", s.relay)

	s.mesh.Mount(mx, func(r *http.Request) (core.Member, error) { return s.verify.Check(r, core.URIWithoutAuth(r)) })
}

// client wraps a handler that only paired clients may call.
func (s *Server) client(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := s.verify.Check(r, core.URIWithoutAuth(r))
		if err != nil {
			code := http.StatusUnauthorized
			if errors.Is(err, core.ErrRevoked) {
				code = http.StatusForbidden // the app wipes its copy when it sees this
			}
			http.Error(w, err.Error(), code)
			return
		}
		if m.Kind != "client" {
			http.Error(w, "clients only", http.StatusForbidden)
			return
		}
		s.mesh.SeenClient(m.ID, s.ev.live(m.ID))
		h(w, r.WithContext(context.WithValue(r.Context(), memberKey, m)))
	}
}

func caller(r *http.Request) core.Member {
	m, _ := r.Context().Value(memberKey).(core.Member)
	return m
}

// Handler is the full HTTP handler (TLS is added by Serve).
func (s *Server) Handler() http.Handler { return s.mux }

// Serve runs HTTPS on the configured port until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	s.prepareSessions()
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(s.cfg.Port),
		Handler:           s.mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{s.id.Cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // TLS probes from scanners are noise
	}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	err := srv.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	kinds := []string{}
	for _, l := range s.cfg.Live().Launchers {
		kinds = append(kinds, l.Kind)
	}
	links := map[string]string{}
	for k, v := range s.cfg.Live().Links {
		links[k] = v
	}
	writeJSON(w, map[string]any{
		"name": s.cfg.Name, "version": core.Version, "node_id": s.id.ID, "host_id": s.id.ID, "net": s.log.NetID(),
		"os": runtime.GOOS, "proto": 2, "goblin": 1, "product": "goblin", "api": core.API,
		// "goblin1" is the name the first app builds look for; keep it
		"features": append(append([]string{}, core.Features...), "goblin1"),
		"kinds":    kinds, "launchers": s.cfg.Live().Launchers, "roots": s.cfg.Live().Roots, "vigil": s.cfg.Live().Links["vigil"] != "",
		"links": links,
	})
}

func (s *Server) speedtest(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
	if n <= 0 || n > 1<<20 {
		n = 64 << 10
	}
	buf := make([]byte, n)
	rand.Read(buf) // incompressible
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf)
}

// postLog: a client sends events it signed (approving a host or client, removing a member).
func (s *Server) postLog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Events []core.Event `json:"events"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	n := s.mesh.MergeEvents(in.Events)
	writeJSON(w, map[string]any{"added": n, "hash": s.log.Hash()})
}

// ---- pairing: `goblin pair` (local access + PIN) made a one-time code; the app redeems it with its key.

type pairCodes map[string]int64 // sha256(code) -> expiry (unix s)

func pairCodesPath(home string) string { return filepath.Join(home, "pair-codes.json") }

func hashCode(code string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return hex.EncodeToString(h[:])
}

// NewPairCode makes a one-time code valid for 10 minutes (called by the CLI).
func NewPairCode(home string) (string, error) {
	b := make([]byte, 9)
	rand.Read(b)
	code := core.B64(b)
	codes := pairCodes{}
	core.ReadJSON(pairCodesPath(home), &codes)
	now := time.Now().Unix()
	for k, exp := range codes {
		if exp < now {
			delete(codes, k)
		}
	}
	codes[hashCode(code)] = now + 600
	return code, core.WriteJSON(pairCodesPath(home), codes)
}

func redeemPairCode(home, code string) bool {
	codes := pairCodes{}
	core.ReadJSON(pairCodesPath(home), &codes)
	h := hashCode(code)
	exp, ok := codes[h]
	if !ok {
		return false
	}
	delete(codes, h)
	core.WriteJSON(pairCodesPath(home), codes)
	return exp >= time.Now().Unix()
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	s.pairMu.Lock()
	now := time.Now()
	var recent []time.Time
	for _, t := range s.pairFail {
		if now.Sub(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.pairFail = recent
	locked := len(recent) >= 5
	s.pairMu.Unlock()
	if locked {
		http.Error(w, "too many wrong codes, try again in 10 minutes", http.StatusTooManyRequests)
		return
	}
	var in struct {
		Code string `json:"code"`
		Name string `json:"name"`
		Pub  string `json:"pub"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil || in.Pub == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if !redeemPairCode(s.home, in.Code) {
		s.pairMu.Lock()
		s.pairFail = append(s.pairFail, now)
		s.pairMu.Unlock()
		time.Sleep(2 * time.Second)
		http.Error(w, "that pairing code is wrong or expired: run `goblin pair` again", http.StatusForbidden)
		return
	}
	name := core.CleanName(in.Name)
	if name == "" {
		name = "device"
	}
	e, err := s.log.AddMember(s.id, "client", in.Pub, name, "")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mesh.MergeEvents(nil)
	s.onMeshChange("members")
	log.Printf("paired client %s (%s) from %s", e.ID, name, r.RemoteAddr)
	writeJSON(w, map[string]any{"id": e.ID, "net": s.log.NetID(), "host": s.id.ID, "events": s.log.All()})
}

// join: a client without a pairing code asks for access; another client approves it (POST again to poll).
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Pub  string `json:"pub"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.mesh.JoinAnswer(w, "client", in.Name, in.Pub, "", nil)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// PairLink makes a one-time pairing link for a new phone or desktop app (`goblin pair`, the local page).
func PairLink(home string, cfg *core.Config, id *core.Identity, netID string) (string, error) {
	code, err := NewPairCode(home)
	if err != nil {
		return "", err
	}
	p := map[string]any{"v": 1, "net": netID, "id": id.ID, "n": cfg.Name, "h": mesh.LocalAddrs(cfg), "c": id.Pin, "k": code}
	b, _ := json.Marshal(p)
	return "goblin://pair/" + base64.RawURLEncoding.EncodeToString(b), nil
}
