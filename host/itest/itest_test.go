// Integration test: three real goblin processes on this machine + a test client.
// go test ./itest/ -v   (skipped with -short; needs tmux; takes about a minute)
package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/levoniust/goblin/host/internal/core"
)

type host struct {
	name, home string
	port       int
	cmd        *exec.Cmd
	out        *bytes.Buffer
	id, pin    string
}

var bin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "goblin-itest-bin")
	bin = filepath.Join(dir, "goblin")
	b := exec.Command("go", "build", "-o", bin, "../cmd/goblin")
	b.Stdout, b.Stderr = os.Stdout, os.Stderr
	if err := b.Run(); err != nil {
		fmt.Println("build failed:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newHost(t *testing.T, name string, port int, extra map[string]any) *host {
	home := t.TempDir()
	cfg := map[string]any{"name": name, "port": port, "beacon_port": 47899, "tmux_session": "gt-" + name + fmt.Sprint(time.Now().Unix()%100000),
		"heartbeat_seconds": 1, "failover_seconds": 6, "ui_port": port + 100, "tailscale": false, "roots": []string{home},
		"net_test_url": "https://127.0.0.1:1/none"} // no internet test in the test: client test or longest-up
	for k, v := range extra {
		cfg[k] = v
	}
	b, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(home, "config.json"), b, 0o600)
	if err := core.SetPIN(home, "4821"); err != nil {
		t.Fatal(err)
	}
	h := &host{name: name, home: home, port: port}
	id, err := core.LoadOrCreateIdentity(home, name)
	if err != nil {
		t.Fatal(err)
	}
	h.id, h.pin = id.ID, id.Pin
	return h
}

func (h *host) start(t *testing.T) {
	h.out = &bytes.Buffer{}
	h.cmd = exec.Command(bin, "run")
	h.cmd.Env = append(os.Environ(), "GOBLIN_HOME="+h.home)
	h.cmd.Stdout, h.cmd.Stderr = h.out, h.out
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.stop() })
}

func (h *host) stop() {
	if h.cmd != nil && h.cmd.Process != nil {
		h.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { h.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			h.cmd.Process.Kill()
		}
		h.cmd = nil
	}
	if cfg, err := core.LoadConfig(h.home); err == nil && cfg.TmuxSession != "" {
		exec.Command("tmux", "-L", "goblin-"+cfg.TmuxSession, "kill-server").Run()
	}
}

func (h *host) addr() string { return fmt.Sprintf("127.0.0.1:%d", h.port) }

// ---- test client (what the app does)

type client struct {
	id   *core.Identity
	pins map[string]string // host id -> pin
	mu   sync.Mutex
}

func pinned(pin string) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			s := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(s[:]) != pin {
				return errors.New("pin mismatch")
			}
			return nil
		}}}}
}

