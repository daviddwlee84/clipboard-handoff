# experiments/ — transport-probe track

Small, LAN-first, MVP-depth Go probes for the cross-platform clipboard hand-off
bake-off. Each probe implements the **same product contract** as the headliners
(`docs/SPEC.md` CLI surface, `docs/PROTOCOL.md` wire envelope + local IPC), so
the bake-off harness (`scripts/roundtrip.sh`) can drive any of them identically.
Their job is to **quantify transport trade-offs**, not to be production apps.

```
experiments/
├─ go.work            # ties the probes to the local shared-go helper
├─ shared-go/         # reusable plumbing (everything above the wire)
├─ lan-go/            # transport = quic-go direct connections + mDNS (grandcat/zeroconf)
└─ libp2p-mesh/       # transport = go-libp2p gossipsub + libp2p mDNS
```

## The design: one daemon, pluggable transport

`shared-go` owns everything that is **not** transport-specific, so a probe only
writes a transport:

| Package | Responsibility |
|---|---|
| `shared-go/wire` | Envelope + CBOR codec + length-prefixed framing + type-sniff (PNG/JPEG/UTF-8, JPEG→PNG, else `file`) + best-effort file MIME + BLAKE3 (PROTOCOL §1–2) |
| `shared-go/ipc` | Local Unix-socket IPC: length-prefixed CBOR request/response + event stream, daemon auto-spawn (PROTOCOL §4) |
| `shared-go/config` | Config dir + `config.json` settings (SPEC §5: `auto_copy`, `save_dir`, `text_file`, `clear_on_exit`, …); hands each transport a path for its identity key |
| `shared-go/clip` | OS clipboard via `golang.design/x/clipboard` (text + PNG) |
| `shared-go/daemon` | Ring buffer, `msg_id` dedupe + last-written-hash echo suppression, auto-copy (`notify`/`on`/`off`), the additive folder/append-file **sinks** + per-**session** tracking & clearing (SPEC §3, §8), IPC handlers |
| `shared-go/cli` | The whole CLI surface (SPEC §2), parameterized by an `App{BinName, NewTransport}` |
| `shared-go/transport` | The pluggable contract: `Identity()`, `Start(ctx)`, `Broadcast(env)`, `OnReceive(fn)`, `Peers()`, `Close()` |

A probe's `main` is ~15 lines: it supplies `cli.App{BinName, NewTransport}` and
the transport does the rest. This is the reuse that lets us add probes cheaply.

> Note: the experiments track deliberately shares `shared-go`. The two
> **headliners** (`mesh-rs`, `room-go`) stay fully independent — they do NOT
> import this module (and this module does not import theirs).

## Build

```sh
cd experiments/lan-go       && go build -o bin/lan ./cmd/lan
cd experiments/libp2p-mesh  && go build -o bin/libp2p-mesh ./cmd/libp2p-mesh
```

`go.work` + per-module `replace` directives both point the probes at
`../shared-go`, so builds work with or without the workspace (`GOWORK=off`).

## Run the bake-off round-trip

From the repo root:

```sh
scripts/roundtrip.sh lan-go
scripts/roundtrip.sh libp2p-mesh
```

Each spins up two daemons (distinct `--config-dir`/`--socket`, same `--room`),
lets them auto-discover over mDNS, then verifies `send --text` A→B and
`send --image` A→B (BLAKE3 hash-equal). Discovery is automatic, so the harness's
default `start_pair()` hook works unchanged — see each probe README.

## Sending files, sinks & clearing (SPEC §2, §3, §8)

These are shared by both probes (they live in `shared-go`), so `lan` and
`libp2p-mesh` behave identically.

**Send anything.** `send` reads a `PATH` (or **stdin** to EOF) and sniffs the
type: PNG/JPEG magic → **image**; valid UTF-8 → **text**; otherwise → **file**
(arbitrary bytes, best-effort MIME from the extension, else
`application/octet-stream`). Force with `--text` / `--image` / `--file`; a
file/image carries its `filename` from the `PATH` basename or `--name`.

```sh
lan send report.pdf                 # → file (filename report.pdf)
lan send --file blob.bin --name x   # force file, override the filename
cat notes.txt | lan send            # stdin → text (valid UTF-8)
lan send shot.png                   # → image (PNG/JPEG only; JPEG transcoded to PNG)
```

Only an **image** is clipboard-pasteable as an image; a **file** has no
clipboard image form — `paste`/`recv --emit-path` give you its path, and `paste`
copies that **path as clipboard text**. Blobs ride inline (`blob_data`, Phase 0)
and their BLAKE3 `blob.hash` is verified on receipt.

**Additive sinks.** A received item can fan out to more than the clipboard,
chosen by type (all subject to the same echo/loop suppression):

```sh
lan config set save_dir  ~/Drop     # received image/file items are written here
lan config set text_file ~/clip.md  # received text is appended here
```

- text → clipboard (per `auto_copy`) **and**, if `text_file` set, appended after
  a `\n---\n<device> <ISO8601 ts>\n` header.
- image → clipboard (per `auto_copy`) **and**, if `save_dir` set, written as
  `<filename or hash>.png` (de-duplicated `name (2).png`).
- file → if `save_dir` set, written as `<filename or hash>` (de-duplicated);
  never the clipboard.

**Sessions & clearing.** The daemon tracks, per session, the transient store
(blob cache + `recv --emit-path` temp files + in-memory buffer), the `text_file`
size at session start, and the `save_dir` files it wrote — so a clear is precise
and never destructive beyond the session:

