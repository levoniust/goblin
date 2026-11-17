# Goblin

**In development, looking for supporters and testers.**

Goblin runs your AI agents (Claude Code, Pi, Kilo Code, a plain shell, …) on every computer you own and joins those
computers into one network that grows and shrinks on its own, keeps working offline, and picks the best computer to
coordinate it ("the hub"). "Quick on the pickup, graceful on the fall."

Goblin is the back end. Its app is **[Switchboard](https://github.com/levoniust/switchboard-next)** (2.0 and later), which
talks to Goblin only through the documented client API, so either can be upgraded on its own.

## What it does
- **Chats that survive anything.** Every chat runs on its computer (tmux on Linux, Goblin's own ConPTY keeper on
  Windows), so a dropped phone, a sleeping laptop or a restarted Goblin never ends it. After a reboot, Goblin offers to
  resume the chats that were open.
- **One network, no single point of failure.** Computers find each other on the LAN and over Tailscale. Any computer
  can be the hub; when the hub goes away another takes over. Every computer is also a switch: when the app (or another
  computer) can't reach a computer directly, its requests are passed on through the ones that can, up to three steps.
- **Status and alerts.** Harnesses report what a chat is doing through `goblin hook` (Claude Code hooks, a Pi
  extension and a Kilo plugin are in `deploy/hooks`): each chat is *working*, *needs permission*, *has a question* or
  *done*, and the app gets an alert the moment that changes.
- **Out of your way.** Chats and the programs they start run at lower priority (`chat_priority`), so the person at the
  computer comes first. On Windows the background program (`goblinw.exe`) has no console window, and
  `goblin hidden -- PROGRAM` starts model servers without one too.
- **Files both ways.** Pictures from the phone land in the computer's `uploads` folder and their path goes into the chat;
  files a chat mentions can be opened read-only from the app.
- **Security.** Every request is signed by a device key and pinned to the computer's certificate. New computers and
  devices join only by approval; pairing and approving need the Goblin PIN.

## On each computer
- `goblin` is one small program (Go). Linux: a systemd user service. Windows: two Scheduled Tasks (the keeper holds the
  chats) running the windowless `goblinw.exe`; `goblin.exe` is for typing commands.
- **Settings:** `goblin.yaml` in the data folder (`~/.config/goblin`, `%APPDATA%\goblin`). Every setting has a comment, so a
  person or an AI can edit it. `goblin config check` lists mistakes with line numbers; most changes apply when you save.
- **This computer's page:** `goblin ui` (or the "Goblin" shortcut) opens a local page on `127.0.0.1:8202`: the network and
  hub, waiting approvals, pairing a phone, the chats on this computer, the settings editor and the log.
- **Log:** `goblin.log` in the data folder.

## Commands
```
goblin init [-name N]       set up this computer and choose the Goblin PIN
goblin pair                 pairing link + QR for Switchboard (needs the PIN)
goblin join HOST:8200       ask another computer's network to let this one in
goblin approve ID           approve a waiting computer or device (needs the PIN)
goblin remove ID            remove a computer or device for good (needs the PIN)
goblin status               network, hub and waiting requests
goblin test                 ask for a new hub test
goblin ui                   this computer's page
goblin config check         check goblin.yaml
goblin hook ...             for harness hooks: report a chat's state (see deploy/hooks)
goblin hidden [--low] -- P  run a program with no window (Windows tasks)
```
`goblin-agent` (host/cmd/goblin-agent) lets an AI agent use the network like the app: list chats on any computer, start
one, type into it and read its screen. It is the first step towards agents handing work to each other.

## Install
- Linux: `deploy/linux/install.sh` (builds with Go, or pass a binary).
- Windows: `deploy/windows/build.sh amd64|arm64 OUT` builds `goblin.exe` and `goblinw.exe`; put both next to
  `deploy/windows/install.ps1` and run it in PowerShell.
- Harness hooks: `deploy/hooks/` (Claude Code settings, `pi/goblin.ts`, `kilo/goblin.ts`); each file says where it goes.

## For developers
- Client API contract: [docs/PROTOCOL.md](docs/PROTOCOL.md) (API 1, feature names, additive-only rule).
- `cd host && go vet ./... && go test ./...` (includes a 3-computer integration test with a broken link and relays).
- `goblin-check` (host/cmd/goblin-check) checks a live network end to end; `-dump-info DIR` records `/api/info` answers
  for Switchboard's contract tests.

MIT license.
