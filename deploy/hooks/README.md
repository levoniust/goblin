# Harness hooks

They tell Goblin what a chat is doing (working, needs permission, has a question, done), which drives the status in
Switchboard and its alerts. All of them call `goblin hook`, do nothing outside a Goblin chat, never print, and never
make the harness wait.

| Harness | File | Where it goes |
|---|---|---|
| Claude Code | `claude/settings-hooks.json` | merge its `hooks` into `~/.claude/settings.json` (Windows: `%USERPROFILE%\.claude\settings.json`, and use `C:/AI/goblin/goblin.exe hook claude` with forward slashes: Claude Code runs hooks through Git Bash) |
| Pi | `pi/goblin.ts` | `~/.pi/agent/extensions/` (or the folder in `PI_CODING_AGENT_DIR`) |
| Kilo Code | `kilo/goblin.ts` | `~/.config/kilo/plugin/` (Windows: `%USERPROFILE%\.config\kilo\plugin\`) |
| anything else | — | call `goblin hook <harness> <working\|permission\|question\|done\|ended> [text]` |
