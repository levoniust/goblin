// Package core holds what every part of the Goblin host shares: config, identity, the member log and request signing.
package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
)

const Version = "0.2.1"

// API is the major version of the client API (docs/PROTOCOL.md). Changes inside one major version only add things;
// a breaking change is API 2, and nodes keep serving API 1 for at least one release.
const API = 1

// Features this node offers through the client API. Clients check these instead of version numbers; a name never
// changes meaning. New names are appended.
var Features = []string{
	"sessions", "keys", "screen", "term", "reader2", "fs", "launchers", "resume", "events",
	"network", "approvals", "relay", "speedtest", "links", "linkproxy", "upload", "alerts", "status", "file",
}

// Launcher is one entry of the "new chat" menu. Launchers are set per machine (the design rules).
type Launcher struct {
	Kind    string `json:"kind" yaml:"kind"`                           // id, e.g. "claude"
	Label   string `json:"label" yaml:"label"`                         // badge text
	Name    string `json:"name" yaml:"name"`                           // default window name
	Command string `json:"command" yaml:"command"`                     // program to run; "" = plain shell
	Resume  string `json:"resume,omitempty" yaml:"resume,omitempty"`   // command that continues the last chat after a reboot
	Claude  bool   `json:"claude" yaml:"claude"`                       // Claude Code: gets hooks / Reader support
	Accent  int    `json:"accent" yaml:"accent"`                       // skin colour 1 or 2
	Icon    string `json:"icon" yaml:"icon"`                           // cloud|chip|terminal|spark
	Process string `json:"process,omitempty" yaml:"process,omitempty"` // text in a process command line that identifies this launcher
}

// Config is goblin.yaml in the data dir (yamlconfig.go). Everything has a default, so an empty file works.
type Config struct {
	Name            string     `json:"name" yaml:"name"`
	Port            int        `json:"port" yaml:"port"`                         // client + mesh API (HTTPS)
	BeaconPort      int        `json:"beacon_port" yaml:"beacon_port"`           // LAN discovery (UDP broadcast)
	Addrs           []string   `json:"addrs" yaml:"addrs"`                       // extra addresses to advertise (auto-detected ones are added)
	Seeds           []string   `json:"seeds" yaml:"seeds"`                       // "host:port" of other hosts to try
	TmuxSession     string     `json:"tmux_session" yaml:"tmux_session"`         // Linux: the tmux session that holds the chats
	Roots           []string   `json:"roots" yaml:"roots"`                       // folders chats may start in
	FailoverSeconds int        `json:"failover_seconds" yaml:"failover_seconds"` // hub silent this long = lost (the owner: default 90)
	HeartbeatSecs   int        `json:"heartbeat_seconds" yaml:"heartbeat_seconds"`
	NeverHub        bool       `json:"never_hub" yaml:"never_hub"`
	PreferHub       bool       `json:"prefer_hub" yaml:"prefer_hub"` // wins ties (results within 20%)
	NetTestURL      string     `json:"net_test_url" yaml:"net_test_url"`
	Tailscale       bool       `json:"tailscale" yaml:"tailscale"`                           // plugin: learn peer addresses from `tailscale status`
	ChatPriority    string     `json:"chat_priority" yaml:"chat_priority"`                   // normal | below_normal (default) | idle
	TestNoDial      []string   `json:"test_no_dial,omitempty" yaml:"test_no_dial,omitempty"` // tests only: never connect to these host ids (a broken link)
	KeeperPort      int        `json:"keeper_port" yaml:"keeper_port"`
	UIPort          int        `json:"ui_port" yaml:"ui_port"` // the local page (127.0.0.1 only)
	Launchers       []Launcher `json:"launchers" yaml:"launchers"`
	// Links: web pages the app can show, shared through /api/info (e.g. "vigil": "http://192.168.1.20:30300").
	Links map[string]string `json:"links,omitempty" yaml:"links,omitempty"`

	live atomic.Pointer[Live] // reloadable settings in use now (live.go)
}

var (
	kindRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	cmdRE  = regexp.MustCompile(`^[A-Za-z0-9_./ =:\\-]*$`)
)

// DefaultLaunchers: day-one harnesses are Claude Code, Pi and a plain shell.
func DefaultLaunchers() []Launcher {
	return []Launcher{
		{Kind: "claude", Label: "Claude", Name: "claude", Command: "claude", Resume: "claude --continue", Claude: true, Accent: 1, Icon: "cloud"},
		{Kind: "pi", Label: "Pi", Name: "pi", Command: "pi", Resume: "pi --continue", Accent: 2, Icon: "chip", Process: "pi-coding-agent"},
		{Kind: "shell", Label: "Shell", Name: "shell", Command: "", Accent: 1, Icon: "terminal"},
	}
}

// Home is the data dir: $GOBLIN_HOME, else ~/.config/goblin (Linux/Mac) or %APPDATA%\goblin (Windows).
func Home() string {
	if h := os.Getenv("GOBLIN_HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		if a := os.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, "goblin")
		}
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(d, "goblin")
}

func (c *Config) fill() {
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
	c.Name = CleanName(c.Name)
	if c.Port == 0 {
		c.Port = 8200
	}
	if c.BeaconPort == 0 {
		c.BeaconPort = 47820
	}
	if c.TmuxSession == "" {
		c.TmuxSession = "goblin"
	}
	if len(c.Roots) == 0 {
		h, _ := os.UserHomeDir()
		c.Roots = []string{h}
	}
	if c.FailoverSeconds <= 0 {
		c.FailoverSeconds = 90
	}
	if c.ChatPriority == "" {
		c.ChatPriority = "below_normal" // 2026-11-17: AI work takes a back seat to the computer's user
	}
	if c.HeartbeatSecs <= 0 {
		c.HeartbeatSecs = 15
	}
	if c.NetTestURL == "" {
		c.NetTestURL = "https://speed.cloudflare.com/__down?bytes=65536"
	}
	if c.KeeperPort == 0 {
		c.KeeperPort = 8201
	}
	if c.UIPort == 0 {
		c.UIPort = 8202
	}
	c.Launchers = ValidLaunchers(c.Launchers)
}

// ValidLaunchers drops bad entries (commands run inside a shell: plain words only) and always keeps a shell.
func ValidLaunchers(in []Launcher) []Launcher {
	if len(in) == 0 {
		in = DefaultLaunchers()
	}
	var out []Launcher
	seen := map[string]bool{}
	for _, l := range in {
		if !kindRE.MatchString(l.Kind) || !cmdRE.MatchString(l.Command) || !cmdRE.MatchString(l.Resume) || seen[l.Kind] {
			continue
		}
		seen[l.Kind] = true
		if l.Label == "" {
			l.Label = l.Kind
		}
		if l.Name == "" {
			l.Name = l.Kind
		}
		if l.Accent != 2 {
			l.Accent = 1
		}
		if l.Icon == "" {
			l.Icon = "chip"
		}
		out = append(out, l)
	}
	if !seen["shell"] {
		out = append(out, DefaultLaunchers()[2])
	}
	return out
}

// CleanName keeps names printable and free of the "|" used in signed text.
func CleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '|' || r < 32 {
			return -1
		}
		return r
	}, s)
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.TrimSpace(s)
}

// WritePrivate replaces a file atomically with mode 600.
func WritePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJSON loads a JSON file into v; a missing file leaves v untouched.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// WriteJSON saves v as private JSON.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return WritePrivate(path, b)
}
