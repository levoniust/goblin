package mesh

import (
	"context"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Hub rules (2026-11-08):
//   - a host alone is its own hub;
//   - a new hub is picked only when the hub connection is lost, when groups rejoin, or when the user asks;
//   - test = speed from each host to the client that communicated most recently; no client visible = internet
//     test; no client and no internet = the host up longest; results within 20% = the "prefer" host wins.

const startupGrace = 10 * time.Second

// groupLocked: hosts reachable now (heard from within 2.5 heartbeats), including this one.
func (m *Mesh) groupLocked() []string {
	g := []string{m.id.ID}
	fresh := m.beat()*5/2 + 2*time.Second
	for id, p := range m.peers {
		if time.Since(p.LastOK) < fresh {
			g = append(g, id)
		}
	}
	return g
}

func (m *Mesh) bootOfLocked(id string) int64 {
	if id == m.id.ID {
		return m.boot
	}
	if p := m.peers[id]; p != nil {
		return p.Hello.Boot
	}
	return 1 << 62
}

// coordinatorLocked: the host up longest in the group runs elections.
func (m *Mesh) coordinatorLocked() string {
	g := m.groupLocked()
	sort.Slice(g, func(i, j int) bool {
		bi, bj := m.bootOfLocked(g[i]), m.bootOfLocked(g[j])
		if bi != bj {
			return bi < bj
		}
		return g[i] < g[j]
	})
	return g[0]
}

func (m *Mesh) mergedClientsLocked() map[string]ClientSeen {
	out := map[string]ClientSeen{}
	add := func(cs map[string]ClientSeen) {
		for id, s := range cs {
			o := out[id]
			if s.Last > o.Last {
				o.Last = s.Last
			}
			o.Live = o.Live || s.Live
			out[id] = o
		}
	}
	add(m.clientsSeen)
	for _, p := range m.peers {
		if time.Since(p.LastOK) < m.failover() {
			add(p.Hello.Clients)
		}
	}
	return out
}

// evaluate runs every 2 s: notices a lost hub and starts an election when this host is the coordinator.
func (m *Mesh) evaluate(ctx context.Context) {
	m.mu.Lock()
	if m.hub != "" && !m.hubAliveLocked(m.hub) {
		log.Printf("hub %s lost (silent > %s)", m.hub, m.failover())
		m.hub = ""
		m.changed("hub")
	}
	need := ""
	switch {
	case m.electing || m.joining != nil:
	case m.hub == "" && time.Since(m.started) > startupGrace:
		need = "no hub"
	case m.testReq > m.hubAt && m.testReq > m.lastElect && m.hub != "":
		need = "test requested"
	}
	coord := m.coordinatorLocked() == m.id.ID
	if need == "" || !coord {
		m.mu.Unlock()
		return
	}
	m.electing = true
	// test requests carry the asking host's clock; one from a host whose clock runs ahead must still count as
	// handled once this election starts, or it triggers a new election every few seconds until our clock catches up
	m.lastElect = max(time.Now().UnixMilli(), m.testReq)
	m.mu.Unlock()
	go func() {
		defer func() { m.mu.Lock(); m.electing = false; m.mu.Unlock() }()
		m.elect(ctx, need)
	}()
}

type candidate struct {
	ID     string
	Boot   int64
	Prefer bool
	Addrs  []string
}

func (m *Mesh) candidates() []candidate {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all, ok []candidate
	for _, id := range m.groupLocked() {
		c := candidate{ID: id, Boot: m.bootOfLocked(id)}
		never := false
		if id == m.id.ID {
			c.Prefer, never, c.Addrs = m.cfg.Live().PreferHub, m.cfg.Live().NeverHub, LocalAddrs(m.cfg)
		} else {
			p := m.peers[id]
			c.Prefer, never, c.Addrs = p.Hello.Prefer, p.Hello.NeverHub, dedupe(append([]string{p.Active}, p.Addrs...))
		}
		all = append(all, c)
		if !never {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return all // every host said "never hub": someone still has to do it
	}
	return ok
}

func (m *Mesh) elect(ctx context.Context, why string) {
	cands := m.candidates()
	results := map[string]float64{}
	how := "only host"
	if len(cands) > 1 {
		results, how = m.clientTest(ctx, cands)
		if len(results) == 0 {
			results, how = m.internetTest(ctx, cands), "internet test"
		}
	}
	winner := Pick(cands, results)
	m.mu.Lock()
	epoch := m.epoch
	for _, p := range m.peers {
		if p.Hello.Epoch > epoch {
			epoch = p.Hello.Epoch
		}
	}
	epoch++
	old := m.hub
	m.hub, m.epoch, m.hubBy = winner, epoch, m.id.ID
	m.hubAt = m.lastElect
	m.mu.Unlock()
	m.save()
	log.Printf("election (%s, %s): hub %s, epoch %d, results %v", why, how, winner, epoch, results)
	if old != winner {
		m.changed("hub")
	}
	m.announce(ctx)
}

// Pick applies the design rules to test results (ms, lower is better; missing = failed).
func Pick(cands []candidate, results map[string]float64) string {
	if len(cands) == 0 {
		return ""
	}
	var best float64 = -1
	for _, c := range cands {
		if r, ok := results[c.ID]; ok && r > 0 && (best < 0 || r < best) {
			best = r
		}
	}
	longest := func(cs []candidate) string {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Boot != cs[j].Boot {
				return cs[i].Boot < cs[j].Boot
			}
			return cs[i].ID < cs[j].ID
		})
		return cs[0].ID
	}
	if best < 0 {
		return longest(append([]candidate{}, cands...))
	}
	var close []candidate
	bestID := ""
	for _, c := range cands {
		r, ok := results[c.ID]
		if !ok || r <= 0 {
			continue
		}
		if r <= best*1.2 {
			close = append(close, c)
		}
		if r == best && (bestID == "" || c.ID < bestID) {
			bestID = c.ID
		}
	}
	var pref []candidate
	for _, c := range close {
		if c.Prefer {
			pref = append(pref, c)
		}
	}
	if len(pref) > 0 {
		sort.Slice(pref, func(i, j int) bool { return results[pref[i].ID] < results[pref[j].ID] })
		return pref[0].ID
	}
	return bestID
}

