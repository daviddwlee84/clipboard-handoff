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
| `clip daemon [--foreground]` · `clip daemon stop` | Run / stop the resident daemon (`stop` applies `clear_on_exit`, §Sessions). |
| `clip send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | Read `PATH` (or stdin to EOF), sniff type (PROTOCOL §2), broadcast. An image/file carries its `filename` (from `PATH` or `--name`). |
| `clip recv [--follow] [--latest-image --emit-path] [--out PATH]` | Text→stdout; image/file→blob-cache file, print path. `--follow` streams; otherwise returns the latest buffered item. |
| `clip paste` | Write the latest received item to the OS clipboard (text→set_text, image→PNG→RGBA→set_image, **file → its path as text**). |
| `clip clear [--all] [--yes]` | Clear this session's received data (§Sessions). |
| `clip tui` | Messenger-style chat TUI attached to the daemon (see below). |
| `clip pair --new [--json]` / `clip pair <ticket>` | Create / join a pairing. |
| `clip peers` / `clip status [--json]` | Inspect peers / daemon state (`status` includes `clipboard: available\|unavailable`). |
| `clip config set KEY VALUE` / `clip config get KEY` | Persist settings (SPEC §5). |

### Sending an arbitrary file

`image` is the only clipboard-pasteable binary type; **`file`** is arbitrary bytes with a
best-effort mime (guessed from the extension, else `application/octet-stream`) and a `filename`.
Sniffing (`--auto`, the default): PNG/JPEG magic → **image**; `--file` or a non-UTF-8 payload →
**file**; valid UTF-8 → **text**.

```sh
clip send report.pdf                  # --auto: binary → file, filename=report.pdf
clip send notes.txt --file            # force a file even though it is UTF-8 text
tar cz dir | clip send --file --name dir.tgz   # stdin + an explicit name
clip send shot.png                    # PNG magic → image (still clipboard-pasteable)
```

Image **and** file bytes ride the same path as before: a small hash-announce envelope is
broadcast, each receiver **pulls** the bytes over a direct stream keyed by `blob.hash`,
**BLAKE3-verifies** them, and materializes them into its blob cache — so `recv --emit-path` and
`paste` always have a local path. `paste` of a `file` copies **that path** onto the clipboard as
text (a file has no image clipboard form).

### Sinks (SPEC §3) — where received items go

Sinks are **additive**: an item can hit the clipboard *and* the folder/append-file.

| Sink | Config key | Gets |
|---|---|---|
| clipboard | `auto_copy` | text, image (a `file` never lands on the clipboard automatically) |
| folder | `save_dir` | image → `<filename\|hash>.png`, file → `<filename\|hash>` — de-duplicated as `name (2).ext` |
| append-file | `text_file` | text, appended as `\n---\n<device> <ISO8601 ts>\n<text>\n` |

```sh
clip config set save_dir  ~/Downloads/clip
clip config set text_file ~/notes/clip.md
```

### Config keys (SPEC §5)

`auto_copy` = `notify` (default) \| `on` \| `off` · `save_dir` = path \| `""` · `text_file` = path \| `""` ·
`clear_on_exit` = `ask` (default) \| `transient` \| `all` \| `never` · `device_name` · `room` · `internet` ·
`broadcast_on_copy`.

- **notify** (default): received items are buffered + a toast is emitted; the clipboard is **not**
  touched. Use `clip paste` (or `y` in the TUI) to place it.
- **on**: received items are written straight to the OS clipboard.
- **off**: never touch the clipboard.

Echo/loop suppression (SPEC §3): dedupe by `msg_id`; the daemon records the hash of what it last
wrote to its own clipboard; received items are never re-broadcast. Sinks run **after** suppression.

## Sessions & clearing (SPEC §8)

A hand-off leaves data behind. The daemon tracks, per session (one daemon run): the **transient
store** (`<config-dir>/blobs` — the fetched-blob cache *and* the `recv --emit-path` files — plus the
in-memory buffer), the **`text_file` size at session start**, and the **`save_dir` files it wrote
this session**. Clearing is therefore precise: it never touches pre-session content and never
deletes a directory.

| Scope | What it does |
|---|---|
| **transient** (always safe) | Empty the in-memory buffer + purge the blob cache / emit-path files. |
| **all** (= transient + sinks) | Also truncate `text_file` back to its session-start offset and delete the `save_dir` files written this session. |

```sh
clip clear                 # transient
clip clear --all           # + revert this session's sink writes (prompts unless --yes)
clip clear --all --yes
clip daemon stop           # applies clear_on_exit; SIGTERM does the same
```

`clear_on_exit` drives `daemon stop` / SIGTERM: `never` · `transient` · `all` · `ask` (prompt on a
TTY, else fall back to `transient`). Quitting the **TUI** (`q` / `Ctrl-C`) after receiving anything
raises the same choice inline: `⚑ clear this session? [t]ransient / [a]ll incl sinks / [n]o`.

Verified end-to-end by `scripts/e2e_sinks.sh` (two daemons, sink writes hash-checked, then
`clear --all --yes` reverts exactly this session's writes).

## Messenger TUI (`clip tui`)

A messenger-style chat bound to the local daemon (SPEC §4). It is a thin **front-end**: it never
opens the OS clipboard itself (that would make it a second clipboard owner) — every copy is routed
through the daemon, which owns the clipboard. Like every other subcommand it **auto-spawns** the
daemon if it isn't running, then `Subscribe`s to the daemon's event stream for live items.

```sh
clip --room demo tui           # auto-spawns the daemon, attaches, and opens the chat
```

Layout: a **header** (room · short endpoint id · peer count · `auto_copy` mode ·
`clipboard: available|unavailable`), a scrollable **message history** in the middle (your own
bubbles are right-aligned and labelled `you`; peers are left-aligned and colour-coded), and a
**composer** (tui-textarea) at the bottom with a one-line keybinding hint.

Keybindings:

| Key | Action |
|---|---|
| type + `Enter` | Send the line as a text item; it appears immediately as your own bubble. |
| `Ctrl-V` (compose) | Send whatever image is on the local OS clipboard — the daemon (arboard owner) reads it and broadcasts it as an image item; the composer stays text-only. Headless → a clean "clipboard unavailable" toast, no panic. |
| `Esc` | Toggle focus between the **composer** and **browse** mode. |
| `↑` / `↓` (or `k` / `j` in browse) | Move the highlight over messages. |
| `y` | Copy the highlighted (or latest) item to the OS clipboard **via the daemon** (a `file` copies its path as text). |
| `s` | Save the highlighted image/file to `~/Downloads/` (else the cwd). |
| `o` | Open the highlighted image/file with the OS default app (`open`/`xdg-open`/`start`). |
| `q` (browse) · `Ctrl-C` (any) | Quit. If anything was received this session, first ask `⚑ clear this session? [t]ransient / [a]ll incl sinks / [n]o` (SPEC §8), run that clear, then restore the terminal. |

**Notify-first (respects `auto_copy`):** in the default `notify` mode the TUI does **not** touch the
clipboard on receipt — incoming items surface with a subtle `● press y to copy` affordance and a
toast; you press `y` to place the highlighted one. In `on` mode the daemon has already copied it
(the TUI just says so); in `off` mode items are only shown.

**Images:** every image bubble always shows metadata (`🖼 W×H · size · filename`). A thumbnail is
rendered inline with ratatui-image's `StatefulProtocol` — a real terminal graphics protocol
(**Kitty / iTerm2 / Sixel**) when one is detected at startup, otherwise **Unicode half-blocks**; if
the file can't be decoded or the thumbnail isn't ready yet, the metadata line stands in as the text
placeholder. Image **decode and resize/encode run off the UI thread** (a `spawn_blocking` decode
plus a resize worker task), so the event loop never blocks on image work. First launch on a terminal
that doesn't answer the graphics-capability query pauses ~1–2 s while that probe times out; terminals
that do answer (most modern ones) start instantly.

Stack: **ratatui 0.29 + ratatui-image 9 + tui-textarea 0.7 + crossterm 0.28**, on the existing tokio
runtime (pinned as one consistent set; tui-textarea caps ratatui at 0.29).

## TUI tests (three layers)

The `clip tui` code splits a pure `App` model from the IO/render shell, tested in layers:

1. **Model + render (default `cargo test`)** — model-logic tests (`handle_key`/`apply_event`/…) plus
   **ratatui `TestBackend`** render tests that draw `App` into an in-memory buffer and assert the drawn
   glyphs (header peer count, bubbles, the `^V send image` hint). Fast, deterministic, no PTY.
2. **PTY end-to-end (gated `#[ignore]`)** — `tests/tui_e2e.rs` launches the built binary under a real
   pseudo-terminal (`expectrl`), waits for the header, quits with Ctrl-C, asserts a clean exit:
   `cargo test --test tui_e2e -- --ignored` (or `just tui-e2e mesh-rs`).
3. **Multiplexer image smoke** — `scripts/tui_mux.sh` runs `clip tui` inside tmux/zellij, sends it an
   image, and asserts the image bubble renders without a panic (`just tui-mux`). tmux is the asserting
   case; zellij is best-effort (its headless scripting is finicky) — verify inline-pixel fidelity visually.

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
  CBOR envelopes; image **and file** bytes are **pulled by BLAKE3 hash** over a direct stream
  (never flooded) and verified on receipt.
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
- **TUI** (ratatui) — **built** (Phase 2): `clip tui`, a messenger-style chat over the daemon's
  event stream with inline image thumbnails (Kitty/iTerm2/Sixel → Unicode half-blocks). See the
  [Messenger TUI](#messenger-tui-clip-tui) section above. **Ctrl-V** in the composer sends the OS
  clipboard image (the daemon reads it via arboard); dragging/pasting a file path into the composer
  is not wired (send arbitrary files with `clip send --image`/`--file`).
- **Trust**: Phase 0 auto-allowlists any peer you pair/connect with. TOFU-with-approval (holding
  first contact pending) is Phase 1.
