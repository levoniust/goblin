package mesh

import (
	"testing"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// A host this one can't reach (two laptops on different networks, no Tailscale on one side) is shown as reachable
// through the peer that hears from it, the hub first.
func TestViaHub(t *testing.T) {
	m := &Mesh{cfg: &core.Config{FailoverSeconds: 90}, hub: "hub", peers: map[string]*Peer{
		"hub":   {LastOK: time.Now(), Hello: Hello{Reach: []string{"far", "other"}}},
		"other": {LastOK: time.Now(), Hello: Hello{Reach: []string{"far"}}},
		"stale": {LastOK: time.Now().Add(-time.Hour), Hello: Hello{Reach: []string{"lost"}}},
	}}
	if v := m.viaLocked("far"); v != "hub" {
		t.Fatalf("via far = %q, want hub", v)
	}
	if v := m.viaLocked("lost"); v != "" {
		t.Fatalf("a peer that is itself silent must not vouch: %q", v)
	}
	if v := m.viaLocked("nobody"); v != "" {
		t.Fatalf("via nobody = %q", v)
	}
}
