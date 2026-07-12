# room-go — Phase 0 (the central-room / SSH bet)

`room-go` is one of the two headliners in the cross-platform clipboard hand-off
bake-off. It implements the shared contract in [`../docs/SPEC.md`](../docs/SPEC.md)
and [`../docs/PROTOCOL.md`](../docs/PROTOCOL.md), using a **central room server
spoken over SSH** (charmbracelet/wish + charmbracelet/ssh, public-key auth) with
**native client daemons** that own each device's OS clipboard.

Binary: **`room`**. Module: `github.com/daviddwlee84/cross-platform-copy/room-go`.
Go 1.26, cgo required for the clipboard backend.

## What Phase 0 delivers

- **Real SSH/wish server** (`room server`) — not a TCP fallback. A device is
  authorized by its SSH public key (PROTOCOL §3: the key fingerprint *is* the
  identity). Each connection joins a room (the SSH exec command = the room name)
  and the server relays every framed CBOR envelope to the room's other members
  via an in-memory broker fan-out adapted from the user's `sshbbs`
  `internal/chat/broker.go` (same map-of-sessions shape, same self-send guard —
  the originator never receives its own message).
- **Native client daemon** (`room daemon`) — holds the persistent SSH
  connection, owns the OS clipboard (golang.design/x/clipboard, PNG-native),
  keeps a ring buffer of received items, and serves thin clients over a local
  Unix-socket IPC (PROTOCOL §4: length-prefixed CBOR, `Subscribe` event stream).
  Auto-spawned by the first client command.
- **Thin clients**: `send` · `recv` · `paste` · `status` · `config` · `join` ·
  `tui` (a messenger-style chat, see below).
- **Auto-copy** default `notify` (received items are surfaced but the clipboard
  is *not* written until `paste`); `on` writes immediately; `off` never touches
  it. Echo/loop suppression: dedupe by `msg_id`, remember the last content-hash
  we wrote, and never re-broadcast a received item.
- **Images are PNG on the wire** with a **BLAKE3** integrity hash verified on
  receipt. Text rides inline.
- **Arbitrary files on the wire** (`type=file`) — any bytes, with a `filename`
  and best-effort MIME (guessed from the extension, else
  `application/octet-stream`). Content-addressed and BLAKE3-verified like an
  image, but **never** clipboard-pasteable as an image; `paste` of a file copies
  its path as clipboard *text*.
- **Additive sinks** (SPEC §3) — a received item can fan out to the clipboard
  **and** a folder/append-file, chosen by type (see below).
- **Sessions & clearing** (SPEC §8) — `clear` / `daemon stop` / TUI-quit can
  revert what a session left behind (see below).

### Sending a file

```sh
# from a PATH (the basename becomes the filename), forced to a file item:
$BIN "${A[@]}" send report.pdf --file
# or from stdin with an explicit name:
cat archive.tgz | $BIN "${A[@]}" send --file --name archive.tgz
```

Type selection follows PROTOCOL §2: `--auto` (default) sniffs — PNG/JPEG magic →
image, valid UTF-8 → text, otherwise → **file**. `--file` forces a file even for
UTF-8 input; `--image`/`--text` force those. A `PATH` argument (or `--name`)
supplies the `filename`.

On the receiver, `recv --emit-path` writes an image/file to a temp path and
prints it; `paste` puts the latest item on the clipboard (a file's *path* as
text).

### Sinks — where received items land (`save_dir` / `text_file`)

Independently of the clipboard/`auto_copy` setting, a received item is routed by
type:

- **text** → clipboard (per `auto_copy`) **and**, if `text_file` is set, appended
  to that file, each entry preceded by a `\n---\n<device> <ISO8601 ts>\n` header.
- **image** → clipboard (per `auto_copy`) **and**, if `save_dir` is set, written
  there as `<filename>` (or `<hash>.png`), de-duplicated `name (2).ext` on
  collision.
- **file** → if `save_dir` is set, written there as `<filename>` (or `<hash>`),
  de-duplicated; **never** the clipboard.

```sh
$BIN "${B[@]}" config set save_dir  ~/Drop     # folder sink (image/file)
$BIN "${B[@]}" config set text_file ~/room.log # append sink (text)
```

### Sessions & clearing (privacy, SPEC §8)

