# mesh-rs — `clip`

**Phase 0** of the P2P clipboard hand-off bake-off: a Rust contender built on
**[iroh](https://docs.rs/iroh)** (QUIC, direct LAN connections). Two devices in the same room
pair with a ticket, then text and images copied/piped on A become receivable/pasteable on B —
the image arriving **BLAKE3 hash-equal** to the source.

Binary name: **`clip`**. Implements the shared contract in
[`../docs/SPEC.md`](../docs/SPEC.md) + [`../docs/PROTOCOL.md`](../docs/PROTOCOL.md).

## Build

```sh
cargo build --manifest-path mesh-rs/Cargo.toml
# binary -> mesh-rs/target/debug/clip
```

Toolchain: rustc/cargo 1.96 (edition 2024). First build pulls the full iroh tree (~390 crates).

## Process model

A resident **daemon** (`clip daemon`) owns the iroh `Endpoint`, the persisted Ed25519 identity
(`<config-dir>/secret.key`), the arboard OS clipboard, an in-memory ring buffer of received items,
and a content-addressed PNG store. Thin client subcommands (`send`/`recv`/`paste`/`pair`/…) talk to
it over a **local Unix-domain socket** (length-prefixed CBOR, PROTOCOL §4) and **auto-spawn** it if
it isn't running.

Global flags (accepted before or after the subcommand; the harness passes them **before**):
`--config-dir PATH`  `--socket PATH`  `--room NAME` (default `default`)  `--json`  `-q/--quiet`  `-v/--verbose`.

## Exact commands the round-trip harness uses

`scripts/roundtrip.sh mesh-rs` drives these (do not edit the harness — it already matches this):

```sh
BIN=mesh-rs/target/debug/clip
A=(--config-dir "$RUN/a" --socket "$RUN/a/d.sock" --room "$ROOM")
B=(--config-dir "$RUN/b" --socket "$RUN/b/d.sock" --room "$ROOM")

# 1. Start a daemon per device (foreground; harness backgrounds + kills them).
"$BIN" "${A[@]}" daemon --foreground &
"$BIN" "${B[@]}" daemon --foreground &

# 2. Pair: A prints a ticket as JSON, B joins with it.
ticket="$("$BIN" "${A[@]}" pair --new --json | sed -n 's/.*"ticket":"\([^"]*\)".*/\1/p')"
"$BIN" "${B[@]}" pair "$ticket"

# 3. (test determinism) keep the clipboard untouched on B
"$BIN" "${B[@]}" config set auto_copy off

# 4. TEXT: A sends, B streams it to stdout
"$BIN" "${B[@]}" recv --follow &
printf 'hi' | "$BIN" "${A[@]}" send --text

# 5. IMAGE: A sends a PNG, B writes it to a temp file and prints the path (last line)
"$BIN" "${A[@]}" send --image < testdata/small.png
"$BIN" "${B[@]}" recv --latest-image --emit-path   # -> /tmp/clip-<blake3>.png
```

`pair --new --json` prints exactly `{"ticket":"<base32>"}` on stdout (the ticket is a base32 CBOR
of the endpoint's `EndpointId` + reachable direct addresses). `pair --new` without `--json` prints
the bare ticket. **Rooms are isolated at the transport**: the room string is folded into the QUIC
ALPN, so a peer in a different `--room` cannot connect (the handshake is rejected).

## LAN auto-discovery (no ticket)

Two daemons started with the **same `--room` on the same LAN auto-discover and connect with no
ticket** — this is the cross-machine zero-config path. Run one daemon on each host:

```sh
# On host 1 (e.g. the Mac):
clip --room demo daemon --foreground        # or just `echo hi | clip --room demo send` (auto-spawns)

# On host 2 (e.g. the headless Linux box), same room:
clip --room demo daemon --foreground

# …then send/recv as usual, no `pair` step:
echo hi | clip --room demo send             # on host 1
clip --room demo recv                        # on host 2 -> hi
```

They find each other over **mDNS** and open a direct QUIC connection. Isolation is preserved twice:
the mDNS service name is **scoped by room** (different rooms never even see each other) and the room
secret is still folded into the **ALPN** (a cross-room dial is rejected at the handshake). To avoid a
double-dial, only the peer with the larger `EndpointId` initiates; the other accepts.

**Ticket fallback (different LANs / no multicast):** mDNS is LAN-local, so across separate networks
(or where multicast is filtered) use the explicit ticket flow — `clip … pair --new` on one host,
`clip … pair <ticket>` on the other (see the harness commands above). Both paths coexist.

**Verify locally:** `bash mesh-rs/scripts/autodiscover.sh` starts two same-room daemons on this host
with **no ticket** and asserts they auto-connect and round-trip text + a PNG (hash-equal). This
complements the shared ticket-based `scripts/roundtrip.sh mesh-rs`.

**Multi-homed / Tailscale caveat:** the endpoint binds all interfaces and advertises every non-loopback
direct address it finds, so QUIC uses whichever path connects (real LAN or Tailscale). mDNS multicast,
however, only reaches peers on interfaces that carry it — a Tailscale/VPN interface generally won't, so
**auto-discovery relies on the real LAN interface** allowing multicast (224.0.0.251 / ff02::fb). If the
LAN blocks multicast, fall back to a ticket; direct QUIC over the LAN address still works.

## Headless / no-display hosts (graceful clipboard degradation)

On a headless Linux server (no X11/Wayland) the OS clipboard can't be opened. The daemon **detects
this once at startup** and degrades gracefully instead of crashing — it logs one line:

```
WARN clipboard unavailable (headless) — send/recv/paste-to-file still work
```

With the clipboard unavailable: `send`, `recv [--follow] [--latest-image --emit-path]`, auto-discovery,
gossip/transfer, and the daemon itself all keep working fully. Only clipboard writes become clear
no-ops: `clip paste` returns a `clipboard unavailable (headless)` message with exit code `1` (never a
panic), and `auto_copy on` keeps buffering received items without touching the clipboard. `clip status`
reports the state as a `clipboard: available|unavailable` field.

To exercise this path on a machine that *does* have a display (macOS, or a test), set
`CLIP_FORCE_HEADLESS=1` when starting the daemon — the probe then reports `unavailable` exactly as on a
real display-less host.

## Full CLI surface

| Command | Behavior |
|---|---|
| `clip daemon [--foreground]` | Run the resident daemon. |
| `clip send [--text\|--image\|--auto]` | Read stdin to EOF, sniff type (PROTOCOL §2), broadcast. |
| `clip recv [--follow] [--latest-image --emit-path] [--out PATH]` | Text→stdout; image→temp file, print path. `--follow` streams; otherwise returns the latest buffered item. |
| `clip paste` | Write the latest received item to the OS clipboard (text→set_text, image→PNG→RGBA→set_image). |
| `clip pair --new [--json]` / `clip pair <ticket>` | Create / join a pairing. |
| `clip peers` / `clip status [--json]` | Inspect peers / daemon state (`status` includes `clipboard: available\|unavailable`). |
| `clip config set KEY VALUE` / `clip config get KEY` | Persist settings (SPEC §5). |

### Config keys (SPEC §5)

`auto_copy` = `notify` (default) \| `on` \| `off` · `device_name` · `room` · `internet` · `broadcast_on_copy`.

- **notify** (default): received items are buffered + a toast is emitted; the clipboard is **not**
  touched. Use `clip paste` (or a TUI key, later) to place it.
- **on**: received items are written straight to the OS clipboard.
- **off**: never touch the clipboard.

Echo/loop suppression (SPEC §3): dedupe by `msg_id`; the daemon records the hash of what it last
wrote to its own clipboard; received items are never re-broadcast.

## Manual smoke test (macOS)

```sh
# terminal A
echo hi | clip --room demo send            # (auto-spawns the daemon)
# terminal B (same machine, distinct dirs/socket)
clip --config-dir /tmp/b --socket /tmp/b.sock --room demo pair "$(clip --room demo pair --new)"
clip --config-dir /tmp/b --socket /tmp/b.sock --room demo recv     # -> hi
clip --config-dir /tmp/b --socket /tmp/b.sock --room demo paste    # -> pbpaste shows hi
```

## Transport notes / what's stubbed

- **Transport = iroh 1.0.2** (pinned exactly; the API churns). Phase 0 uses a minimal iroh
  `Endpoint` (preset `Minimal`, `RelayMode::Disabled`) + **direct QUIC** bidi streams carrying
  CBOR envelopes; image bytes are **pulled by BLAKE3 hash** over a direct stream (never flooded).
  Peers connect either via a copy-pasted **ticket** or via **LAN mDNS auto-discovery** (same
  `--room`, no ticket — see above), using the `iroh-mdns-address-lookup` companion crate (iroh
  1.0.2 renamed "discovery" to *address lookup*; the mDNS/swarm-discovery variant lives in that
  crate). **iroh-gossip** (N-peer text announcement mesh) and **iroh-blobs** (resumable
  content-addressed image transfer) are the next step and are **not** wired yet.
- **Clipboard = arboard** (`image-data`); PNG↔RGBA bridged with the `image` crate (PNG is the
  canonical wire format). macOS text + image verified. Clipboard access is **fallible**: on a
  headless host it's detected once at startup and all writes degrade to clear no-ops (see the
  headless section above). Linux X11 clipboard persistence (owner-must-stay-alive) and
  `wayland-data-control` are Phase 1/3.
- **TUI** (ratatui) is Phase 2 — not built.
- **Trust**: Phase 0 auto-allowlists any peer you pair/connect with. TOFU-with-approval (holding
  first contact pending) is Phase 1.
