# Usage — `clip` (mesh-rs, Rust/iroh P2P)

The **no-server, private-by-default** implementation. Two devices in the same *room* find each other
directly (mDNS on a LAN, or a copy-pasted *ticket* across networks) over an iroh QUIC connection —
no relay, no account, no infrastructure. Text and images copied on one device become
receivable/pasteable on the other, the image arriving **BLAKE3 hash-equal**.

- Binary: **`clip`** · transport: **iroh** (Ed25519 NodeId identity) · impl: [`../mesh-rs/`](https://github.com/daviddwlee84/clipboard-handoff/tree/main/mesh-rs/)
- Shared contract: [SPEC](SPEC.md) (CLI §2, sinks §3, config §5, sessions §8) · wire/IPC: [PROTOCOL](PROTOCOL.md)
- For deeper transport notes see [`../mesh-rs/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/mesh-rs/README.md).

## Install / build

```sh
cargo build --release --manifest-path mesh-rs/Cargo.toml
# binary -> mesh-rs/target/release/clip   (debug: drop --release -> target/debug/clip)
```

The first build pulls the full iroh tree (~390 crates); a release compile takes several minutes.
Put the binary on your `PATH` (e.g. `install mesh-rs/target/release/clip ~/.local/bin/`) so the
examples below can call it as `clip`.

## Quick start (two devices, same LAN)

No pairing step — same `--room` on the same LAN auto-discovers over mDNS.

```sh
# Device B: bring up a daemon (or just run any client command; it auto-spawns one)
clip --room demo daemon --foreground        # leave running

# Device A: send
echo "hello from A" | clip --room demo send

# Device B: receive
clip --room demo recv                        # -> hello from A
clip --room demo paste                       # place it on B's OS clipboard
```

`send`/`recv`/`paste`/`tui` are thin clients that talk to a local resident **daemon** over a Unix
socket; the first one **auto-spawns** the daemon if it isn't already up. The daemon owns the iroh
connection, the identity, the received-item buffer, and the OS clipboard.

## Connecting devices

### 1. mDNS auto-discovery (default, zero-config)

Start a daemon with the **same `--room` on the same LAN** on each host; they discover each other and
open a direct QUIC connection with **no ticket**. Isolation is enforced twice: the mDNS service name
is scoped by room, and the room secret is folded into the QUIC ALPN (a cross-room dial is rejected at
the handshake). To avoid a double-dial, only the peer with the larger NodeId initiates.

```sh
clip --room demo daemon --foreground     # host 1
clip --room demo daemon --foreground     # host 2  — then send/recv, no pair step
```

### 2. Ticket fallback (different LANs / no multicast)

mDNS is LAN-local, so across separate networks — or where multicast (`224.0.0.251` / `ff02::fb`) is
filtered — use an explicit **ticket**:

```sh
# Host 1: mint a ticket
clip --room demo pair --new              # prints a bare base32 ticket
clip --room demo pair --new --json       # -> {"ticket":"<base32>"}

# Host 2: join with it
clip --room demo pair "<ticket>"
```

The ticket is a base32-encoded CBOR of the endpoint's id + reachable direct addresses. Both paths
coexist; the ticket path is the reliable one on multi-homed hosts (see Troubleshooting).

## Command reference

Global flags (accepted before **or** after the subcommand): `--config-dir PATH`, `--socket PATH`,
`--room NAME` (default `default`), `--json`, `-q/--quiet`, `-v/--verbose`.

| Command | What it does |
|---|---|
| `clip send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | Read `PATH` (or **stdin** to EOF), sniff type (default `--auto`), broadcast to peers. Exits `0` even with no peers (item is still buffered) — safe under `set -e`. |
| `clip recv [--follow] [--latest-image --emit-path] [--out PATH]` | Subscribe to incoming items. Text → stdout; image/file → blob-cache file, path printed. `--follow` streams until Ctrl-C; otherwise returns the latest buffered item. `--latest-image` filters to images; `--emit-path`/`--out` control path output. |
| `clip paste` | Write the latest received item to the OS clipboard (text→set_text, image→PNG→set_image, **file → its path as text**). |
| `clip tui` | Launch the messenger-style chat TUI (see below). |
| `clip pair --new [--json]` · `clip pair <ticket>` | Mint a ticket / join with one (see above). |
| `clip peers` | List currently-connected peers. |
| `clip status` | Show daemon state: identity, room, peer count, `auto_copy`, buffer size, `clipboard: available\|unavailable`. Add `--json` for machine output. |
| `clip config set KEY VALUE` · `clip config get KEY` | Persist / read settings (see Config). |
| `clip clear [--all] [--yes]` | Clear this session's data (see Sessions). |
| `clip daemon [--foreground]` · `clip daemon stop` | Run / stop the resident daemon. `stop` applies `clear_on_exit`. |

**Sending an arbitrary file** — `image` is the only clipboard-pasteable binary type; `file` is any
bytes with a best-effort MIME and a `filename`:

```sh
clip send report.pdf                     # --auto: binary → file, filename=report.pdf
clip send notes.txt --file               # force a file even though it is UTF-8
tar cz dir | clip send --file --name dir.tgz   # stdin + explicit name
clip send shot.png                       # PNG magic → image (clipboard-pasteable)
```

Image **and** file bytes are pulled over a direct stream keyed by `blob.hash` and BLAKE3-verified on
receipt — so `recv --emit-path` and `paste` always have a local path. Type sniffing follows
[PROTOCOL §2](PROTOCOL.md).

## Sinks — where received items go (SPEC §3)

Sinks are **additive**: an item can hit the clipboard *and* the folder/append-file.

| Sink | Config key | Receives |
|---|---|---|
| clipboard | `auto_copy` | text, image (a `file` never auto-lands on the clipboard) |
| folder | `save_dir` | image → `<filename\|hash>.png`, file → `<filename\|hash>` — de-duplicated `name (2).ext` |
| append-file | `text_file` | text, appended after a `\n---\n<device> <ISO8601 ts>\n` header |

```sh
clip config set save_dir  ~/Downloads/clip
clip config set text_file ~/notes/clip.md
```

**`auto_copy` modes** (`clip config set auto_copy notify|on|off`):
- **`notify`** (default): buffer the item + emit a toast; do **not** touch the clipboard. Press `y`
  in the TUI, or run `clip paste`, to place it.
- **`on`**: write every received item straight to the OS clipboard.
- **`off`**: never touch the clipboard; items are visible only in the TUI / via `recv`.

## Config keys (SPEC §5)

`auto_copy` (`notify`|`on`|`off`, default `notify`) · `save_dir` (path|"") · `text_file` (path|"") ·
`clear_on_exit` (`ask`|`transient`|`all`|`never`, default `ask`) · `device_name` (default hostname) ·
`room` (default `default`) · `internet` (`on`|`off`, Phase 3) · `broadcast_on_copy` (`on`|`off`, Phase 3).

Config dir: macOS `~/Library/Application Support/mesh-rs/`, Linux `$XDG_CONFIG_HOME/mesh-rs/`,
Windows `%APPDATA%\mesh-rs\`. Override with `--config-dir`.

## Sessions & clearing (SPEC §8)

A session is one daemon run. The daemon tracks, per session: the **transient** store
(`<config-dir>/blobs` — the fetched-blob cache + `recv --emit-path` files — plus the in-memory
buffer), the **`text_file` size at session start**, and the **`save_dir` files it wrote** this
session. A clear is therefore precise and never touches pre-session content.

```sh
clip clear                 # transient only (buffer + blob cache + emit-path temp files)
clip clear --all           # + revert this session's sink writes (prompts unless --yes)
clip clear --all --yes     # non-interactive
clip daemon stop           # applies clear_on_exit; SIGTERM does the same
```

`clear_on_exit` drives `daemon stop`/SIGTERM: `never` · `transient` · `all` · `ask` (prompt on a TTY,
else fall back to `transient`). Quitting the TUI raises the same inline choice.

## TUI (`clip tui`)

A messenger-style chat bound to the local daemon. It is a front-end only — every copy is routed
through the daemon (the clipboard owner). Auto-spawns/attaches the daemon, then streams live items.

```sh
clip --room demo tui
```

Header: room · short endpoint id · peer count · `auto_copy` mode · `clipboard: available|unavailable`.
Middle: scrollable bubble history (your own bubbles right-aligned, labelled `you`). Bottom: a composer.

| Key | Action |
|---|---|
| type + `Enter` | Send the line as a text item (appears immediately as your own bubble). |
| `Esc` | Toggle focus between the **composer** and **browse** mode. |
| `↑` / `↓` (or `k` / `j` in browse) | Move the highlight over messages. |
| `y` | Copy the highlighted (or latest) item to the OS clipboard **via the daemon** (a `file` copies its path as text). |
| `s` | Save the highlighted image/file to `~/Downloads/` (else cwd). |
| `o` | Open the highlighted image/file with the OS default app (`open`/`xdg-open`/`start`). |
| `q` (browse) · `Ctrl-C` (any) | Quit. If anything was received this session, first prompt `⚑ clear this session? [t]ransient / [a]ll incl sinks / [n]o`. |

**Inline images:** every image bubble shows metadata (`🖼 W×H · size · filename`) and a real inline
thumbnail via ratatui-image — a terminal graphics protocol (**Kitty / iTerm2 / Sixel**) when detected
at startup, otherwise **Unicode half-blocks**, otherwise the metadata line as placeholder. Decode/resize
run off the UI thread. First launch on a terminal that doesn't answer the graphics-capability query
pauses ~1–2 s while the probe times out; modern terminals start instantly. Pasting an image *into* the
composer is not wired yet — send images with `clip send --image`.

## Cross-machine (mac ↔ headless Linux)

Deploy the binary to both hosts, use the same `--room`, and rely on mDNS auto-discovery — or the
ticket fallback if the LAN blocks multicast. Verified mac → headless Ubuntu with a hash-equal PNG.

## Headless / no-display hosts

The daemon detects a missing OS clipboard **once at startup** and degrades gracefully (no crash),
logging `WARN clipboard unavailable (headless) — send/recv/paste-to-file still work`. `send`, `recv`
(incl. `--latest-image --emit-path`), discovery, and transfer all keep working; only clipboard writes
become no-ops: `clip paste` returns `clipboard unavailable (headless)` with exit code `1`, and
`auto_copy on` keeps buffering without touching the clipboard. `clip status` reports
`clipboard: available|unavailable`. To exercise this on a machine that *does* have a display, start the
daemon with `CLIP_FORCE_HEADLESS=1`.

## Troubleshooting

- **Peers never connect on a LAN** → mDNS multicast may be filtered, or you are multi-homed
  (Tailscale/VPN + LAN). mDNS only reaches interfaces that carry multicast; a VPN interface usually
  won't. Fall back to the **ticket** flow (`pair --new` / `pair <ticket>`) — direct QUIC over the LAN
  address still works. This is the reliable path on a multi-interface host.
- **Different networks** → mDNS is LAN-local; use a ticket.
- **`clip paste` says "clipboard unavailable (headless)"** → expected on a display-less host; use
  `recv --emit-path` / `save_dir` instead.
- **Nothing to paste/recv** → exit code `5`; you haven't received an item yet.
- **Trust note (Phase 0):** any peer you pair/auto-connect with is auto-allowlisted. TOFU
  hold-for-approval and the SPEC's short-code/QR confirm are Phase 1 (not in this build).
</content>
</invoke>
