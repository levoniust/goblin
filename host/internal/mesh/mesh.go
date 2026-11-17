// Package mesh finds the other hosts, keeps the member log in sync, and picks the hub (plans/PLAN.md, the design rules).
package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// ClientSeen: when this host last heard from a client, and whether it has a live events socket here.
type ClientSeen struct {
	Last int64 `json:"last"`
	Live bool  `json:"live"`
}

// Pending is a host or client waiting for approval.
type Pending struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Name  string   `json:"name"`
	Pub   string   `json:"pub"`
	Pin   string   `json:"pin,omitempty"`
	Addrs []string `json:"addrs,omitempty"`
	Code  string   `json:"code"` // 4 digits, also shown on the device asking
	At    int64    `json:"at"`
	Via   string   `json:"via"` // host it is waiting at
}

// Hello is what hosts tell each other every heartbeat.
type Hello struct {
	ID       string                `json:"id"`
	Name     string                `json:"name"`
	Net      string                `json:"net"`
	Boot     int64                 `json:"boot"`
	Hub      string                `json:"hub"`
	Epoch    int64                 `json:"epoch"`
	HubBy    string                `json:"hub_by"`
	HubAt    int64                 `json:"hub_at"` // when that hub was chosen (ms)
	LogHash  string                `json:"log_hash"`
	Addrs    []string              `json:"addrs"`
	NeverHub bool                  `json:"never_hub"`
	Prefer   bool                  `json:"prefer"`
	Clients  map[string]ClientSeen `json:"clients"`
	Pending  []Pending             `json:"pending"`
	OS       string                `json:"os"`
	Version  string                `json:"version"`
	TestReq  int64                 `json:"test_req,omitempty"` // a hub test was asked for at this time (ms)
	Reach    []string              `json:"reach,omitempty"`    // hosts this one hears from directly now (0.2+)
	Out      []string              `json:"out,omitempty"`      // hosts this one can open connections to now (0.2.1+, relay routing)
}

// Peer is another host of this network.
type Peer struct {
	ID      string
	Addrs   []string // "ip:port"
	Active  string
	LastOK  time.Time
	LastTry time.Time
	Hello   Hello
	Err     string
}

type joinState struct {
	Addr  string `json:"addr"`
	Pin   string `json:"pin"`
	Net   string `json:"net"`
	Since int64  `json:"since"`
}

type savedState struct {
	Epoch int64               `json:"epoch"`
	Peers map[string][]string `json:"peers"`
	Join  *joinState          `json:"join,omitempty"`
}

// HostAddr is a host and the addresses a client can try.
type HostAddr struct {
	ID    string   `json:"id"`
	Addrs []string `json:"addrs"`
}

// Mesh is this host's view of the network.
type Mesh struct {
	cfg  *core.Config
	home string
	id   *core.Identity
	Log  *core.Log
	boot int64

	mu          sync.Mutex
	peers       map[string]*Peer
	hub, hubBy  string
	epoch       int64
	hubAt       int64 // when the current hub was chosen (ms); a test request newer than this needs a test
	clientsSeen map[string]ClientSeen
	pending     map[string]Pending
	joining     *joinState
	testReq     int64 // newest hub-test request (ms) seen anywhere
	lastElect   int64 // when this host last started an election (ms)
	electing    bool
	started     time.Time
	clients     pinnedClients
	kick        chan string          // peer id to say hello to now
	cands       map[string]time.Time // addresses of hosts not known yet -> last probe

	// ClientTester asks a client with a live events socket on this host to measure the given hosts (ms each).
	ClientTester func(ctx context.Context, clientID string, hosts []HostAddr) (map[string]float64, error)
	// OnChange is told about "hub", "members", "pending", "peers".
	OnChange func(what string)
}

// New loads saved state. log must already exist (a lone host is its own network).
func New(cfg *core.Config, home string, id *core.Identity, lg *core.Log) *Mesh {
	m := &Mesh{cfg: cfg, home: home, id: id, Log: lg, boot: time.Now().UnixMilli(), peers: map[string]*Peer{},
		clientsSeen: map[string]ClientSeen{}, pending: map[string]Pending{}, started: time.Now(), kick: make(chan string, 64),
		cands: map[string]time.Time{}}
	var s savedState
	core.ReadJSON(m.statePath(), &s)
	m.epoch = s.Epoch
	m.joining = s.Join
	m.syncPeers()
	for pid, addrs := range s.Peers {
		if p, ok := m.peers[pid]; ok {
			p.Addrs = dedupe(append(p.Addrs, addrs...))
		}
	}
	for _, a := range cfg.Seeds {
		m.addCandidate(a)
	}
	return m
}

