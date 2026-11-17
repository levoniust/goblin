package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIAuth(t *testing.T) {
	tok := strings.Repeat("ab", 24)
	u := &uiServer{token: tok, port: 8202}
	h := u.auth(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	try := func(method, host, cookie string, hdr bool) int {
		r := httptest.NewRequest(method, "http://"+host+"/ui/api/state", nil)
		r.Host = host
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: uiCookie, Value: cookie})
		}
		if hdr {
			r.Header.Set("X-Goblin-UI", "1")
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	cases := []struct {
		name         string
		method, host string
		cookie       string
		hdr          bool
		want         int
	}{
		{"good read", "GET", "127.0.0.1:8202", tok, false, 200},
		{"localhost name", "GET", "localhost:8202", tok, false, 200},
		{"no cookie", "GET", "127.0.0.1:8202", "", false, 403},
		{"wrong token", "GET", "127.0.0.1:8202", strings.Repeat("cd", 24), false, 403},
		{"rebinding host", "GET", "evil.example:8202", tok, false, 403},
		{"other port", "GET", "127.0.0.1:9999", tok, false, 403},
		{"change without header", "POST", "127.0.0.1:8202", tok, false, 403},
		{"change with header", "POST", "127.0.0.1:8202", tok, true, 200},
	}
	for _, c := range cases {
		if got := try(c.method, c.host, c.cookie, c.hdr); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestUIPageToken(t *testing.T) {
	tok := strings.Repeat("ab", 24)
	u := &uiServer{token: tok, port: 8202}
	get := func(url string, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", url, nil)
		r.Host = "127.0.0.1:8202"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: uiCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		u.page(w, r)
		return w
	}
	if w := get("/ui?t="+tok, ""); w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") ||
		!strings.Contains(w.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("token link: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w := get("/ui?t=wrong", ""); w.Code != http.StatusForbidden {
		t.Fatalf("wrong token link: %d", w.Code)
	}
	if w := get("/ui", ""); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "<script") {
		t.Fatalf("no cookie: %d", w.Code)
	}
	if w := get("/ui", tok); w.Code != 200 || !strings.Contains(w.Body.String(), "goblin.yaml") {
		t.Fatalf("page: %d", w.Code)
	}
}