// do: a signed request for host `target`, sent to address `via` (direct) or through the relay when relayVia != "".
func (c *client) do(target, addr, pin, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+addr+path, rd)
	inner := path
	if strings.HasPrefix(path, "/relay/") {
		inner = "/" + strings.SplitN(path, "/", 4)[3]
	}
	req.Header.Set(core.AuthHeader, c.id.SignRequest(target, method, inner))
	resp, err := pinned(pin).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type netView struct {
	Net   string `json:"net"`
	Hub   string `json:"hub"`
	Epoch int64  `json:"epoch"`
	Hosts []struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Pin    string   `json:"pin"`
		Online bool     `json:"online"`
		Addrs  []string `json:"addrs"`
		Via    string   `json:"via"`
	} `json:"hosts"`
	Pending []struct {
		ID, Kind, Name, Pub, Pin, Code string
	} `json:"pending"`
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	A := newHost(t, "alpha", 18211, map[string]any{"prefer_hub": true})
	B := newHost(t, "beta", 18212, nil)
	C := newHost(t, "gamma", 18213, map[string]any{"never_hub": true})
	A.start(t)
	defer func() {
		if t.Failed() {
			for _, h := range []*host{A, B, C} {
				if h.out != nil {
					t.Logf("---- %s log:\n%s", h.name, h.out.String())
				}
			}
		}
	}()
	waitFor(t, "alpha up", 10*time.Second, func() bool {
		_, err := pinned(A.pin).Get("https://" + A.addr() + "/health")
		return err == nil
	})

	// 1. pair the phone with alpha (goblin pair needs the PIN)
	pc := exec.Command(bin, "pair")
	pc.Env = append(os.Environ(), "GOBLIN_HOME="+A.home)
	pc.Stdin = strings.NewReader("4821\n")
	out, err := pc.CombinedOutput()
	if err != nil {
		t.Fatalf("pair: %v %s", err, out)
	}
	link := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "goblin://pair/") {
			link = strings.TrimSpace(l)
		}
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(link, "goblin://pair/"))
	var pl struct {
		Net, ID, C, K string
	}
	json.Unmarshal(raw, &pl)
	if pl.C != A.pin || pl.ID != A.id {
		t.Fatalf("pair link: %s", raw)
	}
	phone := &client{id: core.NewKeyOnly(), pins: map[string]string{}}
	var pr struct {
		ID     string       `json:"id"`
		Events []core.Event `json:"events"`
	}
	req, _ := json.Marshal(map[string]string{"code": pl.K, "name": "test phone", "pub": core.B64(phone.id.Pub)})
	resp, err := pinned(A.pin).Post("https://"+A.addr()+"/api/pair", "application/json", bytes.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.ID != phone.id.ID {
		t.Fatalf("pairing failed: %+v", pr)
	}
	// a reused code fails
	resp, _ = pinned(A.pin).Post("https://"+A.addr()+"/api/pair", "application/json", bytes.NewReader(req))
	if resp.StatusCode != 403 {
		t.Fatalf("reused code: %d", resp.StatusCode)
	}

	// the client API contract (docs/PROTOCOL.md): product, major version and feature names
	var info struct {
		Product  string   `json:"product"`
		API      int      `json:"api"`
		Features []string `json:"features"`
	}
	if err := phone.do(A.id, A.addr(), A.pin, "GET", "/api/info", nil, &info); err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Product != "goblin" || info.API != core.API || !slices.Contains(info.Features, "sessions") || !slices.Contains(info.Features, "goblin1") {
		t.Fatalf("info contract: %+v", info)
	}

	// 2. beta and gamma start fresh, find alpha on the LAN and ask to join; the phone approves both
	B.start(t)
	C.start(t)
	var nv netView
	waitFor(t, "two join requests at alpha", 30*time.Second, func() bool {
		if phone.do(A.id, A.addr(), A.pin, "GET", "/api/network", nil, &nv) != nil {
			return false
		}
		return len(nv.Pending) == 2
	})
	var evs []core.Event
	for _, p := range nv.Pending {
		e := core.Event{T: "add", Kind: p.Kind, ID: p.ID, Pub: p.Pub, Name: p.Name, Pin: p.Pin, TS: time.Now().UnixMilli(), By: phone.id.ID}
		e.Sig = phone.id.Sign(e.Text(nv.Net))
		evs = append(evs, e)
	}
	if err := phone.do(A.id, A.addr(), A.pin, "POST", "/api/log", map[string]any{"events": evs}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three hosts online with one hub", 40*time.Second, func() bool {
		if phone.do(A.id, A.addr(), A.pin, "GET", "/api/network", nil, &nv) != nil || len(nv.Hosts) != 3 || nv.Hub == "" {
			return false
		}
		for _, h := range nv.Hosts {
			if !h.Online {
				return false
			}
		}
		var b netView
		return phone.do(B.id, B.addr(), B.pin, "GET", "/api/network", nil, &b) == nil && b.Hub == nv.Hub && len(b.Hosts) == 3
	})
	t.Logf("network formed, hub %s (alpha=%s)", nv.Hub, A.id)

	// 3. the phone keeps an events socket on beta and answers speed tests: beta is "fastest" to it
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tests := make(chan string, 4)
	go phone.events(ctx, B, func(hosts []string) map[string]float64 {
		r := map[string]float64{}
		for _, h := range hosts {
			switch h {
			case A.id:
				r[h] = 50
			case B.id:
				r[h] = 20
			case C.id:
				r[h] = 5 // fastest, but "never hub"
			}
		}
		tests <- "ok"
		return r
	})
	time.Sleep(2 * time.Second)
	if err := phone.do(A.id, A.addr(), A.pin, "POST", "/api/network/test", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tests:
	case <-time.After(30 * time.Second):
		t.Fatal("the phone never got a speed test")
	}
	waitFor(t, "beta becomes hub after the test", 20*time.Second, func() bool {
		var a, c netView
		return phone.do(A.id, A.addr(), A.pin, "GET", "/api/network", nil, &a) == nil && a.Hub == B.id &&
			phone.do(C.id, C.addr(), C.pin, "GET", "/api/network", nil, &c) == nil && c.Hub == B.id
	})

	// 4. relay: reach gamma through beta, start a shell there and read its screen
	var made struct{ ID, Pane string }
	if err := phone.do(C.id, B.addr(), B.pin, "POST", "/relay/"+C.id+"/api/sessions", map[string]any{"kind": "shell", "name": "t1"}, &made); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := phone.do(C.id, B.addr(), B.pin, "POST", "/relay/"+C.id+"/api/sessions/"+made.ID+"/keys", map[string]any{"text": "echo relay-$((6*7))", "keys": []string{"Enter"}}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "shell output through the relay", 10*time.Second, func() bool {
		var sc struct{ Text string }
		return phone.do(C.id, B.addr(), B.pin, "GET", "/relay/"+C.id+"/api/sessions/"+made.ID+"/screen", nil, &sc) == nil && strings.Contains(sc.Text, "relay-42")
	})
	// terminal websocket through the relay (reader proto 2)
	if err := phone.readerSeed(C, B, made.ID); err != nil {
		t.Fatalf("reader through relay: %v", err)
	}
	// a picture upload through the relay lands in gamma's uploads folder
	ureq, _ := http.NewRequest("POST", "https://"+B.addr()+"/relay/"+C.id+"/api/upload?name=pic.png", bytes.NewReader([]byte("PNGDATA")))
	ureq.Header.Set(core.AuthHeader, phone.id.SignRequest(C.id, "POST", "/api/upload?name=pic.png"))
	ures, err := pinned(B.pin).Do(ureq)
	if err != nil {
		t.Fatal(err)
	}
	var up struct{ Path string }
	json.NewDecoder(ures.Body).Decode(&up)
	ures.Body.Close()
	if got, _ := os.ReadFile(up.Path); ures.StatusCode != 200 || string(got) != "PNGDATA" || !strings.HasPrefix(up.Path, filepath.Join(C.home, "uploads")) {
		t.Fatalf("upload through relay: %d %q %q", ures.StatusCode, up.Path, got)
	}
	// a harness hook inside gamma's chat: the phone's events socket gets the alert, the chat shows the status
	ectx, ecancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ecancel()
	ehdr := http.Header{}
	ehdr.Set(core.AuthHeader, phone.id.SignRequest(C.id, "GET", "/api/events"))
	ews, _, err := websocket.Dial(ectx, "wss://"+B.addr()+"/relay/"+C.id+"/api/events", &websocket.DialOptions{HTTPClient: pinned(B.pin), HTTPHeader: ehdr})
	if err != nil {
		t.Fatalf("events through relay: %v", err)
	}
	gotAlert := make(chan string, 1)
	go func() {
		for {
			_, data, err := ews.Read(ectx)
			if err != nil {
				return
			}
			var m struct {
				Type  string
				Alert struct{ Type, Body string }
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "alert" {
				gotAlert <- m.Alert.Type + ":" + m.Alert.Body
				return
			}
		}
	}()
	hookCmd := "GOBLIN_HOME=" + C.home + " " + bin + " hook test permission may I delete build"
	if err := phone.do(C.id, B.addr(), B.pin, "POST", "/relay/"+C.id+"/api/sessions/"+made.ID+"/keys", map[string]any{"text": hookCmd, "keys": []string{"Enter"}}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-gotAlert:
		if g != "permission:may I delete build" {
			t.Fatalf("alert event: %q", g)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no alert on the events socket")
	}
	ews.Close(websocket.StatusNormalClosure, "")
	var ss []struct {
		ID, Status string
		Pending    *struct{ ID int }
	}
	phone.do(C.id, B.addr(), B.pin, "GET", "/relay/"+C.id+"/api/sessions", nil, &ss)
	for _, x := range ss {
		if x.ID == made.ID && (x.Status != "permission" || x.Pending == nil) {
			t.Fatalf("chat status after the hook: %+v", x)
		}
	}
	var al []struct {
		ID          int
		Type, State string
	}
	if err := phone.do(C.id, B.addr(), B.pin, "GET", "/relay/"+C.id+"/api/alerts", nil, &al); err != nil || len(al) != 1 || al[0].State != "open" {
		t.Fatalf("alerts: %v %+v", err, al)
	}
	phone.do(C.id, B.addr(), B.pin, "POST", "/relay/"+C.id+"/api/sessions/"+made.ID+"/keys", map[string]any{"text": "GOBLIN_HOME=" + C.home + " " + bin + " hook test working", "keys": []string{"Enter"}}, nil)
	waitFor(t, "status back to working, alert closed", 10*time.Second, func() bool {
		var al2 []struct{ State string }
		return phone.do(C.id, B.addr(), B.pin, "GET", "/relay/"+C.id+"/api/alerts", nil, &al2) == nil && len(al2) == 1 && al2[0].State == "closed"
	})
	// outside a Goblin chat the hook does nothing (and never fails)
	if out, err := exec.Command(bin, "hook", "test", "permission", "x").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("hook outside a chat: %v %q", err, out)
	}

	// 4b. a broken link: alpha and gamma can't connect to each other, only beta reaches both. The phone asks alpha
	// for gamma and alpha hands it on to beta (a two-relay route), REST and websocket. Then the link is fixed again.
	setNoDial := func(h *host, ids []string) {
		h.stop()
		f := filepath.Join(h.home, core.ConfigFile)
		b, _ := os.ReadFile(f)
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(l, "test_no_dial:") {
				keep = append(keep, l)
			}
		}
		if len(ids) > 0 {
			keep = append(keep, "test_no_dial: ["+strings.Join(ids, ", ")+"]")
		}
		os.WriteFile(f, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
		h.start(t)
	}
	setNoDial(A, []string{C.id})
	setNoDial(C, []string{A.id})
	waitFor(t, "alpha sees gamma only through beta", 40*time.Second, func() bool {
		var a netView
		if phone.do(A.id, A.addr(), A.pin, "GET", "/api/network", nil, &a) != nil {
			return false
		}
		for _, h := range a.Hosts {
			if h.ID == C.id {
				return h.Via == B.id
			}
		}
		return false
	})
	var chain struct{ ID string }
	waitFor(t, "gamma reached through alpha then beta", 40*time.Second, func() bool {
		return phone.do(C.id, A.addr(), A.pin, "POST", "/relay/"+C.id+"/api/sessions", map[string]any{"kind": "shell", "name": "chain"}, &chain) == nil
	})
	if err := phone.do(C.id, A.addr(), A.pin, "POST", "/relay/"+C.id+"/api/sessions/"+chain.ID+"/keys", map[string]any{"text": "echo relay-$((6*7))", "keys": []string{"Enter"}}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "shell output through two relays", 10*time.Second, func() bool {
		var sc struct{ Text string }
		return phone.do(C.id, A.addr(), A.pin, "GET", "/relay/"+C.id+"/api/sessions/"+chain.ID+"/screen", nil, &sc) == nil && strings.Contains(sc.Text, "relay-42")
	})
	if err := phone.readerSeed(C, A, chain.ID); err != nil {
		t.Fatalf("reader through two relays: %v", err)
	}
	// a request that already passed alpha is refused there (no loops)
	lreq, _ := http.NewRequest("GET", "https://"+A.addr()+"/relay/"+C.id+"/api/sessions", nil)
	lreq.Header.Set(core.AuthHeader, phone.id.SignRequest(C.id, "GET", "/api/sessions"))
	lreq.Header.Set("X-Goblin-Route", A.id)
	if lr, err := pinned(A.pin).Do(lreq); err != nil || lr.StatusCode != http.StatusLoopDetected {
		t.Fatalf("relay loop not refused: %v %v", err, lr)
	} else {
		lr.Body.Close()
	}
	setNoDial(A, nil)
	setNoDial(C, nil)
	waitFor(t, "three hosts online again", 40*time.Second, func() bool {
		var a netView
		if phone.do(A.id, A.addr(), A.pin, "GET", "/api/network", nil, &a) != nil || len(a.Hosts) != 3 {
			return false
		}
		for _, h := range a.Hosts {
			if !h.Online {
				return false
			}
		}
		return true
	})

	// 5. the hub (beta) dies: after the failover time alpha takes over (gamma is never-hub)
	cancel()
	B.stop()
	waitFor(t, "alpha takes over after failover", 40*time.Second, func() bool {
		var a netView
		return phone.do(A.id, A.addr(), A.pin, "GET", "/api/network", nil, &a) == nil && a.Hub == A.id
	})

	// 6. a removed phone is refused everywhere
	other := core.NewKeyOnly()
	_ = other
	tmp := &core.Log{}
	var lg struct {
		Net    string       `json:"net"`
		Events []core.Event `json:"events"`
	}
	phone.do(A.id, A.addr(), A.pin, "GET", "/api/log", nil, &lg)
	tmp.Adopt(lg.Net, lg.Events)
	_ = tmp
	rm := core.Event{T: "remove", ID: phone.id.ID, TS: time.Now().UnixMilli(), By: phone.id.ID}
	rm.Sig = phone.id.Sign(rm.Text(lg.Net))
	phone.do(A.id, A.addr(), A.pin, "POST", "/api/log", map[string]any{"events": []core.Event{rm}}, nil)
	waitFor(t, "removed phone refused at gamma", 20*time.Second, func() bool {
		err := phone.do(C.id, C.addr(), C.pin, "GET", "/api/network", nil, nil)
		return err != nil && strings.Contains(err.Error(), "403")
	})
}

