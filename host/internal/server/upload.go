package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Uploads: a picture (or any file) from the phone lands in <goblin home>/uploads on this host and its path goes into
// the chat, so the agent there can open it (2026-11-16). Old uploads are removed after uploadKeep.
const (
	maxUpload  = 25 << 20
	uploadKeep = 30 * 24 * time.Hour
)

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// uploadName: a safe file name from what the phone sent (no folders, no odd characters, kept short).
func uploadName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Trim(unsafeName.ReplaceAllString(name, "-"), "-.")
	if len(name) > 80 {
		ext := filepath.Ext(name)
		if len(ext) > 10 {
			ext = ""
		}
		name = name[:80-len(ext)] + ext
	}
	if name == "" {
		name = "upload"
	}
	return name
}

func (s *Server) uploadDir() string { return filepath.Join(s.home, "uploads") }

// POST /api/upload?name=<file name>, body = the file's bytes → {path, size}.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	dir := s.uploadDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.cleanUploads(dir)
	rnd := make([]byte, 3)
	rand.Read(rnd)
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+hex.EncodeToString(rnd)+"-"+uploadName(r.URL.Query().Get("name")))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n == 0 {
		err = errors.New("empty file")
	}
	if err != nil {
		os.Remove(path)
		code := 400
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			code, err = http.StatusRequestEntityTooLarge, errors.New("file is larger than 25 MB")
		}
		http.Error(w, "upload: "+err.Error(), code)
		return
	}
	writeJSON(w, map[string]any{"path": path, "size": n})
}

// cleanUploads removes uploads older than uploadKeep.
func (s *Server) cleanUploads(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if i, err := e.Info(); err == nil && !e.IsDir() && time.Since(i.ModTime()) > uploadKeep {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
