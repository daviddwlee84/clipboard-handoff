#!/usr/bin/env bash
# Cross-machine LAN hand-off test: THIS host (sender) -> a remote LAN host (receiver).
# Proves the real-world flow the localhost harness can't: two machines, and the
# remote may be HEADLESS (no display) — delivery is checked via `recv --emit-path`,
# comparing the PNG by hash. Deploys the impl to the remote first (Go: cross-compile
# + scp; Rust: rsync source + build there).
#
# Usage:  scripts/xmachine.sh <impl> [remote-ssh-host]
#   <impl>   ∈ mesh-rs | room-go | lan-go
#   remote   defaults to: local_ubuntu   (must be an ssh alias to a linux/amd64 host)
#
# Requires on the remote: sshd + (for mesh-rs only) a cargo toolchain at ~/.cargo/bin.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMPL="${1:-}"; REMOTE="${2:-local_ubuntu}"
ROOM="xm-$$"; REMDIR="cpc-xm"
IMG="$ROOT/testdata/small.png"
SRC=$(shasum -a 256 "$IMG" | awk '{print $1}')
LPIDS=(); RPIDS=()

say(){ echo "xmachine[$IMPL -> $REMOTE]: $*"; }
die(){ say "FAIL: $*" >&2; exit 1; }
rsh(){ ssh -o BatchMode=yes "$REMOTE" "$@"; }
rbin(){ rsh "cd $REMDIR && $*"; }                       # run a remote command from $REMDIR
cleanup(){
  for p in "${LPIDS[@]:-}"; do kill "$p" 2>/dev/null; done
  rsh "for p in ${RPIDS[*]:-}; do kill \$p 2>/dev/null; done; pkill -f 'room=$ROOM' 2>/dev/null; rm -rf /tmp/xm$ROOM* $REMDIR/state-$ROOM* 2>/dev/null" 2>/dev/null
  rm -rf /tmp/xm$ROOM* 2>/dev/null
}
trap cleanup EXIT

[ -n "$IMPL" ] || die "usage: scripts/xmachine.sh <impl> [remote]"
rsh true || die "cannot ssh to '$REMOTE'"

# ---- per-impl local binary + remote deploy --------------------------------
case "$IMPL" in
  mesh-rs)
    LBIN="$ROOT/mesh-rs/target/debug/clip"
    [ -x "$LBIN" ] || (cd "$ROOT/mesh-rs" && cargo build) || die "local build failed"
    say "deploying source + building on $REMOTE (rust)…"
    rsync -a --delete --exclude target/ "$ROOT/mesh-rs/" "$REMOTE:$REMDIR/mesh-rs/" >/dev/null || die "rsync failed"
    rsh "cd $REMDIR/mesh-rs && ~/.cargo/bin/cargo build >/tmp/xm$ROOM-build.log 2>&1" || die "remote cargo build failed (see /tmp/xm$ROOM-build.log on $REMOTE)"
    RBIN="mesh-rs/target/debug/clip" ;;
  room-go)
    LBIN="$ROOT/room-go/bin/room"; [ -x "$LBIN" ] || (cd "$ROOT/room-go" && go build -o bin/room ./cmd/room)
    say "cross-compiling + deploying (go, headless)…"
    (cd "$ROOT/room-go" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/xm$ROOM-room ./cmd/room) || die "cross-build failed"
    rsh "mkdir -p $REMDIR"; scp -q /tmp/xm$ROOM-room "$REMOTE:$REMDIR/room" && rsh "chmod +x $REMDIR/room"
    RBIN="./room" ;;
  lan-go)
    LBIN="$ROOT/experiments/lan-go/bin/lan"; [ -x "$LBIN" ] || (cd "$ROOT/experiments/lan-go" && go build -o bin/lan ./cmd/lan)
    say "cross-compiling + deploying (go, headless)…"
    (cd "$ROOT/experiments/lan-go" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/xm$ROOM-lan ./cmd/lan) || die "cross-build failed"
    rsh "mkdir -p $REMDIR"; scp -q /tmp/xm$ROOM-lan "$REMOTE:$REMDIR/lan" && rsh "chmod +x $REMDIR/lan"
    RBIN="./lan" ;;
  *) die "unknown impl '$IMPL' (want: mesh-rs | room-go | lan-go)" ;;
