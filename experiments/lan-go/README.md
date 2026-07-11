# lan-go — quic-go + mDNS LAN mesh probe

Binary: `lan`. Transport: **quic-go** direct connections + **mDNS/DNS-SD**
discovery (`github.com/grandcat/zeroconf`). Answers the BAKEOFF question: *is
iroh's weight worth it, or is a hand-rolled LAN mesh good enough?*

## How it works

- **Identity (PROTOCOL §3):** a persisted self-signed TLS cert
  (`<config-dir>/tls_cert.pem`); the device identity is the **SHA-256
  fingerprint of the cert DER** (Syncthing-style). Peers authenticate via mutual
  TLS on the QUIC connection (TOFU: any cert accepted, identified by fingerprint).
- **Discovery:** each daemon advertises a `_clip-lan._udp` service whose TXT
  record carries `room=`, `fp=`, `port=` (the real ephemeral QUIC port) and
  `name=`. It browses for the same service and connects only to peers whose
  `room` matches. Zero config on one LAN.
- **Transport:** envelopes ride **one-per-QUIC-uni-stream** (length-prefixed
  CBOR). Images ride **inline in `blob_data`** for Phase 0; `blob.hash` (BLAKE3)
  is verified on receipt.
- **Same-host / dup-dial:** two instances bind distinct ephemeral UDP ports
  (`:0`) and advertise the real port, so two daemons on one machine find each
  other. To avoid a reciprocal double-connect, the peer with the
  lexicographically **smaller fingerprint dials**; the other accepts.

## CLI (shared surface, SPEC §2)

```
lan [--config-dir P --socket P --room NAME --json -q -v] <cmd>
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
lan --config-dir RUN/a --socket RUN/a/d.sock --room ROOM daemon --foreground &   # device A
lan --config-dir RUN/b --socket RUN/b/d.sock --room ROOM daemon --foreground &   # device B
# (the harness then tries `pair --new --json`; lan's pair is a no-op → harmless
#  "WARN pairing hook not wired yet". The two daemons auto-connect over mDNS.)
```

Build target expected by the harness: `experiments/lan-go/bin/lan`
(`go build -o bin/lan ./cmd/lan`).

## Verified round-trip (observed)

```
$ scripts/roundtrip.sh lan-go
roundtrip[lan-go]: text OK
roundtrip[lan-go]: image OK (hash-equal)
roundtrip[lan-go]: PASS ✅
```

Manual clipboard path (macOS), which the harness does not exercise:
`lan … paste` places received text on the clipboard (verified with `pbpaste`);
`auto_copy on` places a received PNG on the clipboard (PNGf present via
`osascript`).
