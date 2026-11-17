package server

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// linkProxy: /api/link/<name>/<path> fetches <path> from links.<name> in goblin.yaml for a paired client, so a web
// page on the home network (Vigil) works in Switchboard away from home too, through this node or the hub's relay.
// Only configured links are reachable. Root-relative links in HTML pages are rewritten to stay under the prefix.
var linkClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (s *Server) linkProxy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	base := s.cfg.Live().Links[name]
	if base == "" {
		http.Error(w, "no link named "+name+" on this computer (links: in goblin.yaml)", http.StatusNotFound)
		return
	}
	prefix := "/api/link/" + name + "/"
	tail := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	low := strings.ToLower(tail)
	if tail == r.URL.EscapedPath() || strings.Contains(low, "..") || strings.Contains(low, "%2e") || strings.Contains(low, "%2f") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(strings.TrimRight(base, "/") + "/" + tail)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	target.RawQuery = r.URL.RawQuery
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Accept-Language"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := linkClient.Do(req)
	if err != nil {
		http.Error(w, name+" is not reachable from this computer: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
			loc = strings.TrimSuffix(prefix, "/") + loc
		} else if b, err := url.Parse(base); err == nil && strings.HasPrefix(loc, b.Scheme+"://"+b.Host+"/") {
			loc = strings.TrimSuffix(prefix, "/") + strings.TrimPrefix(loc, b.Scheme+"://"+b.Host)
		}
		w.Header().Set("Location", loc)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		p := []byte(strings.TrimSuffix(prefix, "/"))
		for _, q := range [][]byte{[]byte(`href="/`), []byte(`src="/`), []byte(`action="/`), []byte(`fetch('/`), []byte(`fetch("/`)} {
			if bytes.Contains(data, q) {
				// keep protocol-relative "//host" links as they are
				data = rewriteRootLinks(data, q, p)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(data)
}

// rewriteRootLinks puts prefix in front of root-relative links that start with q (q ends in "/").
func rewriteRootLinks(data, q, prefix []byte) []byte {
	var out bytes.Buffer
	for {
		i := bytes.Index(data, q)
		if i < 0 {
			out.Write(data)
			return out.Bytes()
		}
		out.Write(data[:i+len(q)-1])
		rest := data[i+len(q)-1:]
		if !bytes.HasPrefix(rest, []byte("//")) {
			out.Write(prefix)
		}
		data = rest
		out.WriteByte(data[0])
		data = data[1:]
	}
}
