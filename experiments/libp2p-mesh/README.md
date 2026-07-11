# libp2p-mesh — go-libp2p gossipsub + mDNS mesh probe

Binary: `libp2p-mesh`. Transport: **go-libp2p** with **gossipsub** for the mesh
and libp2p **mDNS** for LAN peer discovery. Answers the BAKEOFF question: *iroh
vs libp2p at the same topology — config surface, discovery/dial ergonomics,
dependency weight.*

## How it works

- **Identity (PROTOCOL §3):** a persisted libp2p private key
  (`<config-dir>/libp2p.key`); the device identity is the **libp2p PeerId**.
- **Discovery:** libp2p mDNS (`p2p/discovery/mdns`) with a service tag derived
  from the room (`clipexp<sha256(room)[:16]>`), so only same-room peers find each
  other on the LAN.
- **Topology/transport:** gossipsub (`go-libp2p-pubsub`). Envelopes are
  published to a topic derived from the room (`clip-exp/<sha256(room)>`). Because
  gossipsub `floodPublish` is on by default, a locally-published message reaches
  **all** topic subscribers immediately, without waiting for the mesh to graft.
  Images ride **inline in `blob_data`** for Phase 0; `blob.hash` (BLAKE3) is
  verified on receipt.
- **Same-host / dup-dial:** two instances listen on distinct ephemeral TCP
  ports. To avoid simultaneous-connect TLS handshake collisions (both peers
  dialing at once), the peer with the **smaller PeerId dials** (with retry); the
  other accepts.

## CLI (shared surface, SPEC §2)

```
libp2p-mesh [--config-dir P --socket P --room NAME --json -q -v] <cmd>
  daemon [--foreground]      send [--text|--image|--auto]   recv [--follow] [--latest-image --emit-path] [--out P]
  paste                      status [--json]                peers [--json]
  config set/get KEY [VALUE] pair            # pair is a no-op: discovery is automatic (mDNS)
  tui                        # messenger-style chat TUI (SPEC §4); keybindings in experiments/README.md
```

## Bake-off harness hook

Discovery is automatic, so **no `start_pair()` override is needed** — the
harness's default hook already does exactly what this probe wants:

```sh
# scripts/roundtrip.sh default start_pair(), unchanged:
libp2p-mesh --config-dir RUN/a --socket RUN/a/d.sock --room ROOM daemon --foreground &   # device A
libp2p-mesh --config-dir RUN/b --socket RUN/b/d.sock --room ROOM daemon --foreground &   # device B
# (the harness then tries `pair --new --json`; pair is a no-op → harmless
#  "WARN pairing hook not wired yet". The two daemons auto-connect over mDNS.)
```

Build target expected by the harness: `experiments/libp2p-mesh/bin/libp2p-mesh`
(`go build -o bin/libp2p-mesh ./cmd/libp2p-mesh`).

## Verified round-trip (observed)

```
$ scripts/roundtrip.sh libp2p-mesh
roundtrip[libp2p-mesh]: text OK
roundtrip[libp2p-mesh]: image OK (hash-equal)
roundtrip[libp2p-mesh]: PASS ✅
```

The shared daemon's `send` waits briefly (up to 5s) for a topic peer before
publishing, so the first send right after startup isn't dropped before gossipsub
discovery + subscription propagation completes.
