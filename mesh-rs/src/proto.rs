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
    /// image/file: original file name, advisory (folder/save sinks). Optional + skipped when
    /// unset, so an older peer that doesn't know this field still decodes the envelope.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub filename: Option<String>,
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
    /// Arbitrary bytes: content-addressed like an image, but never clipboard-pasteable as an
    /// image — it lands in the folder sink / `recv --emit-path` (PROTOCOL §1).
    File,
}

/// A content-addressed blob reference (image PNG bytes, or arbitrary file bytes).
/// `w`/`h` are meaningful for images only (0 for files).
#[derive(Serialize, serde::Deserialize, Clone, Debug, PartialEq)]
pub struct Blob {
    pub hash: String, // BLAKE3 hex of the bytes — the integrity check
    pub size: u64,
    #[serde(default)]
    pub w: u32,
    #[serde(default)]
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
    File,
}

#[derive(Serialize, serde::Deserialize, Debug, Clone, Copy, PartialEq)]
pub enum RecvKind {
    Any,
    Text,
    Image,
}

#[derive(Serialize, serde::Deserialize, Debug)]
pub enum Req {
    Send {
        kind: SniffKind,
        bytes: ByteBuf,
        /// image/file: the original name (from `PATH` or `--name`).
        #[serde(default)]
        filename: Option<String>,
    },
    /// `clip clear [--all]` (SPEC §8): transient always; `all` also reverts session sink writes.
    Clear {
        all: bool,
    },
    /// `clip daemon stop` (SPEC §8). `scope` is the clear scope the client resolved from
    /// `clear_on_exit` (prompting on a TTY for `ask`); `None` = let the daemon resolve it.
    DaemonStop {
        #[serde(default)]
        scope: Option<String>,
    },
    RecvLatest { kind: RecvKind },
    Paste,
    /// Copy a *specific* buffered item (by ULID) to the OS clipboard. Used by the TUI's `y`
    /// key so the highlighted bubble — not just the latest — can be placed on the clipboard.
    PasteItem { msg_id: String },
    /// TUI Ctrl+V: read whatever image is on the local OS clipboard and broadcast it as an
    /// image item. The daemon owns arboard, so it does the read — the TUI stays a thin front-end.
    SendClipboardImage,
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
    /// Result of `SendClipboardImage`: the broadcast image's envelope, a locally materialized
    /// path (so the sender's own TUI can render its thumbnail), and how many peers it reached.
    SentImage {
        envelope: Envelope,
        local_path: Option<String>,
        reached: usize,
    },
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

/// The canonical result of sniffing: text, PNG image bytes (the canonical wire format), or
/// arbitrary file bytes with a best-effort mime.
pub enum Sniffed {
    Text(String),
    /// PNG bytes, decoded dimensions.
    Image { png: Vec<u8>, w: u32, h: u32 },
    /// Arbitrary bytes + best-effort mime (from the extension, else application/octet-stream).
    File { bytes: Vec<u8>, mime: String },
}

pub const MIME_OCTET: &str = "application/octet-stream";

/// Best-effort mime for a file, guessed from its extension (PROTOCOL §1).
pub fn mime_for_filename(name: Option<&str>) -> String {
    let ext = name
        .and_then(|n| std::path::Path::new(n).extension())
        .map(|e| e.to_string_lossy().to_lowercase())
        .unwrap_or_default();
    match ext.as_str() {
        "txt" | "log" | "md" => "text/plain; charset=utf-8",
        "json" => "application/json",
        "pdf" => "application/pdf",
        "zip" => "application/zip",
        "gz" | "tgz" => "application/gzip",
        "tar" => "application/x-tar",
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "svg" => "image/svg+xml",
        "mp4" => "video/mp4",
        "mp3" => "audio/mpeg",
        "csv" => "text/csv",
        "html" | "htm" => "text/html",
        _ => MIME_OCTET,
    }
    .to_string()
}

/// Sniff/normalize send bytes per PROTOCOL §2. JPEG is transcoded to PNG; binary (or `--file`)
/// becomes a `file`. Never errors on arbitrary binary — that *is* a valid file.
pub fn sniff(kind: SniffKind, bytes: &[u8], filename: Option<&str>) -> Result<Sniffed> {
    let as_file = || Sniffed::File {
        bytes: bytes.to_vec(),
        mime: mime_for_filename(filename),
    };
    match kind {
        SniffKind::Text => {
            let s = std::str::from_utf8(bytes).context("--text: input is not valid UTF-8")?;
            Ok(Sniffed::Text(s.to_string()))
        }
        SniffKind::Image => to_png_sniffed(bytes),
        SniffKind::File => Ok(as_file()),
        SniffKind::Auto => {
            if is_png(bytes) || is_jpeg(bytes) {
                to_png_sniffed(bytes)
            } else if let Ok(s) = std::str::from_utf8(bytes) {
                Ok(Sniffed::Text(s.to_string()))
            } else {
                Ok(as_file())
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
            filename: Some("shot.png".into()),
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
        match sniff(SniffKind::Auto, &png, None).unwrap() {
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
        match sniff(SniffKind::Auto, &jpg, None).unwrap() {
            Sniffed::Image { png, .. } => assert!(is_png(&png), "jpeg should transcode to png"),
            _ => panic!("expected image"),
        }
    }

    #[test]
    fn sniff_utf8_text() {
        match sniff(SniffKind::Auto, "hello".as_bytes(), None).unwrap() {
            Sniffed::Text(s) => assert_eq!(s, "hello"),
            _ => panic!("expected text"),
        }
    }

    /// PROTOCOL §2: binary with no `--file` still becomes a *file* (octet-stream), never an error.
    #[test]
    fn sniff_binary_becomes_file() {
        let junk = [0xff, 0xfe, 0x00, 0x01, 0x80];
        match sniff(SniffKind::Auto, &junk, None).unwrap() {
            Sniffed::File { bytes, mime } => {
                assert_eq!(bytes, junk);
                assert_eq!(mime, MIME_OCTET);
            }
            _ => panic!("expected file"),
        }
    }

    /// `--file` forces a file even for UTF-8 text, and the mime is guessed from the name.
    #[test]
    fn sniff_force_file_and_mime_from_extension() {
        match sniff(SniffKind::File, b"id,name\n1,a\n", Some("rows.csv")).unwrap() {
            Sniffed::File { mime, .. } => assert_eq!(mime, "text/csv"),
            _ => panic!("expected file"),
        }
        // PNG magic still wins over --auto; but --file on binary with an unknown ext → octet.
        match sniff(SniffKind::File, &[0x00, 0xff], Some("blob.weird")).unwrap() {
            Sniffed::File { mime, .. } => assert_eq!(mime, MIME_OCTET),
            _ => panic!("expected file"),
        }
        assert_eq!(mime_for_filename(Some("a/b/report.pdf")), "application/pdf");
        assert_eq!(mime_for_filename(None), MIME_OCTET);
    }

    /// A `file` envelope (type + filename + blob, no w/h) round-trips through the frame codec.
    #[tokio::test]
    async fn file_envelope_round_trips() {
        let env = Envelope {
            v: PROTO_V,
            msg_id: "01ARZ3NDEKTSV4RRFFQ69G5FAW".into(),
            typ: MsgType::File,
            mime: MIME_OCTET.into(),
            sender: "abcdef".into(),
            device_name: "laptop".into(),
            ts: 1_720_000_000_001,
            filename: Some("payload.bin".into()),
            text: None,
            blob: Some(Blob {
                hash: blake3_hex(b"payload"),
                size: 7,
                w: 0,
                h: 0,
            }),
        };
        let mut buf = Vec::new();
        write_frame(&mut buf, &env).await.unwrap();
        let mut cur = std::io::Cursor::new(buf);
        let back: Envelope = read_frame(&mut cur).await.unwrap();
        assert_eq!(env, back);
        assert_eq!(back.typ, MsgType::File);
        assert_eq!(back.filename.as_deref(), Some("payload.bin"));
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
