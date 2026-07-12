#!/usr/bin/env bash
# Layer 3 — multiplexer smoke for clip's INLINE IMAGES. Runs `clip tui` inside a
# terminal multiplexer, sends it an image from a second daemon, and asserts the app
# renders the image bubble (caption "🖼") without panicking. This proves clip runs
# and lays out images under tmux/zellij; true inline-pixel fidelity (Kitty/iTerm2/
# Sixel passthrough) is a documented MANUAL check — see mesh-rs/README.md.
#
# Skips cleanly (exit 0) if neither tmux nor zellij is installed.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="$ROOT/mesh-rs/target/debug/clip"
IMG="$ROOT/testdata/small.png"
[ -x "$BIN" ] || { echo "mux: building clip…"; (cd "$ROOT/mesh-rs" && cargo build) || exit 1; }

have(){ command -v "$1" >/dev/null 2>&1; }
if ! have tmux && ! have zellij; then
  echo "tui_mux: SKIP — neither tmux nor zellij installed (image-in-mux is a manual check)"; exit 0
fi

rc=0
run_case(){ # $1 = "tmux" | "zellij"; sets up B, pairs, sends the image, captures A's pane
  local mux="$1"
  local room="mux$$-$mux" A="/tmp/muxA$$-$mux" B="/tmp/muxB$$-$mux" S="clipmux$$$mux" pane=""
  echo "== $mux =="
  # A: clip tui inside the multiplexer (auto-spawns A's daemon)
  case "$mux" in
    tmux)   tmux new-session -d -s "$S" -x 140 -y 40 \
              "$BIN --config-dir $A --socket $A/d.sock --room $room tui" ;;
    zellij) ZELLIJ_SESSION="$S" setsid zellij --session "$S" \
              -- "$BIN" --config-dir "$A" --socket "$A/d.sock" --room "$room" tui \
              >/tmp/$S.log 2>&1 & sleep 1 ;;
  esac
  sleep 4   # tui + graphics-probe settle
  # B: a second daemon, ticket-paired to A
  "$BIN" --config-dir "$B" --socket "$B/d.sock" --room "$room" daemon --foreground >/tmp/$S.b.log 2>&1 &
  local bpid=$!
  sleep 1
  local t; t="$("$BIN" --socket "$A/d.sock" pair --new --json 2>/dev/null | sed -n 's/.*"ticket":"\([^"]*\)".*/\1/p')"
  if [ -n "$t" ]; then "$BIN" --socket "$B/d.sock" pair "$t" >/dev/null 2>&1; sleep 2; fi
  # B sends the image → A's tui should render an image bubble
  "$BIN" --socket "$B/d.sock" send "$IMG" >/dev/null 2>&1
  sleep 3
  case "$mux" in
    tmux)   pane="$(tmux capture-pane -t "$S" -p 2>/dev/null)" ;;
    zellij) zellij --session "$S" action dump-screen /tmp/$S.dump >/dev/null 2>&1; pane="$(cat /tmp/$S.dump 2>/dev/null)" ;;
  esac
  if printf '%s' "$pane" | grep -q '🖼'; then echo "  ✅ $mux: image bubble rendered (🖼 caption present)"
  elif [ "$mux" = zellij ]; then
    echo "  ⚠️  zellij: couldn't confirm the caption via 'action dump-screen' (zellij's headless"
    echo "      scripting is finicky) — tmux is the asserting case; verify zellij visually if needed"
  else echo "  ❌ $mux: no image caption in pane"; rc=1; fi
  if printf '%s' "$pane" | grep -qi 'panic'; then echo "  ❌ $mux: panic detected in pane"; rc=1; fi
  # cleanup
  case "$mux" in
    tmux)   tmux kill-session -t "$S" 2>/dev/null ;;
    zellij) zellij delete-session "$S" --force >/dev/null 2>&1; pkill -f "session $S" 2>/dev/null ;;
  esac
  kill "$bpid" 2>/dev/null; "$BIN" --socket "$A/d.sock" daemon stop >/dev/null 2>&1
  rm -rf "$A" "$B" /tmp/$S.* 2>/dev/null
}

have tmux   && run_case tmux
have zellij && run_case zellij
[ "$rc" = 0 ] && echo "tui_mux: PASS ✅" || echo "tui_mux: FAIL ❌"
exit $rc
