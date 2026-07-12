# IMPLEMENTATION — how it's built, and how the approaches compare

This is the *architecture* companion to the per-tool [usage guides](README.md). It explains what all three
implementations share, how each one is actually wired internally, and — the point of the bake-off — how the
transports differ when you put them side by side. It reads the real code; where an impl diverges from the
[SPEC](SPEC.md)/[PROTOCOL](PROTOCOL.md) contract it says so.

Contract references, not restated here: **[SPEC.md](SPEC.md)** (CLI surface, sinks, sessions, config keys),
**[PROTOCOL.md](PROTOCOL.md)** (envelope, framing, identity, IPC), **[BAKEOFF.md](BAKEOFF.md)** (measured
numbers + scorecard). Usage: **[usage-mesh-rs.md](usage-mesh-rs.md)**, **[usage-room-go.md](usage-room-go.md)**,
**[usage-lan-go.md](usage-lan-go.md)**.

| Impl | Binary | Language | Transport | Status |
|---|---|---|---|---|
| `mesh-rs` | `clip` | Rust | iroh (QUIC), direct + mDNS | headliner |
| `room-go` | `room` | Go | SSH/wish central room server | headliner |
| `experiments/lan-go` | `lan` | Go | quic-go + mDNS | probe |
| `experiments/libp2p-mesh` | `libp2p-mesh` | Go | libp2p gossipsub + mDNS | **parked** |

---

## 1. The shared architecture

Every impl is the same shape: **one resident daemon per device** + **thin, ephemeral clients** over a local IPC
socket. Only the network transport underneath the daemon differs. This section is the part that is genuinely
identical (it's why the bake-off harness can drive any of them with the same commands).

### 1.1 Why a daemon is mandatory (not a style choice)

> **Images and files can only land on a device's OS clipboard through a native, resident agent on that device.**

Two hard constraints force it:

1. **Selection ownership.** On X11/Wayland the process that *sets* the clipboard **owns the selection** and must
   stay alive to serve future pastes. A fire-and-forget CLI would drop the selection the instant it exits.
2. **Always-on hand-off.** "Auto-copy on receive" and "notify me, then one-key accept" are behaviors that must run
   while nothing else is; and terminal escapes (OSC 52) carry **text only** (~74 KB, tmux-strips it) — no image
   or file path. So a per-device daemon that owns the clipboard is unavoidable.

The daemon owns: the network connection(s), the device identity key, the trusted-peer (allowlist) set, a bounded
ring buffer of received items, a content-addressed blob cache, and **the OS clipboard**. `send`/`recv`/`paste`/
`clear`/`pair`/`join`/`tui`/… are thin clients that hold no network state and **auto-spawn** the daemon (detached)
if it isn't running. The **TUI is also just a front-end** — it never opens the clipboard itself (that would make
it a second owner); its `y` copy routes through the daemon.

### 1.2 Local IPC (client ⇄ daemon)

Transport-agnostic and identical in shape across all three (PROTOCOL §4):

- **Socket**: Unix domain socket in the runtime dir — e.g. `$XDG_RUNTIME_DIR/<bin>/daemon.sock` (temp-dir
  fallback), overridable with `--socket`. Named pipe on Windows is the planned form.
- **Framing**: **4-byte big-endian length prefix + CBOR** for both requests and responses. One request per
  connection, except `Subscribe` / `recv --follow` / `tui`, which hold the connection open and receive an **event
  stream** (`Item` / `PeerUp` / `PeerDown` / `Toast`).
- **Auto-spawn**: on connect-refused the client launches `<bin> daemon` detached (inheriting `--config-dir` /
  `--socket` / `--room`), then retries with a short backoff (mesh-rs: 50×100 ms; room-go: 5 s / 75 ms).
- **Versioned**: an `ipc_version` (currently 1) is checked; a mismatch fails loudly.

mesh-rs uses the `interprocess` crate + `ciborium`; room-go and shared-go use `net.Listen("unix")` + `fxamacker/
cbor`. Same wire, different libraries.

### 1.3 The transport-agnostic envelope

