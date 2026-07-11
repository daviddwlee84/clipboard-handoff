//! Transport-agnostic wire model (docs/PROTOCOL.md §1), the local IPC contract (§4),
//! type-sniffing (§2), length-prefixed CBOR framing, tickets, and echo-suppression.

use std::collections::HashSet;

use anyhow::{Context, Result, bail};
use serde::{Serialize, de::DeserializeOwned};
use serde_bytes::ByteBuf;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

/// Protocol version (PROTOCOL §1 / §5).
pub const PROTO_V: u16 = 1;
/// Max single frame (guards against a bad length prefix). Large enough for testdata images.
pub const MAX_FRAME: usize = 64 * 1024 * 1024;

// ---------------------------------------------------------------------------
// The envelope (PROTOCOL §1)
// ---------------------------------------------------------------------------

#[derive(Serialize, serde::Deserialize, Clone, Debug, PartialEq)]
pub struct Envelope {
    pub v: u16,
    pub msg_id: String, // ULID — the dedupe key
    #[serde(rename = "type")]
    pub typ: MsgType,
    pub mime: String,
    pub sender: String,      // stable peer identity (iroh EndpointId hex)
    pub device_name: String, // advisory
    pub ts: u64,             // unix ms at sender
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub text: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub blob: Option<Blob>,
}

#[derive(Serialize, serde::Deserialize, Clone, Copy, Debug, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum MsgType {
    Text,
    Image,
}

/// A content-addressed image reference. Image bytes are always PNG on the wire.
#[derive(Serialize, serde::Deserialize, Clone, Debug, PartialEq)]
pub struct Blob {
    pub hash: String, // BLAKE3 hex of the PNG bytes — the integrity check
    pub size: u64,
    pub w: u32,
    pub h: u32,
}

// ---------------------------------------------------------------------------
// Peer-to-peer wire messages (carried over iroh QUIC bidi streams)
// ---------------------------------------------------------------------------

#[derive(Serialize, serde::Deserialize, Debug)]
pub enum WireMsg {
    /// A small announcement (envelope only). Image bytes are pulled separately.
    Announce(Envelope),
    /// Pull the PNG bytes for a `blob.hash` (content-addressed).
    BlobRequest { hash: String },
    /// Response to a `BlobRequest`.
    BlobResponse { ok: bool, bytes: ByteBuf },
}

// ---------------------------------------------------------------------------
// Local IPC (client <-> daemon), PROTOCOL §4
// ---------------------------------------------------------------------------

#[derive(Serialize, serde::Deserialize, Debug, Clone, Copy)]
pub enum SniffKind {
    Auto,
    Text,
    Image,
}

#[derive(Serialize, serde::Deserialize, Debug, Clone, Copy, PartialEq)]
pub enum RecvKind {
    Any,
    Text,
    Image,
}

#[derive(Serialize, serde::Deserialize, Debug)]
pub enum Req {
    Send { kind: SniffKind, bytes: ByteBuf },
    RecvLatest { kind: RecvKind },
    Paste,
    /// Copy a *specific* buffered item (by ULID) to the OS clipboard. Used by the TUI's `y`
    /// key so the highlighted bubble — not just the latest — can be placed on the clipboard.
    PasteItem { msg_id: String },
    Subscribe,
    Peers,
    Status,
    ConfigSet { key: String, value: String },
    ConfigGet { key: String },
    PairNew,
    Pair { ticket: String },
}

#[derive(Serialize, serde::Deserialize, Debug)]
pub enum Resp {
    Ok(OkData),
    Err { code: i32, message: String },
}

#[derive(Serialize, serde::Deserialize, Debug)]
pub enum OkData {
    None,
    Text(String),
    /// Result of a `Send`: the ULID assigned to the broadcast item + how many peers it reached.
    /// The TUI uses `msg_id` to key its own outgoing bubble (so `y` can copy it back later).
    Sent { msg_id: String, reached: usize },
    Item {
        envelope: Envelope,
        local_path: Option<String>,
    },
    Ticket(String),
    Status(StatusInfo),
    Peers(Vec<PeerInfo>),
    Config(Option<String>),
}

