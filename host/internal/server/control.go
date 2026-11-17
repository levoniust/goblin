package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
)

// The CLI (`goblin join`, `goblin approve`, `goblin remove`) talks to the running host through files in
// <home>/control: the host picks them up within 2 seconds. Only the local user can write there (local access).
// The host writes status.json there for `goblin status`.

type ControlOp struct {
	Op     string       `json:"op"` // join | events | test | canceljoin
	Addr   string       `json:"addr,omitempty"`
	Events []core.Event `json:"events,omitempty"`
}

func ControlDir(home string) string { return filepath.Join(home, "control") }

// WriteControl drops an op for the running host.
func WriteControl(home string, op ControlOp) error {
	b, _ := json.Marshal(op)
	name := filepath.Join(ControlDir(home), time.Now().Format("20060102-150405.000000000")+".json")
	return core.WritePrivate(name, b)
}

// StatusPath is where the host keeps its current view for `goblin status`.
func StatusPath(home string) string { return filepath.Join(home, "status.json") }

func (s *Server) ControlLoop(ctx context.Context) {
	os.MkdirAll(ControlDir(s.home), 0o700)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	lastChats := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// a chat whose program ended (or one opened from the computer itself) changes the list without an API call:
		// tell the clients, or they keep showing a chat that is gone
		if ws, err := s.term.Windows(); err == nil {
			var b strings.Builder
			for _, w := range ws {
				fmt.Fprintf(&b, "%s|%s|%v;", w.ID, w.Name, w.Dead)
			}
			if sig := b.String(); sig != lastChats {
				if lastChats != "" {
					s.ev.broadcast(map[string]any{"type": "sessions"})
				}
				lastChats = sig
			}
		}
		st := s.mesh.Status()
		core.WriteJSON(StatusPath(s.home), map[string]any{"updated": time.Now().UnixMilli(), "network": st, "name": s.cfg.Name,
			"id": s.id.ID, "port": s.cfg.Port})
		ents, _ := os.ReadDir(ControlDir(s.home))
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			p := filepath.Join(ControlDir(s.home), e.Name())
			var op ControlOp
			core.ReadJSON(p, &op)
			os.Remove(p)
			switch op.Op {
			case "join":
				s.mesh.StartJoin(op.Addr, "", "")
			case "canceljoin":
				s.mesh.CancelJoin()
			case "events":
				n := s.mesh.MergeEvents(op.Events)
				log.Printf("control: %d member-log events added", n)
			case "test":
				s.mesh.RequestTest()
			}
		}
	}
}
