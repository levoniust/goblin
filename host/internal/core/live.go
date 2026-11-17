package core

import (
	"maps"
	"slices"
)

// Live holds the settings that change while Goblin runs (goblin.yaml is reloaded on save). Readers take one snapshot
// with cfg.Live() and never see half an update. Everything else in Config needs a restart.
type Live struct {
	Launchers []Launcher
	Links     map[string]string
	Roots     []string
	PreferHub bool
	NeverHub  bool
}

// Live returns the current snapshot of the reloadable settings.
func (c *Config) Live() *Live {
	if p := c.live.Load(); p != nil {
		return p
	}
	c.live.CompareAndSwap(nil, c.snapshot())
	return c.live.Load()
}

func (c *Config) snapshot() *Live {
	return &Live{Launchers: slices.Clone(c.Launchers), Links: maps.Clone(c.Links), Roots: slices.Clone(c.Roots),
		PreferHub: c.PreferHub, NeverHub: c.NeverHub}
}

// Apply takes the reloadable settings from a newly loaded config. It returns what changed and which changed
// settings only take effect after a restart.
func (c *Config) Apply(n *Config) (changed, needRestart []string) {
	old := c.Live()
	nl := n.snapshot()
	if yamlValue(old.Launchers) != yamlValue(nl.Launchers) {
		changed = append(changed, "launchers")
	}
	if yamlValue(old.Links) != yamlValue(nl.Links) {
		changed = append(changed, "links")
	}
	if yamlValue(old.Roots) != yamlValue(nl.Roots) {
		changed = append(changed, "roots")
	}
	if old.PreferHub != nl.PreferHub || old.NeverHub != nl.NeverHub {
		changed = append(changed, "hub preference")
	}
	c.live.Store(nl)
	for _, f := range []struct {
		name string
		a, b any
	}{
		{"name", c.Name, n.Name}, {"port", c.Port, n.Port}, {"beacon_port", c.BeaconPort, n.BeaconPort},
		{"keeper_port", c.KeeperPort, n.KeeperPort}, {"ui_port", c.UIPort, n.UIPort}, {"tmux_session", c.TmuxSession, n.TmuxSession},
		{"failover_seconds", c.FailoverSeconds, n.FailoverSeconds}, {"heartbeat_seconds", c.HeartbeatSecs, n.HeartbeatSecs},
		{"net_test_url", c.NetTestURL, n.NetTestURL}, {"tailscale", c.Tailscale, n.Tailscale},
		{"addrs", c.Addrs, n.Addrs}, {"seeds", c.Seeds, n.Seeds},
	} {
		if yamlValue(f.a) != yamlValue(f.b) {
			needRestart = append(needRestart, f.name)
		}
	}
	return changed, needRestart
}
