//! `clip tui` — a messenger-style chat attached to the local daemon (SPEC §4).
//!
//! The TUI is a thin **front-end**: it never touches the OS clipboard itself (that would make it a
//! second clipboard owner). It talks to the resident daemon over the same local IPC the other
//! subcommands use — `Subscribe` for the live event stream, `Send` to broadcast composed text, and
//! `PasteItem` to ask the daemon to place a chosen bubble on the clipboard. The daemon is
//! auto-spawned if it isn't running.
//!
//! Rendering split:
//! * The pure model ([`App`]) holds messages/selection/header/toast and is unit-tested headlessly.
//! * The IO shell ([`run`]) owns the terminal, the image workers, and the tokio `select!` loop.
//!
//! Images: each incoming image bubble always shows metadata (WxH · size · file). A thumbnail is
//! rendered inline via ratatui-image's `StatefulProtocol` — a real graphics protocol
//! (Kitty/iTerm2/Sixel) when the terminal advertises one, otherwise Unicode half-blocks; if decode
//! fails or the thumbnail isn't ready the metadata line stands in as the text placeholder. Decode
//! and resize/encode run **off** the UI thread (a decode `spawn_blocking` + a resize worker task),
//! so the event loop never blocks on image work.

use std::collections::HashMap;
use std::path::PathBuf;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::Result;
use crossterm::event::{Event as CtEvent, KeyCode, KeyEventKind, KeyModifiers};
use ratatui::{
    Frame,
    layout::{Constraint, Layout, Rect},
    style::{Color, Style, Stylize},
    text::{Line, Span, Text},
    widgets::{Block, Borders, Paragraph},
};
use ratatui_image::{
    Resize, ResizeEncodeRender, picker::Picker, protocol::StatefulProtocol,
};
use serde_bytes::ByteBuf;
use tokio::sync::mpsc;
use tui_textarea::TextArea;

use crate::client::{connect_or_spawn, request};
use crate::config::Paths;
use crate::proto::{
    Blob, Envelope, Event, MsgType, OkData, Req, Resp, SniffKind, StatusInfo, read_frame,
    write_frame,
};

const IMG_ROWS: usize = 10; // inline-thumbnail height in cells
const IMG_MAX_COLS: u16 = 52; // inline-thumbnail max width in cells
const IMG_CAPTION_LINES: usize = 2; // header line + metadata caption above the thumbnail
const TOAST_TTL: Duration = Duration::from_secs(4);
const SEL_BG: Color = Color::Indexed(238);

// ---------------------------------------------------------------------------
// Pure model
// ---------------------------------------------------------------------------

/// Static-ish header facts, seeded from `status` and nudged by peer events.
#[derive(Clone, Debug)]
pub struct Header {
    pub room: String,
    pub my_id: String,
    pub my_short: String,
    pub peer_count: usize,
    pub auto_copy: String,
    pub clipboard_available: bool,
}

#[derive(Clone, Debug)]
enum Body {
    Text(String),
    Image {
        blob: Blob,
        local_path: Option<String>,
    },
    /// An arbitrary file: no thumbnail, no image clipboard form — `y` copies its path as text.
    File {
        blob: Blob,
        filename: String,
        local_path: Option<String>,
    },
}

#[derive(Clone, Debug)]
struct Message {
    msg_id: String,
    sender: String,
    mine: bool,
    ts: u64,
    body: Body,
    /// Outgoing bubble awaiting the daemon's send ack.
    pending: bool,
    /// Trailing status note (e.g. "not delivered (no peers)" / "send failed").
    note: Option<String>,
    /// notify-mode incoming item not yet copied — drives the "press y" affordance.
    accept_pending: bool,
    /// Correlates an optimistic outgoing bubble with its async send result.
    client_seq: Option<u64>,
}

/// The action a key press asks the IO shell to perform (kept coarse so the model stays pure).
#[derive(Debug, PartialEq, Eq)]
enum KeyOutcome {
    None,
    Redraw,
    Quit,
    Send(Outgoing),
    Copy,
    Save,
    Open,
    /// Answer to the clear-on-quit prompt (SPEC §8): run this clear on the daemon, then exit.
    /// `all` == transient + session sink writes; otherwise transient only.
    ClearAndQuit { all: bool },
}

/// A composed line handed to the IO shell to broadcast; `seq` ties it back to its bubble.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Outgoing {
    seq: u64,
    text: String,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Focus {
    Compose,
    Browse,
}

/// The whole TUI state that is independent of the terminal/network — unit-testable in isolation.
pub struct App {
    header: Header,
    messages: Vec<Message>,
    /// `None` == pinned to the newest message (follow mode); `Some(i)` == a browsed highlight.
    selected: Option<usize>,
    follow: bool,
    focus: Focus,
    composer: TextArea<'static>,
    toast: Option<(String, Instant)>,
    next_seq: u64,
    should_quit: bool,
    /// Something was received this session → quitting asks whether to clear it (SPEC §8).
    received_any: bool,
    /// The clear-on-quit prompt is up and capturing keys.
    quit_prompt: bool,
}

impl App {
    fn new(header: Header) -> Self {
        Self {
            header,
            messages: Vec::new(),
            selected: None,
            follow: true,
            focus: Focus::Compose,
            composer: make_composer(),
            toast: None,
            next_seq: 1,
            should_quit: false,
            received_any: false,
            quit_prompt: false,
        }
    }

    // ---- event ingest ----------------------------------------------------

    /// Apply a daemon event. Returns true if the view should redraw.
    fn apply_event(&mut self, ev: Event) -> bool {
        match ev {
            Event::Item {
                envelope,
                local_path,
            } => {
                self.push_incoming(envelope, local_path);
                true
            }
            Event::PeerUp { name, .. } => {
                self.header.peer_count += 1;
                self.set_toast(format!("{name} connected"));
                true
            }
            Event::PeerDown { .. } => {
                self.header.peer_count = self.header.peer_count.saturating_sub(1);
                self.set_toast("a peer disconnected".to_string());
                true
            }
            // The daemon's notify Toast wording ("run `clip paste`") is superseded by our own
            // in-TUI "press y" affordance emitted from `push_incoming`, so we drop it here.
            Event::Toast { .. } => false,
        }
    }