A **session** is one daemon run. It tracks, so a clear is precise and never
destructive beyond the session:

- the **transient** store — the fetched-blob cache, `recv --emit-path` temp
  files, and the in-memory received-items buffer (always safe to clear);
- the **`text_file` size at session start** (the truncation offset);
- the **list of files written into `save_dir`** this session.

```sh
$BIN "${B[@]}" clear              # transient only (buffer + blob cache)
$BIN "${B[@]}" clear --all        # + revert session sinks (prompts unless --yes)
$BIN "${B[@]}" clear --all --yes  # non-interactive
```

`clear --all` truncates `text_file` back to its session-start offset (removing
only what this session appended) and deletes the files this session wrote into
`save_dir` — pre-session content is untouched.

`room daemon stop` shuts the daemon down applying `clear_on_exit`
(`never` / `transient` / `all` / `ask`); `ask` prompts on a TTY, else falls back
to `transient`. A SIGTERM applies the same policy non-interactively. In
`room tui`, quitting (`q`/`Ctrl-C`) after receiving anything shows a small prompt
— *clear this session? [t]ransient / [a]ll incl sinks / [n]o* — and runs the same
clear before exiting.

### Phase 0 simplifications (documented, per the task brief)

- **Image bytes ride inline** in the envelope (`blob_data`) and are relayed
  through the server. PNG is still the canonical wire format and the BLAKE3 hash
  is still verified. The PROTOCOL §1 "announce + pull by hash" optimization is
  Phase 1.
- **Server trusts any key by default** (`--authorized-keys` restricts it). The
  SSH-key-as-identity model is fully in place — every client presents a key and
  its fingerprint is captured — but Phase 0 skips the manual allowlist step so
  the automated harness runs without an out-of-band key exchange. Restrict with
  `room server --authorized-keys FILE`.
- **Host key is trusted on connect** (`InsecureIgnoreHostKey`) — fine for
  loopback/LAN Phase 0; host-key pinning is Phase 1.
- **Daemon allowlist/TOFU** for *senders* is trust-all in Phase 0 (allowlist +
  approval prompt is Phase 1). `auto_copy` mode still governs the clipboard.
- The **bare `ssh room@server` bubbletea TUI tier + OSC 52** is Phase 1–2, not
  built here. Phase 0 is the native-client hand-off only.

## Build

```sh
cd room-go
go build -o bin/room ./cmd/room     # same command the harness uses
go test ./...                        # unit tests (add -race to stress the broker)
go vet ./...
```

## Run — the exact Phase 0 flow

Three moving parts: one server, two native daemons (each with its own
`--config-dir`/`--socket`), both joined to the same room.

```sh
BIN=./bin/room
PORT=2299
A=(--config-dir /tmp/a --socket /tmp/a.sock --room demo)
B=(--config-dir /tmp/b --socket /tmp/b.sock --room demo)

# 1) start the SSH room server (host key auto-generated if missing)
$BIN server --addr :$PORT --host-key /tmp/host_key &

# 2) configure + connect each device. `join` generates the client's SSH key on
#    first use, prints its fingerprint (to allowlist server-side if you enable
#    --authorized-keys), auto-spawns the daemon, and connects it to the room.
$BIN "${A[@]}" join room@localhost:$PORT
$BIN "${B[@]}" join room@localhost:$PORT

# 3) text hand-off: A -> B
printf 'hi' | $BIN "${A[@]}" send --text
$BIN "${B[@]}" recv                       # prints: hi

# 4) image hand-off: A -> B, hash-equal
$BIN "${A[@]}" send --image < ../testdata/small.png
$BIN "${B[@]}" recv --latest-image --emit-path   # prints a PNG path; hash == source
$BIN "${B[@]}" paste                      # (auto_copy notify) put it on the clipboard
```

`join` auto-spawns the daemon, so the explicit `daemon` step is optional. To run
a daemon in the foreground for debugging (and set the server in one shot):

```sh
$BIN "${A[@]}" daemon --foreground --server room@localhost:$PORT
```

### Command surface (SPEC §2)

