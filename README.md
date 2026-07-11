# cross-platform-copy

A cross-platform **CLI/TUI for handing off clipboard content — text *and* images — between your own
devices**, over one persistent connection. Copy or pipe something on one machine; it becomes instantly
pasteable on another. Think AirDrop/Handoff, but for terminals, cross-platform, and scriptable.

This repo is a **technical bake-off**: several independent implementations of the same product contract,
so we can compare which architecture is actually nicest to use.

## The contenders

| Dir | Lang | Topology | The bet |
|---|---|---|---|
| [`mesh-rs/`](mesh-rs/) | Rust | P2P mesh, no server | Batteries-included P2P via **iroh** (NAT traversal, relay, blobs for free) |
| [`room-go/`](room-go/) | Go | Central room server | **charmbracelet/wish** SSH room; zero-install text tier + native client for images; easy history |
| [`experiments/`](experiments/) | Go | (various) | Small transport probes — `lan-go` (quic+mDNS), `libp2p-mesh` — to quantify iroh's value |

All implementations obey one **shared contract**:

- [`docs/SPEC.md`](docs/SPEC.md) — CLI surface, UX flows, auto-copy semantics, OS support matrix
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md) — the transport-agnostic wire envelope + image (PNG) rules
- [`docs/BAKEOFF.md`](docs/BAKEOFF.md) — evaluation rubric + scorecard

## The one cross-cutting truth

> **Images can only auto-land on a device's OS clipboard via a native, resident agent on that device.**
> Terminal escapes (OSC 52) carry **text only** (~74 KB, tmux-stripped). So every implementation runs a
> per-device daemon that owns the clipboard; the only real difference between them is the **transport**.

## Status

Early. See `docs/` for the contract and [`.claude/plans/`](.claude/plans/) for the full plan. Phase 0 goal:
two devices on a LAN, copy/pipe text or image on one → notify + one-key paste on the other.

## Quick start (per impl)

```sh
# same CLI surface everywhere; only the binary name differs (clip / room / lan / libp2p-mesh)
echo "hello" | <bin> send                 # broadcast piped stdin
<bin> send < testdata/small.png           # broadcast an image (auto-detected)
<bin> recv --follow                       # print incoming text to stdout
<bin> paste                               # write latest received item to the OS clipboard
<bin> tui                                 # messenger-style chat window
```

See each subdir's README for build/run instructions.
