#!/usr/bin/env bash
# remote.sh — the shared engine behind `<tool> remote <host>`: bootstrap a tool on
# an SSH host (key auth via ~/.ssh/config), start its remote side, and connect the
# local daemon to it. One place, three tools:
#
#   room : start a room server on the remote (loopback) + SSH -L tunnel + join
#   clip : start a remote clip daemon + fetch its iroh ticket + pair (direct QUIC)
#   lan  : start a remote lan daemon (mDNS auto-connects on a shared LAN)
#
#   scripts/remote.sh <tool> <host> [up|down|status] [--room R] [--rport N] [--lport N]
#     tool  = clip | room | lan          host = ssh alias / user@host
#
# State (pids, control socket, tunnel port) lives under ~/.cache/cpc/remote/<tool>@<host>/.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HELPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# install.sh lives next to us (installed) or in the repo's scripts/ dir.
if   [ -x "$HELPER_DIR/install.sh" ];   then INSTALL_SH="$HELPER_DIR/install.sh"
elif [ -x "$ROOT/scripts/install.sh" ]; then INSTALL_SH="$ROOT/scripts/install.sh"
else INSTALL_SH=""; fi
TOOL="${1:-}"; HOST="${2:-}"
ROOM="default"; RPORT="2299"; LPORT=""; ACTION="up"
shift 2 2>/dev/null || true
case "${1:-}" in up|down|status) ACTION="$1"; shift;; esac   # optional action word
while [ $# -gt 0 ]; do case "$1" in
  --room) ROOM="$2"; shift 2;; --rport) RPORT="$2"; shift 2;; --lport) LPORT="$2"; shift 2;;
  --stop) ACTION="down"; shift;;
  up|down|status) ACTION="$1"; shift;;
  *) shift;; esac; done
: "${LPORT:=$RPORT}"

case "$TOOL" in clip|room|lan) ;; *) echo "usage: remote.sh <clip|room|lan> <host> [up|down|status] [--room R]" >&2; exit 2;; esac
[ -n "$HOST" ] || { echo "remote.sh: missing host" >&2; exit 2; }
BIN="$TOOL"
STATE="${XDG_CACHE_HOME:-$HOME/.cache}/cpc/remote/${TOOL}@${HOST}"; mkdir -p "$STATE"
CTL="$STATE/ssh-ctl.sock"
say(){ printf '\033[1;36mremote[%s→%s]:\033[0m %s\n' "$TOOL" "$HOST" "$*"; }
die(){ printf '\033[1;31mremote[%s→%s] error:\033[0m %s\n' "$TOOL" "$HOST" "$*" >&2; exit 1; }

# Run a script block on the remote via `bash -s`, passing args safely (no quoting hell).
rrun(){ ssh -o BatchMode=yes "$HOST" bash -s -- "$@"; }

ensure_remote_bin(){
  if rrun "$BIN" <<'EOF' 2>/dev/null
test -x "$HOME/.local/bin/$1"
EOF
  then say "remote binary present (~/.local/bin/$BIN)"
  else
    say "installing $BIN on $HOST…"
    [ -n "$INSTALL_SH" ] || die "remote binary missing and no install.sh found — run from the repo, or pre-install with scripts/install.sh $BIN --remote $HOST"
    "$INSTALL_SH" "$BIN" --remote "$HOST" >&2 || die "remote install failed"
  fi
}

start_remote_daemon(){ # clip/lan: a client daemon in ROOM. echoes the remote pid.
  rrun "$BIN" "$ROOM" <<'EOF'
bin="$1"; room="$2"; cfg="$HOME/.config/cpc/$bin"; mkdir -p "$cfg" "$HOME/.cache"
if [ -S "$cfg/d.sock" ] && "$HOME/.local/bin/$bin" --socket "$cfg/d.sock" status >/dev/null 2>&1; then
  echo reused; exit 0
fi
setsid nohup "$HOME/.local/bin/$bin" --config-dir "$cfg" --socket "$cfg/d.sock" --room "$room" \
  daemon --foreground >"$HOME/.cache/cpc-$bin.log" 2>&1 </dev/null &
echo "$!"
EOF
}

remote_ticket(){ # clip: print the remote daemon's pairing ticket
  rrun "$BIN" "$ROOM" <<'EOF' | sed -n 's/.*"ticket":"\([^"]*\)".*/\1/p'
bin="$1"; room="$2"; cfg="$HOME/.config/cpc/$bin"
"$HOME/.local/bin/$bin" --config-dir "$cfg" --socket "$cfg/d.sock" --room "$room" pair --new --json 2>/dev/null
EOF
}

local_bin(){ if [ -x "$ROOT/mesh-rs/target/debug/clip" ] && [ "$BIN" = clip ]; then echo "$ROOT/mesh-rs/target/debug/clip"
             elif command -v "$BIN" >/dev/null 2>&1; then echo "$BIN"
             elif [ "$BIN" = room ] && [ -x "$ROOT/room-go/bin/room" ]; then echo "$ROOT/room-go/bin/room"
             elif [ "$BIN" = lan ] && [ -x "$ROOT/experiments/lan-go/bin/lan" ]; then echo "$ROOT/experiments/lan-go/bin/lan"
             else echo "$BIN"; fi; }

up(){
  ensure_remote_bin
  local LB; LB="$(local_bin)"
  case "$TOOL" in
    lan)
      say "starting remote lan daemon (room '$ROOM')..."
      local rp; rp="$(start_remote_daemon)"; echo "$rp" >"$STATE/remote.pid"
      "$LB" --room "$ROOM" status >/dev/null 2>&1 || true      # auto-spawn local daemon in ROOM
      local n=0 i=0
      while [ "$i" -lt 12 ]; do n="$("$LB" --room "$ROOM" peers 2>/dev/null | grep -c . || true)"; [ "${n:-0}" -ge 1 ] && break; sleep 1; i=$((i+1)); done
      sleep 1
      say "connected (${n:-0} peer) - remote & local daemons share room '$ROOM' over mDNS (LAN). Use: $BIN --room $ROOM send ...";;
    clip)
      say "starting remote clip daemon (room '$ROOM')…"
      local rp; rp="$(start_remote_daemon)"; echo "$rp" >"$STATE/remote.pid"; sleep 1
      say "fetching remote ticket…"; local t; t="$(remote_ticket)"
      [ -n "$t" ] || die "could not obtain remote ticket"
      "$LB" --room "$ROOM" pair "$t" >/dev/null 2>&1 || die "local pair failed"
      say "connected — paired to remote clip over iroh (direct QUIC). Use: $BIN --room $ROOM send …";;
    room)
      say "delegating to native 'room remote' (SSH tunnel)…"
      exec "$(local_bin)" remote "$HOST" --room "$ROOM" --rport "$RPORT" --lport "$LPORT";;
  esac
  say "disconnect with: scripts/remote.sh $TOOL $HOST down"
}

down(){
  case "$TOOL" in
    room) exec "$(local_bin)" remote "$HOST" --stop;;
    clip|lan) if [ -f "$STATE/remote.pid" ]; then rrun "$(cat "$STATE/remote.pid")" <<'EOF' >/dev/null 2>&1 || true
kill "$1" 2>/dev/null || true
EOF
          fi; say "remote daemon stopped";;
  esac
  rm -rf "$STATE"; say "disconnected"
}

case "$ACTION" in
  up) up;;
  down) down;;
  status) [ -d "$STATE" ] && ls -1 "$STATE" 2>/dev/null && say "state at $STATE" || say "not connected";;
  *) die "unknown action: $ACTION";;
esac