func (m *Mesh) statePath() string { return filepath.Join(m.home, "mesh.json") }

func (m *Mesh) save() {
	m.mu.Lock()
	s := savedState{Epoch: m.epoch, Peers: map[string][]string{}, Join: m.joining}
	for id, p := range m.peers {
		s.Peers[id] = p.Addrs
	}
	m.mu.Unlock()
	core.WriteJSON(m.statePath(), s)
}

func (m *Mesh) changed(what string) {
	if m.OnChange != nil {
		go m.OnChange(what)
	}
}

// syncPeers makes the peer list match the host members of the log.
func (m *Mesh) syncPeers() {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]bool{}
	for _, mem := range m.Log.Members() {
		if mem.Kind == "host" && mem.ID != m.id.ID {
			want[mem.ID] = true
			if _, ok := m.peers[mem.ID]; !ok {
				m.peers[mem.ID] = &Peer{ID: mem.ID}
			}
		}
	}
	for id := range m.peers {
		if !want[id] {
			delete(m.peers, id)
		}
	}
	for id := range m.pending {
		if _, ok := m.Log.Get(id); ok {
			delete(m.pending, id)
		}
	}
}

// CheckCode is the 4-digit code shown on both the joining device and the approving client.
func CheckCode(pub []byte) string {
	h := sha256.Sum256(pub)
	return fmt.Sprintf("%04d", binary.BigEndian.Uint32(h[:4])%10000)
}

func (m *Mesh) failover() time.Duration { return time.Duration(m.cfg.FailoverSeconds) * time.Second }
func (m *Mesh) beat() time.Duration     { return time.Duration(m.cfg.HeartbeatSecs) * time.Second }

// Run is the main loop.
func (m *Mesh) Run(ctx context.Context) {
	go m.beacon(ctx)
	go m.tailscaleLoop(ctx)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	lastBeat := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case pid := <-m.kick:
			go m.helloPeer(ctx, pid)
		case <-tick.C:
			if time.Since(lastBeat) >= m.beat() {
				lastBeat = time.Now()
				m.mu.Lock()
				ids := make([]string, 0, len(m.peers))
				for id := range m.peers {
					ids = append(ids, id)
				}
				m.mu.Unlock()
				for _, id := range ids {
					go m.helloPeer(ctx, id)
				}
				go m.joinStep(ctx)
				m.pruneCandidates()
			}
			m.evaluate(ctx)
		}
	}
}

func (m *Mesh) myHello() Hello {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := map[string]ClientSeen{}
	for k, v := range m.clientsSeen {
		cs[k] = v
	}
	var pend []Pending
	for _, p := range m.pending {
		if p.Via == m.id.ID {
			pend = append(pend, p)
		}
	}
	var reach, out []string
	for id, p := range m.peers {
		if time.Since(p.LastOK) < m.failover() {
			reach = append(reach, id)
			if p.Active != "" {
				out = append(out, id)
			}
		}
	}
	return Hello{ID: m.id.ID, Name: m.cfg.Name, Net: m.Log.NetID(), Boot: m.boot, Hub: m.hub, Epoch: m.epoch, HubBy: m.hubBy, HubAt: m.hubAt,
		LogHash: m.Log.Hash(), Addrs: LocalAddrs(m.cfg), NeverHub: m.cfg.Live().NeverHub, Prefer: m.cfg.Live().PreferHub, Clients: cs,
		Pending: pend, OS: runtime.GOOS, Version: core.Version, TestReq: m.testReq, Reach: reach, Out: out}
}

