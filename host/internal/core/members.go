package core

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one signed entry of the member log. T = "add" or "remove".
// add:    a host or client joins (or updates its name / certificate pin). Counts if signed by a current member.
// remove: a member is cut off for good. Accepted if its signer is a current member when it arrives; permanent.
type Event struct {
	T    string `json:"t"`
	Kind string `json:"kind,omitempty"` // host | client (add only)
	ID   string `json:"id"`
	Pub  string `json:"pub,omitempty"`  // add only
	Name string `json:"name,omitempty"` // add only
	Pin  string `json:"pin,omitempty"`  // hosts: certificate sha256
	TS   int64  `json:"ts"`             // unix ms
	By   string `json:"by"`
	Sig  string `json:"sig"`
}

// Text is the exact signed form (Go and Dart build it the same way; never JSON).
func (e Event) Text(net string) string {
	return strings.Join([]string{"goblin-ev1", net, e.T, e.Kind, e.ID, e.Pub, e.Name, e.Pin, strconv.FormatInt(e.TS, 10), e.By}, "|")
}

// Key identifies an event (dedupe, log hash).
func (e Event) Key(net string) string {
	h := sha256.Sum256([]byte(e.Text(net) + "|" + e.Sig))
	return hex.EncodeToString(h[:12])
}

// Member is a current (valid) member.
type Member struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Pub   string `json:"pub"`
	Pin   string `json:"pin,omitempty"`
	Added int64  `json:"added"`
	By    string `json:"by"`
}

// Log is the network's member log. Safe for concurrent use.
type Log struct {
	mu      sync.RWMutex
	path    string
	Net     string  `json:"net"`
	Events  []Event `json:"events"`
	keys    map[string]bool
	members map[string]Member
	removed map[string]bool
}

type logFile struct {
	Net    string  `json:"net"`
	Events []Event `json:"events"`
}

var ErrUnknownSigner = errors.New("signer is not a member")

// NewLog starts a network with this host as its first member (genesis). Network id = this host's member id.
func NewLog(path string, id *Identity, name string) (*Log, error) {
	l := &Log{path: path, Net: id.ID}
	l.reindex()
	e := Event{T: "add", Kind: "host", ID: id.ID, Pub: B64(id.Pub), Name: CleanName(name), Pin: id.Pin, TS: time.Now().UnixMilli(), By: id.ID}
	e.Sig = id.Sign(e.Text(l.Net))
	if _, err := l.Merge([]Event{e}); err != nil {
		return nil, err
	}
	return l, l.Save()
}

// LoadLog reads the member log; ok=false when there is none yet.
func LoadLog(path string) (*Log, bool, error) {
	var f logFile
	if err := ReadJSON(path, &f); err != nil {
		return nil, false, err
	}
	if f.Net == "" {
		return nil, false, nil
	}
	l := &Log{path: path, Net: f.Net}
	l.reindex()
	// replay in stored order: removals were accepted in this order before
	if _, err := l.Merge(f.Events); err != nil {
		return nil, false, err
	}
	return l, true, nil
}

// Adopt replaces this log with another network's log (a host that joined another network).
func (l *Log) Adopt(net string, events []Event) error {
	n := &Log{Net: net}
	n.reindex()
	if _, err := n.Merge(events); err != nil {
		return err
	}
	l.mu.Lock()
	l.Net, l.Events, l.keys, l.members, l.removed = n.Net, n.Events, n.keys, n.members, n.removed
	l.mu.Unlock()
	return l.Save()
}

// Save writes the log file.
func (l *Log) Save() error {
	if l.path == "" {
		return nil
	}
	l.mu.RLock()
	f := logFile{Net: l.Net, Events: append([]Event(nil), l.Events...)}
	l.mu.RUnlock()
	return WriteJSON(l.path, f)
}

func (l *Log) reindex() {
	l.keys = map[string]bool{}
	l.members = map[string]Member{}
	l.removed = map[string]bool{}
}

// pubOf finds the public key of a signer from any add event for it (valid or not).
func (l *Log) pubOf(id string, batch []Event) ed25519.PublicKey {
	try := func(es []Event) ed25519.PublicKey {
		for _, e := range es {
			if e.T == "add" && e.ID == id {
				if p, err := UnB64(e.Pub); err == nil && len(p) == ed25519.PublicKeySize && MemberID(p) == id {
					return p
				}
			}
		}
		return nil
	}
	if p := try(l.Events); p != nil {
		return p
	}
	return try(batch)
}

// Merge adds events (from a peer, a client, or this host). Bad or unknown ones are skipped.
// It returns how many were new. Call Save afterwards if it was > 0 (Merge itself does not save).
func (l *Log) Merge(in []Event) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	added := 0
	pending := append([]Event(nil), in...)
	for progress := true; progress && len(pending) > 0; {
		progress = false
		var rest []Event
		for _, e := range pending {
			k := e.Key(l.Net)
			if l.keys[k] {
				continue
			}
			ok, retry := l.check(e, pending)
			if !ok {
				if retry {
					rest = append(rest, e)
				}
				continue
			}
			l.keys[k] = true
			l.Events = append(l.Events, e)
			added++
			progress = true
			l.recompute()
		}
		pending = rest
	}
	return added, nil
}