One logical message shape, encoded as **CBOR on the wire in all three impls** (PROTOCOL allows JSON for
experiments, but nobody actually uses it — see §5 divergences):

```
Envelope { v:1, msg_id: ULID, type: text|image|file, mime, sender, device_name, ts(unix-ms),
           filename?,  text? | blob{ hash: BLAKE3-hex, size, w?, h? } }
```

- **`msg_id`** is a **ULID** (time-sortable) — the dedupe key.
- **`sender`** is the transport's stable identity (Ed25519 node id / TLS-cert fp / SSH-key fp / PeerId), so the
  allowlist check is authenticated by the transport.
- Optional fields are `omitempty` / `skip_serializing_if` and decode is **unknown-field-tolerant**, so adding a
  field (e.g. `filename`) stays wire-compatible with an older peer.

**Type sniffing (`--auto`, PROTOCOL §2)** is the same everywhere: PNG magic (`89 50 4E 47 0D 0A 1A 0A`) or JPEG
(`FF D8 FF`) → **image** (JPEG transcoded to PNG, the canonical wire form; PNG passes through byte-for-byte so its
BLAKE3 is preserved); `--file` or non-UTF-8 bytes → **file** (`application/octet-stream`, mime guessed from the
extension); otherwise valid UTF-8 → **text**. Arbitrary binary is never an error — it *is* a valid file.

**Image rules**: image is always PNG on the wire, is the only type that can land on the clipboard *as an image*,
and its `blob.hash` (BLAKE3) **must be verified on receipt**. A **file** is content-addressed the same way but is
**never** clipboard-pasteable — `paste`/`y` copy its **local path as clipboard text**.

**Where the impls diverge — blob movement.** This is the one meaningful wire difference and it's deliberate:

| | Image/file bytes | Rationale |
|---|---|---|
| `mesh-rs` | **announce + pull**: broadcast the small envelope, each receiver pulls the bytes over a direct QUIC bidi stream keyed by `blob.hash` (`WireMsg::BlobRequest`/`BlobResponse`), BLAKE3-verifies, caches | never floods bytes; the PROTOCOL §1 target |
| `room-go`, `lan-go`, `libp2p-mesh` | **inline `blob_data`**: the PNG/file bytes ride *in* the envelope (a documented Phase-0 simplification) | simplest; `MaxFrame` 32 MiB comfortably fits test images |

Both keep PNG canonical and verify BLAKE3 on receipt, so the bake-off compares like with like. Announce+pull for
the Go impls is a Phase-1 item.

### 1.4 Notify-first auto-copy, echo suppression, sinks, sessions

All four behaviors are contract-identical (SPEC §3/§8); each impl re-implements them over its own daemon state.

- **`auto_copy` (`notify` default | `on` | `off`)** — `notify`: buffer + emit a `Toast`, **do not** touch the
  clipboard (press `y`/run `paste` to accept); `on`: write the clipboard immediately (files excepted — never
  auto-copied); `off`: never touch it.
- **Echo / loop suppression** — (1) dedupe by `msg_id` (a bounded seen-set, oldest evicted; cap 4096); (2) record
  the BLAKE3 of the last value written to *our own* clipboard (`note_written` / `lastCopy`) so `on` mode never
  re-broadcasts what it just pasted; (3) never re-broadcast a received item. Rule (2) is present and unit-tested
  but only *exercised* once `broadcast_on_copy` clipboard-watching lands (Phase 1) — see §5.
- **Additive sinks** — a received item fans out by type, independent of the clipboard: **`save_dir`** gets
  image/file (`<filename|hash>[.png]`, de-duplicated `name (2).ext`); **`text_file`** gets text appended after a
  `\n---\n<device> <ISO8601-ts>\n` header. Sinks run *after* suppression, and everything written this session is
  tracked for `clear`.
- **Sessions & clearing** — the daemon records, per run: the **transient store** (blob cache + `recv --emit-path`
  files + in-memory buffer), the **`text_file` size at session start** (the truncation offset, re-anchored if the
  sink is re-configured mid-session), and the **list of `save_dir` files it wrote**. `clear` purges transient;
  `clear --all` also truncates `text_file` back to the offset and deletes exactly this session's `save_dir` files —
  never pre-session content, never a whole directory. `daemon stop` / SIGTERM apply the `clear_on_exit` policy
  (`ask` → `transient` when non-interactive); the TUI raises the same `[t]ransient / [a]ll / [n]o` prompt on quit.