// helloPeer says hello to one peer, trying its known addresses (the one that worked last first).
func (m *Mesh) helloPeer(ctx context.Context, pid string) {
	mem, ok := m.Log.Get(pid)
	if !ok || slices.Contains(m.cfg.TestNoDial, pid) {
		return
	}
	m.mu.Lock()
	p := m.peers[pid]
	if p == nil || time.Since(p.LastTry) < 1500*time.Millisecond {
		m.mu.Unlock()
		return
	}
	p.LastTry = time.Now()
	addrs := dedupe(append([]string{p.Active}, p.Addrs...))
	m.mu.Unlock()
	mine := m.myHello()
	for _, a := range addrs {
		if a == "" {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		var theirs Hello
		err := m.call(c, a, mem.Pin, pid, "POST", "/mesh/hello", mine, &theirs)
		cancel()
		if err != nil || theirs.ID != pid {
			m.mu.Lock()
			if err != nil {
				p.Err = err.Error()
			}
			m.mu.Unlock()
			continue
		}
		m.gotHello(ctx, theirs, a)
		return
	}
}

// gotHello handles a hello from a peer (as the answer to ours, or their own call to us).
func (m *Mesh) gotHello(ctx context.Context, h Hello, via string) {
	if h.Net != m.Log.NetID() {
		return
	}
	m.mu.Lock()
	p := m.peers[h.ID]
	if p == nil {
		m.mu.Unlock()
		return
	}
	wasOnline := time.Since(p.LastOK) < m.failover()
	p.LastOK = time.Now()
	p.Err = ""
	if via != "" {
		p.Active = via
	}
	p.Hello = h
	p.Addrs = dedupe(append(append([]string{}, h.Addrs...), p.Addrs...))
	if len(p.Addrs) > 24 {
		p.Addrs = p.Addrs[:24]
	}
	for _, pd := range h.Pending {
		if _, member := m.Log.Get(pd.ID); !member {
			m.pending[pd.ID] = pd
		}
	}
	if h.TestReq > m.testReq {
		m.testReq = h.TestReq
	}
	rejoin := m.adoptLocked(h.Hub, h.Epoch, h.HubBy, h.HubAt, !wasOnline)
	logDiff := h.LogHash != m.Log.Hash()
	m.mu.Unlock()
	if !wasOnline {
		m.changed("peers")
	}
	if rejoin {
		log.Printf("groups rejoined (%s says hub %s): asking for a hub test", h.Name, h.Hub)
		m.RequestTest()
	}
	if logDiff {
		go m.syncLog(ctx, h.ID)
	}
}

// adoptLocked takes a hub announcement if it is newer. Returns true when a host that was away comes back with a
// different live hub than ours: two groups rejoined, which needs one new test (the design rules). An announcement
// spreading inside the group (fromNewPeer=false) is never a rejoin.
func (m *Mesh) adoptLocked(hub string, epoch int64, by string, at int64, fromNewPeer bool) bool {
	if hub == "" {
		return false
	}
	rejoin := fromNewPeer && m.hub != "" && hub != m.hub && m.hubAliveLocked(m.hub) && m.hubAliveLocked(hub)
	newer := epoch > m.epoch || (epoch == m.epoch && by > m.hubBy && hub != m.hub)
	// with no hub, take a same-epoch announcement only if that hub is reachable (else it is the hub we just lost)
	fill := m.hub == "" && epoch >= m.epoch && m.hubAliveLocked(hub)
	if newer || fill {
		if m.hub != hub {
			log.Printf("hub is now %s (epoch %d)", hub, epoch)
			m.changed("hub")
		}
		m.hub, m.epoch, m.hubBy, m.hubAt = hub, epoch, by, at
	}
	return rejoin
}

func (m *Mesh) hubAliveLocked(id string) bool {
	if id == m.id.ID {
		return true
	}
	p := m.peers[id]
	return p != nil && time.Since(p.LastOK) < m.failover()
}

// syncLog exchanges member logs with a peer (both sides end up with the union).
func (m *Mesh) syncLog(ctx context.Context, pid string) {
	mem, ok := m.Log.Get(pid)
	m.mu.Lock()
	p := m.peers[pid]
	addr := ""
	if p != nil {
		addr = p.Active
	}
	m.mu.Unlock()
	if !ok || addr == "" {
		return
	}
	var out struct {
		Events []core.Event `json:"events"`
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.call(c, addr, mem.Pin, pid, "POST", "/mesh/sync", map[string]any{"events": m.Log.All()}, &out); err != nil {
		return
	}
	m.MergeEvents(out.Events)
}

// MergeEvents adds events from anywhere (peer, client approval) and saves + spreads them.
func (m *Mesh) MergeEvents(ev []core.Event) int {
	n, _ := m.Log.Merge(ev)
	if n > 0 {
		m.Log.Save()
		m.syncPeers()
		m.save()
		m.changed("members")
		// tell everyone right away instead of waiting for the next heartbeat
		m.mu.Lock()
		for id := range m.peers {
			select {
			case m.kick <- id:
			default:
			}
		}
		m.mu.Unlock()
	}
	return n
}

// SeenClient records traffic from a client (live = it has an events socket open here).
func (m *Mesh) SeenClient(id string, live bool) {
	m.mu.Lock()
	m.clientsSeen[id] = ClientSeen{Last: time.Now().UnixMilli(), Live: live}
	m.mu.Unlock()
}

// RequestTest asks for a new hub test (user request, or groups rejoined). The coordinator runs it.
func (m *Mesh) RequestTest() {
	m.mu.Lock()
	// always newer than anything handled so far, even when this host's clock is behind the host that last asked
	m.testReq = max(time.Now().UnixMilli(), m.testReq+1, m.hubAt+1, m.lastElect+1)
	for id := range m.peers {
		select {
		case m.kick <- id:
		default:
		}
	}
	m.mu.Unlock()
}

// HelloNow says hello to every peer right away (after a settings change they should know about).
func (m *Mesh) HelloNow() {
	m.mu.Lock()
	for id := range m.peers {
		select {
		case m.kick <- id:
		default:
		}
	}
	m.mu.Unlock()
}

// Hub returns the current hub and epoch.
func (m *Mesh) Hub() (string, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hub, m.epoch
}

// ---- views for the client API

type HostView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Pin      string   `json:"pin"`
	Addrs    []string `json:"addrs"`
	Online   bool     `json:"online"`
	Self     bool     `json:"self"`
	LastOK   int64    `json:"last_ok"`
	Boot     int64    `json:"boot"`
	NeverHub bool     `json:"never_hub"`
	Prefer   bool     `json:"prefer"`
	OS       string   `json:"os"`
	Version  string   `json:"version"`
	Hub      bool     `json:"hub"`
	Via      string   `json:"via,omitempty"` // not reachable from here, but this online host hears from it (e.g. the hub)
}

type ClientView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Last int64  `json:"last"`
}

