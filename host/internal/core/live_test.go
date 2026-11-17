package core

import (
	"slices"
	"testing"
)

func TestApplyReload(t *testing.T) {
	home := t.TempDir()
	c, _ := LoadConfig(home)
	before := c.Live()
	n, _ := LoadConfig(home)
	n.Launchers = ValidLaunchers([]Launcher{{Kind: "pi-oss", Command: "pi --model llamacpp/gpt-oss-20b"}})
	n.Links = map[string]string{"vigil": "http://192.168.1.20:30300"}
	n.PreferHub = true
	n.Port = 8300
	ch, restart := c.Apply(n)
	if !slices.Equal(ch, []string{"launchers", "links", "hub preference"}) || !slices.Equal(restart, []string{"port"}) {
		t.Fatalf("changed %v, restart %v", ch, restart)
	}
	l := c.Live()
	if l == before || l.Launchers[0].Kind != "pi-oss" || l.Links["vigil"] == "" || !l.PreferHub {
		t.Fatalf("live not updated: %+v", l)
	}
	if len(before.Launchers) != 3 {
		t.Fatal("old snapshot changed under a reader")
	}
	if ch, _ := c.Apply(n); len(ch) != 0 {
		t.Fatalf("second apply of the same settings changed %v", ch)
	}
}
