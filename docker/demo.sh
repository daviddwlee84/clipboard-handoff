#!/usr/bin/env bash
# Drive a real hand-off ACROSS containers (each container is a "device"), proving
# the cross-machine flow on a single host. Assumes `just up` (or docker compose up)
# is running. Verifies text, image (by hash), and an arbitrary file via the folder sink.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DC="docker compose -f $ROOT/docker/compose.yml"
F="--socket /data/d.sock --room demo"
hostsha(){ shasum -a 256 "$1" | awk '{print $1}'; }
pass=0; fail=0
ok(){ echo "  ✅ $*"; pass=$((pass+1)); }
no(){ echo "  ❌ $*"; fail=$((fail+1)); }

echo "== waiting for room daemons to connect to the server =="
for i in $(seq 1 15); do
  c=$($DC exec -T alice room $F status --json 2>/dev/null | grep -o '"connected":true' || true)
  [ -n "$c" ] && break; sleep 1
done

echo "== room-go: alice -> bob (through the central server) =="
$DC exec -T alice sh -c "printf 'hi-docker' | room $F send --text" >/dev/null 2>&1
sleep 2
got=$($DC exec -T bob room $F recv 2>/dev/null | tr -d '\r\n')
[ "$got" = "hi-docker" ] && ok "text delivered ('$got')" || no "text: got '$got'"

$DC exec -T alice room $F send /testdata/small.png >/dev/null 2>&1
sleep 2
p=$($DC exec -T bob room $F recv --latest-image --emit-path 2>/dev/null | tail -1 | tr -d '\r')
bsha=$($DC exec -T bob sha256sum "$p" 2>/dev/null | awk '{print $1}')
[ -n "$bsha" ] && [ "$bsha" = "$(hostsha "$ROOT/testdata/small.png")" ] && ok "image hash-equal" || no "image (got '$bsha' path '$p')"

echo "== room-go: arbitrary FILE via the folder sink on bob =="
$DC exec -T bob room $F config set save_dir /data/save >/dev/null 2>&1
asha=$($DC exec -T alice sh -c 'head -c 400 /dev/urandom > /tmp/report.bin; sha256sum /tmp/report.bin' 2>/dev/null | awk '{print $1}')
$DC exec -T alice room $F send /tmp/report.bin >/dev/null 2>&1
sleep 2
bfsha=$($DC exec -T bob sh -c 'sha256sum /data/save/report.bin 2>/dev/null' | awk '{print $1}')
[ -n "$bfsha" ] && [ "$bfsha" = "$asha" ] && ok "file landed in bob:/data/save (hash-equal)" || no "file sink (a=$asha b=$bfsha)"

echo "== lan-go: lan-a -> lan-b (mDNS across the docker network) =="
$DC exec -T lan-a sh -c "printf 'lan-docker' | lan $F send --text" >/dev/null 2>&1
sleep 2
lgot=$($DC exec -T lan-b lan $F recv 2>/dev/null | tr -d '\r\n')
if [ "$lgot" = "lan-docker" ]; then ok "lan mDNS text delivered ('$lgot')"
else echo "  ⚠️  lan mDNS did not cross the docker bridge (got '$lgot') — multicast is often filtered on bridge networks; room-go is the reliable sandbox path"; fi

echo "== summary: pass=$pass fail=$fail =="
[ "$fail" -eq 0 ]