type View struct {
	Net         string       `json:"net"`
	Self        string       `json:"self"`
	Hub         string       `json:"hub"`
	Epoch       int64        `json:"epoch"`
	Coordinator string       `json:"coordinator"`
	Hosts       []HostView   `json:"hosts"`
	Clients     []ClientView `json:"clients"`
	Pending     []Pending    `json:"pending"`
	Joining     *joinState   `json:"joining,omitempty"`
}

// Status is the network as this host sees it.
func (m *Mesh) Status() View {
	members := m.Log.Members()
	m.mu.Lock()
	defer m.mu.Unlock()
	v := View{Net: m.Log.NetID(), Self: m.id.ID, Hub: m.hub, Epoch: m.epoch, Coordinator: m.coordinatorLocked(), Joining: m.joining}
	seen := m.mergedClientsLocked()
	for _, mem := range members {
		if mem.Kind == "client" {
			v.Clients = append(v.Clients, ClientView{ID: mem.ID, Name: mem.Name, Last: seen[mem.ID].Last})
			continue
		}
		h := HostView{ID: mem.ID, Name: mem.Name, Pin: mem.Pin, Hub: mem.ID == m.hub}
		if mem.ID == m.id.ID {
			h.Self, h.Online, h.LastOK, h.Boot = true, true, time.Now().UnixMilli(), m.boot
			h.Addrs, h.NeverHub, h.Prefer, h.OS, h.Version = LocalAddrs(m.cfg), m.cfg.Live().NeverHub, m.cfg.Live().PreferHub, runtime.GOOS, core.Version
		} else if p := m.peers[mem.ID]; p != nil {
			h.Addrs = dedupe(append([]string{p.Active}, p.Addrs...))
			h.Online = time.Since(p.LastOK) < m.failover()
			if !p.LastOK.IsZero() {
				h.LastOK = p.LastOK.UnixMilli()
			}
			h.Boot, h.NeverHub, h.Prefer, h.OS, h.Version = p.Hello.Boot, p.Hello.NeverHub, p.Hello.Prefer, p.Hello.OS, p.Hello.Version
		}
		if !h.Self && !h.Online {
			h.Via = m.viaLocked(mem.ID)
		}
		v.Hosts = append(v.Hosts, h)
	}
	for _, p := range m.pending {
		if _, member := m.Log.Get(p.ID); !member && time.Since(time.UnixMilli(p.At)) < 24*time.Hour {
			v.Pending = append(v.Pending, p)
		}
	}
	sort.Slice(v.Pending, func(i, j int) bool { return v.Pending[i].At < v.Pending[j].At })
	return v
}