| Command | Notes |
|---|---|
| `room server [--addr :2222] [--host-key PATH] [--authorized-keys FILE]` | run the SSH room server |
| `room daemon [--foreground] [--server user@host:port]` | run the client daemon (usually auto-spawned) |
| `room join <user@host:port>` | generate/print the client key fingerprint, connect the daemon to a server+room |
| `room remote <ssh-host> [--rport N] [--lport N] [--stop]` | one-command SSH-tunnel connect: install+start a server on the host, tunnel to it, join (see below) |
| `room send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | read PATH or stdin, sniff type, broadcast |
| `room recv [--follow] [--latest-image --emit-path] [--out PATH]` | text→stdout, image/file→file path |
| `room tui` | messenger-style chat attached to the local daemon (SPEC §4) |
| `room paste` | write the latest received item to the OS clipboard (a file's path as text) |
| `room clear [--all] [--yes]` | clear this session's transient store; `--all` also reverts session sinks (§8) |
| `room daemon stop` | shut the daemon down, applying `clear_on_exit` (§8) |
| `room status [--json]` | identity / room / server / connected / auto_copy / buffer |
| `room config set KEY VALUE` · `room config get KEY` | `auto_copy`, `save_dir`, `text_file`, `clear_on_exit`, `device_name`, `room`, `server`, … (SPEC §5) |

Global flags (before the subcommand): `--config-dir PATH`, `--socket PATH`,
`--room NAME`, `--json`, `-q/--quiet`, `-v/--verbose`. Exit codes follow SPEC §2
(`0` ok · `2` usage · `3` no daemon · `4` reserved · `5` nothing to paste/recv).
`send` with no connected peers exits `0` with a stderr warning (item still buffered).

## Remote (SSH tunnel) — one-command connect

`room remote <ssh-host>` is a VSCode-Remote-SSH-style shortcut: point it at a
host you already have in `~/.ssh/config` (key auth) and it stands up the whole
room-over-SSH path for you — no manual server/scp/tunnel steps.

```sh
# host is any ~/.ssh/config alias (keys/agent/ProxyJump all Just Work)
room --room demo remote my-server
# → connected to my-server via SSH tunnel (room "demo") — send/recv/tui now reach it

# now the normal thin clients reach the remote room through the tunnel:
printf hi | room --room demo send --text
room --room demo tui

