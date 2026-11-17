package core

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConfigMigratesJSON(t *testing.T) {
	home := t.TempDir()
	js := `{"name":"gaming-pc","prefer_hub":true,"launchers":[{"kind":"pi","label":"Pi Qwen 27B","command":"pi --model llamacpp/qwen3.8-27b","resume":"pi --model llamacpp/qwen3.8-27b --continue","accent":2,"icon":"chip","process":"pi-coding-agent"}]}`
	os.WriteFile(filepath.Join(home, "config.json"), []byte(js), 0o600)
	c, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "gaming-pc" || !c.PreferHub || len(c.Launchers) != 2 || c.Launchers[0].Command != "pi --model llamacpp/qwen3.8-27b" {
		t.Fatalf("migrated config wrong: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json.bak")); err != nil {
		t.Fatal("config.json.bak missing")
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err == nil {
		t.Fatal("config.json still there")
	}
	b, _ := os.ReadFile(ConfigPath(home))
	if p := CheckConfig(b); p != nil {
		t.Fatalf("written goblin.yaml has problems: %v\n%s", p, b)
	}
	c2, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	r1, _ := RenderConfig(c)
	r2, _ := RenderConfig(c2)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("round trip changed settings:\n%+v\n%+v", c, c2)
	}
}

func TestConfigEmptyAndLinks(t *testing.T) {
	home := t.TempDir()
	c, err := LoadConfig(home)
	if err != nil || c.Port != 8200 || len(c.Launchers) != 3 {
		t.Fatalf("defaults: %v %+v", err, c)
	}
	c.Links = map[string]string{"vigil": "http://192.168.1.20:30300"}
	if err := c.Save(home); err != nil {
		t.Fatal(err)
	}
	c2, _ := LoadConfig(home)
	if c2.Links["vigil"] != "http://192.168.1.20:30300" {
		t.Fatalf("links lost: %+v", c2.Links)
	}
}

func TestCheckConfigFindsMistakes(t *testing.T) {
	text := `name: Surface
prefer_hub: true
never_hub: true
failoverseconds: 90
`
	p := CheckConfig([]byte(text))
	if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), "line 4") || !strings.Contains(strings.Join(p, "\n"), "failoverseconds") {
		t.Fatalf("unknown field not reported with its line: %v", p)
	}
	text = `name: Surface
prefer_hub: true
never_hub: true
links:
  vigil: not-a-url
launchers:
  - kind: shell
    command: ""
  - kind: Bad Kind
    command: "rm -rf / ; echo"
    icon: rocket
`
	got := strings.Join(CheckConfig([]byte(text)), "\n")
	for _, want := range []string{"line 3: never_hub and prefer_hub", "line 4: links.vigil", "line 9: launchers[2]: kind", "line 9: launchers[2] (Bad Kind): command", "icon"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