#[derive(Serialize, serde::Deserialize, Debug, Clone)]
pub struct StatusInfo {
    pub endpoint_id: String,
    pub device_name: String,
    pub room: String,
    pub transport: String,
    pub auto_copy: String,
    /// `available` | `unavailable` — `unavailable` on a headless host (no display).
    pub clipboard: String,
    pub peer_count: usize,
    pub buffer_len: usize,
}

#[derive(Serialize, serde::Deserialize, Debug, Clone)]
pub struct PeerInfo {
    pub id: String,
    pub name: String,
    pub direct: bool,
}

/// Events streamed to a client after `Subscribe` (recv --follow / tui).
#[derive(Serialize, serde::Deserialize, Debug, Clone)]
pub enum Event {
    Item {
        envelope: Envelope,
        local_path: Option<String>,
    },
    Toast {
        text: String,
    },
    PeerUp {
        id: String,
        name: String,
    },
    PeerDown {
        id: String,
    },
}

// ---------------------------------------------------------------------------
// Length-prefixed CBOR framing (PROTOCOL §4): 4-byte big-endian length + CBOR.
// Generic over any tokio stream, so IPC and iroh streams share one codec.
// ---------------------------------------------------------------------------

pub async fn write_frame<W, T>(w: &mut W, value: &T) -> Result<()>
where
    W: AsyncWrite + Unpin,
    T: Serialize,
{
    let mut buf = Vec::new();
    ciborium::into_writer(value, &mut buf).context("cbor encode")?;
    if buf.len() > MAX_FRAME {
        bail!("frame too large: {} bytes", buf.len());
    }
    w.write_all(&(buf.len() as u32).to_be_bytes()).await?;
    w.write_all(&buf).await?;
    w.flush().await?;
    Ok(())
}

pub async fn read_frame<R, T>(r: &mut R) -> Result<T>
where
    R: AsyncRead + Unpin,
    T: DeserializeOwned,
{
    let mut len_buf = [0u8; 4];
    r.read_exact(&mut len_buf).await?;
    let len = u32::from_be_bytes(len_buf) as usize;
    if len > MAX_FRAME {
        bail!("frame length {len} exceeds max");
    }
    let mut buf = vec![0u8; len];
    r.read_exact(&mut buf).await?;
    ciborium::from_reader(&buf[..]).context("cbor decode")
}

// ---------------------------------------------------------------------------
// Type sniffing (PROTOCOL §2)
// ---------------------------------------------------------------------------

const PNG_MAGIC: [u8; 8] = [0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A];
const JPEG_MAGIC: [u8; 3] = [0xFF, 0xD8, 0xFF];

pub fn is_png(b: &[u8]) -> bool {
    b.len() >= 8 && b[..8] == PNG_MAGIC
}
pub fn is_jpeg(b: &[u8]) -> bool {
    b.len() >= 3 && b[..3] == JPEG_MAGIC
}

/// The canonical result of sniffing: text, or PNG image bytes (the canonical wire format).
pub enum Sniffed {
    Text(String),
    /// PNG bytes, decoded dimensions.
    Image { png: Vec<u8>, w: u32, h: u32 },
}

/// Sniff/normalize stdin bytes per PROTOCOL §2. JPEG is transcoded to PNG.
pub fn sniff(kind: SniffKind, bytes: &[u8]) -> Result<Sniffed> {
    match kind {
        SniffKind::Text => {
            let s = std::str::from_utf8(bytes).context("--text: input is not valid UTF-8")?;
            Ok(Sniffed::Text(s.to_string()))
        }
        SniffKind::Image => to_png_sniffed(bytes),
        SniffKind::Auto => {
            if is_png(bytes) || is_jpeg(bytes) {
                to_png_sniffed(bytes)
            } else if let Ok(s) = std::str::from_utf8(bytes) {
                Ok(Sniffed::Text(s.to_string()))
            } else {
                bail!("could not sniff type: not PNG/JPEG and not valid UTF-8")
            }
        }
    }
}

