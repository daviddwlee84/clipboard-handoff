//! The resident daemon: owns the iroh Endpoint + identity, the arboard clipboard,
//! the received-item ring buffer, the trusted-peer set, and the local IPC socket.

use std::{
    collections::{BTreeSet, HashMap, HashSet, VecDeque},
    net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{SystemTime, UNIX_EPOCH},
};

use anyhow::{Context, Result, anyhow, bail};
use iroh::{
    Endpoint, EndpointAddr, EndpointId, RelayMode, TransportAddr,
    endpoint::{Connection, RecvStream, SendStream, presets},
    protocol::{AcceptError, ProtocolHandler, Router},
};
use iroh_mdns_address_lookup::{DiscoveryEvent, MdnsAddressLookup};
use serde_bytes::ByteBuf;
use tokio::sync::broadcast;
use tokio_stream::StreamExt;
use ulid::Ulid;

use interprocess::local_socket::{
    GenericFilePath, ListenerOptions, ToFsName,
    tokio::Stream as IpcStream,
    traits::tokio::{Listener as _, Stream as _},
};

use crate::clipboard;
use crate::config::{ConfigStore, Paths, default_device_name, load_or_create_secret};
use crate::proto::*;

const RING_CAP: usize = 64;

#[derive(Clone)]
struct Item {
    envelope: Envelope,
    local_path: Option<String>,
}

struct PeerHandle {
    conn: Connection,
    name: String,
}

/// All daemon state, shared behind an `Arc`.
pub struct Daemon {
    paths: Paths,
    endpoint: Endpoint,
    device_name: String,
    /// Probed once at startup: false on a headless host (no display). When false, `paste`
    /// and auto-copy become clear no-op errors; everything else keeps working.
    clipboard_available: bool,
    config: Mutex<ConfigStore>,
    peers: Mutex<HashMap<EndpointId, PeerHandle>>,
    /// Peers we're currently dialing via mDNS auto-discovery (in-flight guard, so repeated
    /// `Discovered` events don't open duplicate connections).
    dialing: Mutex<HashSet<EndpointId>>,
    allowlist: Mutex<HashSet<EndpointId>>,
    ring: Mutex<VecDeque<Item>>,
    /// Items this daemon *originated* (locally sent), keyed most-recent-last. Kept separate from
    /// `ring` (which is received-only, so `recv`/roundtrip semantics are untouched) purely so the
    /// TUI can `PasteItem` its own bubbles back to the clipboard.
    sent: Mutex<VecDeque<Item>>,
    /// Content-addressed PNG store (locally-sent + received), keyed by BLAKE3 hex.
    blobs: Mutex<HashMap<String, Vec<u8>>>,
    dedup: Mutex<Dedup>,
    events: broadcast::Sender<Event>,
    /// What this session has fetched/written, so `clear` (SPEC §8) can revert precisely.
    session: Mutex<Session>,
    /// Signals the accept loop to shut down (`daemon stop` / SIGTERM).
    stop: Arc<tokio::sync::Notify>,
    /// The clear scope a `daemon stop` client already resolved+applied (so the shutdown path
    /// doesn't re-derive one from `clear_on_exit`). `None` on SIGTERM.
    stop_scope: Mutex<Option<String>>,
}

// ---------------------------------------------------------------------------
// Session & sinks (SPEC §3 / §8)
// ---------------------------------------------------------------------------

/// Records what this daemon run wrote, so `clear --all` can revert exactly this session:
/// the `text_file`'s size when it became the sink (the truncation offset) and the files
/// written into `save_dir`. The transient store (blob dir + ring) is purged wholesale.
#[derive(Debug, Default)]
pub struct Session {
    text_file: String,
    text_offset: u64,
    saved: Vec<PathBuf>,
    /// True once anything was received this session (drives the TUI quit prompt).
    pub received_any: bool,
}

impl Session {
    /// Capture the session-start offset for a `text_file` sink. Re-captured when the sink is
    /// (re)configured mid-session via `config set text_file`, so a later `clear --all` only
    /// removes what *this* session appended to *that* file.
    pub fn set_text_file(&mut self, path: &str) {
        if path == self.text_file {
            return;
        }
        self.text_file = path.to_string();
        self.text_offset = if path.is_empty() { 0 } else { file_size(Path::new(path)) };
    }
}

fn file_size(p: &Path) -> u64 {
    std::fs::metadata(p).map(|m| m.len()).unwrap_or(0)
}

/// Fan a received item out to the additive folder/append-file sinks (SPEC §3). Independent of
/// (and additive to) the clipboard sink. `bytes` are the verified blob bytes for image/file.
pub fn route_sinks(
    session: &Mutex<Session>,
    save_dir: &str,
    text_file: &str,
    env: &Envelope,
    bytes: Option<&[u8]>,
) {
    match env.typ {
        MsgType::Text => {
            if !text_file.is_empty() {
                append_text_sink(session, text_file, env);
            }
        }
        MsgType::Image | MsgType::File => {
            if !save_dir.is_empty() {
                if let Some(b) = bytes {
                    write_save_dir(session, Path::new(save_dir), &sink_name(env), b);
                }
            }
        }
    }
}

/// The save_dir name for an item: its `filename` if present, else `<hash>` (with `.png` forced
/// for images, which are always PNG on the wire).
pub fn sink_name(env: &Envelope) -> String {
    let hash = env.blob.as_ref().map(|b| b.hash.clone()).unwrap_or_default();
    let base = env
        .filename
        .as_deref()
        .map(|f| {
            Path::new(f)
                .file_name()
                .map(|s| s.to_string_lossy().into_owned())
                .unwrap_or_else(|| f.to_string())
        })
        .filter(|f| !f.is_empty());
    match env.typ {
        MsgType::Image => {
            let name = base.unwrap_or(hash);
            let stem = Path::new(&name)
                .file_stem()
                .map(|s| s.to_string_lossy().into_owned())
                .unwrap_or(name);
            format!("{stem}.png")
        }
        _ => base.unwrap_or_else(|| if hash.is_empty() { "file".into() } else { hash }),
    }
}

/// Write `data` into `dir` under `name`, de-duplicating on collision (`name (2).ext`), and record
/// the path as a this-session write (SPEC §8).
fn write_save_dir(session: &Mutex<Session>, dir: &Path, name: &str, data: &[u8]) {
    if let Err(e) = std::fs::create_dir_all(dir) {
        tracing::warn!("save_dir: mkdir {}: {e}", dir.display());
        return;
    }
    let path = dedup_path(dir, name);
    if let Err(e) = std::fs::write(&path, data) {
        tracing::warn!("save_dir: write {}: {e}", path.display());
        return;
    }
    session.lock().unwrap().saved.push(path.clone());
    tracing::info!("wrote to save_dir: {} ({} bytes)", path.display(), data.len());
}

