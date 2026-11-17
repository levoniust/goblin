// goblin-agent: lets an AI agent on one computer use the Goblin network like the app does: list chats on any host,
// start one, type into it and read its screen. Agents use it to hand work to each other (2026-11-16: "get claude
// on the surface to help").
//
//	GOBLIN_PIN=xxxx goblin-agent pair [NAME]     once: pair this agent as a client of the local host (key kept in
//	                                             ~/.config/goblin-agent, user-only)
//	goblin-agent hosts                           hosts of the network, online or not
//	goblin-agent list HOST                       chats on HOST (name or id) with their status
//	goblin-agent alerts HOST                     HOST's recent alerts (newest first)
//	goblin-agent launchers HOST                  what HOST can start
//	goblin-agent new HOST KIND [NAME] [CWD]      start a chat, prints its id
//	goblin-agent type HOST CHAT TEXT...          type TEXT and press Enter
//	goblin-agent keys HOST CHAT KEY...           press keys (Enter, Escape, Up, C-c, ...)
//	goblin-agent screen HOST CHAT [LINES]        print the chat's screen
//	goblin-agent stop HOST CHAT                  end a chat
//
// A host that does not answer directly is reached through the local host's relay (which finds the way on).
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

type hostRef struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Pin   string   `json:"pin"`
	Addrs []string `json:"addrs"`
	addr  string
}

type state struct {
	Seed  string  `json:"seed"`  // ed25519 seed (b64)
	Local hostRef `json:"local"` // the host this agent paired with (its relay is the fallback route)
}

var (
	dir = filepath.Join(os.Getenv("HOME"), ".config", "goblin-agent")
	st  state
	id  *core.Identity
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "goblin-agent: "+f+"\n", a...)
	os.Exit(1)
}

func pinned(pin string, t time.Duration) *http.Client {
	return &http.Client{Timeout: t, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			s := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(s[:]) != pin {
				return errors.New("certificate pin mismatch")
			}
			return nil
		}}}}
}

func answers(h *hostRef) bool {
	for _, a := range h.Addrs {
		resp, err := pinned(h.Pin, 3*time.Second).Get("https://" + a + "/api/ping")
		if err != nil {
			continue
		}
		var p struct{ ID string }
		json.NewDecoder(resp.Body).Decode(&p)
		resp.Body.Close()
		if p.ID == h.ID {
			h.addr = a
			return true
		}
	}
	return false
}

// call: a signed request for host h, directly when it answers, else through the local host's relay.
func call(h *hostRef, method, path string, body, out any) error {
	if h.addr == "" {
		answers(h)
	}
	addr, pin, u := h.addr, h.Pin, path
	if addr == "" {
		l := st.Local
		if !answers(&l) {
			return fmt.Errorf("%s does not answer, nor does %s (the relay)", h.Name, l.Name)
		}
		addr, pin, u = l.addr, l.Pin, "/relay/"+h.ID+path
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+addr+u, rd)
	req.Header.Set(core.AuthHeader, id.SignRequest(h.ID, method, path))
	resp, err := pinned(pin, 60*time.Second).Do(req)
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

func load() {
	b, err := os.ReadFile(filepath.Join(dir, "agent.json"))
	if err != nil || json.Unmarshal(b, &st) != nil {
		die("not paired: run GOBLIN_PIN=xxxx goblin-agent pair")
	}
	seed, err := core.UnB64(st.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		die("broken key file %s", filepath.Join(dir, "agent.json"))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	id = &core.Identity{Priv: priv, Pub: pub, ID: core.MemberID(pub)}
}

func pair(name string) {
	pin := os.Getenv("GOBLIN_PIN")
	if pin == "" {
		die("set GOBLIN_PIN (the Goblin PIN of this host)")
	}
	cmd := exec.Command("goblin", "pair")
	cmd.Stdin = strings.NewReader(pin + "\n")
	out, _ := cmd.CombinedOutput()
	link := regexp.MustCompile(`goblin://pair/[A-Za-z0-9_=-]+`).FindString(string(out))
	if link == "" {
		die("goblin pair gave no link: %s", out)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimRight(link[len("goblin://pair/"):], "="))
	var L struct {
		ID, N, C, K string
		H           []string
	}
	json.Unmarshal(raw, &L)
	_, priv, _ := ed25519.GenerateKey(nil)
	st = state{Seed: core.B64(priv.Seed()), Local: hostRef{ID: L.ID, Name: L.N, Pin: L.C, Addrs: L.H}}
	pub := priv.Public().(ed25519.PublicKey)
	id = &core.Identity{Priv: priv, Pub: pub, ID: core.MemberID(pub)}
	l := st.Local
	if !answers(&l) {
		die("%s does not answer on %v", l.Name, l.Addrs)
	}
	if err := call(&l, "POST", "/api/pair", map[string]any{"code": L.K, "name": name, "pub": core.B64(pub)}, nil); err != nil {
		die("pair: %v", err)
	}
	os.MkdirAll(dir, 0o700)
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), b, 0o600); err != nil {
		die("%v", err)
	}
	fmt.Printf("paired as %q (client %s) with %s\n", name, id.ID, l.Name)
}