// check verifies an event against the current state. retry=true: might verify once other events are in.
func (l *Log) check(e Event, batch []Event) (ok, retry bool) {
	switch e.T {
	case "add":
		if e.Kind != "host" && e.Kind != "client" {
			return false, false
		}
		p, err := UnB64(e.Pub)
		if err != nil || len(p) != ed25519.PublicKeySize || MemberID(p) != e.ID || CleanName(e.Name) != e.Name {
			return false, false
		}
		if isGenesis := e.By == e.ID && e.ID == l.Net; !isGenesis {
			if _, valid := l.members[e.By]; !valid {
				return false, true // signer not a member (yet): may count once other events are in
			}
		}
		var signer ed25519.PublicKey
		if e.By == e.ID {
			signer = p
		} else {
			signer = l.pubOf(e.By, batch)
		}
		if signer == nil || !verify(signer, e.Text(l.Net), e.Sig) {
			return false, false
		}
		return true, false
	case "remove":
		if _, valid := l.members[e.By]; !valid {
			return false, true
		}
		signer := l.pubOf(e.By, batch)
		if signer == nil || !verify(signer, e.Text(l.Net), e.Sig) {
			return false, false
		}
		return true, false
	}
	return false, false
}

func verify(pub ed25519.PublicKey, text, sig string) bool {
	s, err := UnB64(sig)
	return err == nil && len(s) == ed25519.SignatureSize && ed25519.Verify(pub, []byte(text), s)
}

// recompute replays the log in the order events were accepted: an add counts if its signer is a member at that
// point; a removal cuts its target off for good. Removing a member does not undo what it approved earlier
// (remove those separately), so one removal can never take the whole network down with it.
func (l *Log) recompute() {
	valid := map[string]Member{}
	removed := map[string]bool{}
	for _, e := range l.Events {
		switch e.T {
		case "add":
			if removed[e.ID] {
				continue
			}
			_, byValid := valid[e.By]
			if !(e.ID == l.Net && e.By == l.Net) && !byValid {
				continue
			}
			if m, had := valid[e.ID]; had {
				if e.TS > m.Added {
					m.Name, m.Pin, m.Added = e.Name, e.Pin, e.TS
					valid[e.ID] = m
				}
			} else {
				valid[e.ID] = Member{ID: e.ID, Kind: e.Kind, Name: e.Name, Pub: e.Pub, Pin: e.Pin, Added: e.TS, By: e.By}
			}
		case "remove":
			if _, ok := valid[e.By]; ok {
				removed[e.ID] = true
				delete(valid, e.ID)
			}
		}
	}
	l.members, l.removed = valid, removed
}

// Members lists current members (hosts first, then by name).
func (l *Log) Members() []Member {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Member, 0, len(l.members))
	for _, m := range l.members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "host"
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get returns a current member.
func (l *Log) Get(id string) (Member, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m, ok := l.members[id]
	return m, ok
}

// Removed reports whether id was cut off.
func (l *Log) Removed(id string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.removed[id]
}

// All returns a copy of every event (for syncing).
func (l *Log) All() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Event(nil), l.Events...)
}

// NetID returns the network id.
func (l *Log) NetID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.Net
}

// Hash summarizes the log; equal hashes = nothing to sync.
func (l *Log) Hash() string {
	l.mu.RLock()
	ks := make([]string, 0, len(l.keys))
	for k := range l.keys {
		ks = append(ks, k)
	}
	l.mu.RUnlock()
	sort.Strings(ks)
	h := sha256.Sum256([]byte(strings.Join(ks, ",")))
	return hex.EncodeToString(h[:8])
}

// Signer can sign log events (a host's Identity, or a test key).
type Signer interface {
	Sign(text string) string
}

// AddMember signs an add event with this host's identity and stores it.
func (l *Log) AddMember(by *Identity, kind, pub, name, pin string) (Event, error) {
	p, err := UnB64(pub)
	if err != nil || len(p) != ed25519.PublicKeySize {
		return Event{}, errors.New("bad public key")
	}
	e := Event{T: "add", Kind: kind, ID: MemberID(p), Pub: pub, Name: CleanName(name), Pin: pin, TS: time.Now().UnixMilli(), By: by.ID}
	e.Sig = by.Sign(e.Text(l.NetID()))
	n, err := l.Merge([]Event{e})
	if err != nil {
		return e, err
	}
	if n == 0 {
		return e, fmt.Errorf("the add for %s was not accepted", e.ID)
	}
	return e, l.Save()
}

// RemoveMember cuts a member off for good (what it approved earlier stays).
func (l *Log) RemoveMember(by *Identity, target string) error {
	if target == by.ID {
		return errors.New("a host can't remove itself; remove it from another member")
	}
	e := Event{T: "remove", ID: target, TS: time.Now().UnixMilli(), By: by.ID}
	e.Sig = by.Sign(e.Text(l.NetID()))
	if n, _ := l.Merge([]Event{e}); n == 0 {
		return fmt.Errorf("could not remove %s", target)
	}
	return l.Save()
}
