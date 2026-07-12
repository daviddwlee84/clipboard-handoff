#!/usr/bin/env bash
# Cross-compile the static Go binaries the sandbox image packages (room, lan).
# Static (CGO_ENABLED=0) so they run on any linux/amd64 base with no libc fuss.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAGE="$ROOT/docker/stage"; mkdir -p "$STAGE"
echo "docker/build: room, lan → docker/stage (linux/amd64, static)"
( cd "$ROOT/room-go"          && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$STAGE/room" ./cmd/room )
( cd "$ROOT/experiments/lan-go" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$STAGE/lan"  ./cmd/lan )
echo "docker/build: done ($(du -h "$STAGE/room" | awk '{print $1}') room, $(du -h "$STAGE/lan" | awk '{print $1}') lan)"
