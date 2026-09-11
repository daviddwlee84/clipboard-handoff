# cross-platform-copy

Hand off **text, images, and arbitrary files** between your own devices from the terminal — copy or pipe
something on one machine and it's instantly usable on another. Think AirDrop/Handoff, but cross-platform,
scriptable, and with a messenger TUI.

This repo is also a **technical bake-off**: several independent implementations of one shared contract, so we
could compare which architecture is actually nicest to use. Three are now usable end-to-end.

## The implementations

| Dir | Lang | Topology | The bet |
|---|---|---|---|
| [`mesh-rs/`](mesh-rs/) (`clip`) | Rust | **True P2P mesh**, no server | **iroh** — NAT traversal, relays, blobs for free; mDNS LAN auto-discovery; E2E by default |
| [`room-go/`](room-go/) (`room`) | Go | **Central room server** | **charmbracelet/wish** SSH room; SSH-key = identity; natural history; leanest binary |
| [`experiments/lan-go/`](experiments/lan-go/) (`lan`) | Go | P2P mesh, no server | quic-go + mDNS, hand-rolled — the "is iroh's weight worth it?" probe |
| [`experiments/libp2p-mesh/`](experiments/libp2p-mesh/) | Go | gossipsub mesh | **Parked** — 37 MB / ~140 deps for no gain here |

All obey one contract: [`docs/SPEC.md`](docs/SPEC.md) (CLI, sinks, sessions), [`docs/PROTOCOL.md`](docs/PROTOCOL.md)
(wire envelope), [`docs/BAKEOFF.md`](docs/BAKEOFF.md) (scorecard + measurements).

## Prior art

This started from Alexander Zeitler's work on pasting clipboard images into Claude Code over SSH —
[the article](https://alexanderzeitler.com/articles/paste-clipboard-images-into-claude-code-over-ssh/),
[`claude-ssh-image-skill`](https://github.com/AlexZeitler/claude-ssh-image-skill), and
[`sshimg.nvim`](https://github.com/AlexZeitler/sshimg.nvim). They solve the local→remote image case with
a local daemon + an SSH reverse tunnel, and they're the reason this project uses a resident agent at all.
[`docs/alternatives.md`](docs/alternatives.md) credits them properly, explains their data path, how we
differ (any→any, N devices, text/images/files, landing on the receiving clipboard), and surveys the wider
ecosystem.

## What works (all three, verified)

- **CLI hand-off** — pipe or pass a path; text / images / **arbitrary files**.
- **Messenger TUI** — live chat, select a message, `y` copy · `s` save · `o` open. `mesh-rs` renders **inline
  image thumbnails** (Kitty/iTerm2/Sixel, half-block fallback); the Go TUIs show metadata placeholders.
- **Multi-device (N-peer)** — 3+ devices in a room at once; broadcast reaches all.
- **Cross-machine over a real LAN**, including a **headless** Linux box (no display): the daemon degrades the
  absent clipboard gracefully and delivery still works via `recv --emit-path` / sinks.
- **Additive sinks** — received items can go to the clipboard **and/or** a folder (`save_dir`) **and/or** get
  appended to a file (`text_file`).
- **Session clearing** — `clear`, `daemon stop`, and TUI-quit can purge what a session received. Precisely
  scoped: it never touches data that existed before the session.
- **Notify-first by default** — received content is announced, not silently slammed into your clipboard.

## Quick start

Same CLI everywhere; only the binary name differs (`clip` / `room` / `lan`).

```sh
echo "hello" | clip send                 # pipe text
clip send ./screenshot.png               # an image (auto-detected)
clip send ./report.pdf                   # any file
clip recv --follow                       # stream incoming text to stdout
clip paste                               # put the latest received item on the clipboard
clip tui                                 # messenger chat

# route received items somewhere durable, in addition to the clipboard
clip config set save_dir  ~/Downloads/handoff    # images + files land here
clip config set text_file ~/handoff.md           # received text appends here
clip config set auto_copy notify                 # notify|on|off  (default: notify)

clip clear --all                         # clear what this session received (prompts)
```

Connecting devices: `mesh-rs`/`lan-go` **auto-discover** peers in the same `--room` on a LAN (mDNS; `clip pair`
gives a ticket as a fallback). `room-go` clients `join room@host:port` on a server you run.

**One-command remote (VSCode-Remote style):** point a tool at an SSH host (key auth via `~/.ssh/config`) and it
installs itself there, starts the remote side, and connects — no manual steps:

```sh
room remote  my-server        # install room on my-server, start a room server, SSH tunnel, join
clip remote  my-server        # install clip, start a remote daemon, pair over iroh (direct QUIC)
lan  remote  my-server        # install lan, start a remote daemon (mDNS on a shared LAN)
<tool> remote my-server down  # disconnect / tear down
# just remote clip my-server   ·   just install-remote my-server room lan
```
`room remote` is native Go; `clip`/`lan` share one engine (`scripts/remote.sh`), and `scripts/install.sh` puts it
next to the binaries so it works from `~/.local/bin`. (Binary bootstrap needs the repo or a matching-arch host;
the QUIC mesh tools connect on a shared LAN — cross-internet is the relay path, still LAN-first for now.)

## The one cross-cutting truth

> **Images/files can only land on a device's OS clipboard via a native, resident agent on that device.**
> Terminal escapes (OSC 52) carry **text only** (~74 KB, tmux-stripped). So every implementation runs a
> per-device daemon that owns the clipboard; the only real difference between them is the **transport**.

## Testing

```sh
scripts/roundtrip.sh <impl>        # localhost: text + PNG round-trip, image compared by hash
scripts/xmachine.sh  <impl>        # deploy to a remote LAN host and hand off for real (default: local_ubuntu)
<impl>/scripts/e2e_sinks.sh        # files + folder/append sinks + clear semantics
mesh-rs/scripts/autodiscover.sh    # two daemons connect with no ticket
```

## Status / not done

LAN-first by design. **Not yet:** the internet/relay path (a config flag away for `mesh-rs`), Windows, Linux
**desktop** clipboard validation (the test box is headless), and packaging/signing. See
[`docs/BAKEOFF.md`](docs/BAKEOFF.md) for the measured comparison and [`.claude/plans/`](.claude/plans/) for the plan.

<!-- project-knowledge-harness:readme-roadmap -->

## Roadmap & lessons learned

Future work is indexed in [TODO.md](TODO.md); research lives in [backlog/](backlog/)
and resolved debugging traps in [pitfalls/](pitfalls/).
See [remote image paste notes](backlog/remote-image-paste.md) for the Herdr/Moshi
comparison and an opt-in SSH/Mosh integration proposal (research only, zh-TW).

<!-- project-knowledge-harness:readme-roadmap (end) -->
