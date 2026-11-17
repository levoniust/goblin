package server

import (
	"testing"

	"github.com/levoniust/goblin/host/internal/term"
)

func TestAlertFlow(t *testing.T) {
	home := t.TempDir()
	a := newAlertStore(home)
	w := term.Window{ID: "@1", Pane: "%1", Name: "claude"}
	ev := func(e string, detail string) (*Alert, bool) {
		return a.apply(HookEvent{Chat: "%1", Kind: "claude", Event: e, Detail: detail}, w, "h")
	}
	if al, ch := ev("working", "You: fix it"); al != nil || !ch {
		t.Fatal("working: no alert, but a change")
	}
	if st, last, _ := a.view("%1"); st != "working" || last != "You: fix it" {
		t.Fatalf("status %q last %q", st, last)
	}
	if _, ch := ev("working", ""); ch {
		t.Fatal("working twice is no change")
	}
	al, _ := ev("permission", "rm -rf build")
	if al == nil || al.Type != "permission" || al.State != "open" || al.Title != "Needs permission" || al.Window != "@1" {
		t.Fatalf("permission alert: %+v", al)
	}
	if again, _ := ev("permission", "rm -rf build"); again != nil {
		t.Fatal("the same prompt reported twice made a second alert")
	}
	if st, _, p := a.view("%1"); st != "permission" || p == nil || p.ID != al.ID {
		t.Fatalf("pending: %q %+v", st, p)
	}
	ev("working", "")
	if a.get(al.ID).State != "closed" {
		t.Fatal("moving on did not close the prompt")
	}
	d, _ := ev("done", "All fixed.")
	if d == nil || d.State != "closed" || d.Body != "All fixed." {
		t.Fatalf("done alert: %+v", d)
	}
	if again, _ := ev("done", "idle notice"); again != nil {
		t.Fatal("done twice made a second alert")
	}
	if st, _, p := a.view("%1"); st != "done" || p != nil {
		t.Fatalf("after done: %q %+v", st, p)
	}
	q, _ := ev("question", "Which one?")
	a.markAnswered(q.ID, "2")
	if g := a.get(q.ID); g.State != "answered" || g.Answered != "2" {
		t.Fatalf("answered: %+v", g)
	}
	if list := a.all(); len(list) != 3 || list[0].ID != q.ID {
		t.Fatalf("newest first: %+v", list)
	}
	// alerts survive a restart; open ones come back closed
	ev("permission", "x")
	b := newAlertStore(home)
	if l := b.all(); len(l) != 4 || l[0].State != "closed" || b.next != 4 {
		t.Fatalf("reloaded: %+v next %d", l, b.next)
	}
	a.forget(map[string]bool{})
	if st, _, _ := a.view("%1"); st != "" {
		t.Fatal("a gone chat kept its status")
	}
}

func TestBusyShell(t *testing.T) {
	for cmd, want := range map[string]bool{"bash": false, "zsh": false, "vim": true, "python3": true, "": false,
		`powershell.exe -NoLogo`: false, "/usr/bin/htop": true, "pwsh.exe": false} {
		if busyShell(cmd) != want {
			t.Errorf("busyShell(%q) != %v", cmd, want)
		}
	}
}
