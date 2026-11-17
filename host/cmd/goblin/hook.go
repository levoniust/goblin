package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/levoniust/goblin/host/internal/core"
	"github.com/levoniust/goblin/host/internal/server"
)

// goblin hook: harnesses report what a chat is doing (2026-11-15/16: notifications + working/idle status).
//
//	goblin hook claude                     Claude Code hook (all events): reads the hook JSON on stdin
//	goblin hook <harness> <event> [text]   anything else (Pi, Kilo, scripts): event = working | permission |
//	                                       question | done | ended, text = what to show
//
// It only acts inside a Goblin chat (GOBLIN_KIND set; the chat is GOBLIN_CHAT or tmux's TMUX_PANE), sends one small
// request to this computer's Goblin and always exits 0 quickly, printing nothing.
func cmdHook(home string, args []string) {
	defer func() { recover() }()
	kind, chat := os.Getenv("GOBLIN_KIND"), os.Getenv("GOBLIN_CHAT")
	if chat == "" {
		chat = os.Getenv("TMUX_PANE")
	}
	if len(args) == 0 {
		return
	}
	var e server.HookEvent
	if args[0] == "claude" {
		var in map[string]any
		json.NewDecoder(io.LimitReader(os.Stdin, 4<<20)).Decode(&in)
		e = claudeEvent(in)
	} else if len(args) > 1 {
		e = server.HookEvent{Event: args[1], Detail: strings.Join(args[2:], " ")}
	}
	if kind == "" || e.Event == "" {
		return
	}
	e.Chat, e.Kind, e.Harness = chat, kind, args[0]
	if chat == "" { // the host finds the chat by its process (a chat started before GOBLIN_CHAT existed)
		e.PIDs = ancestors()
	}
	cfg, err := core.LoadConfig(home)
	if err != nil {
		return
	}
	tok, err := os.ReadFile(server.UITokenPath(home))
	if err != nil {
		return
	}
	b, _ := json.Marshal(e)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/hook", cfg.UIPort), bytes.NewReader(b))
	req.Header.Set("X-Goblin-Hook", strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req); err == nil {
		resp.Body.Close()
	}
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// claudeEvent maps a Claude Code hook to a Goblin event.
func claudeEvent(in map[string]any) server.HookEvent {
	tool := str(in, "tool_name")
	ti, _ := in["tool_input"].(map[string]any)
	switch str(in, "hook_event_name") {
	case "UserPromptSubmit":
		return server.HookEvent{Event: "working", Detail: "You: " + str(in, "prompt")}
	case "PreToolUse":
		if tool == "AskUserQuestion" {
			return askEvent(ti)
		}
		return server.HookEvent{Event: "working"}
	case "PostToolUse":
		return server.HookEvent{Event: "working"}
	case "PermissionRequest":
		return server.HookEvent{Event: "permission", Title: "Allow " + tool + "?", Detail: toolSummary(ti)}
	case "Notification":
		msg := str(in, "message")
		switch {
		case strings.Contains(strings.ToLower(msg), "permission"):
			return server.HookEvent{Event: "permission", Title: "Needs permission", Detail: msg}
		case strings.Contains(strings.ToLower(msg), "waiting for your input"):
			return server.HookEvent{Event: "done", Title: "Waiting for you", Detail: msg}
		}
	case "Stop":
		last := str(in, "last_assistant_message")
		if last == "" {
			last = lastAssistantText(str(in, "transcript_path"))
		}
		return server.HookEvent{Event: "done", Title: "Finished", Detail: last}
	case "SessionEnd":
		return server.HookEvent{Event: "ended"}
	}
	return server.HookEvent{}
}

func askEvent(ti map[string]any) server.HookEvent {
	e := server.HookEvent{Event: "question", Title: "Has a question"}
	qs, _ := ti["questions"].([]any)
	if len(qs) == 0 {
		return e
	}
	q, _ := qs[0].(map[string]any)
	e.Detail = str(q, "question")
	opts, _ := q["options"].([]any)
	for i, o := range opts {
		om, _ := o.(map[string]any)
		e.Options = append(e.Options, server.AlertOption{Key: fmt.Sprint(i + 1), Label: str(om, "label"), Detail: str(om, "description")})
	}
	return e
}

func toolSummary(ti map[string]any) string {
	for _, k := range []string{"command", "file_path", "url", "pattern", "path", "description"} {
		if s := str(ti, k); s != "" {
			return s
		}
	}
	b, _ := json.Marshal(ti)
	return string(b)
}

// lastAssistantText: the last text Claude wrote, from the end of the session transcript (JSON lines).
func lastAssistantText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 512<<10 {
		f.Seek(st.Size()-512<<10, io.SeekStart)
	}
	last := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type, Text string
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "assistant" {
			continue
		}
		for _, c := range l.Message.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				last = c.Text
			}
		}
	}
	return last
}
