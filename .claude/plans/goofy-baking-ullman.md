# Cross-Platform Copy — Implementation Plan

## Context

We are building a **cross-platform CLI/TUI tool for sharing clipboard content (text + images) between a
user's own devices**, with the headline experience being an AirDrop/Handoff-like hand-off: content copied
(or piped) on one device becomes instantly pasteable on another. Requirements: multi-device, multiple
simultaneous connections; a CLI that accepts piped stdin over one persistent connection; a messenger-like
TUI; text **and** image support; inspired by "paste a clipboard image into Claude Code over SSH".

Two parallel research streams (a P2P workflow + a focused SSH-room study of the user's own `sshbbs`) settled
one decisive cross-cutting fact:

> **Images can only auto-land on a device's OS clipboard via a native, resident agent on that device.**
> Terminal escapes (OSC 52) carry **text only** (~74 KB cap, tmux-stripped). So every architecture needs a
> per-device daemon that owns the clipboard; the only real fork is the **transport/topology**, not the image
> handling.

**Settled decisions (from the user):**
1. **Two opposed headliner apps for the bake-off** (`mesh-rs`, `room-go`) **plus an open, extensible "experiments" track** of small transport probes (e.g. `lan-go`, `libp2p-mesh`) — a technical bake-off to see which is nicest to use.
2. **All OSes first-class**: macOS, Linux X11, Linux Wayland, Windows.
3. **LAN-first**, with internet reach behind a config flag added later.
4. **Auto-copy default = notify-first / one-key paste** (never silently overwrite the clipboard by default).
5. **MVP everything, harden only the winner**: take the headliners (and probes) to a comparable MVP, run the bake-off, then invest the expensive all-OS/1.0 hardening (Phase 3–4) **only in the winner**.

**Toolchains present:** rustc 1.96, go 1.26, node 24. `/tmp/sshbbs` (the user's wish/bubbletea BBS) is
available as a reuse reference for the Go room-server app.

## The bake-off set: 2 headliners + an experiments track

**Headliners** (the two genuinely-opposed UX bets — what the user was originally torn between). These are built
fully and **independently** (no shared implementation code) so the comparison is honest:

| App | Language | Topology | Transport | Clipboard | TUI | The bet it tests |
|---|---|---|---|---|---|---|
| **`mesh-rs`** | Rust | P2P mesh, no server | iroh (+ iroh-gossip, iroh-blobs) | arboard + `image` | ratatui + ratatui-image | Batteries-included true P2P: NAT/relay/blobs for free |
| **`room-go`** | Go | Central room server | charmbracelet/wish + SSH | golang.design/x/clipboard | bubbletea (+ OSC 52) | Reuse `sshbbs`; zero-install SSH text tier + native client for images; easy history |

**Experiments track** (`experiments/`) — small, LAN-first, MVP-depth transport probes, extensible over time.
Their job is to *quantify* trade-offs, not to be production apps, so they may share a small internal Go helper
(clipboard + envelope) to move fast:

| Probe | Language | Transport | Question it answers |
|---|---|---|---|
| **`lan-go`** | Go | quic-go + mDNS (`grandcat/zeroconf`) | Is iroh's weight worth it, or is a hand-rolled LAN mesh good enough? |
| **`libp2p-mesh`** | Go | go-libp2p (gossipsub + mDNS) | iroh vs libp2p at the same topology (config surface, hole-punch reliability) |
| *(add more)* | — | — | Any transport worth a quick probe |

Everything implements the **same product contract** (below), so any impl can be dropped into the bake-off harness.

## Repo layout (polyglot monorepo)

```
/
├─ README.md
├─ docs/
│  ├─ SPEC.md         # shared product contract: CLI surface, UX flows, auto-copy semantics, OS matrix
│  ├─ PROTOCOL.md     # transport-agnostic wire envelope + PNG image rules
│  └─ BAKEOFF.md      # evaluation rubric + scorecard + manual test scripts
├─ testdata/          # shared: sample.txt, small.png, large.png, screenshot.png
├─ scripts/           # bake-off harness: spawn 2 instances, round-trip text+PNG, assert clipboard/stdout
├─ mesh-rs/           # HEADLINER — Rust cargo workspace (clipd daemon + clip CLI/TUI)
├─ room-go/           # HEADLINER — Go module (wish server + native client + TUI)
└─ experiments/       # small LAN-first transport probes (may share an internal Go helper)
   ├─ shared-go/      #   tiny helper: OS clipboard + envelope (experiments only; headliners stay independent)
   ├─ lan-go/         #   quic-go + mDNS DIY mesh
   └─ libp2p-mesh/    #   go-libp2p gossipsub mesh
```

## Shared product contract (`docs/SPEC.md` + `docs/PROTOCOL.md`)

**Identical CLI surface across all three apps** (only the binary name differs):
- `send [--text|--image|--auto]` — read stdin, sniff type (UTF-8 → text; PNG/JPEG magic → image), broadcast.
- `recv [--follow] [--latest-image --emit-path]` — subscribe; text → stdout, image → temp-file path (shell-composable).
- `paste` — write the latest received item into the local OS clipboard (the CLI "one-key paste" for notify-first mode).
- `tui` — messenger-style chat window.
- `pair` / `join` — establish membership (mesh: ticket/QR + short-code verify; room: server addr + SSH key).
- `peers`, `status`.
- `config set auto_copy notify|on|off` · `config set internet on|off`.
- `daemon` — run the resident daemon (auto-spawned by the other commands on first use).

**Wire envelope (`PROTOCOL.md`), transport-agnostic:**
```
Envelope { v, msg_id(ulid), type: text|image, mime, sender, device_name, ts,
           text?,                        // text: inline
           blob?: { hash, size, w, h } } // image: content-addressed, PNG on the wire
```
Small text rides inline; images are **always PNG on the wire** (encode/decode at each hop), announced by hash
and pulled over a direct stream (mesh) or via the server (room).

**Auto-copy semantics (identical, default `notify`):** on receipt from an allowlisted peer, show a desktop
notification + TUI toast but do **not** write the clipboard; the user presses one key in the TUI or runs
`paste`. `on` = silent auto-write; `off` = never. **Echo/loop suppression**: dedupe by `msg_id` + track the
content-hash of the last value this daemon *wrote*, and never re-broadcast a just-received value (prevents a
mesh of auto-copying peers ping-ponging).

## Per-app architecture

### `mesh-rs` (Rust — the true-P2P bet)
- **Process model — daemon + thin clients:** `clipd` owns the iroh `Endpoint`, the Ed25519 identity secret
  (in the OS config dir), gossip membership, the iroh-blobs store, the arboard clipboard path, the trusted-peer
  allowlist, and a recent-message ring buffer. It exposes a **local IPC socket** (`interprocess`: Unix socket /
  named pipe) speaking length-prefixed CBOR. `clip` CLI (clap) and `clip tui` are ephemeral IPC clients holding
  no network state; they auto-spawn the daemon. *(A resident daemon is mandatory anyway: on X11/Wayland the
  process that sets the clipboard owns the selection and must stay alive to serve pastes.)*
- **Transport:** iroh Endpoint; LAN = `RelayMode::Disabled` + local (mDNS) discovery → direct QUIC, zero infra.
  Internet mode (later, one flag) enables relay + hole punching + DNS/pkarr discovery. Room = 256-bit secret →
  `TopicId` via iroh-gossip; images via iroh-blobs (BLAKE3, chunked/resumable).
- **Clipboard:** arboard (`image-data` + `wayland-data-control` features); bridge arboard's raw RGBA ↔ PNG with
  the `image` crate.
- **TUI:** ratatui + **ratatui-image** (auto-negotiates Kitty/iTerm2/Sixel, degrades to Unicode half-blocks so an
  image always renders — the key to the SSH image flow) + tui-textarea, on crossterm + tokio.
- **Pairing:** `clip pair --new` prints a base32 ticket **and** an in-terminal QR (`qrcode`); peer runs
  `clip pair <ticket>`; both confirm a short verification code (anti-MITM); TOFU allowlist with explicit approval.

### `room-go` (Go — the centralized bet, reuses `sshbbs`)
- **Server:** charmbracelet/wish + `charmbracelet/ssh` with **public-key auth** (a device is authorized by
  allowlisting its key = no pairing protocol). Reuse `sshbbs` patterns directly:
  - `internal/chat/broker.go` → `map[roomID][]*Session` + `program.Send` fan-out (`Send`/`SendToAll`). **Inherit
    its documented self-send-deadlock guard** (never `Send` to your own session from inside `Update`).
  - `internal/store/*` (SQLite via `modernc.org/sqlite`, WAL + busy_timeout + write mutex) for **history +
    replay-on-reconnect** — the natural retention win.
  - `internal/tui/osc52.go` for the free **text** auto-clipboard over bare SSH.
- **Two client tiers against the same room** (this is the point of the room bet):
  - **Tier 1 — bare `ssh room@server`** (zero install): full TUI chat; text auto-clipboard via OSC 52; images
    shown inline (rasterm/timg) or offered as a file-drop. Great "I'm on a random box" fallback, **text-only** for clipboard.
  - **Tier 2 — native `room-go` client binary**: connects to the server (SSH transport or WebSocket) and natively
    writes **text + image** to the local clipboard (golang.design/x/clipboard, PNG). This tier delivers the full
    image hand-off, the CLI pipe, and notify-first.
- **Clipboard:** golang.design/x/clipboard (cgo-free, PNG-native). **TUI:** bubbletea/lipgloss/bubbles.
- **Privacy note:** the server sees plaintext; add optional client-side E2E (server stores ciphertext) as a later layer.

### `experiments/*` (Go — the transport probes)
Small, LAN-first, MVP-depth. Same daemon + thin-client shape and the same product contract, but scrappier
(LAN-only, less polish) since their purpose is transport comparison, not production. They may share
`experiments/shared-go` (OS clipboard via golang.design/x/clipboard + envelope codec) to avoid re-implementing
plumbing per probe.
- **`lan-go`** — no iroh, no server. Discovery via mDNS/DNS-SD (`grandcat/zeroconf`); transport = quic-go direct
  connections with a self-signed TLS cert whose fingerprint **is** the device identity (Syncthing-style); images
  chunked over a QUIC stream. Exposes exactly the hard part iroh hides (internet reach needs a self-hosted relay /
  DIY hole-punch) — that's the point: it quantifies iroh's value.
- **`libp2p-mesh`** — go-libp2p with gossipsub for the mesh + mDNS for LAN; compares iroh vs libp2p at the same
  topology (config surface, ~70% hole-punch reliability, relay/rendezvous needs).
- **(extensible)** — add further probes (e.g. WebRTC data channels, plain TCP+broadcast) if a comparison looks worthwhile.

## Phased roadmap (applied per app; MVPs first so the comparison lands early)

- **Phase 0 — hand-off MVP** (LAN, macOS + Linux/X11 first): 2 devices, copy/pipe text or image on A → notify +
  `paste`/one-key on B. Ships `send` (piped stdin), `recv --follow`, `paste`, arboard/golang.design text+image
  auto-copy, the image transfer path, and 2-device pairing/join. **This single flow is the magic; it validates each bet.**
- **Phase 1 — N-peer + secure membership + notify-first polish:** real mesh/room with N peers, QR/short-code
  pairing (mesh) or key allowlist (room), TOFU + approval prompt, echo/loop suppression, transfer progress.
- **Phase 2 — Messenger TUI with inline images:** chat scrollback + composer; inline thumbnails with
  protocol-detect + half-block fallback (encode off the UI thread); per-image actions copy(y)/save(s)/open(o).
- **Phase 3 — Internet flag + SSH/Claude Code + platform parity:** internet mode (mesh-rs: iroh relay; lan-go:
  relay; room-go: already internet); a Claude Code `/paste-image` skill (`<app> recv --latest-image --emit-path`)
  that beats `ccimg` (full-res, mutual auth, no reverse `ssh -R`); **Windows** (named-pipe IPC, CF_DIB) + native
  **Wayland** (wayland-data-control, XWayland fallback); headless "no-clipboard" daemon mode for SSH targets.
- **Phase 4 — Hardening + distribution:** launchd / systemd --user / Windows service wrappers, auto-start/crash
  recovery; macOS notarization + Windows signing for the background daemon; Homebrew / cargo-binstall / prebuilt
  releases; self-hosted-relay docs.

**Scope-control (confirmed):** headliners `mesh-rs` + `room-go` go through **Phase 0–2** (comparable MVPs);
experiments are **Phase 0–1 probes** (LAN transport only). Then run the bake-off, and invest the expensive
**Phase 3–4** (all-OS hardening, signing, distribution) **only in the winner** — hardening everything to 1.0
across four OSes is ~3× the work and mostly redundant once one wins.

## Key risks & mitigations
- **Clipboard hijack via auto-copy** → default `notify`, gate on TOFU allowlist + approval, echo-suppress by
  `msg_id` + last-written content-hash, never re-broadcast a received value.
- **arboard raw-RGBA + conditional Wayland/GNOME; Windows has no PNG clipboard format** → PNG canonical on the
  wire (`image` crate), enable `wayland-data-control` + XWayland fallback + degrade-to-text; let arboard/golang.design
  handle CF_DIB↔pixels; CI-test each backend.
- **iroh / iroh-gossip / iroh-blobs are heavy & near-1.0 (API churn)** → pin exact versions, wrap iroh behind a thin
  internal transport trait; LAN-first keeps the early dependency surface minimal.
- **mDNS/multicast blocked on corporate LANs** → fall back to manual ticket/QR + direct-address hints (mesh) or the
  server (room).
- **Daemon lifecycle + signing complexity across 4 OSes** → auto-spawn on first use, versioned IPC, per-OS service
  wrappers, stand up a signing/notarization CI pipeline in Phase 4 only.
- **Maintaining three cross-platform apps** → shared SPEC/testdata/harness, comparable MVPs first, winner-takes-hardening.

## Verification (end-to-end, per app)
- **Automated round-trip harness** (`scripts/`): spawn two daemons on loopback/LAN, `send` `testdata/sample.txt`
  and `testdata/small.png` from A, assert B's `recv`/`paste` output matches (compare **BLAKE3 hash** for the PNG).
  Run for each app so results are directly comparable.
- **Manual "feel" test** (mac first): terminal A `echo hi | <app> send`; terminal B `<app> recv --follow` prints
  `hi`, then `<app> paste` and verify with `pbpaste`. Image: `<app> send < testdata/small.png`; on B
  `<app> recv --latest-image --emit-path` writes a file whose hash matches; with `auto_copy on` verify the image
  pastes into an image app. Repeat cross-machine on the LAN.
- **BAKEOFF scorecard** (`docs/BAKEOFF.md`): pairing effort, time-to-first-message, LAN zero-config, offline-LAN,
  image fidelity, latency feel, zero-install story, resource/binary size, privacy, history — filled per app.
- Per-language unit tests: envelope encode/decode, type-sniff, echo-suppression, clipboard round-trip (`cargo test`, `go test`).

## First implementation step (what "go" means)

Start by laying the **shared contract + scaffolding**, then the first magic flow:
1. Write `docs/SPEC.md`, `docs/PROTOCOL.md`, `docs/BAKEOFF.md`; add `testdata/` assets and the `scripts/` round-trip harness skeleton.
2. **`mesh-rs` Phase 0** and **`room-go` Phase 0** in parallel (the two headliners): LAN, 2 devices, text+image
   hand-off with `send`/`recv`/`paste` and notify-first — the smallest thing that delivers the AirDrop feel.
3. Then the `experiments/` probes (`lan-go`, `libp2p-mesh`) to the same Phase 0, and fill the BAKEOFF scorecard.
