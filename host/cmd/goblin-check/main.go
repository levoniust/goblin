// goblin-check: an end-to-end check of a running Goblin network, done the way the app does it.
//
// Run it on a host (it uses `goblin pair` there, so it needs the Goblin PIN in GOBLIN_PIN):
//
//	GOBLIN_PIN=xxxx goblin-check [-llm [-llm-host NAME]] [-keep]
//
// It pairs a temporary client, then on every online host: lists launchers, starts a shell chat directly and
// through the hub's relay, types into it, reads the screen, the live terminal and the reader view; with -llm it
// also asks every Pi launcher a sum. It keeps an events socket open on the hub and answers its speed tests with
// real timings (the client hub test), asks for a hub test and checks the result. At the end it removes the
// temporary client (for good, with `goblin remove`) unless -keep.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/levoniust/goblin/host/internal/core"
)

type host struct {
	ID, Name, Pin string
	Online        bool
	Addrs         []string
	addr          string // address that answered
}

type checker struct {
	id     *core.Identity
	hosts  map[string]*host
	hub    string
	failed int
	mu     sync.Mutex
	tests  int // speed tests answered on the events socket
}

func pinned(pin string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			s := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(s[:]) != pin {
				return errors.New("certificate pin mismatch")
			}
			return nil
		}}}}
}

func (c *checker) result(ok bool, what string, detail ...any) {
	tag := "OK  "
	if !ok {
		tag = "FAIL"
		c.failed++
	}
	msg := what
	if len(detail) > 0 {
		msg += ": " + fmt.Sprint(detail...)
	}
	fmt.Printf("%s %s\n", tag, msg)
}

// do sends a signed request for target h, directly or through the hub's relay (via != nil).
func (c *checker) do(h, via *host, method, path string, body, out any) error {
	addr, pin, url := h.addr, h.Pin, path
	if via != nil {
		addr, pin, url = via.addr, via.Pin, "/relay/"+h.ID+path
	}
	var rd io.Reader
	if raw, ok := body.([]byte); ok { // a file (upload), sent as is
		rd = bytes.NewReader(raw)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+addr+url, rd)
	req.Header.Set(core.AuthHeader, c.id.SignRequest(h.ID, method, path))
	resp, err := pinned(pin, 20*time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *checker) socket(ctx context.Context, h, via *host, path string) (*websocket.Conn, error) {
	addr, pin, url := h.addr, h.Pin, path
	if via != nil {
		addr, pin, url = via.addr, via.Pin, "/relay/"+h.ID+path
	}
	hdr := http.Header{}
	hdr.Set(core.AuthHeader, c.id.SignRequest(h.ID, "GET", path))
	ws, _, err := websocket.Dial(ctx, "wss://"+addr+url, &websocket.DialOptions{HTTPClient: pinned(pin, 0), HTTPHeader: hdr})
	return ws, err
}

// probe finds an address of h that answers /api/ping with h's id.
func probe(h *host) bool {
	for _, a := range h.Addrs {
		var p struct{ ID string }
		resp, err := pinned(h.Pin, 3*time.Second).Get("https://" + a + "/api/ping")
		if err != nil {
			continue
		}
		json.NewDecoder(resp.Body).Decode(&p)
		resp.Body.Close()
		if p.ID == h.ID {
			h.addr = a
			return true
		}
	}
	return false
}

type netView struct {
	Hub   string  `json:"hub"`
	Epoch int64   `json:"epoch"`
	Hosts []*host `json:"hosts"`
}

func (c *checker) refresh(from *host) (netView, error) {
	var nv netView
	if err := c.do(from, nil, "GET", "/api/network", nil, &nv); err != nil {
		return nv, err
	}
	c.hub = nv.Hub
	for _, h := range nv.Hosts {
		if old := c.hosts[h.ID]; old != nil {
			h.addr = old.addr
		}
		c.hosts[h.ID] = h
	}
	return nv, nil
}

func (c *checker) sorted() []*host {
	var hs []*host
	for _, h := range c.hosts {
		hs = append(hs, h)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].Name < hs[j].Name })
	return hs
}

