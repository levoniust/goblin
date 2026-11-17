//go:build !windows

package term

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

func TestKeeper(t *testing.T) {
	home := t.TempDir()
	k := &Keeper{key: "testkey", sess: map[string]*ksess{}, start: startPty}
	core.WritePrivate(keeperKeyPath(home), []byte(k.key))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: k.Handler()}
	go srv.Serve(ln)
	defer srv.Close()
	b := NewKeeperBackend(home, ln.Addr().(*net.TCPAddr).Port)
	if err := b.Ensure(); err != nil {
		t.Fatal(err)
	}
	wid, pane, err := b.NewWindow("w1", home, "sh")
	if err != nil {
		t.Fatal(err)
	}
	b.SendKeys(pane, []string{"Enter"}, "for i in $(seq 1 120); do echo line-$i; done; echo keeper-$((40+2))")
	var snap []string
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		snap, _ = b.Snapshot(pane, 500)
		if strings.Contains(strings.Join(snap, "\n"), "keeper-42") {
			break
		}
	}
	all := strings.Join(snap, "\n")
	if !strings.Contains(all, "keeper-42") || !strings.Contains(all, "line-1\n") && !strings.Contains(all, "line-1\x1b") && !strings.Contains(stripSGR(all), "line-1\n") {
		t.Fatalf("snapshot missing output or scrollback (%d lines): %q", len(snap), all[max(0, len(all)-400):])
	}
	info, err := b.Info(pane)
	if err != nil || info.Hist == 0 {
		t.Fatalf("info %+v %v (scrollback should have lines)", info, err)
	}
	plain, _ := b.Capture(pane, 50)
	if strings.Contains(plain, "\x1b") || !strings.Contains(plain, "keeper-42") {
		t.Fatalf("capture: %q", plain)
	}
	ws, _ := b.Windows()
	if len(ws) != 1 || ws[0].ID != wid || ws[0].Name != "w1" {
		t.Fatalf("windows %+v", ws)
	}
	att, err := b.Attach(ws[0], "fixed", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan bool, 1)
	go func() {
		acc := ""
		for {
			d, err := att.Read()
			if err != nil {
				return
			}
			acc += string(d)
			if strings.Contains(acc, "viewer-ok") {
				got <- true
				return
			}
		}
	}()
	att.Write([]byte("echo viewer-o''k\r"))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no live output through attach")
	}
	att.Close()
	b.Kill(wid)
	if ws, _ := b.Windows(); len(ws) != 0 {
		t.Fatal("kill left the window")
	}
	if string(KeyBytes("C-c")) != "\x03" || string(KeyBytes("Up")) != "\x1b[A" {
		t.Fatal("key mapping")
	}
}
