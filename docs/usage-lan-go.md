# Usage — `lan` (lan-go, Go/quic+mDNS experiment)

The **minimal LAN probe**: quic-go direct connections + mDNS auto-discovery. Zero config on one LAN —
two devices in the same *room* find each other and hand off text/images with no server, no ticket, no
pairing step. It exists to answer the bake-off question *"is iroh's weight worth it, or is a
hand-rolled LAN mesh good enough?"* — see [BAKEOFF](BAKEOFF.md).

> **Experiment, not a headliner.** `lan-go` is **LAN-first with no internet path**: mDNS is local, so
> the moment you leave the LAN you'd be rebuilding what iroh (`clip`) gives for free. For a shippable
> tool pick `clip` (mesh-rs) or `room` (room-go); `lan` is the fast-path proof. The CLI plumbing lives
> in `shared-go`, so `lan` and `libp2p-mesh` behave identically.

- Binary: **`lan`** · transport: **quic-go + mDNS** (`grandcat/zeroconf`); identity = SHA-256 of a
  persisted self-signed TLS cert (Syncthing-style) · impl: [`../experiments/lan-go/`](https://github.com/daviddwlee84/clipboard-handoff/tree/main/experiments/lan-go/)
- Shared contract: [SPEC](SPEC.md) (CLI §2, sinks §3, config §5, sessions §8) · wire/IPC: [PROTOCOL](PROTOCOL.md)
- More detail: [`../experiments/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/experiments/README.md), [`../experiments/lan-go/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/experiments/lan-go/README.md).

## Install / build

```sh
cd experiments/lan-go
go build -o bin/lan ./cmd/lan            # binary -> experiments/lan-go/bin/lan
```

The `go.work` + `replace` directives point the probe at `../shared-go`, so builds work with or without
the workspace (`GOWORK=off`). Put `bin/lan` on your `PATH` to call it as `lan`.

## mDNS auto-discovery (no pairing)

Each daemon advertises a `_clip-lan._udp` service whose TXT record carries `room=`, `fp=`, `port=`
and `name=`; it browses for the same service and connects only to peers whose `room` matches. So the
only "pairing" is: **run a daemon in the same `--room` on the same LAN.** `lan pair` / `lan join`
exist but are documented **no-ops** (they just print a note and exit 0). Two instances on one host work
too (each binds a distinct ephemeral UDP port; the smaller fingerprint dials to avoid a double-connect).

## Quick start (two devices, same LAN)

```sh
# Device B
lan --room demo daemon --foreground      # leave running (auto-spawned otherwise)

# Device A
echo "hello from A" | lan --room demo send

# Device B
lan --room demo recv                      # -> hello from A
lan --room demo paste                     # place it on B's OS clipboard
```

## Command reference

Global flags (before **or** after the subcommand): `--config-dir PATH`, `--socket PATH`,
`--room NAME` (default `default`), `--json`, `-q/--quiet`, `-v/--verbose`.

| Command | What it does |
|---|---|
| `lan send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | Read `PATH` (or **stdin** to EOF), sniff type, broadcast. Exits `0` even with no peers (warning on stderr). |
| `lan recv [--follow] [--latest-image --emit-path] [--out PATH]` | Text → stdout; image/file → materialized file, path printed. `--follow` streams; `--latest-image` filters to images. |
| `lan paste` | Write the latest received item to the OS clipboard (text→set, image→PNG, **file → its path as text**). |
| `lan tui` | Messenger-style chat attached to the local daemon (see below). |
| `lan status [--json]` | identity / device / room / transport / `auto_copy` / `clipboard` / peers / buffer. |
| `lan peers [--json]` | List currently-connected peers (name · id · addr). |
| `lan config set KEY VALUE` · `lan config get KEY` | Persist / read settings (see Config). |
| `lan clear [--all] [--yes]` | Clear this session's data (see Sessions). |
| `lan daemon [--foreground]` · `lan daemon stop` | Run / stop the resident daemon. `stop` applies `clear_on_exit`. |
| `lan pair` / `lan join` | **No-op** — discovery is automatic (prints a note, exits 0). |

**Sending a file** — `image` is the only clipboard-pasteable binary type; `file` is any bytes with a
best-effort MIME and a `filename`:

```sh
lan send report.pdf                 # --auto: binary → file, filename=report.pdf
lan send --file blob.bin --name x   # force file, override the filename
cat notes.txt | lan send            # stdin → text (valid UTF-8)
lan send shot.png                   # → image (PNG/JPEG only; JPEG transcoded to PNG)
```

Sniffing follows [PROTOCOL §2](PROTOCOL.md). Blobs ride **inline** (`blob_data`, Phase 0) and their
BLAKE3 `blob.hash` is verified on receipt.

## Sinks — where received items go (SPEC §3)

Additive; chosen by type, independent of `auto_copy`:

| Sink | Config key | Receives |
|---|---|---|
| clipboard | `auto_copy` | text, image (a `file` never auto-lands on the clipboard) |
| folder | `save_dir` | image → `<filename\|hash>.png`, file → `<filename\|hash>` — de-duplicated `name (2).ext` |
| append-file | `text_file` | text, appended after a `\n---\n<device> <ISO8601 ts>\n` header |

```sh
lan config set save_dir  ~/Drop     # received image/file items land here
lan config set text_file ~/clip.md  # received text is appended here
```

**`auto_copy` modes** (`lan config set auto_copy notify|on|off`): `notify` (default) buffers + toasts
without touching the clipboard (use `lan paste`, or `p`/`y` in the TUI); `on` writes every item to the
clipboard; `off` never touches it.

## Config keys (SPEC §5)

`auto_copy` (`notify`|`on`|`off`, default `notify`) · `save_dir` (path|"") · `text_file` (path|"") ·
`clear_on_exit` (`ask`|`transient`|`all`|`never`, default `ask`) · `device_name` (default hostname) ·
`room` (default `default`) · `internet` (Phase 3) · `broadcast_on_copy` (Phase 3).

Config dir: macOS `~/Library/Application Support/lan/`, Linux `$XDG_CONFIG_HOME/lan/`,
Windows `%APPDATA%\lan\`. Override with `--config-dir`.

## Sessions & clearing (SPEC §8)

A session is one daemon run; it tracks the transient store (blob cache + `recv --emit-path` temp files +
in-memory buffer), the `text_file` size at session start, and the `save_dir` files it wrote.

```sh
lan clear                # transient only
lan clear --all          # + revert this session's sink writes (prompts unless --yes)
lan clear --all --yes    # non-interactive
lan daemon stop          # applies clear_on_exit; SIGTERM does the same
```

`clear_on_exit`: `never` · `transient` · `all` · `ask` (prompt on a TTY, else `transient`). The TUI
raises the same choice on quit.

## TUI (`lan tui`)

A lean messenger-style chat (bubbletea + lipgloss + bubbles) attached to the local daemon; it reuses
the daemon's event stream and IPC ops and never touches the transport/clipboard directly.

```sh
lan --room default tui              # auto-spawns the daemon if it is down
```

Header: room · id · peers · `auto_copy` · `clipboard: available|unavailable`. `Esc` toggles composer ↔
browse.

| Key | Action |
|---|---|
| type + `Enter` | Send the line as a text item (own bubble appears immediately). |
| `Esc` | Toggle focus between composer and browse mode. |
| `↑`/`k`, `↓`/`j` | Move the selection in browse mode (`g` / `G` = top / bottom). |
| `y` | Copy the highlighted bubble to the OS clipboard **via the daemon** (a file copies its path as text). |
| `s` | Save a highlighted image to `~/Downloads` (else temp dir). |
| `o` | Open a highlighted image/file with the OS default app. |
| `p` | Paste the daemon's latest received item to the clipboard. |
| `q` / `Ctrl-C` | Quit — if anything was received this session, first prompt `clear this session? [t]ransient / [a]ll incl sinks / [n]o`. |

**Stubs (Phase 0):** inline image previews are placeholders (`🖼 <hash>.png W×H · size`), not a
Kitty/iTerm2/Sixel thumbnail; a file bubble shows `📎 name · size`; the composer is text-only (no image
paste); and history starts empty (only items received while the TUI is open are shown).

## Troubleshooting

- **Peers never connect** → mDNS multicast may be blocked on the LAN, or the two hosts aren't on the
  same L2 segment. `lan` has no ticket/relay fallback (that's the experiment's point) — for a filtered
  LAN or cross-network hand-off use `clip` (ticket) or `room` (server).
- **Nothing to paste/recv** → exit code `5`. **No daemon** → exit code `3`.
- Headless hosts: the clipboard degrades gracefully (`status` shows `clipboard: unavailable`);
  `send`/`recv`/`save_dir` still work.
