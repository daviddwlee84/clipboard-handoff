# BAKEOFF — evaluation rubric & scorecard

Several implementations of the same product contract, so we can see which architecture is nicest to *use* and
then invest the expensive all-OS/1.0 hardening **only in the winner**. This is a **Phase 0 snapshot**: every impl
is at the same MVP depth (LAN, 2 devices, text+image hand-off via `send`/`recv`/`paste`, notify-first). Read the
caveats — Phase 0 only exercises the LAN path, so it under-credits the headliners' Phase-3 differentiators.

## Measured facts (Phase 0, macOS, this host)

| | `mesh-rs` (Rust/iroh) | `room-go` (Go/wish) | `lan-go` (Go/quic+mDNS) | `libp2p-mesh` (Go) |
|---|---|---|---|---|
| **Round-trip harness** (`scripts/roundtrip.sh`) | PASS ✅ | PASS ✅ | PASS ✅ | PASS ✅ |
| text delivered | ✅ | ✅ | ✅ | ✅ |
| PNG BLAKE3 hash-equal | ✅ | ✅ | ✅ | ✅ |
| unit tests | 9/9 | wire+broker+daemon | shared-go suite | shared-go suite |
| **Binary size** (as built) | **24 MB** release (20 MB stripped) · 79 MB debug | **10 MB** | 13 MB | 37 MB |
| **Idle daemon RSS** | 18.0 MB | **8.2 MB** | 12.0 MB | 19.9 MB |
| **Build weight** | iroh release compile **> 7 min** | seconds | seconds | seconds (~140 deps) |
| transport as built | ticket → direct QUIC bidi (gossip/blobs deferred) | SSH/wish relay via server | quic-go + mDNS auto-discovery | gossipsub + mDNS |
| identity | Ed25519 NodeId | SSH pubkey fp | TLS cert fp | libp2p PeerId |
| LAN pairing steps | 1 (ticket copy) | join server (+key) | **0 (mDNS auto)** | **0 (mDNS auto)** |

Notes: `libp2p-mesh` pulls the full pion/WebRTC stack (~140 transitive deps) even for LAN-only TCP → the 37 MB.
Both Go mesh probes independently had to hand-solve two things — **dedup dialing** (symmetric mDNS → both peers
dial at once; "smaller id dials" rule) and **discovery retry** (mDNS stops re-querying after first hit) — that
**iroh hides for free**. That is the single most decision-relevant finding of the bake-off.

## Cross-machine — mac ↔ headless Ubuntu 24.04, real LAN (`scripts/xmachine.sh`)

The three 1.0 targets were deployed to a **display-less** Ubuntu box (Go: `CGO_ENABLED=0` cross-compile + scp;
Rust: rsync + `cargo build` on the box) and driven through a real cross-machine hand-off (this Mac → Ubuntu),
verifying the PNG by hash. All pass; the headless daemons run fine and degrade the (absent) clipboard gracefully.

| | connectivity across machines | text | image (hash-equal) | headless |
|---|---|---|---|---|
| `mesh-rs` | iroh mDNS auto-discovery (ticket fallback) | ✅ | ✅ | ✅ clipboard degrades cleanly |
| `room-go` | client dials server LAN IP | ✅ | ✅ | ✅ (server needs no display) |
| `lan-go` | mDNS auto-discovery | ✅ | ✅ | ✅ |

Caveat: `mesh-rs` mDNS auto-discovery is timing-sensitive on a multi-interface host (Tailscale + LAN); the ticket
fallback (direct QUIC over the LAN address) is reliable. `room-go` required a real fix — it generated its SSH host
key in a way that chmod-ed `/tmp` (EPERM on a server); now fixed.


## Scorecard

Scores 1 (poor) – 5 (excellent), grounded in the Phase 0 evidence above. Rows tagged **[built]** are exercised by
Phase 0; **[arch]** rows score architectural potential of a Phase-3 feature not yet built. Weights are adjustable.