### 1.5 Headless degradation

Each daemon **probes the clipboard once at startup** (mesh-rs: `arboard::Clipboard::new().is_ok()`, force with
`CLIP_FORCE_HEADLESS`; room-go/shared-go: `clip.Available()`). On a display-less host it logs one warning and
degrades: `send`/`recv`/`--emit-path`/discovery/transfer/sinks all keep working; only `paste` and `auto_copy on`
become clear no-ops (never a panic), and `status` reports `clipboard: unavailable`. This is what lets the
cross-machine tests target a headless Ubuntu box.

---

## 2. Per-implementation internals

### 2.1 `mesh-rs` — `clip` (Rust / iroh)

Files: `mesh-rs/src/{main,daemon,client,proto,config,clipboard,tui}.rs`. Binary `clip`.

- **Transport** — **iroh, pinned exactly `=1.0.2`** (the API churns; the code targets `EndpointId`/`EndpointAddr`,
  `presets::Minimal`, `Router`/`ProtocolHandler`, `open_bi`/`accept_bi`). Phase-0 endpoint = preset `Minimal` +
  `RelayMode::Disabled` (LAN-only). Peer messages are a small CBOR enum over **direct QUIC bidi streams**:
  `WireMsg::Announce(Envelope) | BlobRequest{hash} | BlobResponse{ok,bytes}`.
- **Identity** — an Ed25519 `SecretKey` persisted at `<config-dir>/secret.key` (32 bytes, `0600`); its `EndpointId`
  is the `sender`. Config dir defaults to `directories::ProjectDirs(… "mesh-rs")`.
- **Discovery / connection setup** — two coexisting paths:
  - **Ticket**: `pair --new` returns base32(CBOR of `EndpointAddr` = id + reachable direct addrs, including
    `loopback:port` and discovered LAN addrs). `pair <ticket>` decodes and `endpoint.connect(addr, alpn)`.
  - **mDNS auto-discovery** (same `--room`, no ticket) via the `iroh-mdns-address-lookup =0.4.0` companion crate
    (iroh 1.0.2 renamed "discovery" → *address lookup*). Service name is room-scoped: `clip` + `hex(blake3(room)
    [:8])` → record `<endpoint>._clip<hex>._udp.local`.
- **Room isolation is doubly enforced**: (1) the mDNS service name is per-room (different rooms never see each
  other); (2) the room secret is folded into the **ALPN** — `alpn_for_room = b"mesh-rs/clip/0/" +
  hex(blake3(room)[:8])` — so a cross-room dial is rejected at the QUIC handshake.
- **N-peer** — `broadcast()` opens a bidi to each connected peer's `Connection` and writes an `Announce`. This is
  **pairwise direct QUIC** to every connected peer; **iroh-gossip** (a real N-peer announcement mesh) is *not*
  wired yet (BAKEOFF scores multi-peer 3 for exactly this reason).
- **Images/files** — announce + pull: `fetch_blob` opens a stream, sends `BlobRequest{hash}`, BLAKE3-verifies the
  `BlobResponse`, caches it in memory + writes `<config-dir>/blobs/clip-<hash[:16]><ext>` so `recv --emit-path`
  and `paste` always have a path. **iroh-blobs** (resumable content-addressed transfer) is deferred.
- **Clipboard** — `arboard` (`image-data`); PNG↔RGBA bridged with the `image` crate. `arboard` is blocking and its
  handle isn't `Send`, so every write runs inside `spawn_blocking`.
- **TUI** — `ratatui 0.29 + ratatui-image 9 + tui-textarea 0.7 + crossterm 0.28` on the tokio runtime (pinned as
  one set; tui-textarea caps ratatui at 0.29). **Inline image thumbnails** via ratatui-image's `StatefulProtocol`:
  a real terminal graphics protocol (Kitty/iTerm2/Sixel) if the `Picker` probe finds one at startup, else Unicode
  half-blocks, else the metadata line as a text placeholder. **Decode + resize run off the UI thread** (a
  `spawn_blocking` decode + a resize worker task) so the event loop never blocks; a terminal that ignores the
  graphics-capability query costs a ~1–2 s startup probe timeout.
