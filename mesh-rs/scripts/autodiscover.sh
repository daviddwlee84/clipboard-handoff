#!/usr/bin/env bash
# mesh-rs LAN auto-discovery check (NO ticket).
#
# Starts two `clip` daemons in the SAME --room on the same host, with NO `pair`/ticket step,
# and asserts they auto-discover each other over mDNS and round-trip text + a PNG (hash-equal).
# This is the cross-machine zero-config path exercised locally. It complements (does not replace)
# the shared ticket-based harness `scripts/roundtrip.sh mesh-rs`.
#
# Exit 0 = auto-discovery round-trip verified. Non-zero = failure (message on stderr).
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"          # mesh-rs/
ROOT="$(cd "$DIR/.." && pwd)"                                    # repo root (for testdata)
BIN="$DIR/target/debug/clip"
SRC="$ROOT/testdata/small.png"
RUN="$DIR/target/.autodiscover"
ROOM="autodisc-$$"                                               # unique room => unique mDNS scope
PIDS=()

die() { echo "autodiscover: $*" >&2; exit 1; }
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

hashof() {
  if command -v b3sum >/dev/null 2>&1; then b3sum "$1" | awk '{print $1}';
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

echo "autodiscover: building…"
cargo build --manifest-path "$DIR/Cargo.toml" >/dev/null 2>&1 || die "build failed"
[ -x "$BIN" ] || die "binary missing: $BIN"
[ -f "$SRC" ] || die "testdata missing: $SRC"

rm -rf "$RUN"; mkdir -p "$RUN/a" "$RUN/b"
A=(--config-dir "$RUN/a" --socket "$RUN/a/d.sock" --room "$ROOM")
B=(--config-dir "$RUN/b" --socket "$RUN/b/d.sock" --room "$ROOM")

# 1) Start both daemons. NO pairing, NO ticket — mDNS must connect them.
"$BIN" "${A[@]}" daemon --foreground >"$RUN/a.log" 2>&1 & PIDS+=($!)
"$BIN" "${B[@]}" daemon --foreground >"$RUN/b.log" 2>&1 & PIDS+=($!)

# 2) Wait (up to ~20s) for auto-discovery to establish a peer on A.
echo "autodiscover: waiting for mDNS auto-connect (no ticket)…"
connected=0
for _ in $(seq 1 40); do
  sleep 0.5
  n="$("$BIN" "${A[@]}" status --json 2>/dev/null | sed -n 's/.*"peer_count":\([0-9]*\).*/\1/p')" || true
  if [ -n "${n:-}" ] && [ "$n" -ge 1 ]; then connected=1; break; fi
done
[ "$connected" = 1 ] || die "peers did NOT auto-connect within timeout (see $RUN/a.log, $RUN/b.log)"
echo "autodiscover: peers auto-connected ✅ (no ticket)"

# Keep B's clipboard out of the loop for a deterministic buffer check.
"$BIN" "${B[@]}" config set auto_copy off >/dev/null 2>&1 || true

# 3) TEXT round-trip
echo "autodiscover: text…"
"$BIN" "${B[@]}" recv --follow >"$RUN/text.out" 2>/dev/null & PIDS+=($!)
sleep 0.3
printf 'hi-%s' "$ROOM" | "$BIN" "${A[@]}" send --text
sleep 1.0
grep -q "hi-$ROOM" "$RUN/text.out" || die "TEXT round-trip failed (see $RUN/text.out)"
echo "autodiscover: text OK"

# 4) IMAGE round-trip (hash-equal)
echo "autodiscover: image…"
"$BIN" "${A[@]}" send --image <"$SRC"
sleep 1.0
GOT="$("$BIN" "${B[@]}" recv --latest-image --emit-path 2>/dev/null | tail -n1)"
[ -n "$GOT" ] && [ -f "$GOT" ] || die "IMAGE recv produced no file path"
[ "$(hashof "$SRC")" = "$(hashof "$GOT")" ] || die "IMAGE hash mismatch: $SRC vs $GOT"
echo "autodiscover: image OK (hash-equal)"

echo "autodiscover: PASS ✅ (same-room daemons connected with no ticket)"