    fn push_incoming(&mut self, env: Envelope, local_path: Option<String>) {
        let mine = env.sender == self.header.my_id;
        let sender = if env.device_name.is_empty() {
            short(&env.sender)
        } else {
            env.device_name.clone()
        };
        let blank_blob = Blob {
            hash: String::new(),
            size: 0,
            w: 0,
            h: 0,
        };
        let body = match env.typ {
            MsgType::Text => Body::Text(env.text.clone().unwrap_or_default()),
            MsgType::Image => Body::Image {
                blob: env.blob.clone().unwrap_or(blank_blob),
                local_path,
            },
            MsgType::File => Body::File {
                blob: env.blob.clone().unwrap_or(blank_blob),
                filename: env.filename.clone().unwrap_or_else(|| "file".to_string()),
                local_path,
            },
        };
        // Anything received this session arms the clear-on-quit prompt (SPEC §8).
        self.received_any |= !mine;
        let notify = self.header.auto_copy == "notify";
        let desc = describe(&env);
        self.messages.push(Message {
            msg_id: env.msg_id,
            sender,
            mine,
            ts: env.ts,
            body,
            pending: false,
            note: None,
            accept_pending: notify && !mine,
            client_seq: None,
        });
        if self.follow {
            self.selected = None;
        }
        if !mine {
            match self.header.auto_copy.as_str() {
                "notify" => self.set_toast(format!("{desc} — press y to copy")),
                "on" => self.set_toast(format!("{desc} — copied")),
                _ => self.set_toast(desc),
            }
        }
    }

    /// Read the composer, and if non-empty: clear it, optimistically append an own bubble, and
    /// return the send action for the IO shell. This is the "submitting composer text produces a
    /// send action" seam exercised by the unit tests.
    fn submit_text(&mut self) -> Option<Outgoing> {
        let text = self.composer.lines().join("\n");
        let text = text.trim().to_string();
        if text.is_empty() {
            return None;
        }
        self.composer = make_composer();
        let seq = self.next_seq;
        self.next_seq += 1;
        self.messages.push(Message {
            msg_id: String::new(),
            sender: "you".to_string(),
            mine: true,
            ts: now_ms(),
            body: Body::Text(text.clone()),
            pending: true,
            note: None,
            accept_pending: false,
            client_seq: Some(seq),
        });
        self.selected = None;
        self.follow = true;
        Some(Outgoing { seq, text })
    }

    /// Reconcile an optimistic outgoing bubble with its daemon send result.
    fn on_sent_result(&mut self, seq: u64, result: Result<(String, usize), String>) {
        let Some(m) = self.messages.iter_mut().find(|m| m.client_seq == Some(seq)) else {
            return;
        };
        m.pending = false;
        match result {
            Ok((msg_id, reached)) => {
                m.msg_id = msg_id;
                if reached == 0 {
                    m.note = Some("not delivered (no peers)".to_string());
                }
            }
            Err(e) => {
                m.note = Some(format!("send failed: {e}"));
            }
        }
    }

    fn mark_copied(&mut self, msg_id: &str) {
        if let Some(m) = self.messages.iter_mut().find(|m| m.msg_id == msg_id) {
            m.accept_pending = false;
        }
    }

    // ---- navigation ------------------------------------------------------

    fn select_up(&mut self) {
        if self.messages.is_empty() {
            return;
        }
        self.follow = false;
        let cur = self.selected.unwrap_or(self.messages.len());
        let new = cur.saturating_sub(1).min(self.messages.len() - 1);
        self.selected = Some(new);
    }

    fn select_down(&mut self) {
        if self.messages.is_empty() {
            return;
        }
        let last = self.messages.len() - 1;
        match self.selected {
            None => {}
            Some(i) if i >= last => {
                self.follow = true;
                self.selected = None;
            }
            Some(i) => self.selected = Some(i + 1),
        }
    }

    /// The message a `y`/`s`/`o` action targets: the highlight, else the latest.
    fn target_index(&self) -> Option<usize> {
        if self.messages.is_empty() {
            return None;
        }
        Some(self.selected.unwrap_or(self.messages.len() - 1))
    }

    fn target(&self) -> Option<&Message> {
        self.target_index().and_then(|i| self.messages.get(i))
    }

    // ---- toast -----------------------------------------------------------

    fn set_toast(&mut self, text: String) {
        self.toast = Some((text, Instant::now()));
    }

    fn expire_toast(&mut self) -> bool {
        if let Some((_, at)) = &self.toast {
            if at.elapsed() > TOAST_TTL {
                self.toast = None;
                return true;
            }
        }
        false
    }

    // ---- quit (SPEC §8) --------------------------------------------------

    /// `q`/Ctrl-C: if anything was received this session, raise the clear prompt instead of
    /// quitting outright; otherwise quit straight away.
    fn request_quit(&mut self) -> KeyOutcome {
        if self.received_any {
            self.quit_prompt = true;
            KeyOutcome::Redraw
        } else {
            KeyOutcome::Quit
        }
    }

    // ---- key handling ----------------------------------------------------

    fn handle_key(&mut self, ev: &CtEvent) -> KeyOutcome {
        let CtEvent::Key(key) = ev else {
            return KeyOutcome::None;
        };
        // Ignore key-release / repeat noise (Windows + some *nix terminals emit releases).
        if key.kind == KeyEventKind::Release {
            return KeyOutcome::None;
        }
        // The clear-on-quit prompt (SPEC §8) captures every key until it is answered.
        if self.quit_prompt {
            return match key.code {
                KeyCode::Char('t') => KeyOutcome::ClearAndQuit { all: false },
                KeyCode::Char('a') => KeyOutcome::ClearAndQuit { all: true },
                KeyCode::Char('n') | KeyCode::Esc => KeyOutcome::Quit, // keep everything
                _ => KeyOutcome::None,
            };
        }
        if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c') {
            return self.request_quit();
        }

        match self.focus {
            Focus::Compose => match key.code {
                KeyCode::Enter => match self.submit_text() {
                    Some(o) => KeyOutcome::Send(o),
                    None => KeyOutcome::None,
                },
                KeyCode::Esc => {
                    self.focus = Focus::Browse;
                    if self.selected.is_none() && !self.messages.is_empty() {
                        self.follow = false;
                        self.selected = Some(self.messages.len() - 1);
                    }
                    KeyOutcome::Redraw
                }
                KeyCode::Up => {
                    self.select_up();
                    KeyOutcome::Redraw
                }
                KeyCode::Down => {
                    self.select_down();
                    KeyOutcome::Redraw
                }
                _ => {
                    self.composer.input(ev.clone());
                    KeyOutcome::Redraw
                }
            },
            Focus::Browse => match key.code {
                KeyCode::Esc | KeyCode::Char('i') => {
                    self.focus = Focus::Compose;
                    KeyOutcome::Redraw
                }
                KeyCode::Char('q') => self.request_quit(),
                KeyCode::Up | KeyCode::Char('k') => {
                    self.select_up();
                    KeyOutcome::Redraw
                }
                KeyCode::Down | KeyCode::Char('j') => {
                    self.select_down();
                    KeyOutcome::Redraw
                }
                KeyCode::Char('y') => KeyOutcome::Copy,
                KeyCode::Char('s') => KeyOutcome::Save,
                KeyCode::Char('o') => KeyOutcome::Open,
                _ => KeyOutcome::None,
            },
        }
    }
}

