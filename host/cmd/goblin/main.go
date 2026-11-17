// goblin: the Goblin host. Same program on every computer; see plans/PLAN.md in the handoff and docs/ in the repo.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
	"github.com/levoniust/goblin/host/internal/mesh"
	"github.com/levoniust/goblin/host/internal/server"
	"github.com/levoniust/goblin/host/internal/term"
	"github.com/mdp/qrterminal/v3"
	xterm "golang.org/x/term"
)

const usage = `goblin %s: agents on many computers, one client.

  goblin init [-name N] [-port P]   set up this host (asks for a Goblin PIN)
  goblin run                        run the host (the service does this)
  goblin pair                       pairing link + QR for a new phone or desktop app (needs the PIN)
  goblin join HOST:PORT             ask another host's network to let this host in (approve on your phone)
  goblin approve ID                 approve a waiting host or client from here (needs the PIN)
  goblin remove ID                  cut a host or client off the network for good (needs the PIN)
  goblin status                     network, hub and waiting requests as this host sees them
  goblin test                       ask for a new hub test
  goblin pin                        change the Goblin PIN
  goblin ui                         open this computer's Goblin page in the browser
  goblin config check [FILE]        check goblin.yaml (the settings) and list mistakes with line numbers
  goblin config path                where goblin.yaml is
  goblin keeper                     (Windows) the process that holds the chats
  goblin hidden [--low] -- PROGRAM [ARGS]   run a program with no window (Windows tasks: use goblinw.exe)
  goblin hook claude | <harness> <event> [text]
                                    harness hooks: chat status + phone alerts (working, permission, question, done, ended)

Data folder: %s (set GOBLIN_HOME to use another)
`

func main() {
	log.SetFlags(log.LstdFlags)
	home := core.Home()
	if len(os.Args) < 2 {
		fmt.Printf(usage, core.Version, home)
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(home, args)
	case "run":
		err = cmdRun(home)
	case "pair":
		err = cmdPair(home)
	case "join":
		err = cmdJoin(home, args)
	case "approve":
		err = cmdApprove(home, args)
	case "remove":
		err = cmdRemove(home, args)
	case "status":
		err = cmdStatus(home)
	case "test":
		err = server.WriteControl(home, server.ControlOp{Op: "test"})
		fmt.Println("asked for a hub test")
	case "pin":
		err = cmdPin(home)
	case "config":
		err = cmdConfig(home, args)
	case "ui":
		err = cmdUI(home)
	case "keeper":
		err = runKeeper(home)
	case "hidden":
		err = cmdHidden(args)
	case "hook":
		cmdHook(home, args) // never fails: a harness must not wait on or break because of Goblin
		return
	case "selftest":
		err = term.KeeperSelfTest(home, 18299)
	case "version", "-v", "--version":
		fmt.Println(core.Version)
	default:
		fmt.Printf(usage, core.Version, home)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "goblin:", err)
		os.Exit(1)
	}
}

func setup(home string) (*core.Config, *core.Identity, *core.Log, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, nil, nil, err
	}
	cfg, err := core.LoadConfig(home)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("settings (run `goblin config check`): %w", err)
	}
	id, err := core.LoadOrCreateIdentity(home, cfg.Name)
	if err != nil {
		return nil, nil, nil, err
	}
	lp := filepath.Join(home, "members.json")
	lg, ok, err := core.LoadLog(lp)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("members.json: %w", err)
	}
	if !ok {
		// a host alone is its own network (and hub) until it finds others
		if lg, err = core.NewLog(lp, id, cfg.Name); err != nil {
			return nil, nil, nil, err
		}
	}
	return cfg, id, lg, nil
}

