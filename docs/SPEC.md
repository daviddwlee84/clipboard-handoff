# SPEC — the shared product contract

Every implementation in this repo (`mesh-rs`, `room-go`, `experiments/*`) MUST present this same surface so a
user (and the bake-off harness) can drive any of them identically. Only the **binary name** and the **transport**
differ. Wire/IPC details live in [`PROTOCOL.md`](PROTOCOL.md).

## 1. Mental model

- A device runs one **resident daemon** that owns: the network connection(s), the device identity, the trusted-peer
  set, a small ring buffer of recently-received items, and **the OS clipboard**.
- `send` / `recv` / `paste` / `tui` are **thin, ephemeral clients** that talk to the local daemon over a local IPC
  socket (Unix domain socket, or a named pipe on Windows). The first client command **auto-spawns** the daemon.
- "The CLI just needs one persistent connection" == the daemon. Clients come and go; the daemon stays connected.

Why a daemon is mandatory (not a style choice): on X11/Wayland the process that sets the clipboard **owns the
selection** and must stay alive to serve future pastes; and "auto-copy on receive" is an always-on behavior.

## 2. CLI surface (identical across all impls)

Binary names: `mesh-rs` → `clip`, `room-go` → `room`, `experiments/lan-go` → `lan`, `experiments/libp2p-mesh` →
`libp2p-mesh`. Below, `BIN` stands for whichever.

| Command | Behavior |
|---|---|
| `BIN send [--text\|--image\|--auto]` | Read **stdin** to EOF, sniff type (default `--auto`: valid UTF-8 → text; PNG/JPEG magic bytes → image), broadcast to all connected peers. Exits 0 after the daemon has accepted it. |
| `BIN recv [--follow] [--latest-image --emit-path] [--out PATH]` | Subscribe to incoming items. Text → stdout. Image → write to a temp file and print its path (`--emit-path`) or to `--out`. `--follow` streams until Ctrl-C; without it, waits for the next single item (or returns the latest buffered one). |
| `BIN paste` | Write the **latest received item** into the local OS clipboard (text via set_text, image via set_image). This is the "one-key paste" for the default notify-first mode. |
| `BIN tui` | Launch the messenger-style chat TUI (see §4). |
| `BIN pair` / `BIN join` | Establish membership. **mesh** (`clip`,`lan`,`libp2p-mesh`): `pair --new` prints a ticket + QR; peer runs `pair <ticket>`; short-code confirm. **room** (`room`): `join <user@server>` using an SSH key. |
| `BIN peers` | List currently-connected peers (name, id/fingerprint, direct/relayed, last-seen). |
| `BIN status` | Show daemon state: identity, transport mode (lan/internet), auto_copy setting, peer count, buffer size. |
| `BIN config set KEY VALUE` / `BIN config get KEY` | Persist settings (see §5). |
| `BIN daemon [--foreground]` | Run the resident daemon. Normally auto-spawned; `--foreground` for debugging. |

**Exit codes:** `0` success · `1` generic error · `2` usage error · `3` no daemon / cannot reach daemon ·
`4` reserved · `5` nothing to paste/recv. **`send` with no connected peers exits `0`** with a stderr warning
(the item is still accepted/buffered) — so it is safe under `set -e` and never aborts a pipeline.

**Global flags:** `--room NAME` (default `default`), `--config-dir PATH`, `--socket PATH`, `-q/--quiet`, `-v/--verbose`,
`--json` (machine-readable output for `peers`/`status`).

## 3. Auto-copy semantics (identical; default = `notify`)

`config set auto_copy notify|on|off`:

- **`notify`** (default, safest): on receiving an item from an **allowlisted** peer, show a desktop notification
  and a TUI toast, but do **NOT** write the OS clipboard. The user presses one key in the TUI, or runs `BIN paste`,
  to actually place it. This is the AirDrop "hand-off, then accept" feel without hijacking the clipboard.
- **`on`**: silently write every received item straight to the OS clipboard (fullest AirDrop feel; riskier).
- **`off`**: never touch the clipboard automatically; content is only visible in the TUI / via `recv`.

**Echo / loop suppression (required in every mode):**
1. Dedupe by `msg_id` — never process the same item twice.
2. Track the content-hash of the last value this daemon **wrote** to its own clipboard; ignore a local clipboard
   change that equals it (so `on` mode doesn't re-broadcast what it just pasted).
3. Never re-broadcast an item that was just received. Received ≠ locally-originated.

**Trust:** items are only auto-actioned from peers on the **allowlist**. First contact from an unknown peer is
held pending explicit approval (TOFU). See PROTOCOL §identity.

## 4. TUI (messenger-style)

- A scrollback of chat bubbles (peer name · relative time · content), newest at the bottom, plus a text composer.
- Text bubbles show inline. Image bubbles show a fixed-height inline **thumbnail** when a terminal graphics
  protocol (Kitty/iTerm2/Sixel) is available, else a Unicode half-block render, else a text placeholder
  (`🖼 screenshot.png 1440×900 · 84 KB`). Image encoding/resizing happens **off** the render thread.
- Per-image key actions: `y` copy full-res to OS clipboard · `s` save to file · `o` open externally.
- Typing a line + Enter sends it as text (same as `send`). Pasting an image into the composer (where supported)
  sends it as an image.
- A toast appears for received items in `notify` mode with a one-key **accept → clipboard**.

## 5. Config keys (persisted in the OS config dir)

| Key | Values | Default | Meaning |
|---|---|---|---|
| `auto_copy` | `notify` \| `on` \| `off` | `notify` | §3 |
| `internet` | `on` \| `off` | `off` | LAN-only vs enable relay/hole-punch/global discovery (Phase 3) |
| `device_name` | string | hostname | Shown to peers |
| `room` | string | `default` | Default room/topic |
| `broadcast_on_copy` | `on` \| `off` | `off` | Watch local clipboard and auto-send changes to the mesh (opt-in) |

Config dir: macOS `~/Library/Application Support/<bin>/`, Linux `$XDG_CONFIG_HOME/<bin>/` (or `~/.config/<bin>/`),
Windows `%APPDATA%\<bin>\`. The IPC socket lives in the OS runtime/temp dir.

## 6. OS support matrix (target = all first-class)

| OS | Clipboard text | Clipboard image | Notes |
|---|---|---|---|
| macOS | ✅ | ✅ (PNG via NSPasteboard) | Phase 0 primary dev target |
| Linux X11 | ✅ | ✅ (`image/png` target) | Phase 0 |
| Linux Wayland | ✅ | ✅ | needs `wayland-data-control`; XWayland/text fallback (Phase 3) |
| Windows | ✅ | ✅ (CF_DIB) | named-pipe IPC (Phase 3) |

## 7. Phase 0 acceptance (the MVP that proves the magic)

On one LAN, two devices in the same room, `auto_copy on` for the test:
1. `echo hi | BIN send` on A → `BIN recv` on B prints `hi`; `BIN paste` on B puts `hi` on the clipboard.
2. `BIN send < testdata/small.png` on A → on B, `BIN recv --latest-image --emit-path` writes a PNG whose
   **BLAKE3 hash equals** the source; with `auto_copy on`, the image is on B's clipboard.

The bake-off harness (`scripts/`) automates (1) and (2) for every impl.
