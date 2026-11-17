#!/usr/bin/env bash
# Build the Windows programs (from Linux): goblin.exe (commands, console) and goblinw.exe (the same program without a
# console window, for the Scheduled Tasks and the shortcut).   deploy/windows/build.sh amd64|arm64 OUTDIR
set -euo pipefail
arch=${1:?amd64 or arm64}; out=${2:?output folder}
cd "$(dirname "$0")/../../host"
mkdir -p "$out"
GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$out/goblin.exe" ./cmd/goblin
GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w -H=windowsgui" -o "$out/goblinw.exe" ./cmd/goblin
