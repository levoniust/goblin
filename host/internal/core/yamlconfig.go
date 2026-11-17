package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// ConfigFile is the node's settings file in the data dir. People and AIs edit it; `goblin config check` and the local
// page check it. It replaced config.json in 0.2.0 (migrated once, the old file is kept as config.json.bak).
const ConfigFile = "goblin.yaml"

func ConfigPath(home string) string { return filepath.Join(home, ConfigFile) }

// LoadConfig reads goblin.yaml (or migrates config.json), then fills defaults. A missing file means all defaults.
func LoadConfig(home string) (*Config, error) {
	c := &Config{}
	yp := ConfigPath(home)
	b, err := os.ReadFile(yp)
	if err == nil {
		if err := decodeConfig(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", yp, err)
		}
		c.fill()
		return c, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	jp := filepath.Join(home, "config.json")
	jb, jerr := os.ReadFile(jp)
	if jerr != nil && !errors.Is(jerr, os.ErrNotExist) {
		return nil, jerr
	}
	if len(jb) > 0 {
		if err := json.Unmarshal(jb, c); err != nil {
			return nil, fmt.Errorf("%s: %w", jp, err)
		}
	}
	c.fill()
	if jerr == nil {
		// migrate: write goblin.yaml with the same settings, keep the old file as a backup
		if err := c.Save(home); err == nil {
			os.Rename(jp, jp+".bak")
		}
	}
	return c, nil
}

func decodeConfig(b []byte, c *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true) // a misspelt setting is an error, not silently ignored
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return errors.New(strings.ReplaceAll(err.Error(), " not found in type core.Config", " is not a Goblin setting (misspelt?)"))
	}
	return nil
}

// Save writes goblin.yaml (user-only) with a comment on every setting.
func (c *Config) Save(home string) error {
	b, err := RenderConfig(c)
	if err != nil {
		return err
	}
	return WritePrivate(ConfigPath(home), b)
}

