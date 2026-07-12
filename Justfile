# Justfile — common tasks for cross-platform-copy.
#   `just`         list recipes
#   `just install` build+install all tools to ~/.local/bin
# Tools: clip (mesh-rs, Rust) · room (room-go, Go) · lan (lan-go, Go)

remote := "local_ubuntu"          # default SSH host for remote/xmachine recipes

_default:
    @just --list

# --- build / install -------------------------------------------------------

# Build every tool's debug/native binary in-tree (no install).
build:
    cd mesh-rs && cargo build
    cd room-go && go build -o bin/room ./cmd/room
    cd experiments/lan-go && go build -o bin/lan ./cmd/lan

# Install tools to ~/.local/bin (wizard if no args): e.g. `just install room lan`.
install *tools:
    scripts/install.sh {{tools}}

# Install onto an SSH host's ~/.local/bin: `just install-remote local_ubuntu room`.
install-remote host *tools:
    scripts/install.sh {{tools}} --remote {{host}}

# --- tests / verification --------------------------------------------------

# Unit tests across all impls.
test:
    cd mesh-rs && cargo test
    cd room-go && go test ./...
    cd experiments/shared-go && go test ./...

# Localhost text+PNG round-trip for one impl: `just roundtrip mesh-rs`.
roundtrip impl:
    scripts/roundtrip.sh {{impl}}

# Real cross-machine hand-off to a LAN host: `just xmachine mesh-rs` (host defaults to {{remote}}).
xmachine impl host=remote:
    scripts/xmachine.sh {{impl}} {{host}}

# Files + folder/append sinks + clear semantics e2e: `just sinks room-go`.
sinks impl:
    #!/usr/bin/env bash
    case {{impl}} in
      mesh-rs) bash mesh-rs/scripts/e2e_sinks.sh;;
      room-go) bash room-go/scripts/e2e_sinks.sh;;
      lan-go)  echo "lan-go sinks are covered by shared-go go test + roundtrip";;
      *) echo "unknown impl: {{impl}}"; exit 2;;
    esac

# Two mesh-rs daemons connect with no ticket.
autodiscover:
    bash mesh-rs/scripts/autodiscover.sh

# Bootstrap a tool on an SSH host + connect (VSCode-Remote style).
#   `just remote clip local_ubuntu`   /   `just remote room local_ubuntu down`
remote tool host action="up":
    #!/usr/bin/env bash
    if [ "{{tool}}" = room ]; then
      if [ "{{action}}" = down ]; then ./room-go/bin/room remote {{host}} --stop; else ./room-go/bin/room remote {{host}}; fi
    else
      scripts/remote.sh {{tool}} {{host}} {{action}}
    fi

# Regenerate shared test assets.
testdata:
    python3 scripts/gen_testdata.py

# --- docker cross-machine simulation ---------------------------------------

# Bring up the multi-container "cross-machine on one host" sandbox.
up:
    docker compose -f docker/compose.yml up -d --build

# Tear it down.
down:
    docker compose -f docker/compose.yml down -v

# Run the scripted docker hand-off demo (text + image + file across containers).
demo:
    bash docker/demo.sh

# Open a shell in a sandbox container: `just dsh room-b`.
dsh svc:
    docker compose -f docker/compose.yml exec {{svc}} bash

# --- housekeeping ----------------------------------------------------------

fmt:
    cd mesh-rs && cargo fmt
    cd room-go && gofmt -w .
    cd experiments/shared-go && gofmt -w .

# Free disk: drop build artifacts (Rust target is large).
clean:
    cd mesh-rs && cargo clean
    rm -rf room-go/bin experiments/*/bin scripts/.run
