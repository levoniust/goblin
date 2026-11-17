package server

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxFile = 50 << 20

// GET /api/file?path=<absolute path or ~/...> → the file's bytes (read only), so the app can open a file an agent
// mentions in a chat (2026-11-16: tap an HTML file to see it). A paired client can already run anything in a
// chat on this host, so reading a file it names gives it nothing new.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if strings.HasPrefix(p, "~") {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, p[1:])
	}
	if p == "" || !filepath.IsAbs(p) {
		http.Error(w, "give an absolute path", 400)
		return
	}
	st, err := os.Stat(p)
	switch {
	case err != nil:
		http.Error(w, "no such file", http.StatusNotFound)
		return
	case st.IsDir():
		http.Error(w, "that is a folder", 400)
		return
	case st.Size() > maxFile:
		http.Error(w, "file is larger than 50 MB", http.StatusRequestEntityTooLarge)
		return
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Goblin-Path", p)
	w.Header().Set("Content-Security-Policy", "sandbox") // never runs as a page of this host
	http.ServeFile(w, r, p)
}
