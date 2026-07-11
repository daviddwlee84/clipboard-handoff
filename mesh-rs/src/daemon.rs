//! The resident daemon: owns the iroh Endpoint + identity, the arboard clipboard,
//! the received-item ring buffer, the trusted-peer set, and the local IPC socket.

use std::{
    borrow::Cow,
    collections::{BTreeSet, HashMap, HashSet, VecDeque},
    net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr},
    sync::{Arc, Mutex},
    time::{SystemTime, UNIX_EPOCH},
};

use anyhow::{Context, Result, anyhow, bail};
use iroh::{
    Endpoint, EndpointAddr, EndpointId, RelayMode, TransportAddr,
    endpoint::{Connection, RecvStream, SendStream, presets},
    protocol::{AcceptError, ProtocolHandler, Router},
};
use serde_bytes::ByteBuf;
use tokio::sync::broadcast;
use ulid::Ulid;

use interprocess::local_socket::{
    GenericFilePath, ListenerOptions, ToFsName,
    tokio::Stream as IpcStream,
    traits::tokio::{Listener as _, Stream as _},
};

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
    config: Mutex<ConfigStore>,
    peers: Mutex<HashMap<EndpointId, PeerHandle>>,
    allowlist: Mutex<HashSet<EndpointId>>,
    ring: Mutex<VecDeque<Item>>,
    /// Content-addressed PNG store (locally-sent + received), keyed by BLAKE3 hex.
    blobs: Mutex<HashMap<String, Vec<u8>>>,
    dedup: Mutex<Dedup>,
    events: broadcast::Sender<Event>,
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

    // LAN-only for Phase 0: Minimal preset (crypto provider only, no discovery) +
    // relay disabled -> direct QUIC to the addresses carried in the pairing ticket.
    let endpoint = Endpoint::builder(presets::Minimal)
        .secret_key(secret.clone())
        .alpns(vec![alpn.clone()])
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .map_err(|e| anyhow!("iroh endpoint bind failed: {e}"))?;

    let (events, _rx) = broadcast::channel(256);
    let daemon = Arc::new(Daemon {
        paths: paths.clone(),
        endpoint: endpoint.clone(),
        device_name,
        config: Mutex::new(config),
        peers: Mutex::new(HashMap::new()),
        allowlist: Mutex::new(HashSet::new()),
        ring: Mutex::new(VecDeque::new()),
        blobs: Mutex::new(HashMap::new()),
        dedup: Mutex::new(Dedup::new(4096)),
        events,
    });

    // Accept loop for inbound iroh connections (same-room ALPN only).
    let _router = Router::builder(endpoint.clone())
        .accept(alpn, ClipProto(daemon.clone()))
        .spawn();

    tracing::info!(
        "clip daemon up: endpoint {} · room {:?} · socket {}",
        endpoint.id(),
        paths.room,
        paths.socket.display()
    );

    loop {
        let stream = listener.accept().await?;
        let d = daemon.clone();
        tokio::spawn(async move {
            if let Err(e) = d.handle_ipc(stream).await {
                tracing::debug!("ipc handler error: {e:#}");
            }
        });
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

        let mut local_path = None;
        if env.typ == MsgType::Image {
            let blob = env
                .blob
                .clone()
                .ok_or_else(|| anyhow!("image envelope missing blob ref"))?;
            // Pull the PNG bytes over a direct stream, keyed by hash (PROTOCOL §1).
            let bytes = self.fetch_blob(&conn, &blob.hash).await?;
            let got = blake3_hex(&bytes);
            if got != blob.hash {
                bail!("blob hash mismatch (got {got}, want {})", blob.hash);
            }
            self.blobs.lock().unwrap().insert(blob.hash.clone(), bytes.clone());
            let short = &blob.hash[..blob.hash.len().min(16)];
            let path = std::env::temp_dir().join(format!("clip-{short}.png"));
            std::fs::write(&path, &bytes).with_context(|| format!("write {}", path.display()))?;
            local_path = Some(path.to_string_lossy().to_string());
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

        // Auto-copy behavior (SPEC §3). Default = notify (never touch the clipboard).
        let mode = self.config.lock().unwrap().auto_copy();
        match mode.as_str() {
            "on" => {
                if let Err(e) = self.write_to_clipboard(&env, local_path.as_deref()).await {
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

    // ---- IPC (client <-> daemon) -----------------------------------------

    async fn handle_ipc(self: Arc<Self>, mut stream: IpcStream) -> Result<()> {
        let req: Req = read_frame(&mut stream).await?;
        match req {
            Req::Send { kind, bytes } => {
                let resp = match sniff(kind, &bytes) {
                    Ok(Sniffed::Text(text)) => {
                        let mut env = self.base_envelope(MsgType::Text, "text/plain; charset=utf-8");
                        env.text = Some(text);
                        let n = self.broadcast(&env).await;
                        Resp::Ok(OkData::Text(n.to_string()))
                    }
                    Ok(Sniffed::Image { png, w, h }) => {
                        let hash = blake3_hex(&png);
                        self.blobs.lock().unwrap().insert(hash.clone(), png.clone());
                        let mut env = self.base_envelope(MsgType::Image, "image/png");
                        env.blob = Some(Blob {
                            hash,
                            size: png.len() as u64,
                            w,
                            h,
                        });
                        let n = self.broadcast(&env).await;
                        Resp::Ok(OkData::Text(n.to_string()))
                    }
                    Err(e) => Resp::Err {
                        code: 2,
                        message: e.to_string(),
                    },
                };
                write_frame(&mut stream, &resp).await?;
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
                let resp = match self.paste_latest().await {
                    Ok(desc) => Resp::Ok(OkData::Text(desc)),
                    Err(e) => Resp::Err {
                        code: 5,
                        message: e.to_string(),
                    },
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
                    Ok(()) => Resp::Ok(OkData::None),
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
            text: None,
            blob: None,
        }
    }

    fn latest(&self, kind: RecvKind) -> Option<Item> {
        let ring = self.ring.lock().unwrap();
        ring.iter().rev().find(|it| kind_matches(kind, &it.envelope)).cloned()
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
        enum Payload {
            Text(String),
            ImagePng(Vec<u8>),
        }
        let (hash, payload) = match env.typ {
            MsgType::Text => {
                let text = env.text.clone().unwrap_or_default();
                (blake3_hex(text.as_bytes()), Payload::Text(text))
            }
            MsgType::Image => {
                let blob = env
                    .blob
                    .clone()
                    .ok_or_else(|| anyhow!("image envelope missing blob"))?;
                let bytes = self
                    .blobs
                    .lock()
                    .unwrap()
                    .get(&blob.hash)
                    .cloned()
                    .or_else(|| local_path.and_then(|p| std::fs::read(p).ok()))
                    .ok_or_else(|| anyhow!("image bytes unavailable"))?;
                (blob.hash.clone(), Payload::ImagePng(bytes))
            }
        };

        // arboard is blocking and its Clipboard isn't Send; build + use it inside
        // the blocking thread so nothing crosses threads.
        tokio::task::spawn_blocking(move || -> Result<()> {
            let mut cb = arboard::Clipboard::new().map_err(|e| anyhow!("clipboard init: {e}"))?;
            match payload {
                Payload::Text(t) => cb.set_text(t).map_err(|e| anyhow!("set_text: {e}"))?,
                Payload::ImagePng(png) => {
                    let img = image::load_from_memory(&png)
                        .context("decode png")?
                        .to_rgba8();
                    let data = arboard::ImageData {
                        width: img.width() as usize,
                        height: img.height() as usize,
                        bytes: Cow::Owned(img.into_raw()),
                    };
                    cb.set_image(data).map_err(|e| anyhow!("set_image: {e}"))?;
                }
            }
            Ok(())
        })
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
}

fn kind_matches(kind: RecvKind, env: &Envelope) -> bool {
    match kind {
        RecvKind::Any => true,
        RecvKind::Text => env.typ == MsgType::Text,
        RecvKind::Image => env.typ == MsgType::Image,
    }
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
    }
}

fn short_id(id: &EndpointId) -> String {
    let s = id.to_string();
    s.chars().take(12).collect()
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