func cmdInit(home string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	name := fs.String("name", "", "display name (default: computer name)")
	port := fs.Int("port", 0, "HTTPS port (default 8200)")
	fs.Parse(args)
	os.MkdirAll(home, 0o700)
	cfg, err := core.LoadConfig(home)
	if err != nil {
		return err
	}
	if *name != "" {
		cfg.Name = core.CleanName(*name)
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if _, err := os.Stat(core.ConfigPath(home)); err != nil || *name != "" || *port != 0 {
		if err := cfg.Save(home); err != nil {
			return err
		}
	}
	_, id, lg, err := setup(home)
	if err != nil {
		return err
	}
	if !core.HasPIN(home) {
		fmt.Println("Choose a Goblin PIN (at least 4 characters). `goblin pair` and `goblin approve` ask for it,")
		fmt.Println("so someone who gets into this computer's login still can't add a phone.")
		p1 := readSecret("PIN: ")
		p2 := readSecret("again: ")
		if p1 != p2 {
			return fmt.Errorf("the PINs don't match")
		}
		if err := core.SetPIN(home, p1); err != nil {
			return err
		}
	}
	fmt.Printf("host %q  id %s  network %s\nport %d  data %s\n", cfg.Name, id.ID, lg.NetID(), cfg.Port, home)
	fmt.Println("next: run `goblin run` (or install the service), then `goblin pair` for your phone.")
	return nil
}

func cmdRun(home string) error {
	cfg, id, lg, err := setup(home)
	if err != nil {
		return err
	}
	if lf, err := core.OpenLogFile(home, "goblin.log"); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
	}
	ctx, stop := stopContext("goblin")
	defer stop()
	m := mesh.New(cfg, home, id, lg)
	srv := server.New(cfg, home, id, lg, m, term.NewBackend(cfg))
	log.Printf("goblin %s: host %q (%s), network %s, port %d", core.Version, cfg.Name, id.ID, lg.NetID(), cfg.Port)
	go m.Run(ctx)
	go srv.ControlLoop(ctx)
	go watchConfig(ctx, home, cfg, srv.SettingsChanged)
	go srv.ServeUI(ctx)
	go srv.LowerAllChats()
	return srv.Serve(ctx)
}

// watchConfig reloads goblin.yaml when it is saved. A file with mistakes is not used (the log says why).
func watchConfig(ctx context.Context, home string, cfg *core.Config, changed func()) {
	path := core.ConfigPath(home)
	stamp := func() string {
		st, err := os.Stat(path)
		if err != nil {
			return ""
		}
		return fmt.Sprint(st.ModTime().UnixNano(), st.Size())
	}
	last := stamp()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := stamp()
		if now == last || now == "" {
			continue
		}
		last = now
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if probs := core.CheckConfig(b); len(probs) > 0 {
			log.Printf("goblin.yaml has %d problem(s), still using the previous settings: %s", len(probs), strings.Join(probs, "; "))
			continue
		}
		n, err := core.LoadConfig(home)
		if err != nil {
			log.Printf("goblin.yaml: %v", err)
			continue
		}
		ch, restart := cfg.Apply(n)
		if len(ch) > 0 {
			log.Printf("goblin.yaml reloaded: %s changed", strings.Join(ch, ", "))
			changed()
		}
		if len(restart) > 0 {
			log.Printf("goblin.yaml: %s changed; that takes effect after a restart", strings.Join(restart, ", "))
		}
	}
}

func checkPIN(home string) error {
	if !core.HasPIN(home) {
		return fmt.Errorf("no PIN set yet: run `goblin init` first")
	}
	for i := 0; i < 3; i++ {
		if core.CheckPIN(home, readSecret("Goblin PIN: ")) {
			return nil
		}
		time.Sleep(time.Second)
		fmt.Println("wrong PIN")
	}
	return fmt.Errorf("wrong PIN")
}

// stdin is read through one reader, so piped input (scripts, tests) keeps every line.
var stdin = bufio.NewReader(os.Stdin)