/// `dir/name`, or `dir/name (2).ext`, `dir/name (3).ext` … on collision (SPEC §3).
pub fn dedup_path(dir: &Path, name: &str) -> PathBuf {
    let name = if name.is_empty() { "file" } else { name };
    let cand = dir.join(name);
    if !cand.exists() {
        return cand;
    }
    let p = Path::new(name);
    let stem = p.file_stem().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default();
    let ext = p
        .extension()
        .map(|e| format!(".{}", e.to_string_lossy()))
        .unwrap_or_default();
    for i in 2.. {
        let cand = dir.join(format!("{stem} ({i}){ext}"));
        if !cand.exists() {
            return cand;
        }
    }
    unreachable!()
}

/// Append a received text item to the append-file with the SPEC §3 header:
/// `\n---\n<device> <ISO8601 ts>\n<text>\n`.
fn append_text_sink(session: &Mutex<Session>, path: &str, env: &Envelope) {
    // Anchor the truncation offset to this path before we grow it.
    session.lock().unwrap().set_text_file(path);

    let device = if env.device_name.is_empty() {
        if env.sender.is_empty() { "peer".to_string() } else { short_str(&env.sender) }
    } else {
        env.device_name.clone()
    };
    let ts = iso8601(env.ts);
    let entry = format!(
        "\n---\n{device} {ts}\n{}\n",
        env.text.clone().unwrap_or_default()
    );
    use std::io::Write as _;
    match std::fs::OpenOptions::new().create(true).append(true).open(path) {
        Ok(mut f) => {
            if let Err(e) = f.write_all(entry.as_bytes()) {
                tracing::warn!("text_file: append {path}: {e}");
            } else {
                tracing::info!("appended to text_file: {path}");
            }
        }
        Err(e) => tracing::warn!("text_file: open {path}: {e}"),
    }
}

/// Revert this session's sink writes (SPEC §8): truncate `text_file` back to its session-start
/// offset and delete the files written into `save_dir` this session. Never touches pre-session
/// content, never removes a directory.
pub fn clear_sinks(session: &Mutex<Session>) {
    let (path, off, saved) = {
        let mut s = session.lock().unwrap();
        (s.text_file.clone(), s.text_offset, std::mem::take(&mut s.saved))
    };
    if !path.is_empty() && file_size(Path::new(&path)) > off {
        if let Err(e) = std::fs::OpenOptions::new()
            .write(true)
            .open(&path)
            .and_then(|f| f.set_len(off))
        {
            tracing::warn!("clear: truncate {path}: {e}");
        }
    }
    for f in &saved {
        if let Err(e) = std::fs::remove_file(f) {
            if e.kind() != std::io::ErrorKind::NotFound {
                tracing::warn!("clear: remove {}: {e}", f.display());
            }
        }
    }
    tracing::info!(
        "cleared session sinks (text_file {path:?} @ {off}, {} save_dir files)",
        saved.len()
    );
}

/// UTC ISO-8601 (RFC 3339, second precision) for a unix-ms timestamp.
fn iso8601(ms: u64) -> String {
    let ms = if ms == 0 { now_ms() } else { ms };
    let secs = (ms / 1000) as i64;
    // days-since-epoch → civil date (Howard Hinnant's algorithm), then h:m:s.
    let (mut days, mut rem) = (secs.div_euclid(86_400), secs.rem_euclid(86_400));
    let (h, m, s) = (rem / 3600, (rem % 3600) / 60, rem % 60);
    rem = 0;
    let _ = rem;
    days += 719_468;
    let era = days.div_euclid(146_097);
    let doe = days.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let mo = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if mo <= 2 { y + 1 } else { y };
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{m:02}:{s:02}Z")
}

fn short_str(s: &str) -> String {
    s.chars().take(12).collect()
}

/// iroh accept-side protocol handler: hands each incoming connection to the daemon.
struct ClipProto(Arc<Daemon>);

impl std::fmt::Debug for ClipProto {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("ClipProto")
    }
}

impl ProtocolHandler for ClipProto {
    async fn accept(&self, connection: Connection) -> Result<(), AcceptError> {
        self.0.clone().handle_connection(connection).await;
        Ok(())
    }
}

