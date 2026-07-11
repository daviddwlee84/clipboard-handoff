//! Thin, ephemeral IPC clients for the `send`/`recv`/`paste`/`pair`/`status`/`config`
//! subcommands. They hold no network state and auto-spawn the daemon if absent.

use std::{
    io::Read,
    path::PathBuf,
    process::Stdio,
    time::Duration,
};

use anyhow::{Result, bail};
use serde_bytes::ByteBuf;

use interprocess::local_socket::{
    GenericFilePath, ToFsName, tokio::Stream as IpcStream, traits::tokio::Stream as _,
};

use crate::config::Paths;
use crate::proto::*;

async fn connect(paths: &Paths) -> Result<IpcStream> {
    let name = paths.socket.clone().to_fs_name::<GenericFilePath>()?;
    Ok(IpcStream::connect(name).await?)
}

/// Connect to the daemon, spawning it (detached) if it isn't running yet.
async fn connect_or_spawn(paths: &Paths) -> Result<IpcStream> {
    if let Ok(s) = connect(paths).await {
        return Ok(s);
    }
    spawn_daemon(paths)?;
    for _ in 0..50 {
        tokio::time::sleep(Duration::from_millis(100)).await;
        if let Ok(s) = connect(paths).await {
            return Ok(s);
        }
    }
    bail!("could not reach daemon after auto-spawn");
}

fn spawn_daemon(paths: &Paths) -> Result<()> {
    let exe = std::env::current_exe()?;
    std::process::Command::new(exe)
        .arg("--config-dir")
        .arg(&paths.config_dir)
        .arg("--socket")
        .arg(&paths.socket)
        .arg("--room")
        .arg(&paths.room)
        .arg("daemon")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()?;
    Ok(())
}

/// One-shot request/response.
async fn request(paths: &Paths, req: Req) -> Result<Resp> {
    let mut s = connect_or_spawn(paths).await?;
    write_frame(&mut s, &req).await?;
    read_frame(&mut s).await
}

// ---------------------------------------------------------------------------
// Subcommands. Each returns a process exit code (SPEC §2 exit codes).
// ---------------------------------------------------------------------------

pub async fn cmd_send(paths: &Paths, text: bool, image: bool, _auto: bool) -> Result<i32> {
    let kind = if text {
        SniffKind::Text
    } else if image {
        SniffKind::Image
    } else {
        SniffKind::Auto
    };
    let mut buf = Vec::new();
    std::io::stdin().lock().read_to_end(&mut buf)?;

    let resp = match request(paths, Req::Send { kind, bytes: ByteBuf::from(buf) }).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip send: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(OkData::Text(reached)) => {
            if reached == "0" {
                // no peers: warning, not a failure — item is still accepted/buffered.
                eprintln!("clip send: warning: no peers connected");
            }
            Ok(0)
        }
        Resp::Ok(_) => Ok(0),
        Resp::Err { code, message } => {
            eprintln!("clip send: {message}");
            Ok(code)
        }
    }
}

pub async fn cmd_recv(
    paths: &Paths,
    follow: bool,
    latest_image: bool,
    _emit_path: bool,
    out: Option<PathBuf>,
) -> Result<i32> {
    let kind = if latest_image {
        RecvKind::Image
    } else {
        RecvKind::Any
    };

    if follow {
        let mut s = match connect_or_spawn(paths).await {
            Ok(s) => s,
            Err(e) => {
                eprintln!("clip recv: cannot reach daemon: {e:#}");
                return Ok(3);
            }
        };
        write_frame(&mut s, &Req::Subscribe).await?;
        loop {
            let ev: Event = match read_frame(&mut s).await {
                Ok(e) => e,
                Err(_) => break, // daemon closed
            };
            if let Event::Item { envelope, local_path } = ev {
                if kind_matches(kind, &envelope) {
                    emit_item(&envelope, local_path.as_deref(), out.as_deref())?;
                }
            }
        }
        return Ok(0);
    }

    let resp = match request(paths, Req::RecvLatest { kind }).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip recv: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(OkData::Item { envelope, local_path }) => {
            emit_item(&envelope, local_path.as_deref(), out.as_deref())?;
            Ok(0)
        }
        Resp::Err { code, message } => {
            eprintln!("clip recv: {message}");
            Ok(code)
        }
        _ => Ok(1),
    }
}

/// Text -> stdout; image -> write to `out` (or use the daemon temp file) and print the path.
fn emit_item(env: &Envelope, local_path: Option<&str>, out: Option<&std::path::Path>) -> Result<()> {
    match env.typ {
        MsgType::Text => {
            println!("{}", env.text.clone().unwrap_or_default());
        }
        MsgType::Image => {
            let src = local_path.ok_or_else(|| anyhow::anyhow!("image bytes not yet available"))?;
            let final_path = if let Some(o) = out {
                std::fs::copy(src, o)?;
                o.to_path_buf()
            } else {
                PathBuf::from(src)
            };
            println!("{}", final_path.display());
        }
    }
    Ok(())
}