fn make_composer() -> TextArea<'static> {
    let mut ta = TextArea::default();
    ta.set_cursor_line_style(Style::default());
    ta.set_placeholder_text("Type a message · Enter to send");
    ta
}

// ---------------------------------------------------------------------------
// IO shell: image workers
// ---------------------------------------------------------------------------

/// Per-image render state held only in the IO shell (protocols aren't part of the pure model).
enum ImgState {
    Decoding,
    Ready(Box<StatefulProtocol>),
    Resizing,
    Failed,
}

/// Results flowing back from the decode/resize workers to the UI loop.
enum ImgEvent {
    Decoded { msg_id: String, proto: Box<StatefulProtocol> },
    Resized { msg_id: String, proto: Box<StatefulProtocol> },
    Failed { msg_id: String },
}

/// A resize+encode job dispatched off the UI thread.
struct ResizeJob {
    msg_id: String,
    proto: Box<StatefulProtocol>,
    resize: Resize,
    area: Rect,
}

/// Results of the async IPC commands (send/copy/save/open) routed back to the UI loop.
enum CmdResult {
    Sent { seq: u64, result: Result<(String, usize), String> },
    Copied { msg_id: String, result: Result<String, String> },
    Saved(Result<String, String>),
    Opened(Result<String, String>),
}