/// Bind the IPC socket + endpoint and run forever.
pub async fn run(paths: Paths) -> Result<()> {
    paths.ensure_dir()?;

    // Bind the IPC listener FIRST, before the (slower) iroh endpoint. A unix-socket
    // client `connect()` succeeds as soon as we listen(); its request queues in the
    // backlog until we accept below. This makes the socket reachable within ms of
    // process start, so a client that auto-spawns us never races into spawning a
    // *second* daemon that would clobber this socket (split-brain).
    if paths.socket.exists() {
        // If a daemon is already listening here, don't clobber it — exit idempotently.
        if probe_alive(&paths.socket).await {
            tracing::info!("a daemon is already listening on {}; exiting", paths.socket.display());
            return Ok(());
        }
        let _ = std::fs::remove_file(&paths.socket);
    }
    if let Some(parent) = paths.socket.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let name = paths
        .socket
        .clone()
        .to_fs_name::<GenericFilePath>()
        .context("socket path -> name")?;
    let listener = ListenerOptions::new()
        .name(name)
        .create_tokio()
        .with_context(|| format!("bind IPC socket {}", paths.socket.display()))?;

    let secret = load_or_create_secret(&paths.secret_path())?;
    let config = ConfigStore::load(paths.config_path())?;
    let device_name = config.get("device_name").unwrap_or_else(default_device_name);
    let alpn = alpn_for_room(&paths.room);

    // Detect a headless host ONCE at startup: if arboard can't open a display, degrade
    // gracefully — send/recv/paste-to-file/gossip and the daemon all keep working; only
    // `paste` and auto-copy become clear no-ops. Probe off the runtime (arboard is blocking).
    let clipboard_available = tokio::task::spawn_blocking(clipboard::probe_available)
        .await
        .unwrap_or(false);
    if !clipboard_available {
        tracing::warn!("clipboard unavailable (headless) — send/recv/paste-to-file still work");
    }

    // LAN-only for Phase 0: Minimal preset (crypto provider only) + relay disabled. We add
    // an mDNS address-lookup service below for zero-config LAN auto-discovery; direct QUIC
    // then carries the addresses (from a ticket, or from mDNS, or both).
    let endpoint = Endpoint::builder(presets::Minimal)
        .secret_key(secret.clone())
        .alpns(vec![alpn.clone()])
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .map_err(|e| anyhow!("iroh endpoint bind failed: {e}"))?;

    // The transient store: fetched blobs + `recv --emit-path` temp files live here, so `clear`
    // (SPEC §8) purges exactly this daemon's data and nothing else.
    std::fs::create_dir_all(paths.blob_dir())?;
    let mut session = Session::default();
    session.set_text_file(&config.text_file());

    let (events, _rx) = broadcast::channel(256);
    let daemon = Arc::new(Daemon {
        paths: paths.clone(),
        endpoint: endpoint.clone(),
        device_name,
        clipboard_available,
        config: Mutex::new(config),
        peers: Mutex::new(HashMap::new()),
        dialing: Mutex::new(HashSet::new()),
        allowlist: Mutex::new(HashSet::new()),
        ring: Mutex::new(VecDeque::new()),
        sent: Mutex::new(VecDeque::new()),
        blobs: Mutex::new(HashMap::new()),
        dedup: Mutex::new(Dedup::new(4096)),
        events,
        session: Mutex::new(session),
        stop: Arc::new(tokio::sync::Notify::new()),
        stop_scope: Mutex::new(None),
    });

    // Accept loop for inbound iroh connections (same-room ALPN only).
    let _router = Router::builder(endpoint.clone())
        .accept(alpn.clone(), ClipProto(daemon.clone()))
        .spawn();

    // LAN auto-discovery: same-`--room` daemons on the same network find each other with no
    // ticket. Room isolation is enforced twice: (1) the mDNS service name is scoped by room,
    // so different rooms don't even see each other; (2) the room secret is folded into the
    // ALPN, so a cross-room dial is rejected at the QUIC handshake. Best-effort: if mDNS
    // can't start (e.g. no usable IPv4/IPv6), we log and fall back to ticket pairing.
    match MdnsAddressLookup::builder()
        .service_name(mdns_service_name(&paths.room))
        .build(endpoint.id())
    {
        Ok(mdns) => match endpoint.address_lookup() {
            Ok(services) => {
                services.add(mdns.clone());
                let d = daemon.clone();
                let dial_alpn = alpn.clone();
                tokio::spawn(async move { d.run_discovery(mdns, dial_alpn).await });
                tracing::info!("mDNS discovery enabled (room-scoped) — same-room peers auto-connect without a ticket");
            }
            Err(e) => tracing::warn!("mDNS: address lookup unavailable ({e}); ticket pairing still works"),
        },
        Err(e) => tracing::warn!("mDNS discovery unavailable ({e}); ticket pairing still works"),
    }

    tracing::info!(
        "clip daemon up: endpoint {} · room {:?} · socket {}",
        endpoint.id(),
        paths.room,
        paths.socket.display()
    );

    // SIGTERM is a session end (SPEC §8): apply `clear_on_exit` non-interactively (a bare daemon
    // has no TTY, so `ask` falls back to transient) and shut down cleanly.
    let stop = daemon.stop.clone();
    #[cfg(unix)]
    {
        let stop = stop.clone();
        tokio::spawn(async move {
            let mut sig = match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
                Ok(s) => s,
                Err(e) => {
                    tracing::warn!("SIGTERM handler unavailable: {e}");
                    return;
                }
            };
            sig.recv().await;
            tracing::info!("SIGTERM received — shutting down");
            stop.notify_waiters();
        });
    }

    loop {
        tokio::select! {
            _ = stop.notified() => break,
            accepted = listener.accept() => {
                let stream = accepted?;
                let d = daemon.clone();
                tokio::spawn(async move {
                    if let Err(e) = d.handle_ipc(stream).await {
                        tracing::debug!("ipc handler error: {e:#}");
                    }
                });
            }
        }
    }

    // A `daemon stop` request already applied its resolved scope before signalling; a SIGTERM
    // has not, so apply the configured policy here.
    if daemon.stop_scope.lock().unwrap().is_none() {
        let policy = daemon.config.lock().unwrap().clear_on_exit();
        daemon.apply_clear_scope(&resolve_exit_scope(&policy));
    }
    let _ = std::fs::remove_file(&paths.socket);
    tracing::info!("clip daemon stopped");
    Ok(())
}

/// Map a `clear_on_exit` policy to a concrete scope for a non-interactive session end:
/// `ask` → `transient` (SPEC §8).
pub fn resolve_exit_scope(policy: &str) -> String {
    match policy {
        "ask" | "" => "transient".to_string(),
        other => other.to_string(),
    }
}

impl Daemon {
    // ---- iroh connection handling ----------------------------------------

    async fn handle_connection(self: Arc<Self>, conn: Connection) {
        let peer_id = conn.remote_id();
        let name = short_id(&peer_id);
        self.peers.lock().unwrap().insert(
            peer_id,
            PeerHandle {
                conn: conn.clone(),
                name: name.clone(),
            },
        );
        // Phase 0 trust model: a paired/connected peer is allowlisted. TOFU-with-approval
        // (holding first contact pending) is Phase 1.
        self.allowlist.lock().unwrap().insert(peer_id);
        let _ = self.events.send(Event::PeerUp {
            id: peer_id.to_string(),
            name,
        });
        tracing::info!("peer connected: {}", peer_id);

        loop {
            match conn.accept_bi().await {
                Ok((send, recv)) => {
                    let d = self.clone();
                    let c = conn.clone();
                    tokio::spawn(async move {
                        if let Err(e) = d.handle_stream(c, send, recv).await {
                            tracing::debug!("stream error: {e:#}");
                        }
                    });
                }
                Err(_) => break, // connection closed
            }
        }

        self.peers.lock().unwrap().remove(&peer_id);
        let _ = self.events.send(Event::PeerDown {
            id: peer_id.to_string(),
        });
        tracing::info!("peer disconnected: {}", peer_id);
    }

    async fn handle_stream(
        self: Arc<Self>,
        conn: Connection,
        mut send: SendStream,
        mut recv: RecvStream,
    ) -> Result<()> {
        let msg: WireMsg = read_frame(&mut recv).await?;
        match msg {
            WireMsg::Announce(env) => self.on_announce(conn, env).await?,
            WireMsg::BlobRequest { hash } => {
                let bytes = self.blobs.lock().unwrap().get(&hash).cloned();
                let resp = match bytes {
                    Some(b) => WireMsg::BlobResponse {
                        ok: true,
                        bytes: ByteBuf::from(b),
                    },
                    None => WireMsg::BlobResponse {
                        ok: false,
                        bytes: ByteBuf::new(),
                    },
                };
                write_frame(&mut send, &resp).await?;
                let _ = send.finish();
            }
            WireMsg::BlobResponse { .. } => {} // unsolicited; ignore
        }
        Ok(())
    }