room remote my-server --stop         # tear it down
```

It shells out to the local `ssh`/`scp` binaries (never reimplementing SSH, so
your config, keys, and agent apply) and does, idempotently:

1. **Detect** the remote arch — `ssh <host> uname -sm` → GOOS/GOARCH
   (Linux `x86_64`→`linux/amd64`, `aarch64`→`arm64`, Darwin→`darwin`).
2. **Ensure `~/.local/bin/room`** on the host. If missing it **bootstraps**:
   cross-builds `room` for the remote GOOS/GOARCH with `CGO_ENABLED=0` using the
   local `go` toolchain (the module is located by walking up from the CWD for
   `go.mod`). If there's no module *and* the local host already matches the
   remote arch, it falls back to `scp`-ing the running executable
   (`os.Executable()`); otherwise it fails with an actionable message ("run from
   inside the room-go repo"). Then `mkdir -p ~/.local/bin`, `scp`, `chmod +x`.
3. **Start a room server on the remote, bound to loopback** — reused if already
   listening on `--rport` (default `2299`), else started detached
   (`setsid nohup ~/.local/bin/room server --addr 127.0.0.1:<rport>
   --host-key ~/.config/room/host_key >/tmp/room-remote-server.log 2>&1 &`).
4. **Open the tunnel** — a backgrounded `ssh -N -L <lport>:127.0.0.1:<rport>
   <host>` (default lport `2299`, auto-bumped if the local port is busy). Its
   pid is tracked in a per-host state file under `<config-dir>/remote/`.
5. **Connect locally** — ensures the local client daemon and joins
   `room@127.0.0.1:<lport>` (room from `--room`, default `default`).

`room remote <host> --stop` kills the tunnel, stops the remote server if this
command started it, and removes the state file. Re-running `room remote` reuses
an installed binary, a running server, and a live tunnel, so it is safe to run
repeatedly.

**Flags:** `--rport N` (remote server loopback port, default 2299) ·
`--lport N` (local tunnel port, default 2299) · `--stop` (tear down). The room
name comes from the global `--room` flag.

**Phase 0 scope:** the daemon still trusts the server host key on connect
(`InsecureIgnoreHostKey`, fine for a loopback tunnel); host-key pinning is
Phase 1. `--stop` leaves the installed `~/.local/bin/room` in place by design
(that's the point — the next connect is instant).

## The messenger TUI (`room tui`)

`room tui` is a full-screen chat front-end (charmbracelet **bubbletea +
lipgloss + bubbles**) attached to the local daemon. It auto-spawns/connects the
daemon if needed, then **Subscribes** to the daemon's IPC event stream (the same
stream `recv --follow` uses) and renders incoming items live. It is a
front-end, not a second clipboard owner: the `y` copy action asks the **daemon**
(the clipboard owner) to place the item on the OS clipboard, honoring the
`auto_copy` mode (notify-first).

```sh
# after `join` (or with a server configured), on either device:
$BIN "${A[@]}" tui
```

Layout (top → bottom): a **header** (`room · 🔑fingerprint · connected/server ·
auto_copy · clipboard: available|unavailable`), a scrollable **message history**
of chat bubbles (`sender · relative-time · body`, your own messages
right-aligned in green), a status/toast line, a **composer** (textarea), and a
keybinding **hint** line.

### Keybindings

Two modes; `Esc` toggles between them.

| Mode | Key | Action |
|---|---|---|
| compose | type + `Enter` | send the line as a text item (appears as your own bubble) |
| compose | `Esc` | switch to browse mode |
| browse | `↑`/`k`, `↓`/`j` | move the message selection (highlighted bubble) |
| browse | `g` / `G` | jump to oldest / newest |
| browse | `y` | copy the highlighted (or latest) item to the OS clipboard **via the daemon** |
| browse | `s` | save the selected image to `~/Downloads` (or cwd) |
| browse | `o` | open the selected image externally (`open`/`xdg-open`) |
| browse | `i` / `Enter` | return to the composer |
| any | `Ctrl-C` | quit (prompts to clear the session if anything was received) |
| browse | `q` | quit (same clear prompt) |

In `auto_copy notify` mode (the default), received items are **not** written to
the clipboard automatically — each bubble shows a `press y to copy` nudge and a
toast surfaces the incoming item, matching SPEC §3/§4.

### Images

Image items always render a bubble with metadata (`🖼 name  W×H · size`) plus the
`y`/`s`/`o` affordances. **Inline terminal-graphics rendering (Kitty/iTerm2/
Sixel) is a documented stub** in Phase 0 — the bubble shows a clear placeholder
line rather than pixels; copy/save/open all work on the full-resolution PNG the
daemon materialized. Adding an inline preview later is a drop-in change to
`renderImageBody` (e.g. via `rasterm`) since the local PNG path is already on
each image bubble.

### Verifying the TUI (three layers)

The TUI is covered at three levels of fidelity, so most of it is exercised in
plain `go test` with no TTY:

1. **Model `Update` unit tests** (`internal/tui/tui_test.go`) — assert on the
   values `Update` returns: an incoming item appends a bubble; submitting the
   composer yields a daemon send; `y` copies by `msg_id`; the quit-clear prompt
   appears only after something was received.
2. **Rendered-frame tests** (`internal/tui/render_test.go`, `charmbracelet/x/exp/teatest`)
   — run the model through a simulated 80×24 terminal and assert on the *bytes
   it renders*: the header (`room:… · 🔑fp · ● server · auto_copy:…`), a received
   chat bubble (sender + text + the `press y to copy` nudge), the composed own
   bubble on Enter, and the clear-on-quit prompt. These use
   `teatest.WaitFor` + substring checks (not a byte-exact golden, which is flaky
   across terminfo/ANSI); the header's `clipboard:…` field is checked at a wider
   width where lipgloss doesn't truncate the tail. They run in the default
   `go test ./...`.
3. **PTY end-to-end smoke** (`internal/tui/e2e_test.go`, `creack/pty`, **gated**
   behind the `e2e` build tag) — builds the binary, stands up a `room server` on
   an isolated loopback port with a client daemon `join`ed to it, launches the
   built `room tui` under a real pseudo-terminal, reads the header off the PTY,
   sends `Esc` then `q`, and asserts a clean exit. It does **not** run in the
   default suite:

   ```sh
   go test ./...                       # layers 1 + 2 (fast, no TTY)
   go test -tags e2e ./internal/tui/   # layer 3 (builds the binary + live server/daemon)
   ```

To see it for real: start the server, `join` two daemons to the same room (as
above), run `room tui` on each, and type — messages appear live on the other
side; press `y` on a received bubble to copy it.

## Wiring the bake-off harness (`scripts/roundtrip.sh`)

The harness already knows how to build/run `room-go` (`bin/room`, `go build -o
bin/room ./cmd/room`). Its `start_pair()` has a `room-go` branch left as a TODO.
**Do not edit the harness for me** — the room-go hook is simply: after the two
`daemon --foreground` processes are up, join both to a server. Concretely, the
`room-go)` case in `start_pair()` should become:

```sh
room-go)
  # start the server once (reuse $ROOM as the room name)
  "$BIN" server --addr :2299 --host-key "$RUN/host_key" >"$RUN/server.log" 2>&1 & PIDS+=($!)
  sleep 1
  "$BIN" "${A[@]}" join room@localhost:2299 >/dev/null
  "$BIN" "${B[@]}" join room@localhost:2299 >/dev/null
  ;;
