package term

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// KeeperSelfTest starts a keeper on port, opens a shell chat, types into it and reads the screen + a live view.
// It uses its own scratch folder, so it never touches the real keeper's key.
func KeeperSelfTest(_ string, port int) error {
	home, err := os.MkdirTemp("", "goblin-selftest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunKeeper(ctx, home, port)
	time.Sleep(700 * time.Millisecond)
	b := NewKeeperBackend(home, port)
	if err := b.Ensure(); err != nil {
		return err
	}
	shell := core.Launcher{Kind: "shell"}
	cmd := b.CommandFor(shell, false)
	if runtime.GOOS != "windows" {
		cmd = "sh"
	}
	wid, pane, err := b.NewWindow("selftest", home, cmd)
	if err != nil {
		return fmt.Errorf("new window: %w", err)
	}
	defer b.Kill(wid)
	time.Sleep(1500 * time.Millisecond)
	probe := "echo goblin-selftest-$((6*7))"
	if runtime.GOOS == "windows" {
		probe = "echo ('goblin-selftest-' + (6*7))"
	}
	b.SendKeys(pane, []string{"Enter"}, probe)
	want := "goblin-selftest-42"
	var text string
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		text, _ = b.Capture(pane, 200)
		if strings.Contains(text, want) {
			break
		}
	}
	if !strings.Contains(text, want) {
		return fmt.Errorf("output not on screen: %q", text)
	}
	info, err := b.Info(pane)
	if err != nil {
		return err
	}
	ws, _ := b.Windows()
	att, err := b.Attach(ws[0], "fixed", 100, 30)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	defer att.Close()
	got := make(chan bool, 1)
	go func() {
		acc := ""
		for {
			d, err := att.Read()
			if err != nil {
				return
			}
			acc += string(d)
			if strings.Contains(acc, "live-view-ok") {
				got <- true
				return
			}
		}
	}()
	att.Write([]byte("echo live-view-ok\r"))
	select {
	case <-got:
	case <-time.After(8 * time.Second):
		return fmt.Errorf("no live output through attach")
	}
	// close a chat, then open another: the keeper must survive a kill (a double close once took it down)
	att.Close()
	b.Kill(wid)
	time.Sleep(1500 * time.Millisecond)
	wid2, _, err := b.NewWindow("selftest2", home, cmd)
	if err != nil {
		return fmt.Errorf("keeper did not survive closing a chat: %w", err)
	}
	b.Kill(wid2)
	time.Sleep(1500 * time.Millisecond)
	if _, err := b.Windows(); err != nil {
		return fmt.Errorf("keeper did not survive closing a second chat: %w", err)
	}
	fmt.Printf("keeper ok: screen %dx%d, scrollback %d lines, alt=%v, survives closing chats\n", info.W, info.H, info.Hist, info.Alt)
	return nil
}