    async fn on_announce(self: Arc<Self>, conn: Connection, env: Envelope) -> Result<()> {
        // 1) dedupe by msg_id (SPEC §3.1)
        if !self.dedup.lock().unwrap().mark_seen(&env.msg_id) {
            return Ok(());
        }
        if env.v != PROTO_V {
            bail!("unsupported protocol version {}", env.v);
        }

        // Image AND file are content-addressed: pull the bytes over a direct stream keyed by
        // hash, BLAKE3-verify them, and materialize them into the blob cache so `paste` and
        // `recv --emit-path` always have a local path (PROTOCOL §1).
        let mut local_path = None;
        let mut bytes: Option<Vec<u8>> = None;
        if matches!(env.typ, MsgType::Image | MsgType::File) {
            let blob = env
                .blob
                .clone()
                .ok_or_else(|| anyhow!("{:?} envelope missing blob ref", env.typ))?;
            let b = self.fetch_blob(&conn, &blob.hash).await?;
            let got = blake3_hex(&b);
            if got != blob.hash {
                bail!("blob hash mismatch (got {got}, want {})", blob.hash);
            }
            self.blobs.lock().unwrap().insert(blob.hash.clone(), b.clone());
            let path = self.blob_cache_path(&env, &blob.hash);
            std::fs::write(&path, &b).with_context(|| format!("write {}", path.display()))?;
            local_path = Some(path.to_string_lossy().to_string());
            bytes = Some(b);
        }

        {
            let mut ring = self.ring.lock().unwrap();
            ring.push_back(Item {
                envelope: env.clone(),
                local_path: local_path.clone(),
            });
            while ring.len() > RING_CAP {
                ring.pop_front();
            }
        }
        self.session.lock().unwrap().received_any = true;

        // Additive sinks (SPEC §3): folder / append-file, independent of the clipboard.
        let (save_dir, text_file) = {
            let c = self.config.lock().unwrap();
            (c.save_dir(), c.text_file())
        };
        route_sinks(&self.session, &save_dir, &text_file, &env, bytes.as_deref());

        // Auto-copy behavior (SPEC §3). Default = notify (never touch the clipboard).
        let mode = self.config.lock().unwrap().auto_copy();
        match mode.as_str() {
            "on" => {
                if !self.clipboard_available {
                    // Headless: keep buffering (recv/--emit-path still works); don't spam warns.
                    tracing::debug!(
                        "auto_copy on: clipboard unavailable (headless) — item buffered, not copied"
                    );
                } else if let Err(e) = self.write_to_clipboard(&env, local_path.as_deref()).await {
                    tracing::warn!("auto_copy on: clipboard write failed: {e:#}");
                }
            }
            "notify" => {
                let _ = self.events.send(Event::Toast {
                    text: format!("received {} — run `clip paste` to copy", describe(&env)),
                });
            }
            _ => {} // off
        }

        let _ = self.events.send(Event::Item {
            envelope: env,
            local_path,
        });
        Ok(())
    }

    async fn fetch_blob(&self, conn: &Connection, hash: &str) -> Result<Vec<u8>> {
        let (mut send, mut recv) = conn.open_bi().await.map_err(|e| anyhow!("open_bi: {e}"))?;
        write_frame(&mut send, &WireMsg::BlobRequest { hash: hash.to_string() }).await?;
        let _ = send.finish();
        let resp: WireMsg = read_frame(&mut recv).await?;
        match resp {
            WireMsg::BlobResponse { ok: true, bytes } => Ok(bytes.into_vec()),
            WireMsg::BlobResponse { ok: false, .. } => bail!("peer has no blob {hash}"),
            _ => bail!("unexpected reply to blob request"),
        }
    }

    /// Broadcast an announcement to all connected peers. Returns how many were reached.
    async fn broadcast(&self, env: &Envelope) -> usize {
        let conns: Vec<Connection> = self
            .peers
            .lock()
            .unwrap()
            .values()
            .map(|p| p.conn.clone())
            .collect();
        let mut reached = 0;
        for conn in conns {
            match conn.open_bi().await {
                Ok((mut send, _recv)) => {
                    if write_frame(&mut send, &WireMsg::Announce(env.clone()))
                        .await
                        .is_ok()
                    {
                        let _ = send.finish();
                        reached += 1;
                    }
                }
                Err(e) => tracing::debug!("broadcast to peer failed: {e}"),
            }
        }
        reached
    }

    /// Broadcast PNG bytes as an image item and materialize a local copy in the blob cache (same
    /// as a received image), so the SENDER can render its own thumbnail (TUI) and `PasteItem` it
    /// back. Returns the envelope, that local path, and the peer reach; callers wrap the pieces
    /// into the right `OkData`. Shared by `Req::Send`'s image arm and `Req::SendClipboardImage`.
    async fn broadcast_image(
        &self,
        png: Vec<u8>,
        w: u32,
        h: u32,
        filename: Option<String>,
    ) -> (Envelope, Option<String>, usize) {
        let hash = blake3_hex(&png);
        self.blobs.lock().unwrap().insert(hash.clone(), png.clone());
        let mut env = self.base_envelope(MsgType::Image, "image/png");
        env.filename = filename;
        env.blob = Some(Blob {
            hash: hash.clone(),
            size: png.len() as u64,
            w,
            h,
        });
        let local_path = {
            let path = self.blob_cache_path(&env, &hash);
            match std::fs::write(&path, &png) {
                Ok(()) => Some(path.to_string_lossy().to_string()),
                Err(e) => {
                    tracing::warn!("materialize sent image failed: {e}");
                    None
                }
            }
        };
        let n = self.broadcast(&env).await;
        self.record_sent(&env, local_path.clone());
        (env, local_path, n)
    }

    // ---- IPC (client <-> daemon) -----------------------------------------

