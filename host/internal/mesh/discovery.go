package mesh

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// LocalAddrs: this host's reachable addresses ("ip:port"): every up, non-loopback IPv4 (LAN, Tailscale, hotspot)
// except container/VM bridges, plus the configured extras.
func LocalAddrs(cfg *core.Config) []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || skipIface(i.Name) {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, net.JoinHostPort(n.IP.String(), strconv.Itoa(cfg.Port)))
			}
		}
	}
	for _, a := range cfg.Addrs {
		if !strings.Contains(a, ":") {
			a = net.JoinHostPort(a, strconv.Itoa(cfg.Port))
		}
		out = append(out, a)
	}
	return dedupe(out)
}

func skipIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"docker", "br-", "veth", "virbr", "lxc", "cni", "flannel", "vethernet (wsl", "vethernet (default"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// ---- LAN beacon (UDP broadcast): how hosts on the same network (home LAN, hotspot, travel router) find each other.

type beaconMsg struct {
	G     string `json:"g"`
	Net   string `json:"net"`
	ID    string `json:"id"`
	Port  int    `json:"port"`
	Pin   string `json:"pin"`
	Name  string `json:"name"`
	Fresh bool   `json:"fresh"`
}

func (m *Mesh) beacon(ctx context.Context) {
	lc := net.ListenConfig{Control: reuseControl}
	pc, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(m.cfg.BeaconPort))
	if err != nil {
		log.Printf("LAN discovery off: %v", err)
		return
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var b beaconMsg
			if json.Unmarshal(buf[:n], &b) != nil || b.G != "goblin1" || b.ID == m.id.ID || b.Port <= 0 {
				continue
			}
			ua, ok := from.(*net.UDPAddr)
			if !ok {
				continue
			}
			m.heard(net.JoinHostPort(ua.IP.String(), strconv.Itoa(b.Port)), b.ID, b.Net)
		}
	}()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		msg, _ := json.Marshal(beaconMsg{G: "goblin1", Net: m.Log.NetID(), ID: m.id.ID, Port: m.cfg.Port, Pin: m.id.Pin, Name: m.cfg.Name, Fresh: m.freshLone()})
		for _, dst := range broadcastAddrs(m.cfg.BeaconPort) {
			pc.WriteTo(msg, dst)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func broadcastAddrs(port int) []net.Addr {
	out := []net.Addr{&net.UDPAddr{IP: net.IPv4bcast, Port: port}}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagBroadcast == 0 || skipIface(i.Name) {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			ip, mask := n.IP.To4(), n.Mask
			if len(mask) == 16 {
				mask = mask[12:]
			}
			b := make(net.IP, 4)
			for k := 0; k < 4; k++ {
				b[k] = ip[k] | ^mask[k]
			}
			out = append(out, &net.UDPAddr{IP: b, Port: port})
		}
	}
	return out
}

// heard: an address where a host answered (beacon, Tailscale, seed). Known peer = remember the address and say
// hello now (quick on the pickup); anything else gets probed.
func (m *Mesh) heard(addr, id, netID string) {
	if netID == m.Log.NetID() {
		m.mu.Lock()
		p := m.peers[id]
		if p != nil {
			known := false
			for _, a := range p.Addrs {
				known = known || a == addr
			}
			if !known {
				p.Addrs = append([]string{addr}, p.Addrs...)
			}
			stale := time.Since(p.LastOK) > m.beat()
			m.mu.Unlock()
			if stale || !known {
				select {
				case m.kick <- id:
				default:
				}
			}
			return
		}
		m.mu.Unlock()
		return
	}
	m.addCandidate(addr)
}

func (m *Mesh) addCandidate(addr string) {
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, strconv.Itoa(m.cfg.Port))
	}
	m.mu.Lock()
	last, seen := m.cands[addr]
	if seen && time.Since(last) < 2*time.Minute {
		m.mu.Unlock()
		return
	}
	m.cands[addr] = time.Now()
	m.mu.Unlock()
	go m.probe(addr)
}

func (m *Mesh) pruneCandidates() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for a, t := range m.cands {
		if time.Since(t) > 30*time.Minute {
			delete(m.cands, a)
		}
	}
}

