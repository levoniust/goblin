package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// pinned returns an HTTP client that only talks to a server whose certificate has this sha256 pin
// ("" = accept any certificate: used only to read another host's public beacon info before joining).
type pinnedClients struct {
	mu sync.Mutex
	m  map[string]*http.Client
}

func (p *pinnedClients) get(pin string) *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]*http.Client{}
	}
	if c, ok := p.m[pin]; ok {
		return c
	}
	tc := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	if pin != "" {
		want := pin
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("certificate pin mismatch")
			}
			return nil
		}
	}
	tr := &http.Transport{
		TLSClientConfig:     tc,
		DialContext:         (&net.Dialer{Timeout: 4 * time.Second, KeepAlive: 20 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	c := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	p.m[pin] = c
	return c
}

// Transport returns the pinned transport (the relay's reverse proxy uses it).
func (p *pinnedClients) Transport(pin string) http.RoundTripper { return p.get(pin).Transport }

// call does a signed JSON request to another host and decodes the answer into out (if not nil).
func (m *Mesh) call(ctx context.Context, addr, pin, target, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if target != "" {
		req.Header.Set(core.AuthHeader, m.id.SignRequest(target, method, path))
	}
	resp, err := m.clients.get(pin).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(data[:min(len(data), 200)]))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
