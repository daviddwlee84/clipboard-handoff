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

## Full CLI surface

| Command | Behavior |
|---|---|
| `clip daemon [--foreground]` | Run the resident daemon. |
| `clip send [--text\|--image\|--auto]` | Read stdin to EOF, sniff type (PROTOCOL §2), broadcast. |
| `clip recv [--follow] [--latest-image --emit-path] [--out PATH]` | Text→stdout; image→temp file, print path. `--follow` streams; otherwise returns the latest buffered item. |
| `clip paste` | Write the latest received item to the OS clipboard (text→set_text, image→PNG→RGBA→set_image). |
| `clip pair --new [--json]` / `clip pair <ticket>` | Create / join a pairing. |
| `clip peers` / `clip status [--json]` | Inspect peers / daemon state. |
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
  `Endpoint` (preset `Minimal`, `RelayMode::Disabled`) + ticket-based **direct QUIC** bidi streams
  carrying CBOR envelopes; image bytes are **pulled by BLAKE3 hash** over a direct stream (never
  flooded). This is the fallback path sanctioned by the plan. **iroh-gossip** (N-peer text
  announcement mesh) and **iroh-blobs** (resumable content-addressed image transfer) are the next
  step and are **not** wired yet.
- **Clipboard = arboard** (`image-data`); PNG↔RGBA bridged with the `image` crate (PNG is the
  canonical wire format). macOS text + image verified. Linux X11 clipboard persistence
  (owner-must-stay-alive) and `wayland-data-control` are Phase 1/3.
- **TUI** (ratatui) is Phase 2 — not built.
- **Trust**: Phase 0 auto-allowlists any peer you pair/connect with. TOFU-with-approval (holding
  first contact pending) is Phase 1.