- **Notable code / pitfalls**:
  - **Split-brain guard**: the daemon binds the **IPC socket before the (slower) iroh endpoint**, so a client
    that auto-spawns it can't race into spawning a *second* daemon; `probe_alive` makes startup idempotent (exit
    if someone's already listening).
  - **Double-dial avoidance**: on an mDNS `Discovered` event, only the peer with the **larger `EndpointId`** dials
    (`if my_id < peer_id { continue }`); the other accepts. An in-flight `dialing` set plus a `peers` check stop
    duplicate connections from repeated events.
  - **Multi-homed / Tailscale caveat**: the endpoint advertises every non-loopback address, but mDNS multicast
    only crosses the real LAN interface — a VPN interface generally won't, so auto-discovery needs LAN multicast
    (else fall back to a ticket; direct QUIC over the LAN address still works).
  - A separate `sent` ring holds locally-originated items so the TUI can `PasteItem` its own bubbles.
  - **Trust**: Phase 0 auto-allowlists any peer you connect with; TOFU-with-approval is Phase 1.
- **Costs** — iroh release compile **> 7 min**, 557 locked crates; binary 24 MB release (20 MB stripped) / 79 MB
  debug. This is the price for getting NAT traversal + relay + content-addressed blobs *for free* later.

### 2.2 `room-go` — `room` (Go / wish SSH server)

Files: `room-go/cmd/room/{main,remote,flagset}.go`, `room-go/internal/{server,broker,daemon,ipc,wire,config,clip,
tui}/`. Binary `room`, module `…/room-go`, Go 1.26, **cgo required** (clipboard backend).

- **Transport** — a real central **SSH room server** (`charmbracelet/wish` + `charmbracelet/ssh`, public-key
  auth), not a TCP fallback. The native-client tier uses a **raw SSH session as a byte pipe**: the client execs
  the **room name as the SSH command** (`sess.Start(room)`); the server reads `sess.Command()[0]` as the room and
  relays **opaque length-prefixed CBOR frames** between that room's members. The server never decodes an envelope.
- **Identity** — a client SSH ed25519 key at `<config-dir>/id_ed25519` (OpenSSH PEM, generated on first `join`);
  `sender` = its `FingerprintSHA256`. The server authorizes by key: `--authorized-keys FILE` restricts it, else
  Phase-0 **trust-all** (the fingerprint is still captured). Config dir = `os.UserConfigDir()/room`.
- **Broker** — an in-memory `map[room]map[*Session]struct{}`, adapted from the author's `sshbbs`
  `internal/chat/broker.go`. **Self-send guard**: `Broadcast` explicitly excludes the originator
  (`if s == from { continue }`) so a client never echoes its own send. Recipients are snapshotted under the read
  lock and delivered outside it; a full outbound buffer (256) drops rather than stalls the fan-out.
- **N-peer** — native server fan-out to all room members (BAKEOFF scores multi-peer 5).
- **Images/files** — **inline `blob_data`** in the envelope, relayed through the server; PNG canonical, BLAKE3
  verified, materialized to `<config-dir>/blobs/<hash>[.png|ext]`. Server-side pull-by-hash is Phase 1.
- **Clipboard** — `golang.design/x/clipboard` (PNG-native, cgo). `clip.Available()` gates headless degradation.
- **Connection loop** — `connectLoop` keeps the SSH session alive and redials on drop (2 s backoff);
  `HostKeyCallback = InsecureIgnoreHostKey` (Phase 0 loopback/tunnel; host-key pinning is Phase 1).
- **TUI** — `bubbletea + lipgloss + bubbles`, `Subscribe`d to the daemon's event stream. **Inline image rendering
  is a documented stub**: image bubbles show a metadata placeholder (no pixels); `y`/`s`/`o` operate on the
  full-res PNG the daemon materialized. `renderImageBody` is a drop-in for a later `rasterm`-style preview.