/// Normalize any supported image bytes to PNG and read dimensions.
fn to_png_sniffed(bytes: &[u8]) -> Result<Sniffed> {
    let img = image::load_from_memory(bytes).context("decode image")?;
    let w = image::GenericImageView::width(&img);
    let h = image::GenericImageView::height(&img);
    let png = if is_png(bytes) {
        bytes.to_vec()
    } else {
        let mut out = Vec::new();
        img.write_to(&mut std::io::Cursor::new(&mut out), image::ImageFormat::Png)
            .context("encode png")?;
        out
    };
    Ok(Sniffed::Image { png, w, h })
}

pub fn blake3_hex(bytes: &[u8]) -> String {
    blake3::hash(bytes).to_hex().to_string()
}

// ---------------------------------------------------------------------------
// Room -> ALPN. Folding the room secret into the ALPN enforces the same-room
// requirement at the QUIC layer: mismatched rooms simply cannot connect.
// ---------------------------------------------------------------------------

pub fn alpn_for_room(room: &str) -> Vec<u8> {
    let h = blake3::hash(room.as_bytes());
    let mut alpn = b"mesh-rs/clip/0/".to_vec();
    alpn.extend_from_slice(hex::encode(&h.as_bytes()[..8]).as_bytes());
    alpn
}

// ---------------------------------------------------------------------------
// Echo / loop suppression (SPEC §3)
// ---------------------------------------------------------------------------

/// Tracks seen `msg_id`s (dedupe) and the hash of the last value we wrote to our
/// own clipboard (so `on` mode never re-broadcasts what it just pasted).
#[derive(Debug)]
pub struct Dedup {
    seen: HashSet<String>,
    order: std::collections::VecDeque<String>,
    last_written: Option<String>,
    cap: usize,
}

impl Dedup {
    pub fn new(cap: usize) -> Self {
        Self {
            seen: HashSet::new(),
            order: std::collections::VecDeque::new(),
            last_written: None,
            cap,
        }
    }

    /// Record a msg_id. Returns true if this is the first time we've seen it
    /// (i.e. it should be processed), false if it's a duplicate.
    pub fn mark_seen(&mut self, msg_id: &str) -> bool {
        if self.seen.contains(msg_id) {
            return false;
        }
        self.seen.insert(msg_id.to_string());
        self.order.push_back(msg_id.to_string());
        while self.order.len() > self.cap {
            if let Some(old) = self.order.pop_front() {
                self.seen.remove(&old);
            }
        }
        true
    }

    /// Record the content hash we just wrote to our own OS clipboard.
    pub fn note_written(&mut self, hash: &str) {
        self.last_written = Some(hash.to_string());
    }