```sh
lan clear                # transient only (blob cache, emit temp files, buffer)
lan clear --all          # + revert this session's sinks (prompts unless --yes)
lan clear --all --yes    # truncate text_file to session-start; delete this
                         # session's save_dir files; pre-session content untouched
lan daemon stop          # shut the daemon down, applying clear_on_exit
lan config set clear_on_exit ask|transient|all|never   # default: ask
```

`clear_on_exit` runs on `daemon stop` / SIGTERM: `never` keeps everything,
`transient` purges transient, `all` also reverts session sinks, `ask` prompts on
a TTY (via `daemon stop`) else falls back to `transient`. The **TUI** raises the
same decision on quit (below).



A lean, messenger-style chat attached to the local daemon (SPEC §4). It lives in
`shared-go/cli` (bubbletea + lipgloss + bubbles), so **both probes get the same
TUI**: `lan tui` and `libp2p-mesh tui`. It is a thin client — it reuses the
daemon's live event stream (the same `Subscribe` op `recv --follow` uses) and
the `Send`/`Copy`/`Paste`/`Status` IPC ops; it never touches the transport,
ring buffer or clipboard directly (the daemon owns those).

```sh
cd experiments/lan-go && go build -o bin/lan ./cmd/lan
bin/lan --room default tui          # auto-spawns the daemon if it is down
# another device in the same room:  bin/lan --room default tui
```

Layout: header (`room · id · peers · auto_copy · clipboard: available|unavailable`)
· scrollable bubble history (`sender · relative-time · body`, your own messages
marked `(you)`) · composer (textarea) · keybinding hint line.

| Key | Action |
|---|---|
| type + `Enter` | send the line as a text item (own bubble appears immediately) |
| `Esc` | toggle focus between composer and browse mode |
| `↑`/`k`, `↓`/`j` | move the selection in browse mode (`g`/`G` = top/bottom) |
| `y` | copy the highlighted bubble to the OS clipboard **via the daemon** (a file copies its path as text) |
| `s` | save a highlighted image to `~/Downloads` (else temp dir) |
| `o` | open a highlighted image/file with the OS default app |
| `p` | paste the daemon's latest received item to the clipboard |
| `q` / `Ctrl-C` | quit — if anything was received this session, first prompt *clear this session? `[t]`ransient / `[a]`ll incl sinks / `[n]`o* and run it before exit (SPEC §8) |

Incoming items appear live. `auto_copy` mode is shown in the header and honored
by the daemon (the TUI is front-end only). File bubbles render a `📎 name · size`
placeholder (files land in `save_dir`; no clipboard image). **Stubbed:** inline
image previews — image bubbles show a metadata placeholder (`🖼 <hash>.png W×H ·
size`) plus copy/save/open actions, not a Kitty/iTerm2/Sixel thumbnail; the
composer is text-only (no image paste); and history starts empty (only items
received while the TUI is open are shown — the daemon has no bulk-buffer-replay op).

Visual check on a real terminal: run `bin/lan tui` on two machines (or two
`--config-dir`/`--socket` pairs on one host) in the same `--room`, type on one,
watch the bubble arrive on the other; select it and press `y`, then confirm with
`pbpaste` (macOS) / `xclip -o` (Linux).

## What the probes answer (BAKEOFF)

- **`lan-go`** — is iroh's weight worth it, or is a hand-rolled LAN mesh good
  enough? (quic-go + mDNS + self-signed-cert identity, ~13 MB binary.)
- **`libp2p-mesh`** — iroh vs libp2p at the same topology: config surface,
  discovery/dial ergonomics, dependency weight (~38 MB binary).

## Tests

Default suite (fast, deterministic, no network/TTY needed):

```sh
cd experiments/shared-go && go test ./...   # wire codec/sniff/framing (incl. file envelope), dedupe, sinks + session clear, TUI
```

The TUI (shared by both probes) is covered at three layers:

1. **Model `Update` tests** (`shared-go/cli/tui_test.go`) — drive `Update()`
   directly and assert model state (incoming/own/image bubbles, submit→Send,
   copy, the quit-clear prompt). No terminal.
2. **Rendered-frame tests** (`shared-go/cli/tui_render_test.go`) — run the model
   through a simulated terminal with
   [`charmbracelet/x/exp/teatest`](https://github.com/charmbracelet/x/tree/main/exp/teatest)
   and assert on the **bytes lipgloss actually renders**: the header
   (`room`/`id`/`peers`/`auto_copy`/`clipboard`) and chat bubbles (sender +
   body). They inject items via `tm.Send(itemMsg{…})`, wait with
   `teatest.WaitFor` (substring, not strict golden — robust to frame-diff
   timing), and finish with `Ctrl-C` + `WaitFinished`. Under `go test` stdout is
   a pipe, so lipgloss uses the Ascii profile (plain text) → matches are
   reliable. One pinned 80×24 **golden** (`TestRenderGolden`, in
   `cli/testdata/TestRenderGolden.golden`) captures a full frame; regenerate it
   after intentional layout changes with:

   ```sh
   cd experiments/shared-go && go test ./cli/ -run TestRenderGolden -update
   ```

3. **PTY end-to-end smoke** (`lan-go/e2e/tui_e2e_test.go`, gated behind
   `//go:build e2e`) — builds `lan`, boots `lan tui` under a real pseudo-terminal
   via [`creack/pty`](https://github.com/creack/pty), waits for the header to
   render (the daemon is auto-spawned; peer/mDNS discovery is **not** required),
   quits, asserts a clean exit, and stops the daemon. Excluded from the default
   `go test`; run it explicitly:

   ```sh
   cd experiments/lan-go && go test -tags e2e ./e2e/ -run TestTUIPTYSmoke -v
   ```