    async fn handle_ipc(self: Arc<Self>, mut stream: IpcStream) -> Result<()> {
        let req: Req = read_frame(&mut stream).await?;
        match req {
            Req::Send { kind, bytes, filename } => {
                let name = filename.as_deref().map(base_name);
                let resp = match sniff(kind, &bytes, name.as_deref()) {
                    Ok(Sniffed::Text(text)) => {
                        let mut env = self.base_envelope(MsgType::Text, "text/plain; charset=utf-8");
                        env.text = Some(text);
                        let n = self.broadcast(&env).await;
                        self.record_sent(&env, None);
                        Resp::Ok(OkData::Sent { msg_id: env.msg_id, reached: n })
                    }
                    Ok(Sniffed::Image { png, w, h }) => {
                        let (env, _local_path, reached) = self.broadcast_image(png, w, h, name).await;
                        Resp::Ok(OkData::Sent { msg_id: env.msg_id, reached })
                    }
                    Ok(Sniffed::File { bytes, mime }) => {
                        let hash = blake3_hex(&bytes);
                        self.blobs.lock().unwrap().insert(hash.clone(), bytes.clone());
                        let mut env = self.base_envelope(MsgType::File, &mime);
                        env.filename = name;
                        env.blob = Some(Blob {
                            hash,
                            size: bytes.len() as u64,
                            w: 0,
                            h: 0,
                        });
                        let n = self.broadcast(&env).await;
                        self.record_sent(&env, None);
                        Resp::Ok(OkData::Sent { msg_id: env.msg_id, reached: n })
                    }
                    Err(e) => Resp::Err {
                        code: 2,
                        message: e.to_string(),
                    },
                };
                write_frame(&mut stream, &resp).await?;
            }
            Req::Clear { all } => {
                self.apply_clear_scope(if all { "all" } else { "transient" });
                write_frame(&mut stream, &Resp::Ok(OkData::None)).await?;
            }
            Req::DaemonStop { scope } => {
                // The client resolved `clear_on_exit` (prompting on a TTY for `ask`); fall back
                // to the config policy if it didn't (SPEC §8).
                let scope = scope.unwrap_or_else(|| {
                    resolve_exit_scope(&self.config.lock().unwrap().clear_on_exit())
                });
                self.apply_clear_scope(&scope);
                *self.stop_scope.lock().unwrap() = Some(scope);
                write_frame(&mut stream, &Resp::Ok(OkData::None)).await?;
                self.stop.notify_waiters();
            }
            Req::RecvLatest { kind } => {
                // Subscribe *before* checking the buffer to avoid missing an item
                // that lands in between.
                let mut rx = self.events.subscribe();
                let resp = if let Some(item) = self.latest(kind) {
                    Resp::Ok(OkData::Item {
                        envelope: item.envelope,
                        local_path: item.local_path,
                    })
                } else {
                    let got = tokio::time::timeout(std::time::Duration::from_secs(30), async {
                        loop {
                            match rx.recv().await {
                                Ok(Event::Item {
                                    envelope,
                                    local_path,
                                }) if kind_matches(kind, &envelope) => {
                                    return Some((envelope, local_path));
                                }
                                Ok(_) => continue,
                                Err(_) => return None,
                            }
                        }
                    })
                    .await
                    .ok()
                    .flatten();
                    match got {
                        Some((envelope, local_path)) => Resp::Ok(OkData::Item {
                            envelope,
                            local_path,
                        }),
                        None => Resp::Err {
                            code: 5,
                            message: "nothing to receive".into(),
                        },
                    }
                };
                write_frame(&mut stream, &resp).await?;
            }
            Req::Subscribe => {
                let mut rx = self.events.subscribe();
                loop {
                    match rx.recv().await {
                        Ok(ev) => {
                            if write_frame(&mut stream, &ev).await.is_err() {
                                break; // client disconnected
                            }
                        }
                        Err(broadcast::error::RecvError::Lagged(_)) => continue,
                        Err(_) => break,
                    }
                }
            }
            Req::Paste => {
                let resp = if !self.clipboard_available {
                    // Headless no-op: a clear status instead of a panic. The item is still
                    // buffered — `recv --latest-image --emit-path` / `recv` reach it.
                    Resp::Err {
                        code: 1,
                        message: "clipboard unavailable (headless) — cannot paste; use `recv --emit-path` to get the file".into(),
                    }
                } else {
                    match self.paste_latest().await {
                        Ok(desc) => Resp::Ok(OkData::Text(desc)),
                        Err(e) => Resp::Err {
                            code: 5,
                            message: e.to_string(),
                        },
                    }
                };
                write_frame(&mut stream, &resp).await?;
            }
            Req::PasteItem { msg_id } => {
                let resp = if !self.clipboard_available {
                    Resp::Err {
                        code: 1,
                        message: "clipboard unavailable (headless) — cannot copy".into(),
                    }
                } else {
                    match self.find_item(&msg_id) {
                        Some(item) => match self
                            .write_to_clipboard(&item.envelope, item.local_path.as_deref())
                            .await
                        {
                            Ok(()) => Resp::Ok(OkData::Text(describe(&item.envelope))),
                            Err(e) => Resp::Err { code: 1, message: e.to_string() },
                        },
                        None => Resp::Err {
                            code: 5,
                            message: "item no longer buffered".into(),
                        },
                    }
                };
                write_frame(&mut stream, &resp).await?;
            }
            Req::SendClipboardImage => {
                // The TUI's Ctrl+V: the daemon (arboard owner) reads the OS clipboard image and
                // broadcasts it exactly like `send --image`. Headless → clean error, never a panic.
                let resp = if !self.clipboard_available {
                    Resp::Err {
                        code: 1,
                        message: "clipboard unavailable (headless)".into(),
                    }
                } else {
                    // arboard is blocking and not Send: read on the blocking pool.
                    match tokio::task::spawn_blocking(clipboard::read_image).await {
                        Ok(Some((png, w, h))) => {
                            let (envelope, local_path, reached) =
                                self.broadcast_image(png, w, h, None).await;
                            Resp::Ok(OkData::SentImage { envelope, local_path, reached })
                        }
                        Ok(None) => Resp::Err {
                            code: 5,
                            message: "no image on the clipboard".into(),
                        },
                        Err(_) => Resp::Err {
                            code: 1,
                            message: "clipboard read task panicked".into(),
                        },
                    }
                };
                write_frame(&mut stream, &resp).await?;
            }
            Req::Status => {
                let (auto_copy, buffer_len, peer_count) = (
                    self.config.lock().unwrap().auto_copy(),
                    self.ring.lock().unwrap().len(),
                    self.peers.lock().unwrap().len(),
                );
                let info = StatusInfo {
                    endpoint_id: self.endpoint.id().to_string(),
                    device_name: self.device_name.clone(),
                    room: self.paths.room.clone(),
                    transport: "lan (iroh, relay disabled)".into(),
                    auto_copy,
                    clipboard: if self.clipboard_available {
                        "available".into()
                    } else {
                        "unavailable".into()
                    },
                    peer_count,
                    buffer_len,
                };
                write_frame(&mut stream, &Resp::Ok(OkData::Status(info))).await?;
            }
            Req::Peers => {
                let peers = self
                    .peers
                    .lock()
                    .unwrap()
                    .iter()
                    .map(|(id, p)| PeerInfo {
                        id: id.to_string(),
                        name: p.name.clone(),
                        direct: true,
                    })
                    .collect();
                write_frame(&mut stream, &Resp::Ok(OkData::Peers(peers))).await?;
            }
            Req::ConfigSet { key, value } => {
                let resp = match self.config.lock().unwrap().set(&key, &value) {
                    Ok(()) => {
                        // Re-anchor the session truncation offset when the append sink changes
                        // mid-session, so `clear --all` only removes what *we* appended (SPEC §8).
                        if key == "text_file" {
                            self.session.lock().unwrap().set_text_file(&value);
                        }
                        Resp::Ok(OkData::None)
                    }
                    Err(e) => Resp::Err {
                        code: 1,
                        message: e.to_string(),
                    },
                };
                write_frame(&mut stream, &resp).await?;
            }
            Req::ConfigGet { key } => {
                let v = self.config.lock().unwrap().get(&key);
                write_frame(&mut stream, &Resp::Ok(OkData::Config(v))).await?;
            }
            Req::PairNew => {
                let resp = match self.make_ticket() {
                    Ok(t) => Resp::Ok(OkData::Ticket(t)),
                    Err(e) => Resp::Err {
                        code: 1,
                        message: e.to_string(),
                    },
                };
                write_frame(&mut stream, &resp).await?;
            }
            Req::Pair { ticket } => {
                let resp = match self.clone().pair(&ticket).await {
                    Ok(()) => Resp::Ok(OkData::None),
                    Err(e) => Resp::Err {
                        code: 1,
                        message: e.to_string(),
                    },
                };
                write_frame(&mut stream, &resp).await?;
            }
        }
        Ok(())
    }

