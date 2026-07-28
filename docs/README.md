# docs/

Documentation for the cross-platform clipboard / file hand-off tool. Three usable implementations
share **one product contract** (same CLI, same wire model); only the binary name and the transport
differ.

## Which tool should I use?

| Tool | Binary | Transport | Use it when… |
|---|---|---|---|
| **[mesh-rs](usage-mesh-rs.md)** | `clip` | iroh P2P (QUIC) | You want **no server, works anywhere, private by default** — your own devices, LAN or across the internet. |
| **[room-go](usage-room-go.md)** | `room` | SSH/wish central server | **One small server is fine** and you want history + a zero-install SSH tier. |
| **[lan-go](usage-lan-go.md)** | `lan` | quic + mDNS (experiment) | A **minimal LAN-only probe** — zero-config on one LAN, no internet path. |

Not sure? Start with **`clip`** (mesh-rs) for personal multi-device use, or **`room`** (room-go) if
you already run a server. `lan` is a bake-off experiment, not a shipping target.

## Contents

**The shared contract** (read these to understand any implementation):
- **[SPEC.md](SPEC.md)** — the product contract: CLI surface (§2), sinks & auto-copy (§3), config keys
  (§5), OS matrix (§6), sessions & clearing (§8).
- **[PROTOCOL.md](PROTOCOL.md)** — wire envelope, type sniffing, identity/rooms/trust, local IPC.
- **[BAKEOFF.md](BAKEOFF.md)** — evaluation rubric, measured facts, and the finalist decision.
- **[implementation.md](implementation.md)** — how it's actually built: the shared daemon/IPC/envelope
  architecture, per-impl internals (mesh-rs, room-go, lan-go, parked libp2p-mesh), the cross-transport
  comparison tables, and the cross-machine / `remote` plumbing. Deeper than the usage guides.
- **[alternatives.md](alternatives.md)** — prior art & alternatives: the `ccimg` / `sshimg.nvim`
  projects that inspired this (and how they work), how we differ, and the wider ecosystem
  (OSC 52, kitty OSC 5522, AirDrop, Syncthing, Taildrop, LocalSend, magic-wormhole, …).

**Per-tool usage guides** (install, connect, full command reference, sinks, sessions, TUI keys,
troubleshooting):
- **[usage-mesh-rs.md](usage-mesh-rs.md)** — `clip` (Rust/iroh P2P): mDNS auto-discovery + ticket
  fallback, inline-image TUI, headless notes.
- **[usage-room-go.md](usage-room-go.md)** — `room` (Go/wish server): run a server, `join`, the
  SSH-key identity model.
- **[usage-lan-go.md](usage-lan-go.md)** — `lan` (Go/quic+mDNS): zero-config LAN discovery
  (experiment).

The usage guides cross-link to SPEC/PROTOCOL rather than restating the full contract. Repo-level
overview lives in the [root README](https://github.com/daviddwlee84/clipboard-handoff/blob/main/README.md).
