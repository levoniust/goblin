package mesh

import "testing"

func TestPickRules(t *testing.T) {
	ph := candidate{ID: "powerhouse", Boot: 300, Prefer: true}
	sf := candidate{ID: "surface", Boot: 100}
	pc := candidate{ID: "timpc", Boot: 200}
	all := []candidate{ph, sf, pc}
	cases := []struct {
		name string
		res  map[string]float64
		want string
	}{
		{"fastest wins when clearly faster", map[string]float64{"powerhouse": 100, "surface": 40, "timpc": 90}, "surface"},
		{"prefer wins a close result (within 20%)", map[string]float64{"powerhouse": 47, "surface": 40, "timpc": 90}, "powerhouse"},
		{"prefer loses when just outside 20%", map[string]float64{"powerhouse": 49, "surface": 40}, "surface"},
		{"failed tests are ignored", map[string]float64{"powerhouse": 0, "surface": 80}, "surface"},
		{"no results: longest up", map[string]float64{}, "surface"},
	}
	for _, c := range cases {
		if got := Pick(all, c.res); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	if Pick([]candidate{ph}, nil) != "powerhouse" {
		t.Error("a host alone is its own hub")
	}
}
