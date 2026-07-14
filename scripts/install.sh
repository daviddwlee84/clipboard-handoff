#!/usr/bin/env bash
# install.sh — build & install the cross-platform-copy tools into a bin dir.
#
#   scripts/install.sh [TOOL ...] [--prefix DIR] [--remote HOST]
#
#   TOOL      clip (mesh-rs) | room (room-go) | lan (lan-go) | all   (default: wizard/all)
#   --prefix  install dir (default: ~/.local/bin)
#   --remote  install onto an SSH host's ~/.local/bin instead of locally
#             (Go tools are cross-compiled locally + scp'd; clip is built on the remote,
#              which must have a cargo toolchain, e.g. ~/.cargo/bin)
#
# Examples:
#   scripts/install.sh                 # interactive picker (or all if non-interactive)
#   scripts/install.sh room lan        # install the two Go tools locally
#   scripts/install.sh clip            # build+install the Rust tool (release) locally
#   scripts/install.sh room --remote local_ubuntu
set -euo pipefail

# Locate the repo source tree (holds mesh-rs/, room-go/, experiments/). When run
# directly as scripts/install.sh it is one dir up; but when this file has been
# copied into ~/.local/libexec/cpc so `<tool> remote <host>` can call it, the
# sources are NOT beside it — fall back to $CPC_ROOT, else search upward from the
# current directory (so `<tool> remote` works when invoked from a repo checkout).
find_root(){
  local d
  d="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." 2>/dev/null && pwd)" || d=""
  if [ -n "$d" ] && [ -d "$d/mesh-rs" ]; then printf '%s\n' "$d"; return 0; fi
  if [ -n "${CPC_ROOT:-}" ] && [ -d "$CPC_ROOT/mesh-rs" ]; then printf '%s\n' "$CPC_ROOT"; return 0; fi
  d="$PWD"
  while [ -n "$d" ]; do
    if [ -d "$d/mesh-rs" ] && [ -d "$d/experiments" ]; then printf '%s\n' "$d"; return 0; fi
    if [ "$d" = "/" ]; then break; fi
    d="$(dirname "$d")"
  done
  return 1
}
ROOT="$(find_root || true)"
PREFIX=""            # empty => default to <target>/.local/bin (local or remote HOME)
REMOTE=""
TOOLS=()

say(){ printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn(){ printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die(){ printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
[ -n "$ROOT" ] || die "cannot find the cross-platform-copy source tree — run '<tool> remote' from a repo checkout, or set CPC_ROOT=/path/to/repo"

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX="$2"; shift 2;;
    --remote) REMOTE="$2"; shift 2;;
    -h|--help) sed -n '2,20p' "$0"; exit 0;;
    clip|room|lan) TOOLS+=("$1"); shift;;
    all) TOOLS=(clip room lan); shift;;
    *) die "unknown arg: $1 (want: clip|room|lan|all, --prefix, --remote)";;
  esac
done

# Wizard: if nothing chosen and we have a TTY, ask; else default to all.
if [ ${#TOOLS[@]} -eq 0 ]; then
  if [ -t 0 ]; then
    echo "Which tool(s) to install?"
    echo "  1) clip  — mesh-rs (Rust/iroh, true P2P)"
    echo "  2) room  — room-go (Go/wish, SSH room server)"
    echo "  3) lan   — lan-go  (Go, quic+mDNS LAN mesh)"
    echo "  a) all"
    read -r -p "> [a] " ans; ans="${ans:-a}"
    case "$ans" in
      1) TOOLS=(clip);; 2) TOOLS=(room);; 3) TOOLS=(lan);; *) TOOLS=(clip room lan);;
    esac
  else
    TOOLS=(clip room lan)
  fi
fi

# Resolve target OS/arch (and the default prefix on the target — local or remote HOME).
if [ -n "$REMOTE" ]; then
  read -r UOS UARCH RHOME < <(ssh -o BatchMode=yes "$REMOTE" 'printf "%s %s %s\n" "$(uname -s)" "$(uname -m)" "$HOME"')
  [ -z "$PREFIX" ] && PREFIX="$RHOME/.local/bin"
else
  UOS="$(uname -s)"; UARCH="$(uname -m)"
  [ -z "$PREFIX" ] && PREFIX="$HOME/.local/bin"