// CheckConfig reports every problem in a goblin.yaml text, each starting with "line N:" when the line is known.
// No problems = nil.
func CheckConfig(b []byte) []string {
	var c Config
	if err := decodeConfig(b, &c); err != nil {
		msg := strings.TrimPrefix(err.Error(), "yaml: ")
		msg = strings.ReplaceAll(msg, "unmarshal errors:\n  ", "")
		var out []string
		for _, l := range strings.Split(msg, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	var root yaml.Node
	yaml.Unmarshal(b, &root)
	lineOf := func(key string, idx int) int { return nodeLine(&root, key, idx) }
	var out []string
	add := func(line int, f string, a ...any) {
		m := fmt.Sprintf(f, a...)
		if line > 0 {
			m = fmt.Sprintf("line %d: %s", line, m)
		}
		out = append(out, m)
	}
	if len(c.Name) > 40 || strings.ContainsAny(c.Name, "|\n") {
		add(lineOf("name", -1), "name: at most 40 characters, no | or line breaks")
	}
	for _, p := range []struct {
		key string
		v   int
	}{{"port", c.Port}, {"beacon_port", c.BeaconPort}, {"keeper_port", c.KeeperPort}, {"ui_port", c.UIPort}} {
		if p.v < 0 || p.v > 65535 {
			add(lineOf(p.key, -1), "%s: must be a port number (1-65535)", p.key)
		}
	}
	hb := c.HeartbeatSecs
	if hb == 0 {
		hb = 15
	}
	if c.FailoverSeconds != 0 && c.FailoverSeconds < 2*hb {
		add(lineOf("failover_seconds", -1), "failover_seconds: must be at least two heartbeats (%d)", 2*hb)
	}
	if c.HeartbeatSecs < 0 || c.HeartbeatSecs > 300 {
		add(lineOf("heartbeat_seconds", -1), "heartbeat_seconds: 1-300")
	}
	switch c.ChatPriority {
	case "", "normal", "below_normal", "idle":
	default:
		add(lineOf("chat_priority", -1), "chat_priority: normal, below_normal or idle")
	}
	if c.NeverHub && c.PreferHub {
		add(lineOf("never_hub", -1), "never_hub and prefer_hub are both true: pick one")
	}
	if c.NetTestURL != "" && !isWebURL(c.NetTestURL) {
		add(lineOf("net_test_url", -1), "net_test_url: must start with http:// or https://")
	}
	for k, v := range c.Links {
		if !kindRE.MatchString(k) {
			add(lineOf("links", -1), "links: name %q must be lowercase letters, digits, - or _", k)
		}
		if !isWebURL(v) {
			add(lineOf("links", -1), "links.%s: must be an http:// or https:// address", k)
		}
	}
	seen := map[string]bool{}
	for i, l := range c.Launchers {
		ln := lineOf("launchers", i)
		switch {
		case !kindRE.MatchString(l.Kind):
			add(ln, "launchers[%d]: kind %q must be lowercase letters, digits, - or _ (max 32)", i+1, l.Kind)
		case seen[l.Kind]:
			add(ln, "launchers[%d]: kind %q is used twice", i+1, l.Kind)
		}
		seen[l.Kind] = true
		if !cmdRE.MatchString(l.Command) {
			add(ln, "launchers[%d] (%s): command may only use letters, digits, spaces and . _ / = : \\ -", i+1, l.Kind)
		}
		if !cmdRE.MatchString(l.Resume) {
			add(ln, "launchers[%d] (%s): resume may only use letters, digits, spaces and . _ / = : \\ -", i+1, l.Kind)
		}
		if l.Accent != 0 && l.Accent != 1 && l.Accent != 2 {
			add(ln, "launchers[%d] (%s): accent is 1 or 2", i+1, l.Kind)
		}
		switch l.Icon {
		case "", "cloud", "chip", "terminal", "spark":
		default:
			add(ln, "launchers[%d] (%s): icon is cloud, chip, terminal or spark", i+1, l.Kind)
		}
	}
	return out
}

func isWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// nodeLine finds the line of a top-level key (idx < 0) or of item idx in a top-level list.
func nodeLine(root *yaml.Node, key string, idx int) int {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return 0
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		return 0
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		v := m.Content[i+1]
		if idx >= 0 && v.Kind == yaml.SequenceNode && idx < len(v.Content) {
			return v.Content[idx].Line
		}
		return m.Content[i].Line
	}
	return 0
}

var configTmpl = template.Must(template.New("cfg").Funcs(template.FuncMap{"y": yamlValue, "list": yamlBlock}).Parse(
	`# Goblin node settings. A person or an AI may edit this file.
# Check it after editing: ` + "`goblin config check`" + ` (or the Settings page of ` + "`goblin ui`" + `).
# Saved changes to launchers, links, roots, prefer_hub and never_hub apply within a few seconds.
# Everything else (marked "restart" where it matters most) takes effect when Goblin restarts.
# Every setting has a default: delete a line to go back to it.

# Name shown in Switchboard and to the other computers (max 40 characters).
name: {{y .Name}}

# Hub choice. prefer_hub: this computer wins close results (within 20%). never_hub: never the hub.
prefer_hub: {{y .PreferHub}}
never_hub: {{y .NeverHub}}
# The hub counts as lost after this many silent seconds (default 90).
failover_seconds: {{y .FailoverSeconds}}
# Seconds between hellos to the other computers (default 15).
heartbeat_seconds: {{y .HeartbeatSecs}}
# Small download for the internet speed test, used when no client is around.
net_test_url: {{y .NetTestURL}}
# Learn the other computers' addresses from Tailscale (needs the tailscale command).
tailscale: {{y .Tailscale}}
# How chats share the computer with you: below_normal (default: AI work gives way when you need the CPU or disk),
# idle (only runs on spare time), normal. New chats get it; running chats when Goblin restarts.
chat_priority: {{y .ChatPriority}}

# Web pages Switchboard can show; "vigil" is the Vigil tab. Example:  vigil: http://192.168.1.20:30300
links:{{list .Links}}

# Folders chats may start in.
roots:{{list .Roots}}
# Extra addresses to tell the other computers about ("host" or "host:port"); most are found automatically.
addrs:{{list .Addrs}}
# Other computers to try at start ("host:port").
seeds:{{list .Seeds}}

# Ports (restart). port: Switchboard and the other computers (HTTPS). beacon_port: LAN discovery (UDP).
port: {{y .Port}}
beacon_port: {{y .BeaconPort}}
# Windows: the local keeper that holds the chats (restart). Linux: the tmux session that holds them (restart).
keeper_port: {{y .KeeperPort}}
tmux_session: {{y .TmuxSession}}
# The local page for whoever is logged in to this computer (` + "`goblin ui`" + `), on 127.0.0.1 only (restart).
ui_port: {{y .UIPort}}

# The "new chat" menu of this computer.
#   kind:    id, lowercase letters, digits, - or _ (each kind once)
#   label:   badge text                        name:    default chat name
#   command: program to run ("" = plain shell) resume:  command that continues the last chat after a reboot
#   claude:  true for Claude Code              accent:  skin colour, 1 or 2
#   icon:    cloud, chip, terminal or spark    process: text in the program's command line that identifies it
# Commands may only use letters, digits, spaces and . _ / = : \ - (no quotes, pipes or ;).
# A plain "shell" launcher is always added.
launchers:{{list .Launchers}}
`))

// RenderConfig writes the settings as commented YAML.
func RenderConfig(c *Config) ([]byte, error) {
	var buf bytes.Buffer
	if err := configTmpl.Execute(&buf, c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func yamlValue(v any) string {
	b, _ := yaml.Marshal(v)
	return strings.TrimSpace(string(b))
}

// yamlBlock renders a list or map indented under its key, or " []" / " {}" when empty.
func yamlBlock(v any) string {
	b, _ := yaml.Marshal(v)
	s := strings.TrimRight(string(b), "\n")
	if s == "[]" || s == "{}" || s == "null" {
		switch v.(type) {
		case map[string]string:
			return " {}"
		}
		return " []"
	}
	var out strings.Builder
	for _, l := range strings.Split(s, "\n") {
		out.WriteString("\n  " + l)
	}
	return out.String()
}