pub async fn cmd_paste(paths: &Paths) -> Result<i32> {
    let resp = match request(paths, Req::Paste).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip paste: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(OkData::Text(desc)) => {
            eprintln!("copied to clipboard: {desc}");
            Ok(0)
        }
        Resp::Ok(_) => Ok(0),
        Resp::Err { code, message } => {
            eprintln!("clip paste: {message}");
            Ok(code)
        }
    }
}

pub async fn cmd_pair(
    paths: &Paths,
    new: bool,
    ticket: Option<String>,
    json: bool,
) -> Result<i32> {
    if new {
        let resp = match request(paths, Req::PairNew).await {
            Ok(r) => r,
            Err(e) => {
                eprintln!("clip pair: cannot reach daemon: {e:#}");
                return Ok(3);
            }
        };
        return match resp {
            Resp::Ok(OkData::Ticket(t)) => {
                if json {
                    println!("{}", serde_json::json!({ "ticket": t }));
                } else {
                    // Phase 1 will also render an in-terminal QR here.
                    println!("{t}");
                }
                Ok(0)
            }
            Resp::Err { code, message } => {
                eprintln!("clip pair: {message}");
                Ok(code)
            }
            _ => Ok(1),
        };
    }

    let Some(ticket) = ticket else {
        eprintln!("clip pair: provide --new to create a ticket, or a <ticket> to join");
        return Ok(2);
    };
    let resp = match request(paths, Req::Pair { ticket }).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip pair: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(_) => {
            if !json {
                println!("paired");
            }
            Ok(0)
        }
        Resp::Err { code, message } => {
            eprintln!("clip pair: {message}");
            Ok(code)
        }
    }
}

pub async fn cmd_status(paths: &Paths, json: bool) -> Result<i32> {
    let resp = match request(paths, Req::Status).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip status: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(OkData::Status(s)) => {
            if json {
                println!("{}", serde_json::to_string(&s)?);
            } else {
                println!("endpoint:   {}", s.endpoint_id);
                println!("device:     {}", s.device_name);
                println!("room:       {}", s.room);
                println!("transport:  {}", s.transport);
                println!("auto_copy:  {}", s.auto_copy);
                println!("clipboard:  {}", s.clipboard);
                println!("peers:      {}", s.peer_count);
                println!("buffered:   {}", s.buffer_len);
            }
            Ok(0)
        }
        Resp::Err { code, message } => {
            eprintln!("clip status: {message}");
            Ok(code)
        }
        _ => Ok(1),
    }
}

pub async fn cmd_peers(paths: &Paths, json: bool) -> Result<i32> {
    let resp = match request(paths, Req::Peers).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip peers: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(OkData::Peers(peers)) => {
            if json {
                println!("{}", serde_json::to_string(&peers)?);
            } else if peers.is_empty() {
                eprintln!("(no peers connected)");
            } else {
                for p in peers {
                    println!("{}  {}  {}", p.name, p.id, if p.direct { "direct" } else { "relayed" });
                }
            }
            Ok(0)
        }
        Resp::Err { code, message } => {
            eprintln!("clip peers: {message}");
            Ok(code)
        }
        _ => Ok(1),
    }
}

pub async fn cmd_config_set(paths: &Paths, key: String, value: String) -> Result<i32> {
    let resp = match request(paths, Req::ConfigSet { key, value }).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip config: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(_) => Ok(0),
        Resp::Err { code, message } => {
            eprintln!("clip config: {message}");
            Ok(code)
        }
    }
}

pub async fn cmd_config_get(paths: &Paths, key: String, json: bool) -> Result<i32> {
    let resp = match request(paths, Req::ConfigGet { key: key.clone() }).await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("clip config: cannot reach daemon: {e:#}");
            return Ok(3);
        }
    };
    match resp {
        Resp::Ok(OkData::Config(v)) => {
            match v {
                Some(val) => {
                    if json {
                        println!("{}", serde_json::json!({ key: val }));
                    } else {
                        println!("{val}");
                    }
                    Ok(0)
                }
                None => {
                    eprintln!("clip config: unknown key {key}");
                    Ok(1)
                }
            }
        }
        Resp::Err { code, message } => {
            eprintln!("clip config: {message}");
            Ok(code)
        }
        _ => Ok(1),
    }
}

fn kind_matches(kind: RecvKind, env: &Envelope) -> bool {
    match kind {
        RecvKind::Any => true,
        RecvKind::Text => env.typ == MsgType::Text,
        RecvKind::Image => env.typ == MsgType::Image,
    }
}
