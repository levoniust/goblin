package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/levoniust/goblin/host/internal/term"
)

func qint(r *http.Request, k string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(k))
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

// terminal attaches the app to one chat (same protocol as Switchboard):
//
//	?mode=live    the client's size resizes the window; raw bytes both ways
//	?mode=fixed   never resizes the window; raw bytes at the window's size, announced as {"size":[c,r]}
//	?mode=reader  text updates instead of bytes; with &proto=2 seeded with the whole scrollback ({"full","tail"})
//	              then {"keep_len","tail","trim?"} updates
//
// Binary frames from the client = key bytes; text frames = {"input": "..."} or {"resize": [c, r]} (live mode).
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowFor(r.PathValue("sid"))
	if !ok {
		http.Error(w, "no such session", 404)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "live"
	}
	cols, rows := qint(r, "cols", 80, 10, 1000), qint(r, "rows", 30, 5, 500)
	if mode != "live" {
		if i, err := s.term.Info(win.Pane); err == nil {
			cols, rows = i.W, i.H
		}
	}
	c, err := accept(w, r)
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	att, err := s.term.Attach(win, mode, cols, rows)
	if err != nil {
		c.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	defer att.Close()
	defer c.Close(websocket.StatusNormalClosure, "")
	write := func(typ websocket.MessageType, b []byte) error {
		wc, cl := context.WithTimeout(ctx, 15*time.Second)
		defer cl()
		return c.Write(wc, typ, b)
	}
	writeJ := func(v any) error { b, _ := json.Marshal(v); return write(websocket.MessageText, b) }

	go func() { // terminal output
		defer cancel()
		for {
			data, err := att.Read()
			if err != nil {
				return
			}
			if mode != "reader" { // the reader gets snapshots; its terminal output is only drained
				if write(websocket.MessageBinary, data) != nil {
					return
				}
			}
		}
	}()
	switch mode {
	case "fixed":
		go s.followSize(ctx, cancel, win, att, writeJ, cols, rows)
	case "reader":
		go s.readerLoop(ctx, cancel, win, att, writeJ, r.URL.Query().Get("proto") == "2", cols, rows)
	}
	go func() { // keepalive
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pc, cl := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pc)
				cl()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	log.Printf("client %s attached %s to %s", caller(r).ID, mode, win.Name)
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageBinary {
			att.Write(data)
			continue
		}
		var m struct {
			Input  *string `json:"input"`
			Resize []int   `json:"resize"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.Input != nil {
			att.Write([]byte(*m.Input))
		}
		if len(m.Resize) == 2 && mode == "live" {
			att.Resize(m.Resize[0], m.Resize[1])
		}
	}
}

func (s *Server) followSize(ctx context.Context, cancel func(), win term.Window, att term.Attachment, writeJ func(any) error, cols, rows int) {
	defer cancel()
	if writeJ(map[string]any{"size": []int{cols, rows}}) != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		i, err := s.term.Info(win.Pane)
		if err != nil {
			return
		}
		if i.W != cols || i.H != rows {
			cols, rows = i.W, i.H
			att.Resize(cols, rows)
			if writeJ(map[string]any{"size": []int{cols, rows}}) != nil {
				return
			}
		}
	}
}

// readerLoop sends the chat's text whenever it changes (Switchboard's reader proto 1 and 2).
func (s *Server) readerLoop(ctx context.Context, cancel func(), win term.Window, att term.Attachment, writeJ func(any) error, proto2 bool, cols, rows int) {
	defer cancel()
	const depth = 2000
	var prev, full []string
	prevStart, quiet := 0, 0
	size := [2]int{cols, rows}
	seed := func() error {
		f, err := s.term.Snapshot(win.Pane, term.DeepCapture)
		if err != nil {
			return err
		}
		total := 0
		for _, l := range f {
			total += len(l)
		}
		for len(f) > 20 && total > 3_500_000 { // stay under the client's message cap
			total -= len(f[0])
			f = f[1:]
		}
		full = f
		ii, _ := s.term.Info(win.Pane)
		prev, _ = s.term.Snapshot(win.Pane, depth)
		w, alt := cols, false
		if ii != nil {
			prevStart = max(0, ii.Hist-depth)
			w, alt = ii.W, ii.Alt
		}
		return writeJ(map[string]any{"full": true, "tail": full, "proto": 2, "w": w, "alt": alt})
	}
	for {
		i, err := s.term.Info(win.Pane)
		if err != nil {
			return
		}
		if i.W != size[0] || i.H != size[1] {
			size = [2]int{i.W, i.H}
			att.Resize(i.W, i.H)
		}
		lines, err := s.term.Snapshot(win.Pane, depth)
		if err != nil {
			return
		}
		start := max(0, i.Hist-depth)
		changed := !equalLines(lines, prev)
		switch {
		case proto2 && (full == nil || start < prevStart):
			if seed() != nil {
				return
			}
		case changed:
			d := term.ScreenDiff(prev, lines, start-prevStart)
			if proto2 {
				if d.Full {
					if seed() != nil {
						return
					}
					break
				}
				keep := term.FullKeepLen(len(full), len(prev), d)
				keep = max(0, min(keep, len(full)))
				full = append(full[:keep:keep], d.Tail...)
				msg := map[string]any{"keep_len": keep, "tail": d.Tail, "w": i.W, "alt": i.Alt}
				if trim := len(full) - term.FullCap; trim > 0 {
					full = full[trim:]
					msg["trim"] = trim
				}
				if writeJ(msg) != nil {
					return
				}
			} else if writeJ(map[string]any{"drop": d.Drop, "keep": d.Keep, "tail": d.Tail, "full": d.Full, "w": i.W, "alt": i.Alt}) != nil {
				return
			}
			prev, prevStart, quiet = lines, start, 0
		default:
			quiet++
		}
		wait := 150 * time.Millisecond
		if quiet >= 10 {
			wait = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) || a == nil != (b == nil) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
