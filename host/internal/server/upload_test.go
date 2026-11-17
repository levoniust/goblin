package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadName(t *testing.T) {
	for in, want := range map[string]string{
		"IMG_2041.jpg":                    "IMG_2041.jpg",
		"../../etc/passwd":                "passwd",
		`C:\Users\x\photo 1.png`:          "photo-1.png",
		"":                                "upload",
		"..":                              "upload",
		"what?*:<>.jpeg":                  "what-.jpeg",
		strings.Repeat("a", 200) + ".png": strings.Repeat("a", 76) + ".png",
	} {
		if got := uploadName(in); got != want {
			t.Errorf("uploadName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpload(t *testing.T) {
	s := &Server{home: t.TempDir()}
	dir := s.uploadDir()
	os.MkdirAll(dir, 0o700)
	old := filepath.Join(dir, "old.png")
	os.WriteFile(old, []byte("x"), 0o600)
	past := time.Now().Add(-31 * 24 * time.Hour)
	os.Chtimes(old, past, past)

	w := httptest.NewRecorder()
	s.upload(w, httptest.NewRequest("POST", "/api/upload?name=cat%20pic.jpg", bytes.NewReader([]byte("JPEGDATA"))))
	var out struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(out.Path); string(b) != "JPEGDATA" || out.Size != 8 || filepath.Dir(out.Path) != dir || !strings.HasSuffix(out.Path, "-cat-pic.jpg") {
		t.Fatalf("saved %q (%d bytes): %q", out.Path, out.Size, b)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("an upload older than 30 days was kept")
	}

	w = httptest.NewRecorder()
	s.upload(w, httptest.NewRequest("POST", "/api/upload?name=empty.png", bytes.NewReader(nil)))
	if w.Code != 400 {
		t.Fatalf("empty upload: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.upload(w, httptest.NewRequest("POST", "/api/upload?name=big.png", bytes.NewReader(make([]byte, maxUpload+1))))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too big: %d", w.Code)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("failed uploads left files: %d", len(ents))
	}
}