func readLine() string {
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

func readSecret(prompt string) string {
	fmt.Print(prompt)
	if xterm.IsTerminal(int(os.Stdin.Fd())) {
		b, _ := xterm.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return strings.TrimSpace(string(b))
	}
	return readLine()
}

func cmdPair(home string) error {
	cfg, id, lg, err := setup(home)
	if err != nil {
		return err
	}
	if err := checkPIN(home); err != nil {
		return err
	}
	link, err := server.PairLink(home, cfg, id, lg.NetID())
	if err != nil {
		return err
	}
	fmt.Println("Scan with the phone camera (Switchboard opens it), or open the link on the device. One-time code, valid 10 minutes:")
	fmt.Println()
	fmt.Println(link)
	fmt.Println()
	qrterminal.GenerateWithConfig(link, qrterminal.Config{Level: qrterminal.L, Writer: os.Stdout, HalfBlocks: true,
		BlackChar: qrterminal.BLACK_BLACK, WhiteBlackChar: qrterminal.WHITE_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE, QuietZone: 1})
	return nil
}

func cmdJoin(home string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goblin join HOST:PORT")
	}
	_, id, lg, err := setup(home)
	if err != nil {
		return err
	}
	if len(lg.Members()) > 1 {
		return fmt.Errorf("this host is already in a network with other members; remove it there first")
	}
	addr := args[0]
	if !strings.Contains(addr, ":") {
		addr += ":8200"
	}
	if err := server.WriteControl(home, server.ControlOp{Op: "join", Addr: addr}); err != nil {
		return err
	}
	fmt.Printf("asking %s to let this host in. Approve it on your phone; check code %s.\n", addr, mesh.CheckCode(id.Pub))
	fmt.Println("(the host must be running; `goblin status` shows progress)")
	return nil
}

type statusFile struct {
	Updated int64     `json:"updated"`
	Network mesh.View `json:"network"`
	Name    string    `json:"name"`
	ID      string    `json:"id"`
	Port    int       `json:"port"`
}

func readStatus(home string) (statusFile, error) {
	var st statusFile
	if err := core.ReadJSON(server.StatusPath(home), &st); err != nil {
		return st, err
	}
	if st.Updated == 0 || time.Since(time.UnixMilli(st.Updated)) > 30*time.Second {
		return st, fmt.Errorf("the host isn't running (start it with `goblin run` or the service)")
	}
	return st, nil
}

func cmdStatus(home string) error {
	st, err := readStatus(home)
	if err != nil {
		return err
	}
	n := st.Network
	fmt.Printf("%s (%s)  network %s  hub %s  epoch %d  coordinator %s\n\nHosts:\n", st.Name, st.ID, n.Net, nameOf(n, n.Hub), n.Epoch, nameOf(n, n.Coordinator))
	for _, h := range n.Hosts {
		state := "offline"
		if h.Online {
			state = "online"
		} else if h.Via != "" {
			state = "online via " + nameOf(n, h.Via)
		}
		flags := ""
		if h.Hub {
			flags += " HUB"
		}
		if h.Self {
			flags += " (this host)"
		}
		fmt.Printf("  %s  %-16s %-7s %s%s\n", h.ID, h.Name, state, strings.Join(h.Addrs, " "), flags)
	}
	fmt.Println("\nClients:")
	for _, c := range n.Clients {
		seen := "never"
		if c.Last > 0 {
			seen = time.UnixMilli(c.Last).Format("2006-01-02 15:04")
		}
		fmt.Printf("  %s  %-16s last seen %s\n", c.ID, c.Name, seen)
	}
	if len(n.Pending) > 0 {
		fmt.Println("\nWaiting for approval (`goblin approve ID` or approve on your phone):")
		for _, p := range n.Pending {
			fmt.Printf("  %s  %-6s %-16s check code %s\n", p.ID, p.Kind, p.Name, p.Code)
		}
	}
	if n.Joining != nil {
		code := ""
		if id, err := core.LoadOrCreateIdentity(home, st.Name); err == nil {
			code = "; check code " + mesh.CheckCode(id.Pub)
		}
		fmt.Printf("\nThis host is asking %s to let it in; approve it on your phone%s.\n", n.Joining.Addr, code)
	}
	return nil
}

func nameOf(v mesh.View, id string) string {
	for _, h := range v.Hosts {
		if h.ID == id {
			return h.Name
		}
	}
	if id == "" {
		return "(none yet)"
	}
	return id
}