// events: keep an events socket on h, answer speed tests with fake numbers.
func (c *client) events(ctx context.Context, h *host, answer func(hosts []string) map[string]float64) {
	for ctx.Err() == nil {
		hdr := http.Header{}
		hdr.Set(core.AuthHeader, c.id.SignRequest(h.id, "GET", "/api/events"))
		ws, _, err := websocket.Dial(ctx, "wss://"+h.addr()+"/api/events", &websocket.DialOptions{HTTPClient: pinned(h.pin), HTTPHeader: hdr})
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				break
			}
			var m struct {
				Type  string `json:"type"`
				Test  string `json:"test"`
				Hosts []struct {
					ID string `json:"id"`
				} `json:"hosts"`
			}
			json.Unmarshal(data, &m)
			if m.Type == "speedtest" {
				var ids []string
				for _, x := range m.Hosts {
					ids = append(ids, x.ID)
				}
				b, _ := json.Marshal(map[string]any{"type": "speedtest_result", "test": m.Test, "results": answer(ids)})
				ws.Write(ctx, websocket.MessageText, b)
			}
		}
	}
}

func (c *client) readerSeed(target, via *host, sid string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inner := "/api/sessions/" + sid + "/term?mode=reader&proto=2"
	hdr := http.Header{}
	hdr.Set(core.AuthHeader, c.id.SignRequest(target.id, "GET", inner))
	ws, _, err := websocket.Dial(ctx, "wss://"+via.addr()+"/relay/"+target.id+inner, &websocket.DialOptions{HTTPClient: pinned(via.pin), HTTPHeader: hdr})
	if err != nil {
		return err
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	var m struct {
		Full bool     `json:"full"`
		Tail []string `json:"tail"`
	}
	json.Unmarshal(data, &m)
	if !m.Full || !strings.Contains(strings.Join(m.Tail, "\n"), "relay-42") {
		return fmt.Errorf("unexpected first message: %.200s", data)
	}
	return nil
}
