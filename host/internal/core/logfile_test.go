package core

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestLogFileRotates(t *testing.T) {
	home := t.TempDir()
	l, err := OpenLogFile(home, "goblin.log")
	if err != nil {
		t.Fatal(err)
	}
	l.maxSize = 1000
	for i := 0; i < 200; i++ {
		fmt.Fprintf(l, "line %03d %s\n", i, strings.Repeat("x", 40))
	}
	for _, s := range []string{"", ".1", ".2", ".3"} {
		if st, err := os.Stat(LogPath(home, "goblin.log") + s); err != nil || st.Size() > 1000 {
			t.Fatalf("goblin.log%s: %v", s, err)
		}
	}
	if _, err := os.Stat(LogPath(home, "goblin.log") + ".4"); err == nil {
		t.Fatal("kept more than 3 old logs")
	}
	tail := TailFile(LogPath(home, "goblin.log"), 120)
	if !strings.HasSuffix(tail, "\n") || !strings.HasPrefix(tail, "line ") || !strings.Contains(tail, "line 199") {
		t.Fatalf("tail: %q", tail)
	}
}