// viaLocked: an online peer (the hub first) that hears from host id directly, or "".
func (m *Mesh) viaLocked(id string) string {
	via := ""
	for pid, p := range m.peers {
		if time.Since(p.LastOK) >= m.failover() || !slices.Contains(p.Hello.Reach, id) {
			continue
		}
		if pid == m.hub {
			return pid
		}
		if via == "" || pid < via {
			via = pid
		}
	}
	return via
}

// PeerTarget gives the address + pin to reach a host directly from here (for the relay).
func (m *Mesh) PeerTarget(id string) (addr, pin string, ok bool) {
	mem, member := m.Log.Get(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.peers[id]
	if !member || p == nil || p.Active == "" || time.Since(p.LastOK) > m.failover() {
		return "", "", false
	}
	return p.Active, mem.Pin, true
}

// NextHop picks where a relay sends a request for host target: the target itself when it is reachable directly,
// else an online peer (not in avoid) that can open connections to the target (the hub first, then the lowest id).
// Each relay on the way picks its own next hop, so a route can be longer than two steps.
func (m *Mesh) NextHop(target string, avoid []string) (hop, addr, pin string, ok bool) {
	if a, p, ok := m.PeerTarget(target); ok {
		return target, a, p, true
	}
	m.mu.Lock()
	best := m.relayPeerLocked(target, avoid)
	m.mu.Unlock()
	if best == "" {
		return "", "", "", false
	}
	a, p, ok := m.PeerTarget(best)
	return best, a, p, ok
}

// relayPeerLocked: the online peer (not in avoid) to hand a request for target to, or "".
func (m *Mesh) relayPeerLocked(target string, avoid []string) string {
	best := ""
	for pid, p := range m.peers {
		if pid == target || slices.Contains(avoid, pid) || p.Active == "" || time.Since(p.LastOK) >= m.failover() {
			continue
		}
		routes := p.Hello.Out
		if routes == nil { // 0.2 hosts only report who they hear from
			routes = p.Hello.Reach
		}
		if !slices.Contains(routes, target) {
			continue
		}
		if pid == m.hub {
			return pid
		}
		if best == "" || pid < best {
			best = pid
		}
	}
	return best
}

// Transport for talking to a host with this pin.
func (m *Mesh) Transport(pin string) any { return m.clients.Transport(pin) }

// ---- pending approvals

// AddPending stores a join request (host or client) waiting at this host.
func (m *Mesh) AddPending(kind, name, pub, pin string, addrs []string) (Pending, error) {
	p, err := core.UnB64(pub)
	if err != nil || len(p) != ed25519.PublicKeySize {
		return Pending{}, fmt.Errorf("bad key")
	}
	id := core.MemberID(p)
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.pending[id]; ok {
		old.At = time.Now().UnixMilli()
		old.Addrs = addrs
		m.pending[id] = old
		return old, nil
	}
	n := 0
	for _, x := range m.pending {
		if x.Via == m.id.ID {
			n++
		}
	}
	if n >= 50 {
		return Pending{}, fmt.Errorf("too many requests waiting")
	}
	pd := Pending{ID: id, Kind: kind, Name: core.CleanName(name), Pub: pub, Pin: pin, Addrs: addrs, Code: CheckCode(p), At: time.Now().UnixMilli(), Via: m.id.ID}
	m.pending[id] = pd
	log.Printf("%s %q asks to join (check code %s)", kind, pd.Name, pd.Code)
	m.changed("pending")
	return pd, nil
}

// Dismiss drops a pending request (the user said no).
func (m *Mesh) Dismiss(id string) {
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
	m.changed("pending")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