type netView struct {
	Hub   string `json:"hub"`
	Hosts []struct {
		hostRef
		Online bool   `json:"online"`
		Via    string `json:"via"`
	} `json:"hosts"`
}

func network() netView {
	var nv netView
	l := st.Local
	if err := call(&l, "GET", "/api/network", nil, &nv); err != nil {
		die("network from %s: %v", l.Name, err)
	}
	return nv
}

func findHost(q string) *hostRef {
	for _, h := range network().Hosts {
		if strings.EqualFold(h.Name, q) || h.ID == q {
			r := h.hostRef
			return &r
		}
	}
	die("no host %q (goblin-agent hosts)", q)
	return nil
}

func main() {
	a := os.Args[1:]
	if len(a) == 0 {
		fmt.Println(strings.SplitN(usage, "\n\n", 2)[0])
		os.Exit(2)
	}
	if a[0] == "pair" {
		name := "goblin-agent"
		if len(a) > 1 {
			name = strings.Join(a[1:], " ")
		}
		pair(name)
		return
	}
	load()
	need := func(n int) {
		if len(a) < n {
			die("%s: missing arguments", a[0])
		}
	}
	switch a[0] {
	case "hosts":
		nv := network()
		for _, h := range nv.Hosts {
			s := "offline"
			if h.Online {
				s = "online"
			}
			if h.Via != "" {
				s += " via " + h.Via
			}
			if h.ID == nv.Hub {
				s += " HUB"
			}
			fmt.Printf("%-12s %s  %s\n", h.Name, h.ID, s)
		}
	case "list":
		need(2)
		var ss []map[string]any
		if err := call(findHost(a[1]), "GET", "/api/sessions", nil, &ss); err != nil {
			die("%v", err)
		}
		for _, s := range ss {
			fmt.Printf("%v\t%v\t%v\t%v\t%v\n", s["id"], s["name"], s["status"], s["command"], s["cwd"])
		}
	case "alerts":
		need(2)
		var al []map[string]any
		if err := call(findHost(a[1]), "GET", "/api/alerts", nil, &al); err != nil {
			die("%v", err)
		}
		for _, x := range al {
			fmt.Printf("%v\t%v\t%v\t%v\t%v\t%.80v\n", x["id"], x["pane"], x["type"], x["state"], x["title"], x["body"])
		}
	case "launchers":
		need(2)
		var ls []core.Launcher
		if err := call(findHost(a[1]), "GET", "/api/launchers", nil, &ls); err != nil {
			die("%v", err)
		}
		for _, l := range ls {
			fmt.Printf("%s\t%s\t%s\n", l.Kind, l.Label, l.Command)
		}
	case "new":
		need(3)
		body := map[string]any{"kind": a[2]}
		if len(a) > 3 {
			body["name"] = a[3]
		}
		if len(a) > 4 {
			body["cwd"] = a[4]
		}
		var made struct{ ID string }
		if err := call(findHost(a[1]), "POST", "/api/sessions", body, &made); err != nil {
			die("%v", err)
		}
		fmt.Println(made.ID)
	case "type":
		need(4)
		// text first, Enter a moment later: TUIs (Claude Code) take long text as a paste and swallow an Enter sent with it
		h, p := findHost(a[1]), "/api/sessions/"+url.PathEscape(a[2])+"/keys"
		if err := call(h, "POST", p, map[string]any{"text": strings.Join(a[3:], " ")}, nil); err != nil {
			die("%v", err)
		}
		time.Sleep(700 * time.Millisecond)
		if err := call(h, "POST", p, map[string]any{"keys": []string{"Enter"}}, nil); err != nil {
			die("%v", err)
		}
	case "keys":
		need(4)
		if err := call(findHost(a[1]), "POST", "/api/sessions/"+url.PathEscape(a[2])+"/keys", map[string]any{"keys": a[3:]}, nil); err != nil {
			die("%v", err)
		}
	case "screen":
		need(3)
		p := "/api/sessions/" + url.PathEscape(a[2]) + "/screen"
		if len(a) > 3 {
			p += "?lines=" + a[3]
		}
		var sc struct{ Text string }
		if err := call(findHost(a[1]), "GET", p, nil, &sc); err != nil {
			die("%v", err)
		}
		fmt.Println(sc.Text)
	case "stop":
		need(3)
		if err := call(findHost(a[1]), "DELETE", "/api/sessions/"+url.PathEscape(a[2]), nil, nil); err != nil {
			die("%v", err)
		}
	default:
		die("unknown command %q", a[0])
	}
}

const usage = `usage: goblin-agent pair [NAME] | hosts | list HOST | launchers HOST | new HOST KIND [NAME] [CWD] | type HOST CHAT TEXT | keys HOST CHAT KEY... | screen HOST CHAT [LINES] | stop HOST CHAT

`
