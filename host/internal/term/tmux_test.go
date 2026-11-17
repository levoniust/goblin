//go:build !windows

package term

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

func TestScreenDiff(t *testing.T) {
	prev := []string{"a", "b", "c"}
	d := ScreenDiff(prev, []string{"b", "c", "d"}, 1)
	if d.Drop != 1 || d.Keep != 2 || len(d.Tail) != 1 || d.Tail[0] != "d" || d.Full {
		t.Fatalf("%+v", d)
	}
	if k := FullKeepLen(10, 3, d); k != 10 {
		t.Fatalf("keep len %d", k)
	}
	if !ScreenDiff(nil, []string{"x"}, 0).Full {
		t.Fatal("first diff should be full")
	}
}

func TestParseOptions(t *testing.T) {
	s := "Do you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No\n"
	o := ParseOptions(s)
	if len(o) != 3 || o[2][1] != "No" {
		t.Fatalf("%v", o)
	}
}

func TestTmuxBackend(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	b := &Tmux{Session: "gtest", Socket: "goblin-test-" + time.Now().Format("150405.000"), Bin: "tmux"}
	defer exec.Command("tmux", "-L", b.Socket, "kill-server").Run()
	if err := b.Ensure(); err != nil {
		t.Fatal(err)
	}
	ls := core.DefaultLaunchers()
	wid, pane, err := b.NewWindow("work", t.TempDir(), b.CommandFor(ls[2], false))
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := b.Windows()
	if len(ws) != 2 {
		t.Fatalf("windows: %+v", ws)
	}
	time.Sleep(500 * time.Millisecond)
	if err := b.SendKeys(pane, []string{"Enter"}, "echo goblin-$((40+2))"); err != nil {
		t.Fatal(err)
	}
	var snap []string
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		snap, _ = b.Snapshot(pane, 100)
		if strings.Contains(strings.Join(snap, "\n"), "goblin-42") {
			break
		}
	}
	if !strings.Contains(strings.Join(snap, "\n"), "goblin-42") {
		t.Fatalf("output missing: %q", snap)
	}
	if info, err := b.Info(pane); err != nil || info.W == 0 {
		t.Fatalf("info %v %v", info, err)
	}
	var w Window
	for _, x := range ws {
		if x.ID == wid {
			w = x
		}
	}
	if k := b.DetectKind(w.PID, ls); k != "shell" {
		t.Fatalf("kind %s", k)
	}
	att, err := b.Attach(w, "fixed", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		var all string
		for {
			data, err := att.Read()
			all += string(data)
			if strings.Contains(all, "attached-ok") || err != nil {
				got <- all
				return
			}
		}
	}()
	att.Write([]byte("echo attached-o\"k\"\r"))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no output through the attachment")
	}
	att.Close()
	if err := b.Kill(wid); err != nil {
		t.Fatal(err)
	}
}
