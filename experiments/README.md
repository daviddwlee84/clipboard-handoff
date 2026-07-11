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
| `shared-go/wire` | Envelope + CBOR codec + length-prefixed framing + type-sniff (PNG/JPEG/UTF-8, JPEG→PNG) + BLAKE3 (PROTOCOL §1–2) |
| `shared-go/ipc` | Local Unix-socket IPC: length-prefixed CBOR request/response + event stream, daemon auto-spawn (PROTOCOL §4) |
| `shared-go/config` | Config dir + `config.json` settings (SPEC §5); hands each transport a path for its identity key |
| `shared-go/clip` | OS clipboard via `golang.design/x/clipboard` (text + PNG) |
| `shared-go/daemon` | Ring buffer, `msg_id` dedupe + last-written-hash echo suppression, auto-copy (`notify`/`on`/`off`), IPC handlers |
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

## Messenger TUI (`BIN tui`)

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
| `y` | copy the highlighted bubble (or latest) to the OS clipboard **via the daemon** |
| `s` | save a highlighted image to `~/Downloads` (else temp dir) |
| `o` | open a highlighted image with the OS default app |
| `p` | paste the daemon's latest received item to the clipboard |
| `q` / `Ctrl-C` | quit |

Incoming items appear live. `auto_copy` mode is shown in the header and honored
by the daemon (the TUI is front-end only). **Stubbed:** inline image previews —
image bubbles show a metadata placeholder (`🖼 <hash>.png W×H · size`) plus
copy/save/open actions, not a Kitty/iTerm2/Sixel thumbnail; the composer is
text-only (no image paste); and history starts empty (only items received while
the TUI is open are shown — the daemon has no bulk-buffer-replay op).

Visual check on a real terminal: run `bin/lan tui` on two machines (or two
`--config-dir`/`--socket` pairs on one host) in the same `--room`, type on one,
watch the bubble arrive on the other; select it and press `y`, then confirm with
`pbpaste` (macOS) / `xclip -o` (Linux).

## What the probes answer (BAKEOFF)

- **`lan-go`** — is iroh's weight worth it, or is a hand-rolled LAN mesh good
  enough? (quic-go + mDNS + self-signed-cert identity, ~13 MB binary.)
- **`libp2p-mesh`** — iroh vs libp2p at the same topology: config surface,
  discovery/dial ergonomics, dependency weight (~38 MB binary).

## Unit tests

```sh
cd experiments/shared-go && go test ./...   # wire codec, sniff, framing, dedupe, TUI model
```
