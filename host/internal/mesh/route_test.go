package mesh

import (
	"testing"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// A relay hands a request for a host it can't reach to a peer that can connect to it: the hub first, never a peer
// already on the route, never a silent peer or one this host can't connect to itself; 0.2 peers fall back to reach.
func TestRelayPeer(t *testing.T) {
	now := time.Now()
	m := &Mesh{cfg: &core.Config{FailoverSeconds: 90}, hub: "hub", peers: map[string]*Peer{
		"hub":     {Active: "h:1", LastOK: now, Hello: Hello{Out: []string{"far"}, Reach: []string{"far", "deaf"}}},
		"b":       {Active: "b:1", LastOK: now, Hello: Hello{Out: []string{"far", "deep"}}},
		"a":       {Active: "a:1", LastOK: now, Hello: Hello{Out: []string{"deep"}}},
		"old":     {Active: "o:1", LastOK: now, Hello: Hello{Reach: []string{"legacy"}}},
		"inbound": {LastOK: now, Hello: Hello{Out: []string{"hidden"}}},
		"stale":   {Active: "s:1", LastOK: now.Add(-time.Hour), Hello: Hello{Out: []string{"lost"}}},
	}}
	cases := []struct {
		target string
		avoid  []string
		want   string
	}{
		{"far", nil, "hub"},              // hub preferred
		{"far", []string{"hub"}, "b"},    // hub already on the route
		{"deep", nil, "a"},               // lowest id among non-hub peers
		{"deaf", nil, ""},                // the hub only hears from it: Out wins over Reach
		{"legacy", nil, "old"},           // 0.2 peer: Reach used
		{"hidden", nil, ""},              // can't connect to that peer ourselves
		{"lost", nil, ""},                // silent peer can't vouch
		{"deep", []string{"a", "b"}, ""}, // every candidate already on the route
	}
	for _, c := range cases {
		if got := m.relayPeerLocked(c.target, c.avoid); got != c.want {
			t.Errorf("relay peer for %s (avoid %v) = %q, want %q", c.target, c.avoid, got, c.want)
		}
	}
}
