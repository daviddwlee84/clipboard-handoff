//! `clip` — mesh-rs Phase 0 CLI. Global flags + subcommands per docs/SPEC.md §2.

mod client;
mod config;
mod daemon;
mod proto;

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
    /// Run the resident daemon (normally auto-spawned).
    Daemon {
        #[arg(long)]
        foreground: bool,
    },
    /// Read stdin to EOF, sniff type, and broadcast to peers.
    Send {
        #[arg(long)]
        text: bool,
        #[arg(long)]
        image: bool,
        #[arg(long)]
        auto: bool,
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
}

#[derive(Subcommand)]
enum ConfigAction {
    Set { key: String, value: String },
    Get { key: String },
}

#[tokio::main]
async fn main() {
    let cli = Cli::parse();
    let is_daemon = matches!(cli.cmd, Cmd::Daemon { .. });
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
    match cli.cmd {
        Cmd::Daemon { .. } => {
            daemon::run(paths).await?;
            Ok(0)
        }
        Cmd::Send { text, image, auto } => client::cmd_send(&paths, text, image, auto).await,
        Cmd::Recv {
            follow,
            latest_image,
            emit_path,
            out,
        } => client::cmd_recv(&paths, follow, latest_image, emit_path, out).await,
        Cmd::Paste => client::cmd_paste(&paths).await,
        Cmd::Pair { new, ticket } => client::cmd_pair(&paths, new, ticket, json).await,
        Cmd::Peers => client::cmd_peers(&paths, json).await,
        Cmd::Status => client::cmd_status(&paths, json).await,
        Cmd::Config { action } => match action {
            ConfigAction::Set { key, value } => client::cmd_config_set(&paths, key, value).await,
            ConfigAction::Get { key } => client::cmd_config_get(&paths, key, json).await,
        },
    }
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
