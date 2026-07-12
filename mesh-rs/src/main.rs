//! `clip` — mesh-rs Phase 0 CLI. Global flags + subcommands per docs/SPEC.md §2.

mod client;
mod clipboard;
mod config;
mod daemon;
mod proto;
mod tui;

use std::path::PathBuf;

use anyhow::Result;
use clap::{Parser, Subcommand};

#[derive(Parser)]
#[command(
    name = "clip",
    about = "mesh-rs: P2P clipboard hand-off over iroh (Phase 0)",
    version
)]
struct Cli {
    /// Config/identity directory (default: OS config dir for mesh-rs).
    #[arg(long, global = true)]
    config_dir: Option<PathBuf>,
    /// Local IPC socket path (default: <config-dir>/daemon.sock).
    #[arg(long, global = true)]
    socket: Option<PathBuf>,
    /// Room / topic (shared secret). Only same-room peers connect.
    #[arg(long, global = true, default_value = "default")]
    room: String,
    /// Machine-readable output where applicable.
    #[arg(long, global = true)]
    json: bool,
    #[arg(short, long, global = true)]
    quiet: bool,
    #[arg(short, long, global = true)]
    verbose: bool,

    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Run the resident daemon (normally auto-spawned); `daemon stop` shuts it down.
    Daemon {
        #[arg(long)]
        foreground: bool,
        #[command(subcommand)]
        action: Option<DaemonAction>,
    },
    /// Send PATH (or stdin to EOF): sniff type and broadcast to peers.
    Send {
        /// File to send. Omit to read stdin.
        path: Option<PathBuf>,
        #[arg(long)]
        text: bool,
        #[arg(long)]
        image: bool,
        /// Force an arbitrary-file item (never clipboard-pasteable; lands in `save_dir`).
        #[arg(long)]
        file: bool,
        #[arg(long)]
        auto: bool,
        /// Advisory filename carried with an image/file item (defaults to PATH's base name).
        #[arg(long)]
        name: Option<String>,
    },
    /// Clear this session's received data (SPEC §8).
    Clear {
        /// Also revert this session's sink writes (text_file lines + save_dir files).
        #[arg(long)]
        all: bool,
        /// Skip the confirmation prompt.
        #[arg(long)]
        yes: bool,
    },
    /// Receive items. Text -> stdout, image -> temp file path.
    Recv {
        #[arg(long)]
        follow: bool,
        #[arg(long = "latest-image")]
        latest_image: bool,
        #[arg(long = "emit-path")]
        emit_path: bool,
        #[arg(long)]
        out: Option<PathBuf>,
    },
    /// Write the latest received item to the OS clipboard.
    Paste,
    /// Launch the messenger-style chat TUI (SPEC §4).
    Tui,
    /// Create a pairing ticket (--new) or join with a <ticket>.
    Pair {
        #[arg(long)]
        new: bool,
        ticket: Option<String>,
    },
    /// List connected peers.
    Peers,
    /// Show daemon state.
    Status,
    /// Get/set persisted config (SPEC §5).
    Config {
        #[command(subcommand)]
        action: ConfigAction,
    },
    /// Bootstrap + connect to a remote over SSH (via the shared scripts/remote.sh engine).
    Remote {
        /// SSH host (an ~/.ssh/config alias or user@host).
        host: String,
        /// Forwarded to the engine: [up|down] [--room R] …
        #[arg(trailing_var_arg = true, allow_hyphen_values = true)]
        args: Vec<String>,
    },
}

#[derive(Subcommand)]
enum ConfigAction {
    Set { key: String, value: String },
    Get { key: String },
}

#[derive(Subcommand)]
enum DaemonAction {
    /// Stop the resident daemon, applying `clear_on_exit` (SPEC §8).
    Stop,
}

#[tokio::main]
async fn main() {
    let cli = Cli::parse();
    let is_daemon = matches!(cli.cmd, Cmd::Daemon { action: None, .. });
    init_tracing(cli.verbose, cli.quiet, is_daemon);

    let paths =
        match config::Paths::resolve(cli.config_dir.clone(), cli.socket.clone(), cli.room.clone()) {
            Ok(p) => p,
            Err(e) => {
                eprintln!("clip: {e:#}");
                std::process::exit(2);
            }
        };

    let code = match dispatch(cli, paths).await {
        Ok(code) => code,
        Err(e) => {
            eprintln!("clip: {e:#}");
            1
        }
    };
    std::process::exit(code);
}