type whoami struct {
	G     string `json:"g"`
	Net   string `json:"net"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Pin   string `json:"pin"`
	Port  int    `json:"port"`
	Fresh bool   `json:"fresh"`
}

// probe asks an unknown address who it is. A host of this network: remember the address. Another network while
// this host is fresh and alone: ask to join it (it shows up on the phone for approval).
func (m *Mesh) probe(addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var w whoami
	if err := m.call(ctx, addr, "", "", "GET", "/mesh/whoami", nil, &w); err != nil || w.G != "goblin1" || w.ID == m.id.ID {
		return
	}
	if w.Net == m.Log.NetID() {
		m.heard(addr, w.ID, w.Net)
		return
	}
	if m.freshLone() && !w.Fresh {
		m.StartJoin(addr, w.Pin, w.Net)
	}
}

// freshLone: this host is alone in its own network with no clients yet, so it may join another network.
func (m *Mesh) freshLone() bool {
	ms := m.Log.Members()
	return len(ms) == 1 && ms[0].ID == m.id.ID
}

// StartJoin asks a host of another network to let this one in (approval on a client). pin may be "" (trust on
// first use; the network id is then fixed for the rest of the request).
func (m *Mesh) StartJoin(addr, pin, netID string) {
	m.mu.Lock()
	if m.joining != nil && m.joining.Addr == addr {
		m.mu.Unlock()
		return
	}
	m.joining = &joinState{Addr: addr, Pin: pin, Net: netID, Since: time.Now().UnixMilli()}
	m.mu.Unlock()
	m.save()
	log.Printf("asking %s to join its network (check code %s): approve it on your phone", addr, CheckCode(m.id.Pub))
	go m.joinStep(context.Background())
}

// CancelJoin stops asking.
func (m *Mesh) CancelJoin() {
	m.mu.Lock()
	m.joining = nil
	m.mu.Unlock()
	m.save()
}

type joinReq struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Pub   string   `json:"pub"`
	Pin   string   `json:"pin"`
	Addrs []string `json:"addrs"`
}

type joinResp struct {
	Status string       `json:"status"` // pending | approved
	Code   string       `json:"code,omitempty"`
	Net    string       `json:"net,omitempty"`
	Events []core.Event `json:"events,omitempty"`
	Pin    string       `json:"pin,omitempty"`
}

func (m *Mesh) joinStep(ctx context.Context) {
	m.mu.Lock()
	j := m.joining
	m.mu.Unlock()
	if j == nil {
		return
	}
	if !m.freshLone() || time.Since(time.UnixMilli(j.Since)) > 24*time.Hour {
		m.CancelJoin()
		return
	}
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var r joinResp
	req := joinReq{ID: m.id.ID, Name: m.cfg.Name, Pub: core.B64(m.id.Pub), Pin: m.id.Pin, Addrs: LocalAddrs(m.cfg)}
	if err := m.call(c, j.Addr, j.Pin, "", "POST", "/mesh/join", req, &r); err != nil {
		return
	}
	if r.Status != "approved" {
		return
	}
	if j.Net != "" && r.Net != j.Net {
		log.Printf("join: %s answered for a different network, ignored", j.Addr)
		return
	}
	probe := &core.Log{}
	if err := probe.Adopt(r.Net, r.Events); err != nil {
		return
	}
	if _, ok := probe.Get(m.id.ID); !ok {
		return // its log doesn't include this host yet
	}
	if err := m.Log.Adopt(r.Net, r.Events); err != nil {
		log.Printf("join: %v", err)
		return
	}
	m.mu.Lock()
	m.joining = nil
	m.hub, m.epoch, m.hubBy, m.hubAt = "", 0, "", 0
	m.started = time.Now()
	m.mu.Unlock()
	m.syncPeers()
	m.mu.Lock()
	for _, p := range m.peers {
		p.Addrs = dedupe(append(p.Addrs, j.Addr))
	}
	m.mu.Unlock()
	m.save()
	log.Printf("joined network %s", r.Net)
	m.changed("members")
}

// ---- Tailscale plugin: Tailscale peers are candidate hosts (works across houses, not offline).

type tsStatus struct {
	Peer map[string]struct {
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Peer"`
}

func (m *Mesh) tailscaleLoop(ctx context.Context) {
	if !m.cfg.Tailscale {
		return
	}
	for {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, err := exec.CommandContext(c, "tailscale", "status", "--json").Output()
		cancel()
		if err == nil {
			var st tsStatus
			if json.Unmarshal(out, &st) == nil {
				for _, p := range st.Peer {
					if !p.Online {
						continue
					}
					for _, ip := range p.TailscaleIPs {
						if !strings.Contains(ip, ":") {
							m.tsCandidate(net.JoinHostPort(ip, strconv.Itoa(m.cfg.Port)))
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
	}
}

// tsCandidate probes a Tailscale address unless it is already a working address of a known host.
func (m *Mesh) tsCandidate(addr string) {
	m.mu.Lock()
	for _, p := range m.peers {
		if p.Active == addr && time.Since(p.LastOK) < m.failover() {
			m.mu.Unlock()
			return
		}
	}
	m.mu.Unlock()
	m.addCandidate(addr)
}
