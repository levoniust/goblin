package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/levoniust/goblin/host/internal/core"
)

func TestLinkProxy(t *testing.T) {
	vigil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<a href="/games">g</a><img src="/logo.png"><a href="//cdn.example/x">cdn</a><script>fetch('/api/hosts?x='+r.URL.RawQuery)</script>`)
		case "/old":
			http.Redirect(w, r, "/new", http.StatusFound)
		case "/api/hosts":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"q":"`+r.URL.RawQuery+`","m":"`+r.Method+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer vigil.Close()
	cfg := &core.Config{Links: map[string]string{"vigil": vigil.URL}}
	s := &Server{cfg: cfg}
	mx := http.NewServeMux()
	mx.HandleFunc("/api/link/{name}/{rest...}", s.linkProxy)
	get := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mx.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	w := get("GET", "/api/link/vigil/")
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, `href="/api/link/vigil/games"`) || !strings.Contains(b, `src="/api/link/vigil/logo.png"`) ||
		!strings.Contains(b, `fetch('/api/link/vigil/api/hosts`) || !strings.Contains(b, `href="//cdn.example/x"`) {
		t.Fatalf("page: %d %s", w.Code, b)
	}
	if w := get("GET", "/api/link/vigil/old"); w.Code != 302 || w.Header().Get("Location") != "/api/link/vigil/new" {
		t.Fatalf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("POST", "/api/link/vigil/api/hosts?a=1"); w.Code != 200 || w.Body.String() != `{"q":"a=1","m":"POST"}` {
		t.Fatalf("api: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{"/api/link/other/", "/api/link/vigil/../x", "/api/link/vigil/%2e%2e/x", "/api/link/vigil/a%2Fb"} {
		if w := get("GET", bad); w.Code == 200 {
			t.Errorf("%s allowed", bad)
		}
	}
}
