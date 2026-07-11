#!/usr/bin/env bash
# End-to-end test for the advanced hand-off features (SPEC §3/§8): additive
# sinks (save_dir + text_file), arbitrary-file send, and `clear --all`.
#
# Flow: one SSH room server + two daemons (A sender, B receiver) joined to it.
# On B set save_dir + text_file, seed pre-session content, then send a text, an
# image, and a small arbitrary binary from A. Assert the folder + append-file
# contents and hash-equality, then `clear --all --yes` on B and assert the
# session sink writes reverted while pre-session content is intact. Finally
# exercise `daemon stop`.
#
# Exit 0 = verified. Non-zero = failure (message on stderr).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"   # repo root
DIR="$ROOT/room-go"
BIN="$DIR/bin/room"
RUN="$DIR/.run/e2e_sinks.$$"
ROOM="sinks-$$"
PORT=2311
PIDS=()

die() { echo "e2e_sinks: FAIL: $*" >&2; exit 1; }
say() { echo "e2e_sinks: $*"; }
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$RUN"; }
trap cleanup EXIT

hashof() {
  if command -v b3sum >/dev/null 2>&1; then b3sum "$1" | awk '{print $1}';
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

say "building…"
(cd "$DIR" && go build -o bin/room ./cmd/room) || die "build failed"

rm -rf "$RUN"; mkdir -p "$RUN/a" "$RUN/b" "$RUN/save" "$RUN/src"
A=(--config-dir "$RUN/a" --socket "$RUN/a/d.sock" --room "$ROOM")
B=(--config-dir "$RUN/b" --socket "$RUN/b/d.sock" --room "$ROOM")
SAVE="$RUN/save"
TXT="$RUN/log.txt"

# ---- start server + two daemons, join both ---------------------------------
"$BIN" server --addr ":$PORT" --host-key "$RUN/host_key" >"$RUN/server.log" 2>&1 & PIDS+=($!)
sleep 1
"$BIN" "${A[@]}" daemon --foreground >"$RUN/a/daemon.log" 2>&1 & PIDS+=($!)
"$BIN" "${B[@]}" daemon --foreground >"$RUN/b/daemon.log" 2>&1 & PIDS+=($!)
sleep 1
"$BIN" "${A[@]}" join "room@localhost:$PORT" >/dev/null 2>&1 || die "join A failed"
"$BIN" "${B[@]}" join "room@localhost:$PORT" >/dev/null 2>&1 || die "join B failed"
sleep 1

# ---- configure sinks on B (auto_copy off keeps the test off the clipboard) --
"$BIN" "${B[@]}" config set auto_copy off >/dev/null
"$BIN" "${B[@]}" config set save_dir "$SAVE" >/dev/null

# Pre-session content: a line already in text_file, a file already in save_dir.
# Both must survive `clear --all`. text_file offset is captured when we set it.
printf 'PRE-EXISTING\n' > "$TXT"
printf 'keep me\n' > "$SAVE/keep.dat"
PRE_TXT="$(cat "$TXT")"
"$BIN" "${B[@]}" config set text_file "$TXT" >/dev/null

# ---- send text + image + arbitrary binary from A ---------------------------
say "sending text, image, file…"
printf 'hello sinks' | "$BIN" "${A[@]}" send --text
IMG="$ROOT/testdata/small.png"
"$BIN" "${A[@]}" send --image --name shot.png < "$IMG"
# A small arbitrary binary (non-UTF-8) file.
head -c 512 /dev/urandom > "$RUN/src/payload.bin"
"$BIN" "${A[@]}" send --file --name payload.bin < "$RUN/src/payload.bin"
sleep 1.2

# ---- assert sink writes ----------------------------------------------------
say "checking append-file…"
grep -q '^---$' "$TXT" || die "text_file missing the --- header (got: $(cat "$TXT"))"
grep -q 'hello sinks' "$TXT" || die "text_file missing the appended text"
grep -q 'PRE-EXISTING' "$TXT" || die "pre-session text_file content vanished"

say "checking save_dir image (hash-equal)…"
[ -f "$SAVE/shot.png" ] || die "image not written to save_dir"
[ "$(hashof "$IMG")" = "$(hashof "$SAVE/shot.png")" ] || die "image hash mismatch"

say "checking save_dir file (hash-equal)…"
[ -f "$SAVE/payload.bin" ] || die "file not written to save_dir"
[ "$(hashof "$RUN/src/payload.bin")" = "$(hashof "$SAVE/payload.bin")" ] || die "file hash mismatch"
say "sink writes OK"

# ---- clear --all --yes on B, assert session writes reverted ----------------
say "clear --all --yes…"
"$BIN" "${B[@]}" clear --all --yes >/dev/null || die "clear --all failed"

[ "$(cat "$TXT")" = "$PRE_TXT" ] || die "text_file not reverted to session start (got: $(cat "$TXT"))"
[ ! -f "$SAVE/shot.png" ]    || die "session image not removed by clear --all"
[ ! -f "$SAVE/payload.bin" ] || die "session file not removed by clear --all"
[ -f "$SAVE/keep.dat" ]      || die "pre-session save_dir file wrongly deleted"
[ "$(cat "$SAVE/keep.dat")" = "keep me" ] || die "pre-session file content changed"
# Transient store (blob cache) emptied too.
if [ -d "$RUN/b/blobs" ] && [ -n "$(ls -A "$RUN/b/blobs" 2>/dev/null)" ]; then
  die "transient blob cache not cleared: $(ls -A "$RUN/b/blobs")"
fi
say "clear --all reverted session sinks; pre-session content intact"

# ---- daemon stop -----------------------------------------------------------
# stdin from /dev/null: no TTY, so clear_on_exit=ask resolves to transient
# non-interactively (SPEC §8) instead of prompting.
say "daemon stop…"
"$BIN" "${B[@]}" daemon stop </dev/null >/dev/null || die "daemon stop failed"
sleep 0.5
[ ! -S "$RUN/b/d.sock" ] || die "daemon socket still present after stop"

say "PASS ✅"