    // ---- helpers ---------------------------------------------------------

    fn base_envelope(&self, typ: MsgType, mime: &str) -> Envelope {
        Envelope {
            v: PROTO_V,
            msg_id: Ulid::new().to_string(),
            typ,
            mime: mime.to_string(),
            sender: self.endpoint.id().to_string(),
            device_name: self.device_name.clone(),
            ts: now_ms(),
            filename: None,
            text: None,
            blob: None,
        }
    }

    /// Where a fetched blob is materialized in the transient store: `<blob_dir>/<hash><ext>`
    /// (`.png` for images; the filename's extension for files). Content-addressed, so a repeat
    /// of the same bytes reuses the same path.
    fn blob_cache_path(&self, env: &Envelope, hash: &str) -> PathBuf {
        let short: String = hash.chars().take(16).collect();
        let ext = match env.typ {
            MsgType::Image => ".png".to_string(),
            _ => env
                .filename
                .as_deref()
                .and_then(|f| Path::new(f).extension().map(|e| format!(".{}", e.to_string_lossy())))
                .unwrap_or_default(),
        };
        self.paths.blob_dir().join(format!("clip-{short}{ext}"))
    }

    // ---- clearing (SPEC §8) ----------------------------------------------

    /// Purge the always-safe transient store: the in-memory received buffer, the fetched-blob
    /// cache in memory, and the on-disk blob dir (which also holds `recv --emit-path` files).
    fn clear_transient(&self) {
        self.ring.lock().unwrap().clear();
        self.blobs.lock().unwrap().clear();
        self.sent.lock().unwrap().clear();
        let dir = self.paths.blob_dir();
        if let Ok(entries) = std::fs::read_dir(&dir) {
            let mut n = 0;
            for e in entries.flatten() {
                if std::fs::remove_file(e.path()).is_ok() {
                    n += 1;
                }
            }
            tracing::info!("cleared transient store (buffer emptied, {n} blobs removed)");
        }
        self.session.lock().unwrap().received_any = false;
    }

    /// Run a concrete clear scope: `never` | `transient` | `all`.
    pub fn apply_clear_scope(&self, scope: &str) {
        match scope {
            "all" => {
                self.clear_transient();
                clear_sinks(&self.session);
            }
            "transient" => self.clear_transient(),
            "never" | "" => {}
            other => tracing::warn!("unknown clear scope {other:?}; keeping everything"),
        }
    }

    fn latest(&self, kind: RecvKind) -> Option<Item> {
        let ring = self.ring.lock().unwrap();
        ring.iter().rev().find(|it| kind_matches(kind, &it.envelope)).cloned()
    }

    /// Remember a locally-sent item (for the TUI's `PasteItem`). Capped like the recv ring.
    fn record_sent(&self, env: &Envelope, local_path: Option<String>) {
        let mut sent = self.sent.lock().unwrap();
        sent.push_back(Item { envelope: env.clone(), local_path });
        while sent.len() > RING_CAP {
            sent.pop_front();
        }
    }

    /// Find a buffered item by ULID — searches received items first, then locally-sent ones.
    fn find_item(&self, msg_id: &str) -> Option<Item> {
        if let Some(it) = self
            .ring
            .lock()
            .unwrap()
            .iter()
            .rev()
            .find(|it| it.envelope.msg_id == msg_id)
        {
            return Some(it.clone());
        }
        self.sent
            .lock()
            .unwrap()
            .iter()
            .rev()
            .find(|it| it.envelope.msg_id == msg_id)
            .cloned()
    }

    async fn paste_latest(&self) -> Result<String> {
        let item = self
            .latest(RecvKind::Any)
            .ok_or_else(|| anyhow!("nothing to paste"))?;
        self.write_to_clipboard(&item.envelope, item.local_path.as_deref())
            .await?;
        Ok(describe(&item.envelope))
    }

    async fn write_to_clipboard(&self, env: &Envelope, local_path: Option<&str>) -> Result<()> {
        if !self.clipboard_available {
            bail!("clipboard unavailable (headless) — send/recv/paste-to-file still work");
        }
        let cached = env
            .blob
            .as_ref()
            .and_then(|b| self.blobs.lock().unwrap().get(&b.hash).cloned());
        let (hash, payload) = clip_payload(env, local_path, cached)?;

        // arboard is blocking and its Clipboard isn't Send; do the whole op inside the
        // blocking thread. The wrapper returns Err (never panics) if the clipboard fails.
        tokio::task::spawn_blocking(move || clipboard::write(payload))
            .await
            .context("clipboard task")??;

        // Record what we wrote so echo-suppression (`on` mode) doesn't re-broadcast it.
        self.dedup.lock().unwrap().note_written(&hash);
        Ok(())
    }

    /// Build a base32 pairing ticket carrying our EndpointId + reachable direct addrs.
    fn make_ticket(&self) -> Result<String> {
        let id = self.endpoint.id();
        let mut addrs: BTreeSet<TransportAddr> = BTreeSet::new();
        // Same-machine reachability is guaranteed via loopback:port.
        for sa in self.endpoint.bound_sockets() {
            let loopback = match sa.ip() {
                IpAddr::V4(_) => SocketAddr::new(IpAddr::V4(Ipv4Addr::LOCALHOST), sa.port()),
                IpAddr::V6(_) => SocketAddr::new(IpAddr::V6(Ipv6Addr::LOCALHOST), sa.port()),
            };
            addrs.insert(TransportAddr::Ip(loopback));
        }
        // Also advertise discovered (LAN) direct addresses, for cross-machine use.
        for a in self.endpoint.addr().addrs {
            if let TransportAddr::Ip(sa) = &a {
                if !sa.ip().is_unspecified() {
                    addrs.insert(a);
                }
            }
        }
        let addr = EndpointAddr { id, addrs };
        let mut buf = Vec::new();
        ciborium::into_writer(&addr, &mut buf).context("encode ticket")?;
        Ok(data_encoding::BASE32_NOPAD.encode(&buf))
    }

