# PROTOCOL — wire envelope, framing, and local IPC

This defines the **transport-agnostic** message model every implementation shares, plus the local IPC contract
between the thin clients and the daemon. Individual transports (iroh gossip/blobs, wish/SSH, quic+mDNS,
libp2p gossipsub) carry these envelopes their own way, but the envelope shape and the image rules are common so
that the bake-off compares like with like.

## 1. The envelope

Logical fields (encode as CBOR on the wire for the binary impls; JSON is acceptable for experiments as long as
the fields match):

```
Envelope {
  v:          u16          // protocol version, currently 1
  msg_id:     string       // ULID (lexicographically sortable, time-based) — the dedupe key
  type:       "text" | "image"
  mime:       string       // "text/plain; charset=utf-8" | "image/png"
  sender:     string       // stable peer identity (see §3): Ed25519 node id / TLS cert fp / ssh key fp
  device_name:string       // human label, advisory only
  ts:         u64          // unix milliseconds at the sender
  // exactly one of the following, per `type`:
  text?:      string       // type=text: the UTF-8 payload, inline
  blob?: {                 // type=image: content-addressed reference
    hash:     string       // BLAKE3 hex of the PNG bytes — also the integrity check
    size:     u64          // PNG byte length
    w:        u32
    h:        u32
  }
}
```

Rules:
- **Text** payloads ride **inline** in `text`. (A soft cap of 1 MiB inline; larger text may be sent as a blob with
  `mime: text/plain` at the impl's discretion — not required for Phase 0.)
- **Images** are **always PNG on the wire.** The sender encodes to PNG (from raw RGBA / whatever the OS clipboard
  gave it); the receiver decodes PNG → the OS-native clipboard image format. `blob.hash` is BLAKE3 of the exact
  PNG bytes and MUST be verified on receipt.
- Image bytes are **not** flooded through a gossip/broadcast channel. The sender broadcasts the small envelope
  (announcement); each receiver **pulls** the PNG bytes over a direct stream keyed by `blob.hash` (mesh: iroh-blobs
  or a direct QUIC stream; room: fetch from the server). Experiments MAY inline-chunk small images for simplicity.
- **Phase 0 inline extension:** an impl MAY carry the PNG bytes inline in an optional `blob_data` field (alongside
  `blob.hash`/`size`/`w`/`h`) instead of announce+pull, for small test images. PNG stays canonical and `blob.hash`
  is still verified on receipt. `room-go` Phase 0 uses this; announce+pull is the Phase 1 target. Receivers that
  understand only announce+pull must ignore unknown fields gracefully.

## 2. Type sniffing (`--auto`)

- If the first bytes match a **PNG** signature (`89 50 4E 47 0D 0A 1A 0A`) or **JPEG** (`FF D8 FF`) → treat as image.
  JPEG is transcoded to PNG before sending (canonical wire format is PNG).
- Else if the whole payload is valid UTF-8 → treat as text.
- Else → error (`2`), unless `--image` forces raw bytes (future: arbitrary blobs).

## 3. Identity, rooms, trust

- **Identity** is a stable per-device key fingerprint:
  - `mesh-rs`: iroh **Ed25519 NodeId**.
  - `lan-go`: SHA-256 fingerprint of a self-signed **TLS cert** (persisted; Syncthing-style).
  - `libp2p-mesh`: libp2p **PeerId**.
  - `room-go`: the client's **SSH public-key** fingerprint (the server authorizes by key).
- **Room** = a shared secret string. mesh impls derive the gossip/topic id as `hash(room_secret)`; the room impl
  uses a named channel on the server. Only holders of the secret (mesh) / an allowlisted key in the channel (room)
  participate.
- **Allowlist (TOFU):** each daemon persists a set of trusted `sender` ids. First envelope from an unknown sender
  is **held pending approval**; nothing from it is auto-copied until the user approves (CLI prompt / TUI). Every
  envelope is authenticated by the transport's peer identity (signed gossip / mutual TLS / SSH), so `sender` is
  trustworthy for allowlist checks.

## 4. Local IPC (client ⇄ daemon)

Transport: Unix domain socket (macOS/Linux) at `$XDG_RUNTIME_DIR/<bin>/daemon.sock` (fallback temp dir) /
named pipe `\\.\pipe\<bin>-daemon` (Windows). Framing: **length-prefixed** (4-byte big-endian length) CBOR
request/response + an event stream. One request per connection, except `recv --follow` / `tui` which hold the
connection open and receive an event stream.

Requests (client → daemon):
```
Req = Send{ type, mime, bytes }            // bytes = raw stdin; daemon sniffs + broadcasts
    | RecvLatest{ kind: any|text|image }   // returns the newest buffered item (or blocks for next if --follow)
    | Paste                                 // daemon writes latest buffered item to OS clipboard
    | Subscribe                             // opens an event stream (recv --follow, tui)
    | Peers | Status
    | ConfigSet{ key, value } | ConfigGet{ key }
    | PairNew | Pair{ ticket } | Join{ target }
```
Responses / events (daemon → client):
```
Resp  = Ok{ ... } | Err{ code, message }
Event = Item{ envelope, local_path? }      // a received item (local_path set once blob is fetched)
      | PeerUp{ id, name } | PeerDown{ id }
      | PairPrompt{ id, name }             // unknown peer awaiting approval
      | Toast{ text }
```

If no daemon is listening, the client **spawns** `BIN daemon` (detached), waits for the socket (short backoff),
then retries. `--foreground` runs the daemon in the client's terminal for debugging.

## 5. Versioning

`v` is bumped on any breaking envelope change. Daemons reject envelopes with an unknown major `v` with `Err`.
IPC is versioned alongside; a client and daemon of mismatched IPC versions must fail loudly, not silently.
