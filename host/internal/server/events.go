package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/levoniust/goblin/host/internal/core"
	"github.com/levoniust/goblin/host/internal/mesh"
)

// events: one websocket per client per host. Server -> client: hello, sessions, network, speedtest.
// Client -> server: focus, speedtest_result.
type events struct {
	s     *Server
	mu    sync.Mutex
	conns map[*evConn]bool
	waits map[string]chan map[string]float64
}

type evConn struct {
	c      *websocket.Conn
	client string
	send   chan []byte
	cancel context.CancelFunc
}

func newEvents(s *Server) *events {
	return &events{s: s, conns: map[*evConn]bool{}, waits: map[string]chan map[string]float64{}}
}

func accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	// clients are apps, not browsers: no origin check needed (auth is the signed request)
	return websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
}

func (e *events) serve(w http.ResponseWriter, r *http.Request) {
	m := caller(r)
	c, err := accept(w, r)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	ec := &evConn{c: c, client: m.ID, send: make(chan []byte, 64), cancel: cancel}
	e.mu.Lock()
	e.conns[ec] = true
	e.mu.Unlock()
	e.s.mesh.SeenClient(m.ID, true)
	defer func() {
		e.mu.Lock()
		delete(e.conns, ec)
		e.mu.Unlock()
		e.s.mesh.SeenClient(m.ID, e.live(m.ID))
		cancel()
		c.Close(websocket.StatusNormalClosure, "")
	}()
	go func() { // writer + keepalive
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-ec.send:
				wc, cl := context.WithTimeout(ctx, 10*time.Second)
				err := c.Write(wc, websocket.MessageText, b)
				cl()
				if err != nil {
					cancel()
					return
				}
			case <-t.C:
				pc, cl := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pc)
				cl()
				if err != nil {
					cancel()
					return
				}
				e.s.mesh.SeenClient(m.ID, true)
			}
		}
	}()
	hub, epoch := e.s.mesh.Hub()
	e.sendTo(ec, map[string]any{"type": "hello", "version": core.Version, "host": e.s.id.ID, "net": e.s.log.NetID(), "hub": hub, "epoch": epoch})
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Type    string             `json:"type"`
			Test    string             `json:"test"`
			Results map[string]float64 `json:"results"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "speedtest_result":
			e.mu.Lock()
			ch := e.waits[msg.Test]
			e.mu.Unlock()
			if ch != nil {
				select {
				case ch <- msg.Results:
				default:
				}
			}
		}
		e.s.mesh.SeenClient(m.ID, true)
	}
}

func (e *events) sendTo(ec *evConn, v any) {
	b, _ := json.Marshal(v)
	select {
	case ec.send <- b:
	default: // a stuck client misses updates rather than blocking everyone
	}
}

func (e *events) broadcast(v any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ec := range e.conns {
		e.sendTo(ec, v)
	}
}

func (e *events) live(client string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ec := range e.conns {
		if ec.client == client {
			return true
		}
	}
	return false
}

// dropRemoved closes sockets of clients that were removed from the network.
func (e *events) dropRemoved() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ec := range e.conns {
		if _, ok := e.s.log.Get(ec.client); !ok {
			ec.c.Close(4003, "removed")
			ec.cancel()
		}
	}
}

// speedTest asks a client (live here) to time each host: round trip + a small download. Used by the hub election.
func (e *events) speedTest(ctx context.Context, client string, hosts []mesh.HostAddr) (map[string]float64, error) {
	e.mu.Lock()
	var target *evConn
	for ec := range e.conns {
		if ec.client == client {
			target = ec
		}
	}
	b := make([]byte, 6)
	rand.Read(b)
	tid := hex.EncodeToString(b)
	ch := make(chan map[string]float64, 1)
	e.waits[tid] = ch
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.waits, tid); e.mu.Unlock() }()
	if target == nil {
		return nil, errors.New("client not connected here")
	}
	e.sendTo(target, map[string]any{"type": "speedtest", "test": tid, "hosts": hosts})
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return nil, errors.New("client did not answer the speed test")
	}
}