    async fn pair(self: Arc<Self>, ticket: &str) -> Result<()> {
        let raw = data_encoding::BASE32_NOPAD
            .decode(ticket.trim().to_uppercase().as_bytes())
            .context("decode ticket (base32)")?;
        let addr: EndpointAddr = ciborium::from_reader(&raw[..]).context("parse ticket")?;
        let alpn = alpn_for_room(&self.paths.room);
        let conn = self
            .endpoint
            .connect(addr, &alpn)
            .await
            .map_err(|e| anyhow!("connect to peer failed: {e}"))?;
        let d = self.clone();
        tokio::spawn(async move { d.handle_connection(conn).await });
        Ok(())
    }

    /// Auto-connect loop: subscribe to room-scoped mDNS discovery and dial newly-found peers
    /// with no ticket. To avoid opening the connection from both ends, only the peer with the
    /// **larger** EndpointId dials; the other accepts the inbound. The ALPN still gates the
    /// handshake, so a stray cross-room peer (should one ever share our service name) can't
    /// complete the connection.
    async fn run_discovery(self: Arc<Self>, mdns: MdnsAddressLookup, alpn: Vec<u8>) {
        let my_id = self.endpoint.id();
        let mut events = mdns.subscribe().await;
        while let Some(ev) = events.next().await {
            let DiscoveryEvent::Discovered { endpoint_info, .. } = ev else {
                continue; // Expired / other: nothing to dial
            };
            let peer_id = endpoint_info.endpoint_id;
            if peer_id == my_id {
                continue; // ourselves
            }
            // Deterministic tie-break: only the larger id initiates.
            if my_id < peer_id {
                continue;
            }
            // Skip if already connected, or a dial to this peer is already in flight.
            {
                if self.peers.lock().unwrap().contains_key(&peer_id) {
                    continue;
                }
                if !self.dialing.lock().unwrap().insert(peer_id) {
                    continue;
                }
            }
            let addr = endpoint_info.to_endpoint_addr();
            let d = self.clone();
            let alpn = alpn.clone();
            tokio::spawn(async move {
                let res = d.endpoint.connect(addr, &alpn).await;
                d.dialing.lock().unwrap().remove(&peer_id);
                match res {
                    Ok(conn) => {
                        tracing::info!("mDNS auto-connect -> {}", peer_id);
                        d.handle_connection(conn).await;
                    }
                    Err(e) => tracing::debug!("mDNS auto-connect to {} failed: {e}", peer_id),
                }
            });
        }
    }
}

/// The OS-clipboard representation of an item, plus the content hash recorded for echo
/// suppression (SPEC §3 rule 2). Text copies its text; an image copies PNG bytes; a **file** has
/// no image clipboard form, so it copies its **local path as text** (SPEC §2).
pub fn clip_payload(
    env: &Envelope,
    local_path: Option<&str>,
    cached: Option<Vec<u8>>,
) -> Result<(String, clipboard::Payload)> {
    match env.typ {
        MsgType::Text => {
            let text = env.text.clone().unwrap_or_default();
            Ok((blake3_hex(text.as_bytes()), clipboard::Payload::Text(text)))
        }
        MsgType::Image => {
            let blob = env
                .blob
                .clone()
                .ok_or_else(|| anyhow!("image envelope missing blob"))?;
            let bytes = cached
                .or_else(|| local_path.and_then(|p| std::fs::read(p).ok()))
                .ok_or_else(|| anyhow!("image bytes unavailable"))?;
            Ok((blob.hash.clone(), clipboard::Payload::ImagePng(bytes)))
        }
        MsgType::File => {
            let path = local_path
                .ok_or_else(|| anyhow!("file has no local path yet"))?
                .to_string();
            Ok((blake3_hex(path.as_bytes()), clipboard::Payload::Text(path)))
        }
    }
}

fn kind_matches(kind: RecvKind, env: &Envelope) -> bool {
    match kind {
        RecvKind::Any => true,
        RecvKind::Text => env.typ == MsgType::Text,
        RecvKind::Image => env.typ == MsgType::Image,
    }
}

fn base_name(s: &str) -> String {
    Path::new(s)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| s.to_string())
}

fn describe(env: &Envelope) -> String {
    match env.typ {
        MsgType::Text => {
            let preview: String = env
                .text
                .clone()
                .unwrap_or_default()
                .chars()
                .take(48)
                .collect();
            format!("text from {}: {preview}", env.device_name)
        }
        MsgType::Image => {
            let (w, h) = env.blob.as_ref().map(|b| (b.w, b.h)).unwrap_or((0, 0));
            format!("image from {} ({w}x{h})", env.device_name)
        }
        MsgType::File => {
            let size = env.blob.as_ref().map(|b| b.size).unwrap_or(0);
            let name = env.filename.clone().unwrap_or_else(|| "file".into());
            format!("file from {} ({name}, {size} bytes)", env.device_name)
        }
    }
}

fn short_id(id: &EndpointId) -> String {
    let s = id.to_string();
    s.chars().take(12).collect()
}