// clientTest: the most recent client with a live events socket measures every candidate.
func (m *Mesh) clientTest(ctx context.Context, cands []candidate) (map[string]float64, string) {
	m.mu.Lock()
	seen := m.mergedClientsLocked()
	newest, newestT := "", int64(0)
	for id, s := range seen {
		if _, member := m.Log.Get(id); member && s.Last > newestT {
			newest, newestT = id, s.Last
		}
	}
	liveAt := ""
	if newest != "" {
		if m.clientsSeen[newest].Live {
			liveAt = m.id.ID
		} else {
			for pid, p := range m.peers {
				if time.Since(p.LastOK) < m.failover() && p.Hello.Clients[newest].Live {
					liveAt = pid
					break
				}
			}
		}
	}
	m.mu.Unlock()
	if liveAt == "" {
		return nil, "no live client"
	}
	hosts := make([]HostAddr, 0, len(cands))
	for _, c := range cands {
		hosts = append(hosts, HostAddr{ID: c.ID, Addrs: c.Addrs})
	}
	c, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var res map[string]float64
	var err error
	if liveAt == m.id.ID {
		if m.ClientTester == nil {
			return nil, "no tester"
		}
		res, err = m.ClientTester(c, newest, hosts)
	} else {
		addr, pin, ok := m.PeerTarget(liveAt)
		if !ok {
			return nil, "client host unreachable"
		}
		var out struct {
			Results map[string]float64 `json:"results"`
		}
		err = m.call(c, addr, pin, liveAt, "POST", "/mesh/clienttest", map[string]any{"client": newest, "hosts": hosts}, &out)
		res = out.Results
	}
	if err != nil {
		log.Printf("client speed test failed: %v", err)
		return nil, "client test failed"
	}
	return res, "client test (" + newest + ")"
}

// internetTest: every candidate times a small download from the internet.
func (m *Mesh) internetTest(ctx context.Context, cands []candidate) map[string]float64 {
	out := map[string]float64{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range cands {
		wg.Add(1)
		go func(c candidate) {
			defer wg.Done()
			var ms float64
			if c.ID == m.id.ID {
				ms = m.NetTest(ctx)
			} else if addr, pin, ok := m.PeerTarget(c.ID); ok {
				var r struct {
					MS float64 `json:"ms"`
				}
				cc, cancel := context.WithTimeout(ctx, 15*time.Second)
				if m.call(cc, addr, pin, c.ID, "POST", "/mesh/nettest", nil, &r) == nil {
					ms = r.MS
				}
				cancel()
			}
			if ms > 0 {
				mu.Lock()
				out[c.ID] = ms
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return out
}

var netClient = &http.Client{Timeout: 4 * time.Second} // 3 tries stay under the 15 s the coordinator waits

// NetTest times a small download from net_test_url (ms; 0 = no internet).
// It keeps the best of three tries: one slow sample (seen up to 3.5 s on a LAN that normally answers in 35 ms)
// would otherwise decide the hub.
func (m *Mesh) NetTest(ctx context.Context) float64 {
	best := 0.0
	for i := 0; i < 3; i++ {
		if ms := m.netTestOnce(ctx); ms > 0 && (best == 0 || ms < best) {
			best = ms
		}
	}
	return best
}

func (m *Mesh) netTestOnce(ctx context.Context) float64 {
	req, err := http.NewRequestWithContext(ctx, "GET", m.cfg.NetTestURL, nil)
	if err != nil {
		return 0
	}
	t0 := time.Now()
	resp, err := netClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil || resp.StatusCode != 200 {
		return 0
	}
	return float64(time.Since(t0).Microseconds()) / 1000
}

// announce pushes the hub choice to every reachable host right away.
func (m *Mesh) announce(ctx context.Context) {
	m.mu.Lock()
	body := map[string]any{"hub": m.hub, "epoch": m.epoch, "by": m.hubBy, "at": m.hubAt}
	var ids []string
	for id, p := range m.peers {
		if time.Since(p.LastOK) < m.failover() {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		go func(id string) {
			if addr, pin, ok := m.PeerTarget(id); ok {
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				m.call(c, addr, pin, id, "POST", "/mesh/announce", body, nil)
				cancel()
			}
		}(id)
	}
}
