package core

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newHost(t *testing.T) (*Identity, *Log) {
	t.Helper()
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir, "h")
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewLog(filepath.Join(dir, "members.json"), id, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	return id, l
}

func TestGenesisAndAdd(t *testing.T) {
	h, l := newHost(t)
	if l.Net != h.ID || len(l.Members()) != 1 {
		t.Fatalf("genesis: %+v", l.Members())
	}
	phone := NewKeyOnly()
	if _, err := l.AddMember(h, "client", B64(phone.Pub), "S23", ""); err != nil {
		t.Fatal(err)
	}
	// the phone approves a second host
	h2 := NewKeyOnly()
	e := Event{T: "add", Kind: "host", ID: h2.ID, Pub: B64(h2.Pub), Name: "beta", Pin: "ab", TS: 5, By: phone.ID}
	e.Sig = phone.Sign(e.Text(l.Net))
	if n, _ := l.Merge([]Event{e}); n != 1 {
		t.Fatal("phone-signed add not accepted")
	}
	l.Save()
	if _, ok := l.Get(h2.ID); !ok {
		t.Fatal("beta missing")
	}
	// reload from disk keeps everything
	l2, ok, err := LoadLog(l.path)
	if err != nil || !ok || len(l2.Members()) != 3 || l2.Hash() != l.Hash() {
		t.Fatalf("reload: ok=%v err=%v n=%d", ok, err, len(l2.Members()))
	}
}

func TestForgedAndStrangerEventsRejected(t *testing.T) {
	h, l := newHost(t)
	stranger := NewKeyOnly()
	victim := NewKeyOnly()
	e := Event{T: "add", Kind: "client", ID: victim.ID, Pub: B64(victim.Pub), Name: "x", TS: 1, By: stranger.ID}
	e.Sig = stranger.Sign(e.Text(l.Net))
	if n, _ := l.Merge([]Event{e}); n != 0 {
		t.Fatal("stranger could add a member")
	}
	e2 := Event{T: "add", Kind: "client", ID: victim.ID, Pub: B64(victim.Pub), Name: "x", TS: 1, By: h.ID, Sig: stranger.Sign("nope")}
	if n, _ := l.Merge([]Event{e2}); n != 0 {
		t.Fatal("bad signature accepted")
	}
	// id that doesn't match the key
	e3 := Event{T: "add", Kind: "client", ID: "0000000000000000", Pub: B64(victim.Pub), Name: "x", TS: 1, By: h.ID}
	e3.Sig = h.Sign(e3.Text(l.Net))
	if n, _ := l.Merge([]Event{e3}); n != 0 {
		t.Fatal("mismatched id accepted")
	}
	// a removal signed by a non-member does nothing
	r := Event{T: "remove", ID: h.ID, TS: 2, By: stranger.ID}
	r.Sig = stranger.Sign(r.Text(l.Net))
	l.Merge([]Event{r})
	if _, ok := l.Get(h.ID); !ok {
		t.Fatal("stranger removed the genesis host")
	}
}

func TestOutOfOrderSync(t *testing.T) {
	h, l := newHost(t)
	phone := NewKeyOnly()
	l.AddMember(h, "client", B64(phone.Pub), "S23", "")
	h2 := NewKeyOnly()
	e := Event{T: "add", Kind: "host", ID: h2.ID, Pub: B64(h2.Pub), Name: "beta", TS: 9, By: phone.ID}
	e.Sig = phone.Sign(e.Text(l.Net))
	l.Merge([]Event{e})
	all := l.All()
	// a fresh copy receiving the events newest first still ends with the same members
	rev := make([]Event, len(all))
	for i := range all {
		rev[i] = all[len(all)-1-i]
	}
	c := &Log{Net: l.Net}
	c.reindex()
	c.Merge(rev)
	if len(c.Members()) != 3 || c.Hash() != l.Hash() {
		t.Fatalf("out of order: %d members", len(c.Members()))
	}
}

