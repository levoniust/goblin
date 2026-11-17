package server

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// RouteHeader lists the relays a request has passed; MaxRelayHops caps them.
const (
	RouteHeader  = "X-Goblin-Route"
	MaxRelayHops = 3
)

// relay: /relay/<host>/<path> forwards a client's request to a host it can't reach directly (usually through the
// hub). The client signs the request for the target host; the relay checks that signature first, so it is no open
// proxy, and the target checks it again. Websockets (terminal, events) pass through.
func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("host")
	rest := "/" + r.PathValue("rest")
	// keep the path exactly as the client encoded (and signed) it, e.g. /api/sessions/%403/keys
	restEsc := strings.TrimPrefix(r.URL.EscapedPath(), "/relay/"+url.PathEscape(target))
	if !strings.HasPrefix(restEsc, "/") {
		restEsc = rest
	}
	inner := *r.URL
	inner.Path, inner.RawPath = rest, restEsc
	innerReq := r.Clone(r.Context())
	innerReq.URL = &inner
	innerReq.RequestURI = inner.RequestURI()
	if target == s.id.ID {
		s.mux.ServeHTTP(w, innerReq)
		return
	}
	if !strings.HasPrefix(rest, "/api/") {
		http.Error(w, "only the client API can be relayed", 400)
		return
	}
	if _, err := s.verify.CheckFor(innerReq, core.URIWithoutAuth(innerReq), target); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// relays already passed (each one adds itself): stops loops and long chains
	var route []string
	if h := r.Header.Get(RouteHeader); h != "" {
		route = strings.Split(h, ",")
	}
	if slices.Contains(route, s.id.ID) {
		http.Error(w, "relay loop", http.StatusLoopDetected)
		return
	}
	if len(route) >= MaxRelayHops {
		http.Error(w, "that host is too many relays away", http.StatusBadGateway)
		return
	}
	route = append(route, s.id.ID)
	hop, addr, pin, ok := s.mesh.NextHop(target, route)
	if !ok {
		http.Error(w, "that host is not reachable from here", http.StatusBadGateway)
		return
	}
	outPath, outEsc := rest, restEsc
	if hop != target { // not direct: hand it to the next relay
		outPath, outEsc = "/relay/"+target+rest, "/relay/"+url.PathEscape(target)+restEsc
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "https", Host: addr, Path: outPath, RawPath: outEsc, RawQuery: r.URL.RawQuery}
			pr.Out.Host = addr
			pr.Out.Header.Set(RouteHeader, strings.Join(route, ","))
		},
		Transport:     s.mesh.Transport(pin).(http.RoundTripper),
		FlushInterval: 50 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "relay: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}