fi
case "$UOS" in Linux) GOOS=linux;; Darwin) GOOS=darwin;; *) die "unsupported OS: $UOS";; esac
case "$UARCH" in x86_64|amd64) GOARCH=amd64;; arm64|aarch64) GOARCH=arm64;; *) die "unsupported arch: $UARCH";; esac
say "target: $GOOS/$GOARCH  prefix: ${REMOTE:+$REMOTE:}$PREFIX"

# Ensure the destination bin dir exists.
if [ -n "$REMOTE" ]; then ssh -o BatchMode=yes "$REMOTE" "mkdir -p '$PREFIX'"; else mkdir -p "$PREFIX"; fi

build_go(){ # $1=bin  $2=module-dir  $3=pkg
  local bin="$1" dir="$2" pkg="$3" out; out="$(mktemp -d)/$bin"
  say "building $bin ($GOOS/$GOARCH)…"
  ( cd "$ROOT/$dir" && CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -o "$out" "$pkg" )
  if [ -n "$REMOTE" ]; then scp -q "$out" "$REMOTE:$PREFIX/$bin"; ssh "$REMOTE" "chmod +x '$PREFIX/$bin'"
  else install -m 0755 "$out" "$PREFIX/$bin"; fi
  say "installed $bin → ${REMOTE:+$REMOTE:}$PREFIX/$bin"
}

build_clip(){
  if [ -n "$REMOTE" ]; then
    say "clip: building on $REMOTE (cargo; this compiles iroh, be patient)…"
    ssh -o BatchMode=yes "$REMOTE" 'command -v ~/.cargo/bin/cargo >/dev/null || command -v cargo >/dev/null' \
      || die "clip --remote needs a cargo toolchain on $REMOTE (install rustup)"
    rsync -a --delete --exclude target/ --rsync-path='mkdir -p cpc-build/mesh-rs && rsync' "$ROOT/mesh-rs/" "$REMOTE:cpc-build/mesh-rs/" >/dev/null
    ssh "$REMOTE" 'cd cpc-build/mesh-rs && ${HOME}/.cargo/bin/cargo build --release'
    ssh "$REMOTE" "install -m 0755 cpc-build/mesh-rs/target/release/clip '$PREFIX/clip'"
    say "installed clip → $REMOTE:$PREFIX/clip"
  else
    command -v cargo >/dev/null || die "clip needs a cargo toolchain locally"
    say "clip: building release (compiles iroh, be patient)…"
    ( cd "$ROOT/mesh-rs" && cargo build --release )
    install -m 0755 "$ROOT/mesh-rs/target/release/clip" "$PREFIX/clip"
    say "installed clip → $PREFIX/clip"
  fi
}

for t in "${TOOLS[@]}"; do
  case "$t" in
    room) build_go room "room-go" "./cmd/room";;
    lan)  build_go lan  "experiments/lan-go" "./cmd/lan";;
    clip) build_clip;;
  esac
done

# Install the shared remote engine (+ this installer) so `<tool> remote <host>`
# can find it from ~/.local/bin even outside the repo.
LIBEXEC="$(dirname "$PREFIX")/libexec/cpc"
if [ -n "$REMOTE" ]; then
  ssh -o BatchMode=yes "$REMOTE" "mkdir -p '$LIBEXEC'"
  scp -q "$ROOT/scripts/remote.sh" "$ROOT/scripts/install.sh" "$REMOTE:$LIBEXEC/"
  ssh "$REMOTE" "chmod +x '$LIBEXEC/remote.sh' '$LIBEXEC/install.sh'"
else
  mkdir -p "$LIBEXEC"
  install -m 0755 "$ROOT/scripts/remote.sh" "$LIBEXEC/remote.sh"
  install -m 0755 "$ROOT/scripts/install.sh" "$LIBEXEC/install.sh"
fi
say "installed remote engine → ${REMOTE:+$REMOTE:}$LIBEXEC/remote.sh"

# PATH hint.
on_path(){ if [ -n "$REMOTE" ]; then ssh "$REMOTE" "case \":\$PATH:\" in *\":$PREFIX:\"*) exit 0;; *) exit 1;; esac"
           else case ":$PATH:" in *":$PREFIX:"*) return 0;; *) return 1;; esac; fi; }
if ! on_path; then
  warn "$PREFIX is not on PATH${REMOTE:+ on $REMOTE}. Add:"
  echo "    export PATH=\"$PREFIX:\$PATH\"   # in your shell rc"
fi
say "done."