/// mDNS service name scoped to a room, so different `--room`s don't discover each other.
/// Folds the room secret into a DNS-SD-safe label (`clip` + 16 lowercase hex). The mDNS
/// record becomes `<endpoint>._clip<hex>._udp.local`. (Belt-and-suspenders: the ALPN still
/// enforces room isolation at the QUIC handshake — see `alpn_for_room`.)
fn mdns_service_name(room: &str) -> String {
    let h = blake3::hash(room.as_bytes());
    format!("clip{}", hex::encode(&h.as_bytes()[..8]))
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// True if a daemon is currently accepting connections on `socket`.
async fn probe_alive(socket: &std::path::Path) -> bool {
    let Ok(name) = socket.to_path_buf().to_fs_name::<GenericFilePath>() else {
        return false;
    };
    matches!(
        tokio::time::timeout(std::time::Duration::from_millis(300), IpcStream::connect(name)).await,
        Ok(Ok(_))
    )
}

// ---------------------------------------------------------------------------
// Tests: the sink/session/clear logic is pure (no Endpoint), so it is exercised
// directly here — the same code path the daemon runs on every received item.
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    fn env_text(text: &str, device: &str) -> Envelope {
        Envelope {
            v: PROTO_V,
            msg_id: Ulid::new().to_string(),
            typ: MsgType::Text,
            mime: "text/plain; charset=utf-8".into(),
            sender: "peer0".into(),
            device_name: device.into(),
            ts: 1_720_000_000_000,
            filename: None,
            text: Some(text.into()),
            blob: None,
        }
    }

    fn env_blob(typ: MsgType, bytes: &[u8], filename: Option<&str>) -> Envelope {
        Envelope {
            v: PROTO_V,
            msg_id: Ulid::new().to_string(),
            typ,
            mime: "application/octet-stream".into(),
            sender: "peer0".into(),
            device_name: "peerA".into(),
            ts: 1_720_000_000_000,
            filename: filename.map(|s| s.to_string()),
            text: None,
            blob: Some(Blob {
                hash: blake3_hex(bytes),
                size: bytes.len() as u64,
                w: 0,
                h: 0,
            }),
        }
    }

    /// Text appends to text_file with the SPEC §3 header; image/file land in save_dir under
    /// their filename, de-duplicated on collision.
    #[test]
    fn sinks_append_text_and_save_image_and_file_with_dedup() {
        let tmp = tempfile::tempdir().unwrap();
        let save = tmp.path().join("save");
        let txt = tmp.path().join("log.txt");
        std::fs::write(&txt, "PRE-EXISTING\n").unwrap();
        let (sd, tf) = (save.to_string_lossy().to_string(), txt.to_string_lossy().to_string());

        let sess = Mutex::new(Session::default());
        sess.lock().unwrap().set_text_file(&tf);

        route_sinks(&sess, &sd, &tf, &env_text("hello sinks", "peerA"), None);
        let got = std::fs::read_to_string(&txt).unwrap();
        assert!(got.starts_with("PRE-EXISTING\n"), "pre-session content lost: {got:?}");
        assert!(got.contains("\n---\n"), "missing --- header: {got:?}");
        assert!(got.contains("peerA 20"), "missing device + ISO ts: {got:?}");
        assert!(got.contains("hello sinks"));

        let png = b"\x89PNG\r\n\x1a\nfake";
        route_sinks(&sess, &sd, &tf, &env_blob(MsgType::Image, png, Some("shot.png")), Some(png));
        assert_eq!(std::fs::read(save.join("shot.png")).unwrap(), png);
        // same name again -> dedup
        route_sinks(&sess, &sd, &tf, &env_blob(MsgType::Image, png, Some("shot.png")), Some(png));
        assert!(save.join("shot (2).png").exists(), "image dedup name missing");

        let data = [0x00u8, 0x01, b'b', b'i', b'n', 0xff];
        route_sinks(&sess, &sd, &tf, &env_blob(MsgType::File, &data, Some("report.bin")), Some(&data));
        assert_eq!(std::fs::read(save.join("report.bin")).unwrap(), data);
        route_sinks(&sess, &sd, &tf, &env_blob(MsgType::File, &data, Some("report.bin")), Some(&data));
        assert!(save.join("report (2).bin").exists(), "file dedup name missing");

        // No filename -> <hash>(.png)
        let anon = env_blob(MsgType::File, &data, None);
        let hash = anon.blob.as_ref().unwrap().hash.clone();
        route_sinks(&sess, &sd, &tf, &anon, Some(&data));
        assert!(save.join(&hash).exists(), "hash-named file missing");
    }

    /// `clear --all` truncates text_file back to the session-start offset and deletes only the
    /// files this session wrote; pre-session content survives.
    #[test]
    fn clear_all_reverts_session_sinks_only() {
        let tmp = tempfile::tempdir().unwrap();
        let save = tmp.path().join("save");
        let txt = tmp.path().join("log.txt");
        let pre = "PRE-EXISTING LINE\n";
        std::fs::write(&txt, pre).unwrap();
        std::fs::create_dir_all(&save).unwrap();
        std::fs::write(save.join("keep.dat"), "keep me").unwrap();
        let (sd, tf) = (save.to_string_lossy().to_string(), txt.to_string_lossy().to_string());

        let sess = Mutex::new(Session::default());
        sess.lock().unwrap().set_text_file(&tf); // session-start offset = len(pre)

        route_sinks(&sess, &sd, &tf, &env_text("session text", "peerA"), None);
        let bytes = b"session-bytes";
        route_sinks(&sess, &sd, &tf, &env_blob(MsgType::File, bytes, Some("a.bin")), Some(bytes));
        route_sinks(&sess, &sd, &tf, &env_blob(MsgType::Image, bytes, Some("b.png")), Some(bytes));
        assert_ne!(std::fs::read_to_string(&txt).unwrap(), pre);
        assert!(save.join("a.bin").exists() && save.join("b.png").exists());

        clear_sinks(&sess);

        assert_eq!(std::fs::read_to_string(&txt).unwrap(), pre, "text_file not truncated to offset");
        assert!(!save.join("a.bin").exists(), "session file not removed");
        assert!(!save.join("b.png").exists(), "session image not removed");
        assert!(save.join("keep.dat").exists(), "pre-session file wrongly deleted");
        assert_eq!(std::fs::read_to_string(save.join("keep.dat")).unwrap(), "keep me");
    }

    /// Setting text_file mid-session anchors the offset to that file's current size.
    #[test]
    fn config_set_text_file_anchors_offset_mid_session() {
        let tmp = tempfile::tempdir().unwrap();
        let txt = tmp.path().join("log.txt");
        let pre = "OLD CONTENT\n";
        std::fs::write(&txt, pre).unwrap();
        let tf = txt.to_string_lossy().to_string();

        let sess = Mutex::new(Session::default()); // starts with no text_file
        sess.lock().unwrap().set_text_file(&tf); // `config set text_file <path>`
        route_sinks(&sess, "", &tf, &env_text("appended this session", "peerZ"), None);
        assert_ne!(std::fs::read_to_string(&txt).unwrap(), pre);

        clear_sinks(&sess);
        assert_eq!(std::fs::read_to_string(&txt).unwrap(), pre);
    }

    /// A `file` has no image clipboard form: pasting it copies its **path as text** (SPEC §2).
    #[test]
    fn file_paste_copies_path_as_text() {
        let bytes = b"\x00\x01binary";
        let env = env_blob(MsgType::File, bytes, Some("r.bin"));
        let path = "/tmp/clip-blobs/deadbeef.bin";
        let (hash, payload) = clip_payload(&env, Some(path), None).unwrap();
        match payload {
            clipboard::Payload::Text(t) => assert_eq!(t, path),
            _ => panic!("file must copy its path as clipboard text, not an image"),
        }
        assert_eq!(hash, blake3_hex(path.as_bytes()));

        // Image still copies PNG bytes; text still copies its text.
        let img = env_blob(MsgType::Image, bytes, Some("s.png"));
        match clip_payload(&img, None, Some(bytes.to_vec())).unwrap().1 {
            clipboard::Payload::ImagePng(b) => assert_eq!(b, bytes),
            _ => panic!("image must copy PNG bytes"),
        }
        match clip_payload(&env_text("hi", "d"), None, None).unwrap().1 {
            clipboard::Payload::Text(t) => assert_eq!(t, "hi"),
            _ => panic!("text must copy text"),
        }
    }

    /// `clear_on_exit` resolution for a non-interactive session end (SPEC §8).
    #[test]
    fn exit_scope_resolution() {
        assert_eq!(resolve_exit_scope("ask"), "transient");
        assert_eq!(resolve_exit_scope(""), "transient");
        assert_eq!(resolve_exit_scope("all"), "all");
        assert_eq!(resolve_exit_scope("never"), "never");
    }
}