    /// Is `hash` equal to the last value we wrote to our own clipboard?
    /// (Used to ignore a local clipboard change that we caused ourselves — wired up
    /// once `broadcast_on_copy` clipboard-watching lands in Phase 1; unit-tested now.)
    #[allow(dead_code)]
    pub fn is_echo(&self, hash: &str) -> bool {
        self.last_written.as_deref() == Some(hash)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_bytes::ByteBuf;

    #[tokio::test]
    async fn envelope_cbor_round_trips() {
        let env = Envelope {
            v: PROTO_V,
            msg_id: "01ARZ3NDEKTSV4RRFFQ69G5FAV".into(),
            typ: MsgType::Image,
            mime: "image/png".into(),
            sender: "abcdef".into(),
            device_name: "laptop".into(),
            ts: 1_720_000_000_000,
            text: None,
            blob: Some(Blob {
                hash: "deadbeef".into(),
                size: 1234,
                w: 64,
                h: 64,
            }),
        };
        // via the actual frame codec used on the wire
        let mut buf = Vec::new();
        write_frame(&mut buf, &env).await.unwrap();
        let mut cur = std::io::Cursor::new(buf);
        let back: Envelope = read_frame(&mut cur).await.unwrap();
        assert_eq!(env, back);
    }

    #[tokio::test]
    async fn wire_msg_round_trips() {
        let m = WireMsg::BlobResponse {
            ok: true,
            bytes: ByteBuf::from(vec![1u8, 2, 3, 4]),
        };
        let mut buf = Vec::new();
        write_frame(&mut buf, &m).await.unwrap();
        let mut cur = std::io::Cursor::new(buf);
        let back: WireMsg = read_frame(&mut cur).await.unwrap();
        match back {
            WireMsg::BlobResponse { ok, bytes } => {
                assert!(ok);
                assert_eq!(bytes.into_vec(), vec![1, 2, 3, 4]);
            }
            _ => panic!("wrong variant"),
        }
    }

    #[test]
    fn sniff_png() {
        // 1x1 PNG produced by the image crate
        let mut png = Vec::new();
        let img = image::RgbaImage::from_pixel(1, 1, image::Rgba([10, 20, 30, 255]));
        image::DynamicImage::ImageRgba8(img)
            .write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png)
            .unwrap();
        assert!(is_png(&png));
        match sniff(SniffKind::Auto, &png).unwrap() {
            Sniffed::Image { w, h, png: out } => {
                assert_eq!((w, h), (1, 1));
                assert!(is_png(&out));
            }
            _ => panic!("expected image"),
        }
    }

    #[test]
    fn sniff_jpeg_transcodes_to_png() {
        // Build a tiny JPEG in-memory, then confirm sniff returns PNG bytes.
        let mut jpg = Vec::new();
        let img = image::RgbImage::from_pixel(2, 2, image::Rgb([200, 100, 50]));
        image::DynamicImage::ImageRgb8(img)
            .write_to(&mut std::io::Cursor::new(&mut jpg), image::ImageFormat::Jpeg)
            .unwrap();
        assert!(is_jpeg(&jpg));
        match sniff(SniffKind::Auto, &jpg).unwrap() {
            Sniffed::Image { png, .. } => assert!(is_png(&png), "jpeg should transcode to png"),
            _ => panic!("expected image"),
        }
    }

    #[test]
    fn sniff_utf8_text() {
        match sniff(SniffKind::Auto, "hello".as_bytes()).unwrap() {
            Sniffed::Text(s) => assert_eq!(s, "hello"),
            _ => panic!("expected text"),
        }
    }

    #[test]
    fn sniff_rejects_binary_garbage() {
        // invalid UTF-8, not an image
        let junk = [0xff, 0xfe, 0x00, 0x01, 0x80];
        assert!(sniff(SniffKind::Auto, &junk).is_err());
    }

    #[test]
    fn dedupe_by_msg_id() {
        let mut d = Dedup::new(8);
        assert!(d.mark_seen("a"));
        assert!(!d.mark_seen("a")); // duplicate suppressed
        assert!(d.mark_seen("b"));
        assert!(!d.mark_seen("b"));
    }

    #[test]
    fn echo_suppression_by_written_hash() {
        let mut d = Dedup::new(8);
        assert!(!d.is_echo("h1"));
        d.note_written("h1");
        assert!(d.is_echo("h1")); // we wrote this ourselves -> ignore local change
        assert!(!d.is_echo("h2"));
    }

    #[test]
    fn different_rooms_yield_different_alpns() {
        assert_ne!(alpn_for_room("default"), alpn_for_room("other"));
        assert_eq!(alpn_for_room("default"), alpn_for_room("default"));
    }
}
