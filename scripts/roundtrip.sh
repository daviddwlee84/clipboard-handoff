#!/usr/bin/env bash
# Bake-off round-trip harness: spawn two daemons of one implementation on the loopback/LAN,
# send text + a PNG from A, and assert B receives them intact (BLAKE3 hash-equal for the image).
#
# Usage:  scripts/roundtrip.sh <impl>
#   <impl> ∈ mesh-rs | room-go | lan-go | libp2p-mesh
#
# This drives ONLY the shared CLI surface from docs/SPEC.md, so it works against any impl that
# implements it. Per-impl specifics (binary path, build command, pairing) live in small hooks below.
#
# Exit 0 = round-trip verified. Non-zero = failure (message on stderr).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMPL="${1:-}"
RUN="$ROOT/scripts/.run/$IMPL"
ROOM="bakeoff-$$"
PIDS=()

die() { echo "roundtrip[$IMPL]: $*" >&2; exit 1; }
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

hashof() {  # BLAKE3 if available, else SHA-256 — same tool used on both sides so comparison is valid
  if command -v b3sum >/dev/null 2>&1; then b3sum "$1" | awk '{print $1}';
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# ---- per-impl resolution ---------------------------------------------------
case "$IMPL" in
  mesh-rs)      DIR="$ROOT/mesh-rs";            BIN="$DIR/target/debug/clip"; BUILD=(cargo build --manifest-path "$DIR/Cargo.toml") ;;
  room-go)      DIR="$ROOT/room-go";            BIN="$DIR/bin/room";          BUILD=(bash -c "cd '$DIR' && go build -o bin/room ./cmd/room") ;;
  lan-go)       DIR="$ROOT/experiments/lan-go"; BIN="$DIR/bin/lan";           BUILD=(bash -c "cd '$DIR' && go build -o bin/lan ./cmd/lan") ;;
  libp2p-mesh)  DIR="$ROOT/experiments/libp2p-mesh"; BIN="$DIR/bin/libp2p-mesh"; BUILD=(bash -c "cd '$DIR' && go build -o bin/libp2p-mesh ./cmd/libp2p-mesh") ;;
  *) die "unknown impl '$IMPL' (want: mesh-rs | room-go | lan-go | libp2p-mesh)" ;;
esac
[ -d "$DIR" ] || die "impl dir not found: $DIR (not built yet?)"

echo "roundtrip[$IMPL]: building…"
"${BUILD[@]}" || die "build failed"
[ -x "$BIN" ] || die "binary missing after build: $BIN"

# Two isolated 'devices' A and B, each with its own config dir + IPC socket.
rm -rf "$RUN"; mkdir -p "$RUN/a" "$RUN/b" "$RUN/out"
A=(--config-dir "$RUN/a" --socket "$RUN/a/d.sock" --room "$ROOM")
B=(--config-dir "$RUN/b" --socket "$RUN/b/d.sock" --room "$ROOM")

# ---- start daemons + establish membership (impl-specific hook) --------------
# The impl author wires start_pair() to the impl's actual daemon/pair/join commands.
start_pair() {
  # Default assumes: `daemon --foreground` + ticket-based `pair`. Override per impl as needed.
  "$BIN" "${A[@]}" daemon --foreground >"$RUN/a/daemon.log" 2>&1 & PIDS+=($!)
  "$BIN" "${B[@]}" daemon --foreground >"$RUN/b/daemon.log" 2>&1 & PIDS+=($!)
  sleep 1
  case "$IMPL" in
    room-go)
      # room model: start the SSH room server once, then join both daemons to it.
      "$BIN" server --addr :2299 --host-key "$RUN/host_key" >"$RUN/server.log" 2>&1 & PIDS+=($!)
      sleep 1
      "$BIN" "${A[@]}" join room@localhost:2299 >/dev/null 2>&1 || echo "roundtrip[$IMPL]: WARN join A failed" >&2
      "$BIN" "${B[@]}" join room@localhost:2299 >/dev/null 2>&1 || echo "roundtrip[$IMPL]: WARN join B failed" >&2 ;;
    lan-go|libp2p-mesh)
      # mesh probes auto-discover peers over mDNS within the same --room; no pairing step needed
      echo "roundtrip[$IMPL]: mDNS auto-discovery (no explicit pairing)" >&2 ;;
    *)
      local ticket
      ticket="$("$BIN" "${A[@]}" pair --new --json 2>/dev/null | sed -n 's/.*"ticket":"\([^"]*\)".*/\1/p')" || true
      [ -n "$ticket" ] && "$BIN" "${B[@]}" pair "$ticket" >/dev/null 2>&1 || \
        echo "roundtrip[$IMPL]: WARN pairing hook not wired yet" >&2 ;;
  esac
  sleep 1
}
start_pair

# For the automated test we want the image on B's buffer deterministically.
"$BIN" "${B[@]}" config set auto_copy off >/dev/null 2>&1 || true

# ---- 1) TEXT round-trip ----------------------------------------------------
echo "roundtrip[$IMPL]: text…"
"$BIN" "${B[@]}" recv --follow >"$RUN/out/text.out" 2>/dev/null & PIDS+=($!)
sleep 0.3
printf 'hi-%s' "$ROOM" | "$BIN" "${A[@]}" send --text
sleep 0.8
grep -q "hi-$ROOM" "$RUN/out/text.out" || die "TEXT round-trip failed (see $RUN/out/text.out)"
echo "roundtrip[$IMPL]: text OK"

# ---- 2) IMAGE round-trip (hash-equal) --------------------------------------
echo "roundtrip[$IMPL]: image…"
SRC="$ROOT/testdata/small.png"
"$BIN" "${A[@]}" send --image <"$SRC"
sleep 0.8
GOT="$("$BIN" "${B[@]}" recv --latest-image --emit-path 2>/dev/null | tail -n1)"
[ -n "$GOT" ] && [ -f "$GOT" ] || die "IMAGE recv produced no file path"
[ "$(hashof "$SRC")" = "$(hashof "$GOT")" ] || die "IMAGE hash mismatch: $SRC vs $GOT"
echo "roundtrip[$IMPL]: image OK (hash-equal)"

echo "roundtrip[$IMPL]: PASS ✅"