func TestRemoveKeepsWhatTheRemovedMemberAdded(t *testing.T) {
	h, l := newHost(t)
	phone := NewKeyOnly()
	l.AddMember(h, "client", B64(phone.Pub), "lost phone", "")
	h2 := NewKeyOnly()
	e := Event{T: "add", Kind: "host", ID: h2.ID, Pub: B64(h2.Pub), Name: "beta", TS: 9, By: phone.ID}
	e.Sig = phone.Sign(e.Text(l.Net))
	l.Merge([]Event{e})
	if err := l.RemoveMember(h, phone.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Get(phone.ID); ok || !l.Removed(phone.ID) {
		t.Fatal("phone still a member")
	}
	if _, ok := l.Get(h2.ID); !ok {
		t.Fatal("host added by the removed phone fell off")
	}
	// the removed phone can no longer add anyone
	x := NewKeyOnly()
	e2 := Event{T: "add", Kind: "client", ID: x.ID, Pub: B64(x.Pub), Name: "thief", TS: 99, By: phone.ID}
	e2.Sig = phone.Sign(e2.Text(l.Net))
	if n, _ := l.Merge([]Event{e2}); n != 0 {
		if _, ok := l.Get(x.ID); ok {
			t.Fatal("removed member added someone")
		}
	}
	// another host that syncs the whole log agrees
	c := &Log{Net: l.Net}
	c.reindex()
	c.Merge(l.All())
	if _, ok := c.Get(phone.ID); ok {
		t.Fatal("sync brought the phone back")
	}
	if _, ok := c.Get(h2.ID); !ok {
		t.Fatal("sync lost beta")
	}
}

func TestSignedRequests(t *testing.T) {
	h, l := newHost(t)
	phone := NewKeyOnly()
	l.AddMember(h, "client", B64(phone.Pub), "S23", "")
	v := NewVerifier(h.ID, l)
	r := httptest.NewRequest("GET", "/api/sessions?x=1", nil)
	r.Header.Set(AuthHeader, phone.SignRequest(h.ID, "GET", "/api/sessions?x=1"))
	if m, err := v.Check(r, URIWithoutAuth(r)); err != nil || m.ID != phone.ID {
		t.Fatalf("valid request refused: %v", err)
	}
	if _, err := v.Check(r, URIWithoutAuth(r)); err == nil {
		t.Fatal("replay accepted")
	}
	r2 := httptest.NewRequest("GET", "/api/sessions", nil)
	r2.Header.Set(AuthHeader, phone.SignRequest("someotherhost000", "GET", "/api/sessions"))
	if _, err := v.Check(r2, URIWithoutAuth(r2)); err == nil {
		t.Fatal("request for another host accepted")
	}
	// signature in the query (websockets)
	sig := phone.SignRequest(h.ID, "GET", "/api/events")
	r3 := httptest.NewRequest("GET", "/api/events?goblin_auth="+sig, nil)
	if _, err := v.Check(r3, URIWithoutAuth(r3)); err != nil {
		t.Fatalf("query auth: %v", err)
	}
	l.RemoveMember(h, phone.ID)
	r4 := httptest.NewRequest("GET", "/api/sessions", nil)
	r4.Header.Set(AuthHeader, phone.SignRequest(h.ID, "GET", "/api/sessions"))
	if _, err := v.Check(r4, URIWithoutAuth(r4)); err != ErrRevoked {
		t.Fatalf("revoked phone: %v", err)
	}
}

func TestPIN(t *testing.T) {
	d := t.TempDir()
	if HasPIN(d) || CheckPIN(d, "1234") {
		t.Fatal("no PIN yet")
	}
	if err := SetPIN(d, "4821"); err != nil {
		t.Fatal(err)
	}
	if !CheckPIN(d, "4821") || CheckPIN(d, "4822") {
		t.Fatal("PIN check wrong")
	}
}