// chat starts a chat with launcher kind on h (through via when set), types input, waits for want on the screen.
func (c *checker) chat(h, via *host, kind, input, want string, wait time.Duration, extra func(sid string)) {
	route := "direct"
	if via != nil {
		route = "through " + via.Name
	}
	what := fmt.Sprintf("%s %s chat %s", h.Name, kind, route)
	var made struct {
		ID string `json:"id"`
	}
	if err := c.do(h, via, "POST", "/api/sessions", map[string]any{"kind": kind, "name": "goblin-check-" + kind}, &made); err != nil {
		c.result(false, what, "start: ", err)
		return
	}
	defer c.do(h, via, "DELETE", "/api/sessions/"+made.ID, nil, nil)
	if kind != "shell" {
		time.Sleep(6 * time.Second) // let the harness draw its prompt
	} else {
		time.Sleep(2 * time.Second)
	}
	if err := c.do(h, via, "POST", "/api/sessions/"+made.ID+"/keys", map[string]any{"text": input, "keys": []string{"Enter"}}, nil); err != nil {
		c.result(false, what, "type: ", err)
		return
	}
	var sc struct {
		Text string `json:"text"`
	}
	t0 := time.Now()
	for time.Since(t0) < wait {
		time.Sleep(time.Second)
		if c.do(h, via, "GET", "/api/sessions/"+made.ID+"/screen", nil, &sc) == nil && strings.Contains(sc.Text, want) {
			c.result(true, what, fmt.Sprintf("answer %q after %.0f s", want, time.Since(t0).Seconds()))
			if extra != nil {
				extra(made.ID)
			}
			return
		}
	}
	lines := strings.Split(strings.TrimSpace(sc.Text), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	c.result(false, what, "no "+want+" on screen; last lines:\n    "+strings.Join(lines, "\n    "))
}

// views checks the live terminal (type through it, see the echo) and the reader seed of a chat.
func (c *checker) views(h, via *host, sid, input, want, seen string) {
	route := "direct"
	if via != nil {
		route = "through " + via.Name
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ws, err := c.socket(ctx, h, via, "/api/sessions/"+sid+"/term?mode=live&cols=100&rows=30")
	if err != nil {
		c.result(false, h.Name+" live terminal "+route, err)
	} else {
		ws.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"input":%q}`, input+"\r")))
		acc, ok := "", false
		for !ok {
			_, d, err := ws.Read(ctx)
			if err != nil {
				break
			}
			acc += string(d)
			ok = strings.Contains(stripANSI(acc), want)
		}
		ws.Close(websocket.StatusNormalClosure, "")
		c.result(ok, h.Name+" live terminal "+route, map[bool]string{true: "typed and saw " + want, false: "no " + want + " in the stream"}[ok])
	}
	rs, err := c.socket(ctx, h, via, "/api/sessions/"+sid+"/term?mode=reader&proto=2")
	if err != nil {
		c.result(false, h.Name+" reader view "+route, err)
		return
	}
	defer rs.Close(websocket.StatusNormalClosure, "")
	_, d, err := rs.Read(ctx)
	var m struct {
		Full bool     `json:"full"`
		Tail []string `json:"tail"`
	}
	json.Unmarshal(d, &m)
	ok := err == nil && m.Full && strings.Contains(strings.Join(m.Tail, "\n"), seen)
	c.result(ok, h.Name+" reader view "+route, map[bool]string{true: "scrollback seeded", false: fmt.Sprintf("bad first message %.120s %v", d, err)}[ok])
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// speed times a 64 KB download from h directly, like the app's hub test.
func (c *checker) speed(h *host) float64 {
	if h.addr == "" && !probe(h) {
		return 0
	}
	const p = "/api/speedtest?bytes=65536"
	req, _ := http.NewRequest("GET", "https://"+h.addr+p, nil)
	req.Header.Set(core.AuthHeader, c.id.SignRequest(h.ID, "GET", p))
	t0 := time.Now()
	resp, err := pinned(h.Pin, 10*time.Second).Do(req)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0
	}
	return float64(time.Since(t0).Microseconds()) / 1000
}

// events keeps an events socket on h (the app's live connection) and answers speed tests.
func (c *checker) events(ctx context.Context, h *host, up chan<- bool) {
	ws, err := c.socket(ctx, h, nil, "/api/events")
	if err != nil {
		c.result(false, "events socket on "+h.Name, err)
		up <- false
		return
	}
	up <- true
	defer ws.Close(websocket.StatusNormalClosure, "")
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			Type  string `json:"type"`
			Test  string `json:"test"`
			Hosts []struct {
				ID string `json:"id"`
			} `json:"hosts"`
		}
		json.Unmarshal(data, &m)
		if m.Type != "speedtest" {
			continue
		}
		res := map[string]float64{}
		for _, x := range m.Hosts {
			if hh := c.hosts[x.ID]; hh != nil {
				best := 0.0
				for i := 0; i < 3; i++ {
					if ms := c.speed(hh); ms > 0 && (best == 0 || ms < best) {
						best = ms
					}
				}
				if best > 0 {
					res[x.ID] = best
				}
			}
		}
		c.mu.Lock()
		c.tests++
		c.mu.Unlock()
		fmt.Printf("     (hub asked this client for a speed test: %v)\n", named(c, res))
		b, _ := json.Marshal(map[string]any{"type": "speedtest_result", "test": m.Test, "results": res})
		ws.Write(ctx, websocket.MessageText, b)
	}
}

func named(c *checker, m map[string]float64) string {
	var out []string
	for id, v := range m {
		n := id
		if h := c.hosts[id]; h != nil {
			n = h.Name
		}
		out = append(out, fmt.Sprintf("%s %.1f ms", n, v))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func goblin(stdin string, args ...string) (string, error) {
	cmd := exec.Command("goblin", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func main() {
	llm := flag.Bool("llm", false, "also ask every Pi launcher a sum (loads models; slow)")
	llmHost := flag.String("llm-host", "", "with -llm: only ask the Pi launchers of this host (name)")
	keep := flag.Bool("keep", false, "keep the temporary client paired")
	dumpInfo := flag.String("dump-info", "", "save each host's /api/info as DIR/info-<name>.json (contract test fixtures)")
	flag.Parse()
	pin := os.Getenv("GOBLIN_PIN")
	if pin == "" {
		fmt.Fprintln(os.Stderr, "set GOBLIN_PIN (the Goblin PIN of this host)")
		os.Exit(2)
	}
	c := &checker{id: core.NewKeyOnly(), hosts: map[string]*host{}}

	// 1. pair like the app: a one-time link from `goblin pair`
	out, err := goblin(pin+"\n", "pair")
	link := regexp.MustCompile(`goblin://pair/[A-Za-z0-9_=-]+`).FindString(out)
	if link == "" {
		fmt.Println("FAIL goblin pair:", err, out)
		os.Exit(1)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimRight(link[len("goblin://pair/"):], "="))
	var L struct {
		ID string   `json:"id"`
		N  string   `json:"n"`
		H  []string `json:"h"`
		C  string   `json:"c"`
		K  string   `json:"k"`
	}
	json.Unmarshal(raw, &L)
	local := &host{ID: L.ID, Name: L.N, Pin: L.C, Addrs: L.H}
	if !probe(local) {
		fmt.Println("FAIL local host does not answer on", L.H)
		os.Exit(1)
	}
	name := "goblin-check " + time.Now().Format("01-02 15:04")
	err = c.do(local, nil, "POST", "/api/pair", map[string]any{"code": L.K, "name": name, "pub": core.B64(c.id.Pub)}, nil)
	if err != nil {
		c.result(false, "pair a client on "+local.Name, err)
		os.Exit(1)
	}
	c.result(true, "pair a client on "+local.Name)
	defer func() {
		if *keep {
			fmt.Println("kept client", c.id.ID)
			return
		}
		out, err := goblin("y\n"+pin+"\n", "remove", c.id.ID)
		c.result(err == nil, "remove the temporary client", strings.TrimSpace(out[strings.LastIndex(out, ":")+1:]))
		fmt.Printf("\n%d failed\n", c.failed)
		if c.failed > 0 {
			os.Exit(1)
		}
	}()

	// 2. network view, every host reachable and agreeing on the hub, client known everywhere
	nv, err := c.refresh(local)
	if err != nil {
		c.result(false, "network view", err)
		return
	}
	var online []*host
	for _, h := range c.sorted() {
		if !h.Online {
			c.result(false, h.Name+" is offline", "last addresses ", h.Addrs)
			continue
		}
		if !probe(h) {
			c.result(false, h.Name+" says online but answers on none of", h.Addrs)
			continue
		}
		online = append(online, h)
	}
	hub := c.hosts[nv.Hub]
	if hub == nil || !hub.Online {
		c.result(false, "hub", "hub "+nv.Hub+" is not online")
		return
	}
	fmt.Printf("     network %s, hub %s (epoch %d), online: %d of %d hosts\n", L.ID, hub.Name, nv.Epoch, len(online), len(c.hosts))
	for _, h := range online {
		var v netView
		ok := false
		for i := 0; i < 20 && !ok; i++ { // the new client reaches other hosts with the next log sync
			if err = c.do(h, nil, "GET", "/api/network", nil, &v); err == nil {
				ok = v.Hub == nv.Hub
			}
			if !ok {
				time.Sleep(time.Second)
			}
		}
		c.result(ok, h.Name+" knows this client and agrees on the hub", map[bool]any{true: hub.Name, false: err}[ok])
	}

	// 3. chats on every host, directly, through the hub and through every other host
	for _, h := range online {
		if *dumpInfo != "" {
			var raw json.RawMessage
			if c.do(h, nil, "GET", "/api/info", nil, &raw) == nil {
				var pretty bytes.Buffer
				json.Indent(&pretty, raw, "", "  ")
				os.WriteFile(filepath.Join(*dumpInfo, "info-"+strings.ToLower(h.Name)+".json"), pretty.Bytes(), 0o644)
			}
		}
		var ln []core.Launcher
		c.do(h, nil, "GET", "/api/launchers", nil, &ln)
		var kinds []string
		for _, l := range ln {
			kinds = append(kinds, l.Kind)
		}
		fmt.Printf("     %s launchers: %s\n", h.Name, strings.Join(kinds, ", "))
		input, want := "echo goblin-check-$((6*7))", "goblin-check-42"
		if isWindows(c, h) {
			input = "echo ('goblin-check-' + 6*7)"
		}
		c.chat(h, nil, "shell", input, want, 20*time.Second, func(sid string) {
			c.views(h, nil, sid, strings.Replace(input, "check", "live", 1), "goblin-live-42", want)
		})
		if h.ID != hub.ID {
			c.chat(h, hub, "shell", input, want, 20*time.Second, func(sid string) {
				c.views(h, hub, sid, strings.Replace(input, "check", "live", 1), "goblin-live-42", want)
			})
		}
		// picture upload (0.2.1): directly and through the hub, the file lands in that host's uploads folder
		for _, via := range []*host{nil, hub} {
			if via == h {
				continue
			}
			route := "direct"
			if via != nil {
				route = "through " + via.Name
			}
			var up struct {
				Path string `json:"path"`
				Size int    `json:"size"`
			}
			err := c.do(h, via, "POST", "/api/upload?name=goblin-check.txt", []byte("goblin-check upload"), &up)
			ok := err == nil && up.Size == 19 && strings.Contains(strings.ReplaceAll(up.Path, `\`, "/"), "/uploads/")
			c.result(ok, h.Name+" upload "+route, map[bool]any{true: up.Path, false: fmt.Sprint(err, " ", up.Path)}[ok])
		}
		// every host is a switch (0.2.1): through each other host too, which hands it on when it can't reach h itself
		for _, r := range online {
			if r.ID != h.ID && r.ID != hub.ID {
				c.chat(h, r, "shell", input, want, 20*time.Second, nil)
			}
		}
		if *llm && (*llmHost == "" || strings.EqualFold(*llmHost, h.Name)) {
			for i, l := range ln {
				if strings.HasPrefix(l.Kind, "pi") {
					a, b := 17+i, 23+i
					c.chat(h, nil, l.Kind, fmt.Sprintf("What is %d*%d? Reply with only the number.", a, b), fmt.Sprint(a*b), 240*time.Second, nil)
				}
			}
		}
	}

	// 4. the client hub test: keep an events socket on the hub, ask for a test, answer it, check the result
	if len(online) > 1 {
		ctx, cancel := context.WithCancel(context.Background())
		up := make(chan bool, 1)
		go c.events(ctx, hub, up)
		if <-up {
			time.Sleep(2 * time.Second)
			err := c.do(hub, nil, "POST", "/api/network/test", map[string]any{}, nil)
			c.result(err == nil, "ask for a hub test", err)
			var after netView
			for i := 0; i < 40; i++ {
				time.Sleep(time.Second)
				c.mu.Lock()
				n := c.tests
				c.mu.Unlock()
				if n > 0 && c.do(c.hosts[hub.ID], nil, "GET", "/api/network", nil, &after) == nil && after.Epoch > nv.Epoch {
					break
				}
			}
			c.mu.Lock()
			n := c.tests
			c.mu.Unlock()
			c.result(n > 0, "hub test used this client's speed test", fmt.Sprintf("%d asked", n))
			hubName := after.Hub
			if nh := c.hosts[after.Hub]; nh != nil {
				hubName = nh.Name
			}
			c.result(after.Epoch > nv.Epoch, "hub test finished", fmt.Sprintf("hub %s, epoch %d", hubName, after.Epoch))
			for _, h := range online {
				var v netView
				ok := false
				for i := 0; i < 15 && !ok; i++ {
					ok = c.do(h, nil, "GET", "/api/network", nil, &v) == nil && v.Hub == after.Hub
					if !ok {
						time.Sleep(time.Second)
					}
				}
				c.result(ok, h.Name+" agrees on the new hub", v.Hub)
			}
		}
		cancel()
	}
}

// isWindows: hosts report their OS in the network view only through /api/info; ask.
func isWindows(c *checker, h *host) bool {
	var info map[string]any
	if c.do(h, nil, "GET", "/api/info", nil, &info) != nil {
		return false
	}
	return fmt.Sprint(info["os"]) == "windows"
}
