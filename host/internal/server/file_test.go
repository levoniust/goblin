package server

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestFile(t *testing.T) {
	s := &Server{}
	d := t.TempDir()
	f := filepath.Join(d, "report.html")
	os.WriteFile(f, []byte("<h1>hi</h1>"), 0o600)
	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.file(w, httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(p), nil))
		return w
	}
	if w := get(f); w.Code != 200 || w.Body.String() != "<h1>hi</h1>" || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("html: %d %q %v", w.Code, w.Body, w.Header())
	}
	for p, code := range map[string]int{"": 400, "relative.txt": 400, d: 400, filepath.Join(d, "nope"): 404} {
		if w := get(p); w.Code != code {
			t.Errorf("%q: %d, want %d", p, w.Code, code)
		}
	}
}
