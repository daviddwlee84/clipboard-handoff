# Prior art & alternatives

This project didn't appear from nowhere: it started from three projects that solve one sharp version of
the problem — *"I'm SSH'd into a box and I want to paste a clipboard image into the session."* This page
credits them, explains **how they work**, and places them (and the wider ecosystem) against what we built.

## The three projects that started this

| | What it is |
|---|---|
| [Paste clipboard images into Claude Code over SSH](https://alexanderzeitler.com/articles/paste-clipboard-images-into-claude-code-over-ssh/) | Alexander Zeitler's write-up of the problem and the reverse-tunnel solution |
| [`AlexZeitler/claude-ssh-image-skill`](https://github.com/AlexZeitler/claude-ssh-image-skill) | `ccimgd` (local daemon) + `ccimg` (remote client) + a Claude Code skill |
| [`AlexZeitler/sshimg.nvim`](https://github.com/AlexZeitler/sshimg.nvim) | The same idea wired into Neovim (`imgd` local daemon) |

### How they work (the data path)

Both tools exist because of one hard constraint we hit too:

> A process on the **remote** host cannot read or write the **local** machine's clipboard. The terminal
> escape that *can* (OSC 52) is **text-only** — there is no image form of it.

So they move the bytes out-of-band, over the SSH connection you already have:

- **`claude-ssh-image-skill` (pull).** A daemon `ccimgd` runs on your **laptop**, listening on
  `127.0.0.1:9998`. It reads the local clipboard image with `pngpaste` (macOS) / `wl-paste` (Wayland) /
  `xclip` (X11) and serves it as base64 PNG over JSON. Your SSH session carries a **reverse tunnel**
  (`ssh -R 9998:localhost:9998 host`, or a `RemoteForward` in `~/.ssh/config`). On the remote, `ccimg`
  dials `127.0.0.1:9998` — which the tunnel forwards *back to the laptop* — receives the PNG, and writes
  a temp file that Claude Code can `Read`.
- **`sshimg.nvim` (push).** Same shape, opposite trigger: remote Neovim signals the local `imgd` daemon
  through the reverse tunnel, `imgd` `scp`s the clipboard PNG to the remote host, and Neovim inserts a
  markdown link to it.

**What we took from them:** the core architectural lesson — *a native, resident agent on the machine that
owns the clipboard is unavoidable; terminal escapes can't carry images.* That conclusion shaped this
project's daemon-plus-thin-clients design (see [`implementation.md`](implementation.md) §1.1).

**Where we differ:** their direction is **local → remote** (get my laptop's clipboard into the remote
session). Ours is **any device → any device**, in both directions, and the receiving side can write its
own OS clipboard. We also carry text and arbitrary files, not just images, and don't require an SSH
session to exist at all.

### Side by side

| | `ccimg` / `sshimg.nvim` | this project (`clip` / `room` / `lan`) |
|---|---|---|
| **Problem solved** | paste a local clipboard image into one remote session | general hand-off between your devices |
| **Direction** | local → remote | bidirectional, any peer → any peer |
| **Topology** | 1:1, tied to one SSH session | N devices at once (mesh or room) |
| **Transport** | existing SSH connection + reverse tunnel (`-R`) | iroh QUIC (`clip`), SSH room server (`room`), quic+mDNS (`lan`) |
| **Content** | images | text · images · **arbitrary files** |
| **Lands as** | a temp file on the remote (path handed to the editor/agent) | the **OS clipboard**, and/or a folder, and/or an appended file |
| **Setup** | a local daemon + `-R` tunnel per session | pair once (`clip remote <host>`, ticket, or mDNS); daemon is resident |
| **Needs SSH?** | yes, by design | no (`clip`/`lan`); `room` uses SSH as its transport |
| **Extras** | Claude Code skill / nvim integration | messenger TUI, session clearing, notify-first, auto clipboard sync |

**Use theirs if** you want the smallest possible thing for exactly one flow — a laptop clipboard image
into a remote Claude Code/Neovim session, over an SSH connection you already have. It's less machinery
and nothing to pair.

**Use this if** you want several devices connected at once, both directions, text/files as well as
images, and content landing directly on the receiving clipboard (or in a folder) rather than as a path.

## The wider ecosystem

Other things people reach for, and why they didn't fit this project's goal (terminal-first hand-off of
text **and** images between your own machines):

| Approach | What it does | Why not (for this goal) |
|---|---|---|
| **OSC 52** (terminal escape) | Terminal writes the local clipboard from a remote process | **Text only**, ~74 KB cap, stripped/limited by tmux and many terminals. We use it only as a text bonus. |
| **kitty's OSC 5522** | kitty's extended clipboard protocol — *can* carry `image/png` | Requires everyone to use **kitty**; permission-gated; no other terminal implements it. |
| **AirDrop / Handoff** | The UX we're imitating | Apple-only; no Linux/Windows; not scriptable from a terminal. |
| **KDE Connect / Warpinator / LocalSend / PairDrop** | Device-to-device file & clipboard sharing on a LAN | GUI-first; not a CLI/TUI you can pipe into; often desktop-environment-bound. |
| **Syncthing** | Continuous folder sync between devices | Folder-shaped, not clipboard-shaped; no "paste this now" moment. Its discovery/relay design *did* inform ours. |
| **Tailscale / Taildrop** | Mesh VPN + file send | Excellent substrate, but needs a control plane/account; file-oriented, not clipboard-oriented. |
| **magic-wormhole / croc** | One-shot encrypted file transfer via a code phrase | Per-transfer handshake; no resident connection, no clipboard integration. |
| **`ssh host pbcopy` / `xclip` over SSH** | The DIY one-liner | One-way, per-OS, text-only in practice, and needs an SSH session each time. |
| **Cloud clipboard managers** | Sync via a vendor's server | Third-party sees your clipboard; often no CLI; not local-first. |

## Where our own approaches sit

We built three implementations against one contract precisely to compare these trade-offs — a true P2P
mesh, a central SSH room, and a minimal LAN mesh. See [`BAKEOFF.md`](BAKEOFF.md) for the measured
comparison and [`implementation.md`](implementation.md) for how each is built.