enum DaemonMsg {
    Event(Event),
    Disconnected,
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

pub async fn run(paths: &Paths) -> Result<i32> {
    // 1. Reach the daemon (auto-spawns) and read its state for the header.
    let status = match request(paths, Req::Status).await {
        Ok(Resp::Ok(OkData::Status(s))) => s,
        Ok(Resp::Err { message, .. }) => {
            eprintln!("clip tui: {message}");
            return Ok(1);
        }
        Ok(_) => {
            eprintln!("clip tui: unexpected status response");
            return Ok(1);
        }
        Err(e) => {
            eprintln!("clip tui: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };

    // 2. Open a dedicated Subscribe connection and pump its events onto a channel. Spawning the
    // reader here (before we grab the terminal) keeps the stream's concrete type out of the loop.
    let mut sub = match connect_or_spawn(paths).await {
        Ok(s) => s,
        Err(e) => {
            eprintln!("clip tui: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    if let Err(e) = write_frame(&mut sub, &Req::Subscribe).await {
        eprintln!("clip tui: subscribe failed: {e:#}");
        return Ok(1);
    }
    let (daemon_tx, daemon_rx) = mpsc::unbounded_channel::<DaemonMsg>();
    tokio::spawn(async move {
        loop {
            match read_frame::<_, Event>(&mut sub).await {
                Ok(ev) => {
                    if daemon_tx.send(DaemonMsg::Event(ev)).is_err() {
                        break;
                    }
                }
                Err(_) => {
                    let _ = daemon_tx.send(DaemonMsg::Disconnected);
                    break;
                }
            }
        }
    });

    // 3. Take over the terminal (raw mode + alt screen + panic-restore hook).
    let mut terminal = match ratatui::try_init() {
        Ok(t) => t,
        Err(e) => {
            eprintln!("clip tui: not a terminal ({e}). Run `clip tui` in an interactive shell.");
            return Ok(1);
        }
    };
    // Detect the terminal's graphics protocol (Kitty/iTerm2/Sixel); fall back to half-blocks.
    let picker = Picker::from_query_stdio().unwrap_or_else(|_| Picker::halfblocks());

    let res = event_loop(paths, &mut terminal, status, picker, daemon_rx).await;

    ratatui::restore();
    res
}

async fn event_loop(
    paths: &Paths,
    terminal: &mut ratatui::DefaultTerminal,
    status: StatusInfo,
    picker: Picker,
    mut daemon_rx: mpsc::UnboundedReceiver<DaemonMsg>,
) -> Result<i32> {
    let header = Header {
        my_short: short(&status.endpoint_id),
        room: status.room,
        my_id: status.endpoint_id,
        peer_count: status.peer_count,
        auto_copy: status.auto_copy,
        clipboard_available: status.clipboard == "available",
    };
    let mut app = App::new(header);
    let mut images: HashMap<String, ImgState> = HashMap::new();

    // ---- channels + workers ---------------------------------------------
    // Blocking key reader on its own OS thread → tokio channel (avoids Stream trait juggling).
    let (key_tx, mut key_rx) = mpsc::unbounded_channel::<CtEvent>();
    std::thread::spawn(move || {
        while let Ok(ev) = crossterm::event::read() {
            if key_tx.send(ev).is_err() {
                break;
            }
        }
    });

    let (img_tx, mut img_rx) = mpsc::unbounded_channel::<ImgEvent>();
    let (resize_tx, mut resize_rx) = mpsc::unbounded_channel::<ResizeJob>();
    {
        // Resize/encode worker: one job at a time, each on the blocking pool, off the UI thread.
        let img_tx = img_tx.clone();
        tokio::spawn(async move {
            while let Some(job) = resize_rx.recv().await {
                let img_tx = img_tx.clone();
                tokio::task::spawn_blocking(move || {
                    let ResizeJob {
                        msg_id,
                        mut proto,
                        resize,
                        area,
                    } = job;
                    ResizeEncodeRender::resize_encode(&mut *proto, &resize, area);
                    let ok = proto
                        .last_encoding_result()
                        .map(|r| r.is_ok())
                        .unwrap_or(true);
                    let _ = img_tx.send(if ok {
                        ImgEvent::Resized { msg_id, proto }
                    } else {
                        ImgEvent::Failed { msg_id }
                    });
                });
            }
        });
    }
    let (cmd_tx, mut cmd_rx) = mpsc::unbounded_channel::<CmdResult>();

    let mut tick = tokio::time::interval(Duration::from_millis(300));

    loop {
        terminal.draw(|f| ui(f, &app, &mut images, &resize_tx))?;
        if app.should_quit {
            break;
        }

        tokio::select! {
            Some(ev) = key_rx.recv() => {
                match app.handle_key(&ev) {
                    KeyOutcome::Quit => app.should_quit = true,
                    // Run the same clear the CLI's `clip clear [--all]` runs, then exit (SPEC §8).
                    KeyOutcome::ClearAndQuit { all } => {
                        let _ = request(paths, Req::Clear { all }).await;
                        app.should_quit = true;
                    }
                    KeyOutcome::Send(o) => spawn_send(paths, &cmd_tx, o),
                    KeyOutcome::Copy => do_copy(&mut app, paths, &cmd_tx),
                    KeyOutcome::Save => do_save(&mut app, paths, &cmd_tx),
                    KeyOutcome::Open => do_open(&mut app, paths, &cmd_tx),
                    KeyOutcome::None | KeyOutcome::Redraw => {}
                }
            }
            Some(msg) = daemon_rx.recv() => match msg {
                DaemonMsg::Event(ev) => {
                    if let Event::Item { envelope, local_path } = &ev {
                        if envelope.typ == MsgType::Image {
                            dispatch_decode(&mut images, &picker, &img_tx, envelope.msg_id.clone(), local_path.clone());
                        }
                    }
                    app.apply_event(ev);
                }
                DaemonMsg::Disconnected => app.set_toast("daemon disconnected".to_string()),
            },
            Some(ie) = img_rx.recv() => apply_img_event(&mut images, ie),
            Some(cr) = cmd_rx.recv() => apply_cmd_result(&mut app, cr),
            _ = tick.tick() => { app.expire_toast(); }
        }
    }
    Ok(0)
}

fn apply_img_event(images: &mut HashMap<String, ImgState>, ie: ImgEvent) {
    match ie {
        ImgEvent::Decoded { msg_id, proto } => {
            images.insert(msg_id, ImgState::Ready(proto));
        }
        ImgEvent::Resized { msg_id, proto } => {
            images.insert(msg_id, ImgState::Ready(proto));
        }
        ImgEvent::Failed { msg_id } => {
            images.insert(msg_id, ImgState::Failed);
        }
    }
}

fn apply_cmd_result(app: &mut App, cr: CmdResult) {
    match cr {
        CmdResult::Sent { seq, result } => app.on_sent_result(seq, result),
        CmdResult::Copied { msg_id, result } => match result {
            Ok(desc) => {
                app.mark_copied(&msg_id);
                app.set_toast(format!("copied: {desc}"));
            }
            Err(e) => app.set_toast(format!("copy failed: {e}")),
        },
        CmdResult::Saved(Ok(path)) => app.set_toast(format!("saved {path}")),
        CmdResult::Saved(Err(e)) => app.set_toast(format!("save failed: {e}")),
        CmdResult::Opened(Ok(path)) => app.set_toast(format!("opened {path}")),
        CmdResult::Opened(Err(e)) => app.set_toast(format!("open failed: {e}")),
    }
}

// ---------------------------------------------------------------------------
// IO shell: async command dispatch
// ---------------------------------------------------------------------------

fn spawn_send(paths: &Paths, cmd_tx: &mpsc::UnboundedSender<CmdResult>, o: Outgoing) {
    let paths = paths.clone();
    let tx = cmd_tx.clone();
    tokio::spawn(async move {
        let res = request(
            &paths,
            Req::Send {
                kind: SniffKind::Text,
                bytes: ByteBuf::from(o.text.into_bytes()),
                filename: None,
            },
        )
        .await;
        let out = match res {
            Ok(Resp::Ok(OkData::Sent { msg_id, reached })) => Ok((msg_id, reached)),
            Ok(Resp::Err { message, .. }) => Err(message),
            Ok(_) => Err("unexpected send response".to_string()),
            Err(e) => Err(e.to_string()),
        };
        let _ = tx.send(CmdResult::Sent { seq: o.seq, result: out });
    });
}

fn do_copy(app: &mut App, paths: &Paths, cmd_tx: &mpsc::UnboundedSender<CmdResult>) {
    if !app.header.clipboard_available {
        app.set_toast("clipboard unavailable (headless)".to_string());
        return;
    }
    let Some(m) = app.target() else {
        app.set_toast("nothing to copy".to_string());
        return;
    };
    if m.pending || m.msg_id.is_empty() {
        app.set_toast("still sending — try again in a moment".to_string());
        return;
    }
    let msg_id = m.msg_id.clone();
    let paths = paths.clone();
    let tx = cmd_tx.clone();
    tokio::spawn(async move {
        let res = request(&paths, Req::PasteItem { msg_id: msg_id.clone() }).await;
        let out = match res {
            Ok(Resp::Ok(OkData::Text(desc))) => Ok(desc),
            Ok(Resp::Err { message, .. }) => Err(message),
            Ok(_) => Err("unexpected copy response".to_string()),
            Err(e) => Err(e.to_string()),
        };
        let _ = tx.send(CmdResult::Copied { msg_id, result: out });
    });
}

fn do_save(app: &mut App, _paths: &Paths, cmd_tx: &mpsc::UnboundedSender<CmdResult>) {
    let Some(m) = app.target() else {
        app.set_toast("nothing selected".to_string());
        return;
    };
    let (hash, src, name) = match &m.body {
        Body::Image {
            blob,
            local_path: Some(p),
        } => (blob.hash.clone(), p.clone(), None),
        Body::File {
            blob,
            filename,
            local_path: Some(p),
        } => (blob.hash.clone(), p.clone(), Some(filename.clone())),
        Body::Image { .. } | Body::File { .. } => {
            app.set_toast("not downloaded yet".to_string());
            return;
        }
        Body::Text(_) => {
            app.set_toast("not a file (s only saves images/files)".to_string());
            return;
        }
    };
    let dest = match name {
        Some(n) => {
            let dir = save_dest(&hash);
            dir.with_file_name(n)
        }
        None => save_dest(&hash),
    };
    let tx = cmd_tx.clone();
    tokio::spawn(async move {
        let dest_str = dest.display().to_string();
        let res = tokio::task::spawn_blocking(move || {
            std::fs::copy(&src, &dest)
                .map(|_| dest.display().to_string())
                .map_err(|e| e.to_string())
        })
        .await
        .unwrap_or_else(|_| Err(format!("save task panicked ({dest_str})")));
        let _ = tx.send(CmdResult::Saved(res));
    });
}

fn do_open(app: &mut App, _paths: &Paths, cmd_tx: &mpsc::UnboundedSender<CmdResult>) {
    let Some(m) = app.target() else {
        app.set_toast("nothing selected".to_string());
        return;
    };
    let src = match &m.body {
        Body::Image {
            local_path: Some(p),
            ..
        }
        | Body::File {
            local_path: Some(p),
            ..
        } => p.clone(),
        Body::Image { .. } | Body::File { .. } => {
            app.set_toast("not downloaded yet".to_string());
            return;
        }
        Body::Text(_) => {
            app.set_toast("not a file (o only opens images/files)".to_string());
            return;
        }
    };
    let tx = cmd_tx.clone();
    tokio::spawn(async move {
        let res = tokio::task::spawn_blocking(move || open_external(&src))
            .await
            .unwrap_or_else(|_| Err("open task panicked".to_string()));
        let _ = tx.send(CmdResult::Opened(res));
    });
}

fn dispatch_decode(
    images: &mut HashMap<String, ImgState>,
    picker: &Picker,
    img_tx: &mpsc::UnboundedSender<ImgEvent>,
    msg_id: String,
    local_path: Option<String>,
) {
    let Some(path) = local_path else {
        images.insert(msg_id, ImgState::Failed);
        return;
    };
    images.insert(msg_id.clone(), ImgState::Decoding);
    let picker = picker.clone();
    let tx = img_tx.clone();
    tokio::task::spawn_blocking(move || {
        let decoded = std::fs::read(&path)
            .ok()
            .and_then(|b| image::load_from_memory(&b).ok());
        let ev = match decoded {
            Some(img) => ImgEvent::Decoded {
                msg_id,
                proto: Box::new(picker.new_resize_protocol(img)),
            },
            None => ImgEvent::Failed { msg_id },
        };
        let _ = tx.send(ev);
    });
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

/// A message laid out into a row-range in the scrollback, plus its image placement (if any).
struct BlockInfo {
    start: usize,
    height: usize,
    img: Option<String>, // msg_id of an image whose thumbnail overlays this block
}

fn ui(
    f: &mut Frame,
    app: &App,
    images: &mut HashMap<String, ImgState>,
    resize_tx: &mpsc::UnboundedSender<ResizeJob>,
) {
    let chunks = Layout::vertical([
        Constraint::Length(3), // header
        Constraint::Min(1),    // scrollback
        Constraint::Length(3), // composer
        Constraint::Length(1), // hint / toast
    ])
    .split(f.area());

    render_header(f, app, chunks[0]);
    render_messages(f, app, images, resize_tx, chunks[1]);
    render_composer(f, app, chunks[2]);
    render_hint(f, app, chunks[3]);
}

fn render_header(f: &mut Frame, app: &App, area: Rect) {
    let h = &app.header;
    let clip = if h.clipboard_available {
        Span::raw("clipboard: available").green()
    } else {
        Span::raw("clipboard: unavailable").red()
    };
    let line = Line::from(vec![
        Span::raw(format!("room {}", h.room)).bold(),
        Span::raw("  ·  ").dim(),
        Span::raw(format!("id {}", h.my_short)),
        Span::raw("  ·  ").dim(),
        Span::raw(format!("{} peer(s)", h.peer_count)).cyan(),
        Span::raw("  ·  ").dim(),
        Span::raw(format!("auto_copy {}", h.auto_copy)),
        Span::raw("  ·  ").dim(),
        clip,
    ]);
    let block = Block::default()
        .borders(Borders::ALL)
        .title(Line::from(Span::raw(" clip · messenger ").bold()));
    f.render_widget(Paragraph::new(line).block(block), area);
}

fn render_messages(
    f: &mut Frame,
    app: &App,
    images: &mut HashMap<String, ImgState>,
    resize_tx: &mpsc::UnboundedSender<ResizeJob>,
    area: Rect,
) {
    let block = Block::default().borders(Borders::ALL).title(" chat ");
    let inner = block.inner(area);
    f.render_widget(block, area);
    if inner.width == 0 || inner.height == 0 {
        return;
    }

    let now = now_ms();
    let (lines, blocks, total) = build_lines(app, inner.width as usize, now);
    let viewport = inner.height as usize;

    // Scroll: pin to bottom while following, else keep the highlighted block visible.
    let max_off = total.saturating_sub(viewport);
    let offset = match (app.follow, app.selected) {
        (false, Some(i)) if i < blocks.len() => {
            let b = &blocks[i];
            let end = b.start + b.height;
            let mut off = end.saturating_sub(viewport);
            if off > b.start {
                off = b.start;
            }
            off.min(max_off)
        }
        _ => max_off,
    };

    f.render_widget(
        Paragraph::new(Text::from(lines)).scroll((offset as u16, 0)),
        inner,
    );

    // Overlay image thumbnails that are fully within the viewport.
    for b in &blocks {
        let Some(msg_id) = &b.img else { continue };
        let img_abs = b.start + IMG_CAPTION_LINES;
        if img_abs < offset {
            continue;
        }
        let rel = img_abs - offset;
        if rel + IMG_ROWS > viewport {
            continue;
        }
        let rect = Rect {
            x: inner.x,
            y: inner.y + rel as u16,
            width: inner.width.min(IMG_MAX_COLS),
            height: IMG_ROWS as u16,
        };
        render_thumbnail(f, images, msg_id, rect, resize_tx);
    }
}

/// Render an image thumbnail into `rect` without ever blocking the UI thread: if the protocol
/// still needs a (re)encode for this area, hand it to the resize worker and skip drawing this frame
/// (the caption placeholder shows through); otherwise draw the cached, already-encoded thumbnail.
fn render_thumbnail(
    f: &mut Frame,
    images: &mut HashMap<String, ImgState>,
    msg_id: &str,
    rect: Rect,
    resize_tx: &mpsc::UnboundedSender<ResizeJob>,
) {
    let Some(state) = images.get_mut(msg_id) else {
        return;
    };
    let resize = Resize::Fit(None);
    let need = if let ImgState::Ready(p) = state {
        p.needs_resize(&resize, rect)
    } else {
        None
    };
    match state {
        ImgState::Ready(p) if need.is_none() => {
            p.render(rect, f.buffer_mut());
        }
        ImgState::Ready(_) => {
            if let ImgState::Ready(proto) = std::mem::replace(state, ImgState::Resizing) {
                let _ = resize_tx.send(ResizeJob {
                    msg_id: msg_id.to_string(),
                    proto,
                    resize,
                    area: need.unwrap_or(rect),
                });
            }
        }
        _ => {}
    }
}

/// Build the whole scrollback as styled lines plus per-message block metadata (for scroll + image
/// overlay). Wrapping is done here so heights are exact and image rows line up with the buffer.
fn build_lines(app: &App, width: usize, now: u64) -> (Vec<Line<'static>>, Vec<BlockInfo>, usize) {
    let width = width.max(1);
    let mut lines: Vec<Line<'static>> = Vec::new();
    let mut blocks: Vec<BlockInfo> = Vec::new();
    let target = app.target_index();

    if app.messages.is_empty() {
        lines.push(Line::from(Span::raw("No messages yet. Type below and press Enter.").dim()));
    }

    for (i, m) in app.messages.iter().enumerate() {
        let selected = Some(i) == target && !app.follow;
        let start = lines.len();
        let reltime = reltime(m.ts, now);

        // Header line: sender · time (+ status / affordance).
        let mut hspans: Vec<Span<'static>> = Vec::new();
        if m.mine {
            hspans.push(Span::raw("you").green().bold());
        } else {
            hspans.push(Span::raw(m.sender.clone()).cyan().bold());
        }
        hspans.push(Span::raw(format!("  ·  {reltime}")).dim());
        if m.pending {
            hspans.push(Span::raw("  · sending…").dim());
        }
        if let Some(note) = &m.note {
            hspans.push(Span::raw(format!("  · {note}")).yellow());
        }
        if m.accept_pending {
            hspans.push(Span::raw("  ● press y to copy").yellow().bold());
        }
        let mut header = Line::from(hspans);
        if m.mine {
            header = header.right_aligned();
        }
        lines.push(header);

        let mut img = None;
        match &m.body {
            Body::Text(t) => {
                for wl in wrap_line(t, width) {
                    let mut l = Line::from(Span::raw(wl));
                    if m.mine {
                        l = l.right_aligned();
                    }
                    lines.push(l);
                }
            }
            Body::Image { blob, local_path } => {
                let file = local_path
                    .as_deref()
                    .and_then(|p| PathBuf::from(p).file_name().map(|s| s.to_string_lossy().into_owned()))
                    .unwrap_or_else(|| "image.png".to_string());
                let caption = format!(
                    "🖼  {}×{} · {} · {}",
                    blob.w,
                    blob.h,
                    human_size(blob.size),
                    file
                );
                lines.push(Line::from(Span::raw(caption).magenta()));
                for _ in 0..IMG_ROWS {
                    lines.push(Line::from(Span::raw(String::new())));
                }
                img = Some(m.msg_id.clone());
            }
            // A file has no visual form: a one-line card (name · size), `y` copies its path.
            Body::File {
                blob,
                filename,
                local_path,
            } => {
                let caption = format!("📄 {} · {}", filename, human_size(blob.size));
                let mut l = Line::from(Span::raw(caption).blue());
                if m.mine {
                    l = l.right_aligned();
                }
                lines.push(l);
                if let Some(p) = local_path {
                    lines.push(Line::from(Span::raw(format!("   {p}")).dim()));
                }
            }
        }

        // Blank spacer between bubbles.
        lines.push(Line::from(Span::raw(String::new())));

        if selected {
            for l in lines.iter_mut().skip(start) {
                *l = std::mem::take(l).patch_style(Style::default().bg(SEL_BG));
            }
        }
        let height = lines.len() - start;
        blocks.push(BlockInfo { start, height, img });
    }

    let total = lines.len();
    (lines, blocks, total)
}

fn render_composer(f: &mut Frame, app: &App, area: Rect) {
    let focused = app.focus == Focus::Compose;
    let title = if focused {
        " compose (Enter=send · Esc=browse) "
    } else {
        " compose (Esc/i to focus) "
    };
    let border = if focused {
        Style::default().fg(Color::Green)
    } else {
        Style::default().add_modifier(ratatui::style::Modifier::DIM)
    };
    let block = Block::default()
        .borders(Borders::ALL)
        .border_style(border)
        .title(title);
    let inner = block.inner(area);
    f.render_widget(block, area);
    f.render_widget(&app.composer, inner);
}

fn render_hint(f: &mut Frame, app: &App, area: Rect) {
    let line = if app.quit_prompt {
        // SPEC §8: the TUI quit prompt.
        Line::from(
            Span::raw(" ⚑ clear this session? [t]ransient / [a]ll incl sinks / [n]o")
                .yellow()
                .bold(),
        )
    } else if let Some((t, _)) = &app.toast {
        Line::from(Span::raw(format!(" {t}")).yellow().bold())
    } else {
        Line::from(
            Span::raw(
                " Enter send · Esc browse/compose · ↑↓/kj select · y copy · s save · o open · q quit",
            )
            .dim(),
        )
    };
    f.render_widget(Paragraph::new(line), area);
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

fn wrap_line(s: &str, width: usize) -> Vec<String> {
    let width = width.max(1);
    let mut out = Vec::new();
    for raw in s.split('\n') {
        if raw.is_empty() {
            out.push(String::new());
            continue;
        }
        let mut cur = String::new();
        let mut cur_w = 0usize;
        let push_char = |ch: char, cur: &mut String, cur_w: &mut usize, out: &mut Vec<String>| {
            if *cur_w >= width {
                out.push(std::mem::take(cur));
                *cur_w = 0;
            }
            cur.push(ch);
            *cur_w += 1;
        };
        for word in raw.split(' ') {
            let ww = word.chars().count();
            if cur_w == 0 {
                if ww <= width {
                    cur = word.to_string();
                    cur_w = ww;
                } else {
                    for ch in word.chars() {
                        push_char(ch, &mut cur, &mut cur_w, &mut out);
                    }
                }
            } else if cur_w + 1 + ww <= width {
                cur.push(' ');
                cur.push_str(word);
                cur_w += 1 + ww;
            } else {
                out.push(std::mem::take(&mut cur));
                cur_w = 0;
                if ww <= width {
                    cur = word.to_string();
                    cur_w = ww;
                } else {
                    for ch in word.chars() {
                        push_char(ch, &mut cur, &mut cur_w, &mut out);
                    }
                }
            }
        }
        out.push(cur);
    }
    if out.is_empty() {
        out.push(String::new());
    }
    out
}

fn human_size(n: u64) -> String {
    if n < 1024 {
        format!("{n} B")
    } else if n < 1024 * 1024 {
        format!("{:.1} KB", n as f64 / 1024.0)
    } else {
        format!("{:.1} MB", n as f64 / (1024.0 * 1024.0))
    }
}

fn reltime(ts_ms: u64, now_ms: u64) -> String {
    if ts_ms == 0 {
        return String::new();
    }
    let d = now_ms.saturating_sub(ts_ms) / 1000;
    if d < 2 {
        "now".to_string()
    } else if d < 60 {
        format!("{d}s")
    } else if d < 3600 {
        format!("{}m", d / 60)
    } else if d < 86400 {
        format!("{}h", d / 3600)
    } else {
        format!("{}d", d / 86400)
    }
}

fn describe(env: &Envelope) -> String {
    let who = if env.device_name.is_empty() {
        short(&env.sender)
    } else {
        env.device_name.clone()
    };
    match env.typ {
        MsgType::Text => {
            let preview: String = env.text.clone().unwrap_or_default().chars().take(40).collect();
            format!("text from {who}: {preview}")
        }
        MsgType::Image => {
            let (w, h) = env.blob.as_ref().map(|b| (b.w, b.h)).unwrap_or((0, 0));
            format!("image from {who} ({w}×{h})")
        }
        MsgType::File => {
            let size = env.blob.as_ref().map(|b| b.size).unwrap_or(0);
            let name = env.filename.clone().unwrap_or_else(|| "file".into());
            format!("file from {who} ({name} · {})", human_size(size))
        }
    }
}

fn short(id: &str) -> String {
    id.chars().take(12).collect()
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

fn save_dest(hash: &str) -> PathBuf {
    let short = &hash[..hash.len().min(12)];
    let name = format!("clip-{short}.png");
    if let Some(dirs) = directories::UserDirs::new() {
        if let Some(dl) = dirs.download_dir() {
            return dl.join(name);
        }
    }
    std::env::current_dir()
        .unwrap_or_else(|_| std::env::temp_dir())
        .join(name)
}

/// Open a file with the OS's default handler. Returns the path on success.
fn open_external(path: &str) -> Result<String, String> {
    #[cfg(target_os = "macos")]
    let mut cmd = {
        let mut c = std::process::Command::new("open");
        c.arg(path);
        c
    };
    #[cfg(target_os = "windows")]
    let mut cmd = {
        let mut c = std::process::Command::new("cmd");
        c.args(["/C", "start", "", path]);
        c
    };
    #[cfg(all(not(target_os = "macos"), not(target_os = "windows")))]
    let mut cmd = {
        let mut c = std::process::Command::new("xdg-open");
        c.arg(path);
        c
    };
    cmd.stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .map(|_| path.to_string())
        .map_err(|e| e.to_string())
}

// ---------------------------------------------------------------------------
// Tests — drive the pure model without a terminal or a daemon.
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{Blob, MsgType, PROTO_V};

    fn test_app() -> App {
        App::new(Header {
            room: "default".into(),
            my_id: "MYENDPOINTID0000".into(),
            my_short: "MYENDPOINTID".into(),
            peer_count: 1,
            auto_copy: "notify".into(),
            clipboard_available: true,
        })
    }

    fn text_env(sender: &str, text: &str) -> Envelope {
        Envelope {
            v: PROTO_V,
            msg_id: format!("id-{text}"),
            typ: MsgType::Text,
            mime: "text/plain; charset=utf-8".into(),
            sender: sender.into(),
            device_name: "peer-box".into(),
            ts: now_ms(),
            filename: None,
            text: Some(text.into()),
            blob: None,
        }
    }

    #[test]
    fn incoming_item_appends_a_bubble() {
        let mut app = test_app();
        assert_eq!(app.messages.len(), 0);
        let redraw = app.apply_event(Event::Item {
            envelope: text_env("PEERID", "hello from peer"),
            local_path: None,
        });
        assert!(redraw);
        assert_eq!(app.messages.len(), 1);
        let m = &app.messages[0];
        assert!(!m.mine, "peer message must not be marked mine");
        assert!(m.accept_pending, "notify mode must flag it 'press y'");
        match &m.body {
            Body::Text(t) => assert_eq!(t, "hello from peer"),
            _ => panic!("expected a text bubble"),
        }
    }

    #[test]
    fn submitting_composer_text_produces_a_send_action_and_own_bubble() {
        let mut app = test_app();
        app.composer.insert_str("hi there");
        let action = app.submit_text();
        // The send action carries the composed text…
        let o = action.expect("non-empty composer must yield a send action");
        assert_eq!(o.text, "hi there");
        // …an optimistic own bubble is appended…
        assert_eq!(app.messages.len(), 1);
        let m = &app.messages[0];
        assert!(m.mine);
        assert!(m.pending);
        assert_eq!(m.client_seq, Some(o.seq));
        match &m.body {
            Body::Text(t) => assert_eq!(t, "hi there"),
            _ => panic!("expected a text bubble"),
        }
        // …and the composer is cleared.
        assert!(app.composer.is_empty());
    }

    #[test]
    fn empty_composer_produces_no_send_action() {
        let mut app = test_app();
        app.composer.insert_str("   ");
        assert!(app.submit_text().is_none());
        assert_eq!(app.messages.len(), 0);
    }

    #[test]
    fn send_result_reconciles_the_optimistic_bubble() {
        let mut app = test_app();
        app.composer.insert_str("ping");
        let o = app.submit_text().unwrap();
        app.on_sent_result(o.seq, Ok(("real-ulid".into(), 2)));
        let m = &app.messages[0];
        assert!(!m.pending);
        assert_eq!(m.msg_id, "real-ulid");
        assert!(m.note.is_none(), "delivered to peers => no note");
    }

    #[test]
    fn send_result_with_no_peers_notes_undelivered() {
        let mut app = test_app();
        app.composer.insert_str("ping");
        let o = app.submit_text().unwrap();
        app.on_sent_result(o.seq, Ok(("real-ulid".into(), 0)));
        assert_eq!(
            app.messages[0].note.as_deref(),
            Some("not delivered (no peers)")
        );
    }

    #[test]
    fn image_event_appends_bubble_with_metadata() {
        let mut app = test_app();
        let env = Envelope {
            v: PROTO_V,
            msg_id: "img1".into(),
            typ: MsgType::Image,
            mime: "image/png".into(),
            sender: "PEERID".into(),
            device_name: "peer-box".into(),
            ts: now_ms(),
            filename: Some("shot.png".into()),
            text: None,
            blob: Some(Blob {
                hash: "abc123".into(),
                size: 4096,
                w: 640,
                h: 480,
            }),
        };
        app.apply_event(Event::Item {
            envelope: env,
            local_path: Some("/tmp/clip-abc123.png".into()),
        });
        match &app.messages[0].body {
            Body::Image { blob, local_path } => {
                assert_eq!((blob.w, blob.h), (640, 480));
                assert_eq!(local_path.as_deref(), Some("/tmp/clip-abc123.png"));
            }
            _ => panic!("expected an image bubble"),
        }
    }

    #[test]
    fn navigation_selects_and_follows() {
        let mut app = test_app();
        for n in 0..3 {
            app.apply_event(Event::Item {
                envelope: text_env("PEERID", &format!("m{n}")),
                local_path: None,
            });
        }
        assert_eq!(app.selected, None, "starts following the newest");
        app.select_up();
        assert_eq!(app.selected, Some(2));
        app.select_up();
        assert_eq!(app.selected, Some(1));
        // target() falls back to the highlight
        assert!(matches!(&app.target().unwrap().body, Body::Text(t) if t == "m1"));
        app.select_down();
        app.select_down();
        assert_eq!(app.selected, None, "returning to the bottom re-enters follow");
        assert!(app.follow);
    }

    #[test]
    fn wrap_line_hard_breaks_long_words() {
        let parts = wrap_line("abcdefghij", 4);
        assert_eq!(parts, vec!["abcd", "efgh", "ij"]);
        let words = wrap_line("aa bb cc", 5);
        assert_eq!(words, vec!["aa bb", "cc"]);
    }

    fn key(code: KeyCode, mods: KeyModifiers) -> CtEvent {
        CtEvent::Key(crossterm::event::KeyEvent::new(code, mods))
    }

    #[test]
    fn ctrl_c_quits_from_any_focus() {
        let mut app = test_app();
        assert_eq!(
            app.handle_key(&key(KeyCode::Char('c'), KeyModifiers::CONTROL)),
            KeyOutcome::Quit
        );
    }

    #[test]
    fn enter_in_compose_produces_send() {
        let mut app = test_app();
        app.composer.insert_str("send me");
        match app.handle_key(&key(KeyCode::Enter, KeyModifiers::NONE)) {
            KeyOutcome::Send(o) => assert_eq!(o.text, "send me"),
            other => panic!("expected Send, got {other:?}"),
        }
    }

    #[test]
    fn esc_then_q_browses_then_quits() {
        let mut app = test_app();
        app.apply_event(Event::Item {
            envelope: text_env("PEERID", "hi"),
            local_path: None,
        });
        // Esc leaves the composer for browse mode…
        assert_eq!(
            app.handle_key(&key(KeyCode::Esc, KeyModifiers::NONE)),
            KeyOutcome::Redraw
        );
        // …and now `q` quits (in compose mode it would be a literal character). Something was
        // received, so it first raises the clear-on-quit prompt (SPEC §8); `n` = quit, keep all.
        assert_eq!(
            app.handle_key(&key(KeyCode::Char('q'), KeyModifiers::NONE)),
            KeyOutcome::Redraw
        );
        assert!(app.quit_prompt);
        assert_eq!(
            app.handle_key(&key(KeyCode::Char('n'), KeyModifiers::NONE)),
            KeyOutcome::Quit
        );
    }

    /// SPEC §8: quitting the TUI after receiving something prompts [t]/[a]/[n]; `t` clears the
    /// transient store, `a` also reverts session sink writes. With nothing received, `q` just quits.
    #[test]
    fn quit_prompt_offers_transient_and_all_clears() {
        let mut app = test_app();
        app.focus = Focus::Browse;
        // Nothing received yet → no prompt.
        assert_eq!(
            app.handle_key(&key(KeyCode::Char('q'), KeyModifiers::NONE)),
            KeyOutcome::Quit
        );
        assert!(!app.quit_prompt);

        app.apply_event(Event::Item {
            envelope: text_env("PEERID", "hi"),
            local_path: None,
        });
        assert!(app.received_any);
        assert_eq!(
            app.handle_key(&key(KeyCode::Char('q'), KeyModifiers::NONE)),
            KeyOutcome::Redraw
        );
        assert!(app.quit_prompt, "prompt must be up once something was received");
        assert_eq!(
            app.handle_key(&key(KeyCode::Char('t'), KeyModifiers::NONE)),
            KeyOutcome::ClearAndQuit { all: false }
        );
        assert_eq!(
            app.handle_key(&key(KeyCode::Char('a'), KeyModifiers::NONE)),
            KeyOutcome::ClearAndQuit { all: true }
        );
    }

    #[test]
    fn q_in_compose_is_typed_not_quit() {
        let mut app = test_app();
        let outcome = app.handle_key(&key(KeyCode::Char('q'), KeyModifiers::NONE));
        assert_eq!(outcome, KeyOutcome::Redraw);
        assert!(!app.composer.is_empty(), "'q' should be typed into the composer");
    }

    /// Exercise the exact image pipeline the workers run (decode → protocol → off-thread
    /// resize/encode) so a regression in those ratatui-image calls is caught headlessly. The
    /// half-block picker needs no terminal, so this runs in CI without a graphics protocol.
    #[test]
    fn image_pipeline_decodes_and_encodes_without_panic() {
        let img = image::DynamicImage::ImageRgba8(image::RgbaImage::from_pixel(
            32,
            24,
            image::Rgba([120, 40, 200, 255]),
        ));
        let picker = Picker::halfblocks();
        let mut proto = picker.new_resize_protocol(img);
        let area = Rect::new(0, 0, IMG_MAX_COLS, IMG_ROWS as u16);
        // Fresh protocols report the fitted rect they must be encoded to (what the worker uses)…
        let need = proto
            .needs_resize(&Resize::Fit(None), area)
            .expect("a fresh protocol needs a first encode");
        // …encoding for that rect must succeed…
        ResizeEncodeRender::resize_encode(&mut proto, &Resize::Fit(None), need);
        assert!(
            matches!(proto.last_encoding_result(), Some(Ok(()))),
            "resize/encode should succeed for a halfblocks thumbnail"
        );
        // …and afterwards the same render area no longer needs work (cheap UI-thread render).
        assert!(proto.needs_resize(&Resize::Fit(None), area).is_none());
    }
}
