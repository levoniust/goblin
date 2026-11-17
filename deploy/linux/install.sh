#!/usr/bin/env bash
# Install (or update) the Goblin host on Linux as a systemd user service.
#   deploy/linux/install.sh [path/to/goblin]     (default: build from host/ with Go)
# Chats live on Goblin's own tmux server (`tmux -L goblin-goblin attach -t goblin` to look from a terminal).
# Run `goblin init` once afterwards to choose your Goblin PIN, then `goblin pair` for your phone.
set -euo pipefail
here="$(cd "$(dirname "$0")/../.." && pwd)"
bin="${1:-}"
if [[ -z "$bin" ]]; then
  command -v go >/dev/null || { echo "Go not found: pass a goblin binary or install Go"; exit 1; }
  (cd "$here/host" && go build -ldflags "-s -w" -o "$here/host/goblin" ./cmd/goblin)
  bin="$here/host/goblin"
fi
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
install -m 755 "$bin" "$HOME/.local/bin/goblin.new"
mv "$HOME/.local/bin/goblin.new" "$HOME/.local/bin/goblin"
cat > "$HOME/.config/systemd/user/goblin.service" <<UNIT
[Unit]
Description=Goblin host (agents on many computers, one client)
After=network-online.target

[Service]
ExecStart=%h/.local/bin/goblin run
Restart=on-failure
RestartSec=3
# the chats' tmux server runs in its own scope, so restarting this service never ends them

[Install]
WantedBy=default.target
UNIT
# "Goblin" in the desktop's app menu: opens this computer's Goblin page (goblin ui)
mkdir -p "$HOME/.local/share/applications"
cat > "$HOME/.local/share/applications/goblin.desktop" <<DESK
[Desktop Entry]
Type=Application
Name=Goblin
Comment=This computer's Goblin page
Exec=$HOME/.local/bin/goblin ui
Terminal=false
Icon=utilities-terminal
Categories=Utility;Network;
DESK
systemctl --user daemon-reload
systemctl --user enable goblin.service >/dev/null
systemctl --user restart goblin.service
loginctl show-user "$USER" -p Linger 2>/dev/null | grep -q yes || echo "note: run 'loginctl enable-linger $USER' so Goblin runs without a login"
sleep 2
systemctl --user --no-pager status goblin.service | head -5
echo
echo "Next: 'goblin init' (choose your PIN), then 'goblin pair' and scan the QR with Switchboard."
echo "Settings: ~/.config/goblin/goblin.yaml ('goblin config check'). This computer's page: 'goblin ui'."