```

Everything else in the harness (the `send --text` / `recv --follow` text check
and the `send --image` / `recv --latest-image --emit-path` hash check) works
against `room` unchanged. The harness sets `config set auto_copy off` for a
deterministic buffer; `room` honors it.

## Layout

```
cmd/room/            CLI: global-flag parsing + subcommand dispatch
internal/wire/       Envelope, CBOR codec, length-prefixed framing, type-sniff, BLAKE3   (+ tests)
internal/broker/     server-side per-room fan-out (adapted from sshbbs broker)           (+ tests)
internal/server/     charmbracelet/wish SSH server + pubkey auth + relay handler
internal/daemon/     resident agent: SSH conn, ring buffer, IPC, auto-copy, dedupe, sinks + session/clear (§3/§8)   (+ tests)
internal/ipc/        client<->daemon request/response types, framing, dial + auto-spawn
internal/tui/        messenger-style chat (`room tui`): bubbletea model, IPC client, Subscribe stream  (+ test)
internal/config/     config dir, config.json (SPEC §5), SSH identity key
internal/clip/       golang.design/x/clipboard wrapper (lazy init; text + PNG)
```

## Tests

- `internal/wire`: envelope CBOR round-trip (text + image + **file**, incl.
  inline blob), unknown-field tolerance (wire back-compat), type-sniff
  (PNG/JPEG/UTF-8/invalid) and `Classify`/`MimeForFilename` (the `--auto`
  file/text/image decision + MIME guess), PNG passthrough preserves bytes,
  JPEG→PNG transcode, frame round-trip.
- `internal/broker`: fan-out excludes the sender, room isolation, unregister
  cleanup, and a `-race` concurrency stress.
- `internal/daemon`: `msg_id` dedupe / echo-suppression + bounded eviction;
  **sinks** (text append with header, image/file to `save_dir` with dedup);
  **`clear --all`** truncates `text_file` to the session-start offset and removes
  only this-session `save_dir` files (pre-session content untouched); `clipContent`
  type routing (a file copies its path as text).
- `internal/tui`: three layers (see *Verifying the TUI* above). **Model
  `Update`** (`tui_test.go`) — an incoming-item message appends a bubble;
  submitting the composer yields a daemon send; `y` copies by `msg_id`; the
  **quit-clear prompt** appears only after something was received and its
  `t`/`a`/`n` answers drive the daemon clear. **Rendered frames**
  (`render_test.go`, `teatest`, default `go test`) — the header, a received
  bubble, an own bubble, and the clear prompt actually render. **PTY E2E**
  (`e2e_test.go`, `-tags e2e`) — the built `room tui` launched under a real
  pseudo-terminal against a live server + daemon renders its header and quits
  cleanly.

## End-to-end (sinks + clear)

`scripts/e2e_sinks.sh` drives the full flow through a live server: two daemons,
`save_dir` + `text_file` on the receiver, sends a text + an image + a small
arbitrary binary, asserts the folder + append-file contents and hash-equality,
then `clear --all --yes` and asserts the session sink writes reverted while
pre-session content is intact, and finally `daemon stop`.

```sh
bash scripts/e2e_sinks.sh
```
