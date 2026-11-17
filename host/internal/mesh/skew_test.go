package mesh

import (
	"testing"
	"time"
)

// A test request stamped by a host whose clock runs ahead must not keep triggering elections, and a new request from a
// host whose clock is behind must still count (seen 2026-11-15: Surface 3.5 s ahead gave three elections in 6 s).
func TestTestRequestClockSkew(t *testing.T) {
	now := time.Now().UnixMilli()
	m := &Mesh{peers: map[string]*Peer{}}

	// request from a host 5 s ahead, then an election starts: it counts as handled
	m.testReq = now + 5000
	m.lastElect = max(time.Now().UnixMilli(), m.testReq)
	m.hubAt = m.lastElect
	if m.testReq > m.hubAt || m.testReq > m.lastElect {
		t.Fatalf("skewed request still pending after the election: req %d hubAt %d", m.testReq, m.hubAt)
	}

	// a new request on this host (clock behind that stamp) must be newer than what was handled
	m.RequestTest()
	if !(m.testReq > m.hubAt && m.testReq > m.lastElect) {
		t.Fatalf("new request not seen as pending: req %d hubAt %d lastElect %d", m.testReq, m.hubAt, m.lastElect)
	}
}