| Dimension | W | `mesh-rs` | `room-go` | `lan-go` | `libp2p-mesh` |
|---|---:|:---:|:---:|:---:|:---:|
| Pairing effort **[built]** | 3 | 4 | 3 | 5 | 5 |
| Time-to-first-message **[built]** | 2 | 4 | 4 | 4 | 3 |
| LAN zero-config **[built]** | 3 | 3¹ | 2 | 5 | 5 |
| Offline LAN **[built]** | 2 | 5 | 4 | 5 | 5 |
| Cross-internet **[arch]** | 2 | 5 | 5 | 2 | 4 |
| Image fidelity **[built]** | 3 | 5 | 5 | 5 | 5 |
| Latency feel **[built]** | 2 | 5 | 4 | 5 | 4 |
| Zero-install story **[arch]** | 1 | 2 | 4² | 2 | 2 |
| Resource use (idle RSS) **[built]** | 1 | 3 | 5 | 4 | 3 |
| Binary size **[built]** | 1 | 3³ | 5 | 4 | 2 |
| Privacy (E2E by default?) **[arch]** | 2 | 5 | 2⁴ | 5 | 5 |
| History/retention **[arch]** | 1 | 2 | 5⁵ | 2 | 2 |
| Setup/ops burden **[built]** | 2 | 5 | 2 | 5 | 5 |
| Multi-peer **[built]** | 2 | 3⁶ | 5 | 4 | 5 |
| Dev/maintenance cost **[built]** | 1 | 2 | 4 | 5 | 2 |
| **Weighted total / 140** | | **112 (80%)** | **105 (75%)** | **122 (87%)** | **118 (84%)** |

¹ iroh supports local mDNS discovery; the Phase-0 impl used tickets. Enabling local discovery (small Phase-1 add)
would raise this to ~5. ² room-go's bare-`ssh room@server` tier (Phase 1) needs nothing installed on the client
for text. ³ mesh-rs release binary is 24 MB (20 MB stripped) vs room-go 10 MB. ⁴ the server sees plaintext unless
client-side E2E is added. ⁵ server-side SQLite history is natural (Phase 1) — the others have no shared durable
store. ⁶ mesh-rs Phase 0 is pairwise direct QUIC; N-peer needs iroh-gossip (Phase 1).

## How to read the numbers (important)

The Phase-0 totals **reward zero-config LAN** and **cannot yet credit the headliners' whole reason to exist**:
`mesh-rs`'s effortless *cross-internet* P2P (no server, no infra, E2E) and `room-go`'s *retained history* +
*zero-install SSH* tier are Phase-3/Phase-1 features, scored as [arch] potential only. So the raw ranking
(`lan-go` > `libp2p-mesh` > `mesh-rs` > `room-go`) really says: **"on a LAN, the scrappy zero-config mesh feels
best and costs least."** That is a genuine, useful result — but it is not the whole product.

## Qualitative

- **`mesh-rs`** — cleanest match to "my own devices, anywhere, no server, private by default." The Phase-0 cost is
  visible: 79 MB debug binary and a >7 min iroh release compile, API churn (pinned `iroh =1.0.2`). But it is the
  only impl that gets cross-internet NAT traversal + relay + content-addressed blobs *for free* later, and the two
  hard mesh problems the Go probes hand-solved are non-issues here.
- **`room-go`** — leanest binary (10 MB) and idle RSS (8.2 MB), and the only one with a natural path to **history**
  and a **zero-install** client. Cost: you run and trust a server (it sees plaintext unless E2E is added), and LAN
  use isn't zero-config (a server must be reachable).
- **`lan-go`** — the surprise. Zero-config on the LAN, tiny, simplest code, top Phase-0 score. Weakness is exactly
  what iroh hides: its **cross-internet story is DIY** (self-hosted relay / hole-punch) — the moment you leave the
  LAN, you start rebuilding `mesh-rs`.
- **`libp2p-mesh`** — works, gossipsub gives N-peer cleanly, but **37 MB / ~140 deps** for no Phase-0 advantage over
  `lan-go`, plus the same dial/discovery taming. Only compelling if browser/multi-language reach becomes a hard
  requirement (its true edge, unused here).

## Decision

The **finalists are `mesh-rs` and `room-go`** — the two opposed *product* bets. The experiments settled their job:
- `lan-go` proves the LAN experience can be excellent and nearly free — a strong **embedded LAN fast-path**, but not
  a whole-internet product on its own.
- `libp2p-mesh` proves libp2p buys little here for a big size/complexity tax — **park it** unless browsers matter.

Pick the winner by the axis that matters most (this determines who gets Phase 3–4 hardening):
- **No server, works anywhere, private by default** → **`mesh-rs`** (fold in `lan-go`'s mDNS zero-config as iroh's
  local-discovery fast path).
- **One small server is fine; want zero-install SSH + retained history** → **`room-go`**.

_Winner: pending user selection._
