package mesh

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/levoniust/goblin/host/internal/core"
)

// Mount adds the host-to-host API. check verifies a signed request and returns the calling member.
func (m *Mesh) Mount(mux *http.ServeMux, check func(r *http.Request) (core.Member, error)) {
	host := func(h func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mem, err := check(r)
			if err != nil || mem.Kind != "host" {
				http.Error(w, "hosts only", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
			h(w, r, body)
		}
	}
	mux.HandleFunc("POST /mesh/hello", host(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var h Hello
		if json.Unmarshal(body, &h) != nil {
			http.Error(w, "bad hello", 400)
			return
		}
		m.gotHello(context.Background(), h, "")
		writeJSON(w, m.myHello())
	}))
	mux.HandleFunc("POST /mesh/sync", host(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var in struct {
			Events []core.Event `json:"events"`
		}
		json.Unmarshal(body, &in)
		m.MergeEvents(in.Events)
		writeJSON(w, map[string]any{"events": m.Log.All()})
	}))
	mux.HandleFunc("POST /mesh/nettest", host(func(w http.ResponseWriter, r *http.Request, body []byte) {
		writeJSON(w, map[string]any{"ms": m.NetTest(r.Context())})
	}))
	mux.HandleFunc("POST /mesh/clienttest", host(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var in struct {
			Client string     `json:"client"`
			Hosts  []HostAddr `json:"hosts"`
		}
		json.Unmarshal(body, &in)
		if m.ClientTester == nil {
			http.Error(w, "no tester", 503)
			return
		}
		res, err := m.ClientTester(r.Context(), in.Client, in.Hosts)
		if err != nil {
			http.Error(w, err.Error(), 504)
			return
		}
		writeJSON(w, map[string]any{"results": res})
	}))
	mux.HandleFunc("POST /mesh/announce", host(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var in struct {
			Hub   string `json:"hub"`
			Epoch int64  `json:"epoch"`
			By    string `json:"by"`
			At    int64  `json:"at"`
		}
		json.Unmarshal(body, &in)
		m.mu.Lock()
		m.adoptLocked(in.Hub, in.Epoch, in.By, in.At, false)
		m.mu.Unlock()
		m.save()
		writeJSON(w, map[string]any{"ok": true})
	}))
	// no auth: who is this (lets a host found on the LAN or Tailscale be recognised)
	mux.HandleFunc("GET /mesh/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, whoami{G: "goblin1", Net: m.Log.NetID(), ID: m.id.ID, Name: m.cfg.Name, Pin: m.id.Pin, Port: m.cfg.Port, Fresh: m.freshLone()})
	})
	// no auth: a new host asks to join; it waits here until a client approves it
	mux.HandleFunc("POST /mesh/join", func(w http.ResponseWriter, r *http.Request) {
		var in joinReq
		if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		m.JoinAnswer(w, "host", in.Name, in.Pub, in.Pin, in.Addrs)
	})
}

// JoinAnswer handles a join request from a host (mesh) or a client (client API): approved = here is the log.
func (m *Mesh) JoinAnswer(w http.ResponseWriter, kind, name, pub, pin string, addrs []string) {
	p, err := core.UnB64(pub)
	if err != nil || len(p) != 32 {
		http.Error(w, "bad key", 400)
		return
	}
	id := core.MemberID(p)
	if m.Log.Removed(id) {
		http.Error(w, "removed from this network", 403)
		return
	}
	if mem, ok := m.Log.Get(id); ok && mem.Kind == kind {
		writeJSON(w, joinResp{Status: "approved", Net: m.Log.NetID(), Events: m.Log.All(), Pin: m.id.Pin})
		return
	}
	if len(addrs) > 16 {
		addrs = addrs[:16]
	}
	pd, err := m.AddPending(kind, name, pub, pin, addrs)
	if err != nil {
		http.Error(w, err.Error(), 429)
		return
	}
	writeJSON(w, joinResp{Status: "pending", Code: pd.Code, Net: m.Log.NetID(), Pin: m.id.Pin})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
