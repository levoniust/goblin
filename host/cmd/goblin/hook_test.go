package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeEvent(t *testing.T) {
	cases := []struct {
		in    map[string]any
		event string
	}{
		{map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "hi"}, "working"},
		{map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash"}, "working"},
		{map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}}, "permission"},
		{map[string]any{"hook_event_name": "Notification", "message": "Claude needs your permission to use Bash"}, "permission"},
		{map[string]any{"hook_event_name": "Notification", "message": "Claude is waiting for your input"}, "done"},
		{map[string]any{"hook_event_name": "Notification", "message": "something else"}, ""},
		{map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Done."}, "done"},
		{map[string]any{"hook_event_name": "SessionEnd"}, "ended"},
		{map[string]any{"hook_event_name": "SubagentStop"}, ""},
	}
	for _, c := range cases {
		if e := claudeEvent(c.in); e.Event != c.event {
			t.Errorf("%v -> %q, want %q", c.in["hook_event_name"], e.Event, c.event)
		}
	}
	p := claudeEvent(map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": map[string]any{"command": "rm x"}})
	if p.Title != "Allow Bash?" || p.Detail != "rm x" {
		t.Fatalf("permission: %+v", p)
	}
	q := claudeEvent(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_input": map[string]any{
		"questions": []any{map[string]any{"question": "Which?", "options": []any{map[string]any{"label": "A"}, map[string]any{"label": "B", "description": "bee"}}}}}})
	if q.Event != "question" || q.Detail != "Which?" || len(q.Options) != 2 || q.Options[1].Key != "2" || q.Options[1].Detail != "bee" {
		t.Fatalf("question: %+v", q)
	}
}

func TestLastAssistantText(t *testing.T) {
	f := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(f, []byte(`{"type":"user","message":{"content":"hi"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"All done."}]}}
{"type":"user","message":{"content":[{"type":"tool_result"}]}}
`), 0o600)
	if got := lastAssistantText(f); got != "All done." {
		t.Fatalf("got %q", got)
	}
	if lastAssistantText(filepath.Join(t.TempDir(), "none")) != "" {
		t.Fatal("missing file")
	}
}

func TestAncestors(t *testing.T) {
	a := ancestors()
	if len(a) < 2 || a[0] != os.Getpid() || a[1] != os.Getppid() {
		t.Fatalf("ancestors %v (pid %d ppid %d)", a, os.Getpid(), os.Getppid())
	}
}