func cmdApprove(home string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goblin approve ID (see `goblin status`)")
	}
	st, err := readStatus(home)
	if err != nil {
		return err
	}
	var p *mesh.Pending
	for i := range st.Network.Pending {
		if st.Network.Pending[i].ID == args[0] {
			p = &st.Network.Pending[i]
		}
	}
	if p == nil {
		return fmt.Errorf("nothing waiting with id %s", args[0])
	}
	fmt.Printf("approve %s %q with check code %s? It must match the code shown on that device. [y/N] ", p.Kind, p.Name, p.Code)
	if !strings.HasPrefix(strings.ToLower(readLine()), "y") {
		return fmt.Errorf("not approved")
	}
	if err := checkPIN(home); err != nil {
		return err
	}
	_, id, lg, err := setup(home)
	if err != nil {
		return err
	}
	e := core.Event{T: "add", Kind: p.Kind, ID: p.ID, Pub: p.Pub, Name: p.Name, Pin: p.Pin, TS: time.Now().UnixMilli(), By: id.ID}
	e.Sig = id.Sign(e.Text(lg.NetID()))
	fmt.Println("approved; it can connect within a few seconds")
	return server.WriteControl(home, server.ControlOp{Op: "events", Events: []core.Event{e}})
}

func cmdRemove(home string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goblin remove ID")
	}
	if _, err := readStatus(home); err != nil {
		return err
	}
	_, id, lg, err := setup(home)
	if err != nil {
		return err
	}
	m, ok := lg.Get(args[0])
	if !ok {
		return fmt.Errorf("no member %s", args[0])
	}
	fmt.Printf("remove %s %q for good? It can never come back with this key. [y/N] ", m.Kind, m.Name)
	if !strings.HasPrefix(strings.ToLower(readLine()), "y") {
		return fmt.Errorf("not removed")
	}
	if err := checkPIN(home); err != nil {
		return err
	}
	// sign on a scratch copy, then hand the new events to the running host
	tmp := &core.Log{}
	if err := tmp.Adopt(lg.NetID(), lg.All()); err != nil {
		return err
	}
	before := len(tmp.All())
	if err := tmp.RemoveMember(id, m.ID); err != nil {
		return err
	}
	ev := tmp.All()[before:]
	fmt.Println("removed; every host learns it as soon as it connects")
	return server.WriteControl(home, server.ControlOp{Op: "events", Events: ev})
}

func cmdPin(home string) error {
	if core.HasPIN(home) {
		if err := checkPIN(home); err != nil {
			return err
		}
	}
	p1 := readSecret("new PIN: ")
	if p1 != readSecret("again: ") {
		return fmt.Errorf("the PINs don't match")
	}
	return core.SetPIN(home, p1)
}

func cmdConfig(home string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: goblin config check [FILE] | goblin config path")
	}
	switch args[0] {
	case "path":
		fmt.Println(core.ConfigPath(home))
		return nil
	case "check":
		path := core.ConfigPath(home)
		if len(args) > 1 {
			path = args[1]
		} else {
			core.LoadConfig(home) // migrates config.json the first time
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		probs := core.CheckConfig(b)
		if len(probs) == 0 {
			fmt.Printf("%s: OK\n", path)
			return nil
		}
		fmt.Printf("%s: %d problem(s)\n", path, len(probs))
		for _, p := range probs {
			fmt.Println("  " + p)
		}
		os.Exit(1)
	}
	return fmt.Errorf("usage: goblin config check [FILE] | goblin config path")
}

// cmdUI opens the local page (127.0.0.1 only) with this user's token.
func cmdUI(home string) error {
	cfg, err := core.LoadConfig(home)
	if err != nil {
		return err
	}
	tok, err := server.UIToken(home)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/ui?t=%s", cfg.UIPort, tok)
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		c = exec.Command("open", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	if err := c.Start(); err != nil {
		fmt.Println("open this in a browser on this computer:")
	} else {
		fmt.Println("opening the Goblin page; if no browser appears, open:")
	}
	fmt.Println("  " + u)
	return nil
}

// stopContext ends on Ctrl+C or a stop request and logs which one. On Windows a console close, sign-out or
// shutdown arrives as SIGTERM: the log then shows why a hidden task stopped (seen 2026-11-15 on the Surface).
func stopContext(what string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-c:
			log.Printf("%s stopping: %v", what, sig)
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(c)
	}()
	return ctx, cancel
}