esac

LCFG=/tmp/xm$ROOM-L; LSOCK=$LCFG/d.sock; RCFG=/tmp/xm$ROOM-R; RSOCK=$RCFG/d.sock
L=("--config-dir" "$LCFG" "--socket" "$LSOCK" "--room" "$ROOM")
mkdir -p "$LCFG"

# ---- start daemons + establish membership ---------------------------------
start_remote_daemon(){ rsh "cd $REMDIR && mkdir -p $RCFG && setsid nohup $RBIN --config-dir $RCFG --socket $RSOCK --room $ROOM daemon --foreground >/tmp/xm$ROOM-R.log 2>&1 </dev/null & echo \$!"; }

case "$IMPL" in
  room-go)
    say "starting remote server + joining both clients…"
    RPIDS+=("$(rsh "cd $REMDIR && setsid nohup $RBIN server --addr :2299 --host-key /tmp/xm$ROOM-hk >/tmp/xm$ROOM-S.log 2>&1 </dev/null & echo \$!")")
    sleep 1.5
    RPIDS+=("$(start_remote_daemon)")
    rbin "$RBIN --config-dir $RCFG --socket $RSOCK --room $ROOM join room@localhost:2299" >/dev/null 2>&1
    "$LBIN" "${L[@]}" daemon --foreground >/tmp/xm$ROOM-L.log 2>&1 & LPIDS+=($!)
    sleep 0.5
    RIP=$(rsh "hostname -I | awk '{print \$1}'")
    "$LBIN" "${L[@]}" join "room@$RIP:2299" >/dev/null 2>&1
    sleep 2 ;;
  mesh-rs|lan-go)
    say "starting daemons (mDNS auto-discovery, same room)…"
    RPIDS+=("$(start_remote_daemon)")
    "$LBIN" "${L[@]}" daemon --foreground >/tmp/xm$ROOM-L.log 2>&1 & LPIDS+=($!)
    sleep 9
    # mesh-rs: ticket fallback if multicast didn't cross
    if [ "$IMPL" = mesh-rs ] && ! "$LBIN" --socket "$LSOCK" peers 2>/dev/null | grep -q .; then
      say "no auto-discovered peer; trying ticket fallback…"
      T=$("$LBIN" --socket "$LSOCK" pair --new --json 2>/dev/null | sed -n 's/.*"ticket":"\([^"]*\)".*/\1/p')
      [ -n "$T" ] && rbin "$RBIN --socket $RSOCK pair '$T'" >/dev/null 2>&1 && sleep 3
    fi ;;
esac

# ---- round-trip: local (sender) -> remote (receiver) ----------------------
say "TEXT…"
printf 'xm-%s' "$ROOM" | "$LBIN" --socket "$LSOCK" send --text
sleep 2
GOTTXT=$(rbin "timeout 8 $RBIN --socket $RSOCK recv" 2>/dev/null)
echo "$GOTTXT" | grep -q "xm-$ROOM" || die "TEXT not received (got: '$GOTTXT')"
say "text OK"

say "IMAGE…"
"$LBIN" --socket "$LSOCK" send --image < "$IMG"
sleep 2
GOTIMG=$(rbin "timeout 8 $RBIN --socket $RSOCK recv --latest-image --emit-path 2>/dev/null | tail -1")
DST=$(rsh "sha256sum '$GOTIMG' 2>/dev/null | awk '{print \$1}'")
[ -n "$DST" ] && [ "$DST" = "$SRC" ] || die "IMAGE hash mismatch (src=$SRC dst=$DST path=$GOTIMG)"
say "image OK (hash-equal)"

say "PASS ✅  (cross-machine hand-off $(hostname) -> $REMOTE)"