async fn dispatch(cli: Cli, paths: config::Paths) -> Result<i32> {
    let json = cli.json;
    let room = cli.room.clone();
    match cli.cmd {
        Cmd::Daemon { action: Some(DaemonAction::Stop), .. } => {
            client::cmd_daemon_stop(&paths).await
        }
        Cmd::Daemon { .. } => {
            daemon::run(paths).await?;
            Ok(0)
        }
        Cmd::Send {
            path,
            text,
            image,
            file,
            auto,
            name,
        } => client::cmd_send(&paths, path, text, image, file, auto, name).await,
        Cmd::Clear { all, yes } => client::cmd_clear(&paths, all, yes).await,
        Cmd::Recv {
            follow,
            latest_image,
            emit_path,
            out,
        } => client::cmd_recv(&paths, follow, latest_image, emit_path, out).await,
        Cmd::Paste => client::cmd_paste(&paths).await,
        Cmd::Tui => tui::run(&paths).await,
        Cmd::Pair { new, ticket } => client::cmd_pair(&paths, new, ticket, json).await,
        Cmd::Peers => client::cmd_peers(&paths, json).await,
        Cmd::Status => client::cmd_status(&paths, json).await,
        Cmd::Config { action } => match action {
            ConfigAction::Set { key, value } => client::cmd_config_set(&paths, key, value).await,
            ConfigAction::Get { key } => client::cmd_config_get(&paths, key, json).await,
        },
        Cmd::Remote { host, args } => cmd_remote(&host, &args, &room),
    }
}

/// `clip remote <host> …` — locate the shared engine (scripts/remote.sh) and exec it
/// with this tool's name. The engine handles bootstrap + start-remote-daemon + iroh
/// ticket pairing. Kept out of the binary so all tools share one implementation.
fn cmd_remote(host: &str, args: &[String], room: &str) -> Result<i32> {
    use std::os::unix::process::CommandExt;
    let helper = find_remote_helper().ok_or_else(|| {
        anyhow::anyhow!(
            "could not locate remote.sh — set CPC_REMOTE_HELPER, run `scripts/install.sh` \
             (installs it to ~/.local/libexec/cpc/), or run from the repo"
        )
    })?;
    // exec replaces this process; it only returns on failure. Forward --room:
    // clap's trailing_var_arg may already carry one (flag after `remote`); only
    // append the global cli.room otherwise, so the engine never sees two.
    let mut cmd = std::process::Command::new("bash");
    cmd.arg(&helper).arg("clip").arg(host).args(args);
    if !args.iter().any(|a| a == "--room") {
        cmd.arg("--room").arg(room);
    }
    let err = cmd.exec();
    Err(err.into())
}

fn find_remote_helper() -> Option<std::path::PathBuf> {
    use std::path::PathBuf;
    if let Ok(p) = std::env::var("CPC_REMOTE_HELPER") {
        if !p.is_empty() {
            return Some(PathBuf::from(p));
        }
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            for c in [dir.join("remote.sh"), dir.join("../libexec/cpc/remote.sh")] {
                if c.is_file() {
                    return Some(c);
                }
            }
        }
    }
    if let Ok(home) = std::env::var("HOME") {
        let c = PathBuf::from(home).join(".local/libexec/cpc/remote.sh");
        if c.is_file() {
            return Some(c);
        }
    }
    if let Ok(mut d) = std::env::current_dir() {
        loop {
            let c = d.join("scripts/remote.sh");
            if c.is_file() {
                return Some(c);
            }
            if !d.pop() {
                break;
            }
        }
    }
    None
}

/// Logs go to **stderr** so client stdout stays clean for `recv`.
fn init_tracing(verbose: bool, quiet: bool, is_daemon: bool) {
    use tracing_subscriber::EnvFilter;
    let default = if quiet {
        "error"
    } else if verbose {
        "debug"
    } else if is_daemon {
        "info"
    } else {
        "warn"
    };
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(default));
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(std::io::stderr)
        .with_target(false)
        .try_init();
}
