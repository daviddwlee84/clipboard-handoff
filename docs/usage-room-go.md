# Usage — `room` (room-go, Go/wish central server)

The **one-small-server** implementation. Devices connect to a central **SSH room server**
(charmbracelet/wish, public-key auth) and the server relays clipboard items between everyone in the
same room. The trade for running a server: a natural path to retained history and a zero-install SSH
client tier. The server sees plaintext (no client-side E2E in Phase 0).

- Binary: **`room`** · transport: **SSH/wish relay** (SSH public-key fingerprint = identity) ·
  impl: [`../room-go/`](https://github.com/daviddwlee84/clipboard-handoff/tree/main/room-go/)
- Shared contract: [SPEC](SPEC.md) (CLI §2, sinks §3, config §5, sessions §8) · wire/IPC: [PROTOCOL](PROTOCOL.md)
- For deeper server/harness notes see [`../room-go/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/room-go/README.md).

## Install / build

```sh
cd room-go
go build -o bin/room ./cmd/room          # binary -> room-go/bin/room
```

Go 1.26; cgo is required for the clipboard backend (golang.design/x/clipboard). Put `bin/room` on your
`PATH` so the examples can call it as `room`.

## Identity model (SSH keys)

There is no ticket. A device is identified by its **SSH public-key fingerprint** (PROTOCOL §3): the
key *is* the identity. On first `join`, the client generates its key, prints the fingerprint + an
`authorized_keys` line, auto-spawns the daemon, and connects. The server authorizes by key — trust-all
in Phase 0, or restricted with `room server --authorized-keys FILE`. The room name is the SSH exec
command; only members of the same room see each other's items.

## Running a server

```sh
room server --addr :2299 --host-key /tmp/host_key
# --addr defaults to :2222; --host-key auto-generates if the path is missing/empty
# --authorized-keys FILE  -> restrict to listed keys (default: trust any key, Phase 0)
```

The server needs **no display** (it never touches a clipboard) and is the only piece that must be
reachable by every device. Run it once on a box both devices can dial.

## Quick start (server + two devices)

```sh
BIN=./bin/room
PORT=2299
A=(--config-dir /tmp/a --socket /tmp/a.sock --room demo)
B=(--config-dir /tmp/b --socket /tmp/b.sock --room demo)

# 1) server
$BIN server --addr :$PORT --host-key /tmp/host_key &

# 2) connect each device — `join` prints its key fingerprint, auto-spawns the daemon, connects
$BIN "${A[@]}" join room@localhost:$PORT
$BIN "${B[@]}" join room@localhost:$PORT

# 3) text hand-off A -> B
printf 'hi' | $BIN "${A[@]}" send --text
$BIN "${B[@]}" recv                       # -> hi

# 4) image hand-off A -> B (hash-equal)
$BIN "${A[@]}" send --image < ../testdata/small.png
$BIN "${B[@]}" recv --latest-image --emit-path   # prints a PNG path; hash == source
$BIN "${B[@]}" paste                      # (auto_copy notify) place it on B's clipboard
```

`join` auto-spawns the daemon, so an explicit `daemon` step is optional. Across real machines, point
`join` at the server's LAN IP instead of `localhost` (e.g. `room join room@192.168.1.20:2299`).

> **One-command SSH tunnel — `room remote <host>`.** A sibling change adds a `room remote <host>`
> convenience that connects through an SSH tunnel in one step. It is not documented here to avoid
> going stale — see [`../room-go/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/room-go/README.md) for its current flags and behavior.

## Command reference

Global flags (before the subcommand): `--config-dir PATH`, `--socket PATH`, `--room NAME`
(default `default`), `--json`, `-q/--quiet`, `-v/--verbose`.

| Command | What it does |
|---|---|
| `room server [--addr :2222] [--host-key PATH] [--authorized-keys FILE]` | Run the SSH room server. |
| `room join <user@host:port>` | Print the client key fingerprint, connect the daemon to a server + room. |
| `room send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | Read `PATH` (or **stdin** to EOF), sniff type, broadcast. Exits `0` even with no peers (warning on stderr). |
| `room recv [--follow] [--latest-image --emit-path] [--out PATH]` | Text → stdout; image/file → materialized file, path printed. `--follow` streams; `--latest-image` filters to images. |
| `room paste` | Write the latest received item to the OS clipboard (text→set, image→PNG, **file → its path as text**). |
| `room tui` | Messenger-style chat attached to the local daemon (see below). |
| `room status [--json]` | identity / device / room / server / connected / `auto_copy` / `clipboard` / buffer. |
| `room config set KEY VALUE` · `room config get KEY` | Persist / read settings (see Config). |
| `room clear [--all] [--yes]` | Clear this session's data (see Sessions). |
| `room daemon [--foreground] [--server user@host:port]` | Run the client daemon (usually auto-spawned). `--server` sets + connects in one shot. |
| `room daemon stop` | Shut the daemon down, applying `clear_on_exit`. |

> **Note on `room peers`:** in Phase 0 `peers` is aliased to `status` — it prints the status block
> (which includes a peer count), not a standalone peer table.

**Sending a file** — `image` is the only clipboard-pasteable binary type; `file` is any bytes with a
best-effort MIME and a `filename`:

```sh
room "${A[@]}" send report.pdf --file                 # force a file item, filename=report.pdf
cat archive.tgz | room "${A[@]}" send --file --name archive.tgz
```

Sniffing follows [PROTOCOL §2](PROTOCOL.md): `--auto` (default) → PNG/JPEG magic = image, valid UTF-8 =
text, else file. In Phase 0 image/file bytes ride **inline** in the envelope and are relayed through the
server (announce+pull is Phase 1); PNG stays canonical and `blob.hash` (BLAKE3) is verified on receipt.

## Sinks — where received items go (SPEC §3)

Additive; chosen by type, independent of `auto_copy`:

| Sink | Config key | Receives |
|---|---|---|
| clipboard | `auto_copy` | text, image (a `file` never auto-lands on the clipboard) |
| folder | `save_dir` | image → `<filename\|hash>.png`, file → `<filename\|hash>` — de-duplicated `name (2).ext` |
| append-file | `text_file` | text, appended after a `\n---\n<device> <ISO8601 ts>\n` header |

```sh
room "${B[@]}" config set save_dir  ~/Drop      # folder sink (image/file)
room "${B[@]}" config set text_file ~/room.log  # append sink (text)
```

**`auto_copy` modes** (`room config set auto_copy notify|on|off`): `notify` (default) buffers + toasts
without touching the clipboard (use `room paste` or `y` in the TUI); `on` writes every item straight to
the clipboard; `off` never touches it.

## Config keys (SPEC §5, plus `server`)

`auto_copy` (`notify`|`on`|`off`, default `notify`) · `save_dir` (path|"") · `text_file` (path|"") ·
`clear_on_exit` (`ask`|`transient`|`all`|`never`, default `ask`) · `device_name` (default hostname) ·
`room` (default `default`) · `internet` (Phase 3) · `broadcast_on_copy` (Phase 3) ·
**`server`** (`user@host:port`, set by `join`) — room-go's addition so the daemon reconnects to the
right server.

Config dir: macOS `~/Library/Application Support/room/`, Linux `$XDG_CONFIG_HOME/room/`,
Windows `%APPDATA%\room\`. Override with `--config-dir`.

## Sessions & clearing (SPEC §8)

A session is one daemon run. It tracks the transient store (fetched-blob cache + `recv --emit-path`
temp files + in-memory buffer), the `text_file` size at session start, and the `save_dir` files it
wrote — so a clear is precise and never touches pre-session content.

```sh
room "${B[@]}" clear              # transient only
room "${B[@]}" clear --all        # + revert this session's sink writes (prompts unless --yes)
room "${B[@]}" clear --all --yes  # non-interactive
room "${B[@]}" daemon stop        # applies clear_on_exit; SIGTERM does the same
```

`clear_on_exit`: `never` · `transient` · `all` · `ask` (prompt on a TTY, else `transient`). The TUI
raises the same choice on quit.

## TUI (`room tui`)

A full-screen chat (charmbracelet bubbletea + lipgloss + bubbles) attached to the local daemon. It
auto-spawns/connects the daemon, subscribes to its event stream, and renders items live. Front-end
only — `y` asks the daemon to copy, honoring `auto_copy` (notify-first).

```sh
room "${A[@]}" tui        # after `join`, or with a server configured
```

Header: room · 🔑fingerprint · connected/server · `auto_copy` · `clipboard: available|unavailable`.
Two modes; `Esc` toggles between them.

| Mode | Key | Action |
|---|---|---|
| compose | type + `Enter` | Send the line as a text item (appears as your own bubble). |
| compose | `Esc` | Switch to browse mode. |
| browse | `↑`/`k`, `↓`/`j` | Move the message selection. |
| browse | `g` / `G` | Jump to oldest / newest. |
| browse | `y` | Copy the highlighted (or latest) item to the OS clipboard **via the daemon**. |
| browse | `s` | Save the selected image to `~/Downloads` (else cwd). |
| browse | `o` | Open the selected image externally (`open`/`xdg-open`). |
| browse | `i` / `Enter` | Return to the composer. |
| browse | `q` | Quit (prompts to clear the session if anything was received). |
| any | `Ctrl-C` | Quit (same clear prompt). |

**Images:** each image bubble shows metadata (`🖼 name  W×H · size`) plus `y`/`s`/`o` on the
full-resolution PNG the daemon materialized. Inline terminal-graphics rendering (Kitty/iTerm2/Sixel)
is a **documented stub** in Phase 0 — a placeholder line, not pixels (unlike `clip`, which renders
inline thumbnails).

## Cross-machine notes

The server is display-less by design and just needs to be reachable. Deploy `bin/room` to each client
(`CGO_ENABLED=0` cross-compile + scp works for the daemons), then `join` each at the server's address.
On a display-less client the clipboard degrades gracefully (`status` shows `clipboard: unavailable`);
`send`/`recv`/`save_dir` still work.

## Troubleshooting

- **`join` fails** → confirm the server is reachable (`--addr` host:port) and, if you set
  `--authorized-keys`, that this device's fingerprint (printed by `join`) is in that file.
- **Nothing to paste/recv** → exit code `5`; nothing received yet.
- **No daemon** → exit code `3`; a client couldn't reach/spawn the daemon.
- **Phase 0 trust caveats:** the server trusts any key and the host key is trusted on connect
  (`InsecureIgnoreHostKey`); sender allowlist/TOFU is Phase 1. Fine for loopback/LAN.
