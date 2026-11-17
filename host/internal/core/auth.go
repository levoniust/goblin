package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AuthHeader carries a signed request: "<member_id>.<unix_ms>.<nonce>.<sig>".
// sig = Ed25519 over "goblin-req1|<target host id>|<METHOD>|<path?query>|<unix_ms>|<nonce>".
// Including the target host means a relaying hub can't replay a request to a different host.
const AuthHeader = "X-Goblin-Auth"

// AuthQuery is the same value as a query parameter (for clients that can't set websocket headers).
const AuthQuery = "goblin_auth"

const authSkew = 120 * time.Second

func reqText(target, method, uri, ts, nonce string) string {
	return strings.Join([]string{"goblin-req1", target, method, uri, ts, nonce}, "|")
}

// SignRequest builds the header value for a request this host sends to target.
func (id *Identity) SignRequest(target, method, uri string) string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nb := make([]byte, 9)
	rand.Read(nb)
	nonce := B64(nb)
	return strings.Join([]string{id.ID, ts, nonce, id.Sign(reqText(target, method, uri, ts, nonce))}, ".")
}

// Verifier checks signed requests against the member log and remembers nonces.
type Verifier struct {
	Self string // this host's member id (the target the signature must name)
	Log  *Log
	mu   sync.Mutex
	seen map[string]time.Time
}

func NewVerifier(self string, log *Log) *Verifier {
	return &Verifier{Self: self, Log: log, seen: map[string]time.Time{}}
}

var (
	ErrNoAuth  = errors.New("not signed")
	ErrBadAuth = errors.New("bad signature")
	ErrRevoked = errors.New("this device was removed from the network")
)

// URIWithoutAuth is the request URI as signed: path + query, minus the goblin_auth parameter.
func URIWithoutAuth(r *http.Request) string {
	q := r.URL.Query()
	if q.Has(AuthQuery) {
		q.Del(AuthQuery)
		if len(q) == 0 {
			return r.URL.Path
		}
		return r.URL.Path + "?" + q.Encode()
	}
	return r.URL.RequestURI()
}

// Check returns the calling member, or an error. uri = path + query as the caller signed it.
func (v *Verifier) Check(r *http.Request, uri string) (Member, error) {
	return v.CheckFor(r, uri, v.Self)
}

// CheckFor verifies a request signed for another host (the relay checks before forwarding, so it is no open proxy).
func (v *Verifier) CheckFor(r *http.Request, uri, target string) (Member, error) {
	h := r.Header.Get(AuthHeader)
	if h == "" {
		h = r.URL.Query().Get(AuthQuery)
	}
	if h == "" {
		return Member{}, ErrNoAuth
	}
	p := strings.Split(h, ".")
	if len(p) != 4 {
		return Member{}, ErrBadAuth
	}
	id, ts, nonce, sig := p[0], p[1], p[2], p[3]
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Member{}, ErrBadAuth
	}
	if d := time.Since(time.UnixMilli(ms)); d > authSkew || d < -authSkew {
		return Member{}, errors.New("clock differs by more than 2 minutes (check the device time)")
	}
	m, ok := v.Log.Get(id)
	if !ok {
		if v.Log.Removed(id) {
			return Member{}, ErrRevoked
		}
		return Member{}, ErrBadAuth
	}
	pub, err := UnB64(m.Pub)
	s, err2 := UnB64(sig)
	if err != nil || err2 != nil || len(s) != ed25519.SignatureSize ||
		!ed25519.Verify(pub, []byte(reqText(target, r.Method, uri, ts, nonce)), s) {
		return Member{}, ErrBadAuth
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if len(v.seen) > 20000 {
		for k, t := range v.seen {
			if now.Sub(t) > 2*authSkew {
				delete(v.seen, k)
			}
		}
	}
	key := target + "." + id + "." + nonce
	if _, dup := v.seen[key]; dup {
		return Member{}, errors.New("request replayed")
	}
	v.seen[key] = now
	return m, nil
}