- **Notable code / pitfalls**:
  - **Host-key generation fix** (a real cross-machine bug from the bake-off): `wish.WithHostKeyPath` delegates to
    `charmbracelet/keygen`, which **chmods the key's parent directory** → `EPERM` when the key lives under a shared
    dir like `/tmp` on a server. `room` instead loads/generates the ed25519 host key itself
    (`hostKeyPEM` + `wish.WithHostKeyPEM`), creating missing parents but never chmod-ing a pre-existing dir.
  - `peers` reuses `status` output in Phase 0 (the server doesn't push a peer list; `status.peers` is 0).
  - `send` with no server/peers returns IPC **code 4** — the client warns but exits 0 (safe under `set -e`).
- **Strengths / costs** — leanest binary (**10 MB**) and idle RSS (**8.2 MB**); the only impl with a natural path
  to **server-side history** (SQLite) and a **zero-install `ssh room@server` + OSC-52 text tier** (Phase 1). Cost:
  you run and trust a server (it sees plaintext unless E2E is added) and LAN use isn't zero-config.

### 2.3 `experiments/lan-go` — `lan` (Go / quic-go + mDNS), on `shared-go`

Files: `experiments/shared-go/{wire,ipc,config,clip,daemon,cli,transport}/`,
`experiments/lan-go/internal/lantransport/`. Binary `lan`.

**The shared-go design — one daemon, pluggable transport.** `shared-go` owns everything above the wire; a probe
writes *only* a transport implementing:

```
Transport = Identity() · Start(ctx) · Broadcast(env) · OnReceive(fn) · Peers() · Close()
```

A probe's `main` is ~15 lines (`cli.App{BinName, NewTransport}`). `shared-go/daemon` provides the ring buffer,
`msg_id` dedupe + last-written-hash suppression, `notify`/`on`/`off` auto-copy, the additive sinks, and the
session/clear tracking — **identical semantics to §1.4**, so `lan` and `libp2p-mesh` behave the same above the
transport. (The two headliners stay fully independent: they do **not** import `shared-go`, and it doesn't import
them.)

**lan-go transport** (`lantransport`):

- **Transport** — `quic-go` **v0.60.0** direct connections. Envelopes ride **one-per-QUIC-uni-stream**
  (length-prefixed CBOR). Images ride **inline `blob_data`** (BLAKE3 verified on receipt).
- **Identity** — a persisted self-signed **TLS cert** (`<config-dir>/tls_cert.pem` + `tls_key.pem`, an ECDSA
  P-256 key); the identity is the **SHA-256 fingerprint of the cert DER** (Syncthing-style). Peers authenticate
  via **mutual TLS** on the QUIC connection (ALPN `clip-lan/0`; TOFU — any cert accepted, keyed by fingerprint).
- **Discovery** — mDNS/DNS-SD via `grandcat/zeroconf` **v1.0.0**: each daemon advertises a `_clip-lan._udp`
  service whose **TXT record carries `room=` / `fp=` / `port=` (the real ephemeral QUIC port) / `name=`**, and
  browses the same service, connecting only to peers whose `room` matches. **Room isolation is by the TXT `room=`
  match**, not an ALPN (the ALPN is a per-tool constant).
- **Dedup dialing** — two daemons on one host bind distinct ephemeral UDP ports (`:0`) and advertise the real
  port. To avoid a reciprocal double-connect, the peer with the **lexicographically smaller fingerprint dials**
  (`if t.fp >= fp { return }`); the other accepts. (Note the *opposite* direction to mesh-rs's larger-id rule —
  same idea, different sign.)
- **The two problems the Go probes hand-solved** (BAKEOFF's most decision-relevant finding): symmetric mDNS makes
  **both peers try to dial at once** (solved by the smaller-fingerprint rule) and **mDNS stops re-querying after
  the first hit** — lan-go solves that with a **fresh short-lived resolver per browse round** (a 3 s browse + 1 s
  pause, looped), so late-arriving/reconnecting peers are rediscovered — *both of which iroh hides for free*. (The
  shared-go daemon also grace-waits for a peer before `Broadcast`, so a `send` immediately after startup isn't
  dropped.)

### 2.4 `experiments/libp2p-mesh` — parked

Files: `experiments/libp2p-mesh/internal/meshtransport/`. Binary `libp2p-mesh`. Same `shared-go` daemon; only the
transport differs.

- **Transport/topology** — `go-libp2p` **v0.48.0** + `go-libp2p-pubsub` **v0.17.0** **gossipsub**
  (`NewGossipSub` with no options, so `floodPublish` stays at its library default — on). Envelopes are published
  to a topic `clip-exp/<sha256(room)>` (`topic.Publish`, 5 s timeout). The real "first send after startup isn't
  dropped before subscription propagates" guard lives one layer up in the **shared-go daemon's `waitForPeers`**
  (poll every 100 ms up to 5 s, then a 300 ms settle grace) — used by lan-go too — plus the daemon pre-marks its
  own `msg_id` seen before broadcasting so gossipsub's self-delivery is suppressed.
- **Identity** — a persisted libp2p private key (`<config-dir>/libp2p.key`, an Ed25519 key on first run); identity
  = the **PeerId**. Host listens on `/ip4/0.0.0.0/tcp/0` (ephemeral TCP).
- **Discovery** — libp2p mDNS (`p2p/discovery/mdns`) with a room-derived service tag `clipexp<sha256(room)[:16]>`.
  Dedup dialing: the peer with the **smaller PeerId dials** (`t.host.ID() >= pi.ID → return`); the other accepts.
  Its mDNS re-query workaround is a **`connectPeer` retry: up to 8 attempts 750 ms apart**, since libp2p mDNS
  doesn't re-query on a fixed interval once it has a response.
- **Verdict** — works and gives N-peer cleanly, but pulls the full pion/WebRTC stack (**~140 transitive deps →
  37–39 MB**) for **no Phase-0 advantage over `lan-go`**, plus the same dial/discovery taming. **Parked** unless
  browser / multi-language reach becomes a hard requirement (its true, here-unused edge).

---

## 3. How it connects — the cross-comparison

### 3.1 The full横向 comparison

| Dimension | `mesh-rs` (`clip`) | `room-go` (`room`) | `lan-go` (`lan`) | `libp2p-mesh` (parked) |
|---|---|---|---|---|
| **Topology** | true P2P mesh, no server | central SSH room server | P2P mesh, no server | gossipsub P2P mesh |
| **Transport** | iroh `=1.0.2`, direct QUIC bidi | wish/`ssh` relay (raw session pipe) | quic-go v0.60 uni-streams | libp2p + gossipsub |
| **Identity** | Ed25519 `EndpointId` (`secret.key`) | SSH pubkey fp (`id_ed25519`) | SHA-256 of self-signed TLS cert | libp2p `PeerId` (`libp2p.key`) |
| **LAN discovery** | mDNS address-lookup, room-scoped service + ALPN | none (dial a server) | `grandcat/zeroconf` `_clip-lan._udp`, TXT `room=` | libp2p mDNS, room-tag service |
| **Cross-internet path** | iroh relay/hole-punch (a config flag away; not built) | works today over the SSH tunnel | DIY relay (not built) | libp2p relay/NAT (not built) |
| **N-peer** | pairwise direct QUIC to each peer (gossip deferred) | server fan-out (native) | per-peer QUIC conns | gossipsub topic (native) |
| **Image/file transfer** | **announce + pull by BLAKE3** over a direct stream | inline `blob_data` via server | inline `blob_data` | inline `blob_data` |
| **Clipboard lib** | `arboard` (PNG↔RGBA via `image`) | `golang.design/x/clipboard` | shared-go `clip` (same Go lib) | same as lan-go |
| **TUI + inline images** | ratatui + ratatui-image — **real Kitty/iTerm2/Sixel thumbnails** (half-block fallback) | bubbletea — metadata **stub** | bubbletea (shared-go) — metadata **stub** | same stub |
| **Headless** | ✅ degrades cleanly | ✅ (server needs no display) | ✅ | ✅ |
| **`remote` method** | **iroh ticket pair** (`scripts/remote.sh`) | **SSH tunnel + join** (native Go) | **mDNS on shared LAN** (`scripts/remote.sh`) | — |
| **Binary size** | 24 MB rel (20 stripped) / 79 debug | **10 MB** | 13 MB | 37 MB |
| **Idle RSS** | 18.0 MB | **8.2 MB** | 12.0 MB | 19.9 MB |
| **Privacy** | E2E by default (no server) | server sees plaintext (unless E2E added) | E2E on LAN | E2E |
| **Dependency weight** | 557 crates, >7 min iroh compile | small charm stack | quic-go + zeroconf | ~140 deps (pion/WebRTC) |

Numbers are the Phase-0 measurements from [BAKEOFF.md](BAKEOFF.md) (macOS, this host). The bake-off caveat holds:
the LAN-only Phase 0 rewards zero-config mesh and can't yet credit the headliners' whole reason to exist
(mesh-rs's effortless cross-internet P2P; room-go's history + zero-install SSH tier).

### 3.2 The three `remote` bootstrap+connect flows, step by step

All three are `<tool> remote <ssh-host>` (VSCode-Remote-SSH style); the mechanics differ by transport.

| Step | `room remote` (native Go) | `clip remote` (via `remote.sh`) | `lan remote` (via `remote.sh`) |
|---|---|---|---|
| 1. Detect / install | `ssh uname -sm` → GOOS/GOARCH; cross-build `room` (CGO_ENABLED=0) or scp running exe → `~/.local/bin/room` | `ensure_remote_bin` → `install.sh --remote` (Go cross-compile+scp; clip built on remote via cargo) | same as clip |
| 2. Start remote side | `room server` on `127.0.0.1:<rport>` (setsid+nohup; reused if listening) | remote **clip daemon** in `--room` (setsid+nohup) | remote **lan daemon** in `--room` (setsid+nohup) |
| 3. Bridge | **SSH `-N -L <lport>:127.0.0.1:<rport>`** tunnel (backgrounded, port auto-bumped) | **fetch the daemon's `pair --new --json` ticket** over ssh | (nothing — rely on the LAN) |
| 4. Connect locally | `join room@127.0.0.1:<lport>` through the tunnel | local `clip pair <ticket>` → **direct QUIC** | local daemon in the same `--room` **auto-connects over mDNS** |
| 5. State / teardown | per-host state file under `<config-dir>/remote/`; `--stop` kills the tunnel group + server it started | state under `~/.cache/cpc/remote/<tool>@<host>/`; `down` kills the remote daemon | same as clip |

Concretely, `clip remote` / `lan remote` are thin: the `clip` binary's `cmd_remote` locates `scripts/remote.sh`
(via `CPC_REMOTE_HELPER`, the exe dir, `~/.local/libexec/cpc/`, or walking up for `scripts/`) and `exec`s
`bash remote.sh clip <host> …`. `room remote` reimplements none of SSH — it shells out to the local `ssh`/`scp`
so your `~/.ssh/config`, keys, agent, and ProxyJump all Just Work.

---

## 4. Cross-machine & remote plumbing

Four pieces let a single dev box drive (or simulate) real multi-machine hand-off.

### 4.1 `scripts/remote.sh` — the shared engine

One script, three tools (`scripts/remote.sh <clip|room|lan> <host> [up|down|status] [--room R] [--rport N]
[--lport N]`). State lives under `~/.cache/cpc/remote/<tool>@<host>/`.

- **`room`** → `exec`s the native `room remote <host>` (SSH `-L` tunnel + `join`; see §3.2).
- **`clip`** → `start_remote_daemon` (setsid+nohup `clip … daemon --foreground`), `remote_ticket` fetches
  `pair --new --json` over ssh, local `clip pair <ticket>` → **direct QUIC over iroh**.
- **`lan`** → `start_remote_daemon`, then the local daemon in the same `--room` **auto-connects over mDNS** (the
  script polls `peers` up to ~12 s).

`ensure_remote_bin` bootstraps the binary through `install.sh --remote` if `~/.local/bin/<tool>` is missing.

### 4.2 `scripts/install.sh [--remote HOST]`

Builds and installs to `~/.local/bin` (or a `--remote` host's). **Go tools** are cross-compiled locally
(`CGO_ENABLED=0 GOOS/GOARCH`) and `scp`'d; **clip** is built *on the remote* with cargo (which must have a
toolchain — iroh compiles there). It also installs `remote.sh` + `install.sh` to `~/.local/libexec/cpc/` so
`<tool> remote` resolves the engine even outside the repo.

### 4.3 `docker/` — cross-machine simulated on one host

`docker/build.sh` cross-compiles static `room` + `lan` into `docker/stage/`; `docker/compose.yml` runs each
container as a "device": **room-go** = `server` + `alice` + `bob` (central SSH room), **lan-go** = `lan-a` +
`lan-b` (mDNS on the docker network). `docker/demo.sh` drives a real text + image (hash-checked) + file
(via the `save_dir` sink) hand-off across containers. **Honest limit**: mDNS multicast is often filtered on a
docker bridge network, so `lan` may not cross it (the demo warns instead of failing) — **room-go is the reliable
sandbox path** because it dials a server, not multicast.

### 4.4 `scripts/xmachine.sh` — the real two-machine test

Deploys to a **real LAN host** (default `local_ubuntu`, may be **headless**) and drives a genuine cross-machine
hand-off (this Mac → the box), verifying the PNG by hash. Go impls cross-compile + scp; Rust rsyncs the source and
`cargo build`s on the box. room-go starts a server there + joins both clients; mesh-rs/lan-go rely on mDNS
auto-discovery (with a ticket fallback for mesh-rs if multicast doesn't cross). This is what BAKEOFF's cross-machine
row measures.

### 4.5 The honest limits

- **Binary bootstrap** needs either the repo (a local `go`/`cargo` toolchain to cross-build) **or** a matching-arch
  host to copy the running binary — `room remote` fails with an actionable message otherwise.
- **The QUIC mesh tools (`clip`, `lan`) need a shared LAN** for the zero-config path; **cross-internet is the relay
  path, deferred** (mesh-rs's `internet=on` / iroh relay is "a config flag away" but not built; lan-go's is DIY).
- **`room` works over the tunnel today** — SSH reaches any host, so `room remote` is the one path that already
  spans the internet, at the cost of running/trusting a server.

---

## 5. Where the code diverges from the contract (worth reconciling)

- **CBOR everywhere.** PROTOCOL §1 permits JSON for the experiments, but **all four impls encode the envelope as
  CBOR** on the wire (and for IPC). No impl uses JSON — the `--json` flag is only for CLI *output*.
- **Config-dir name = module, not binary.** SPEC §5 says the config dir is `<bin>`, but mesh-rs uses
  `ProjectDirs("mesh-rs")` (binary is `clip`) and room-go uses `os.UserConfigDir()/room`. So the on-disk dir for
  `clip` is `…/mesh-rs/`, not `…/clip/`.
- **Opposite dial tie-breaks.** mesh-rs dials from the **larger** `EndpointId`; lan-go/libp2p dial from the
  **smaller** fingerprint/PeerId. Both are correct (deterministic, single-initiator) but the sign is inconsistent
  across impls.
- **Echo rule 2 is dormant.** The last-written-hash suppression is implemented and (in mesh-rs) unit-tested, but
  it only matters once `broadcast_on_copy` (local clipboard watching) exists — which is not built. In shared-go
  the `lastCopy` hash is recorded in three places but **never read** anywhere; in mesh-rs `is_echo` is
  `#[allow(dead_code)]`. So half the dedupe design is present-but-inert until Phase 1/3.
- **Inline images in the Go TUIs.** Only mesh-rs renders real inline thumbnails; room-go/lan-go/libp2p-mesh TUIs
  render a metadata placeholder (documented stubs).
- **`room` announce+pull.** room-go, lan-go and libp2p all inline `blob_data` (Phase 0); only mesh-rs does the
  PROTOCOL §1 announce+pull. Bringing the Go impls onto pull-by-hash is a Phase-1 item.
