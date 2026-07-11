// Package transport defines the pluggable transport contract for the
// experiments track. The shared daemon owns everything above the wire (IPC,
// ring buffer, dedupe, auto-copy, clipboard); a probe only implements this
// interface to carry Envelopes between peers over its transport of choice
// (quic-go + mDNS for lan-go, go-libp2p gossipsub + mDNS for libp2p-mesh).
package transport

import (
	"context"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// Peer is a currently-connected peer surfaced to `peers` / `status`.
type Peer struct {
	ID   string `json:"id"`   // stable identity: TLS cert fp / libp2p PeerId
	Name string `json:"name"` // advisory device name (may be empty)
	Addr string `json:"addr"` // remote address, advisory
}

// ReceiveFunc is invoked by a transport for every inbound envelope. The daemon
// registers one via OnReceive; the transport is responsible for decoding bytes
// into an *wire.Envelope before calling it.
type ReceiveFunc func(*wire.Envelope)

// Transport is the minimal surface a probe implements. The daemon calls
// OnReceive then Start; Broadcast/Peers/Identity are called for the lifetime of
// the daemon; Close tears the transport down.
type Transport interface {
	// Identity is the stable per-device identity string (PROTOCOL §3): the
	// TLS cert SHA-256 fingerprint (lan-go) or the libp2p PeerId (libp2p-mesh).
	Identity() string
	// Start brings the transport up (discovery + listeners). It must return
	// promptly, running its own goroutines in the background, and stop when ctx
	// is cancelled.
	Start(ctx context.Context) error
	// Broadcast delivers env to all currently-connected peers in the room.
	Broadcast(env *wire.Envelope) error
	// OnReceive registers the daemon's ingest callback. Called once before Start.
	OnReceive(fn ReceiveFunc)
	// Peers lists currently-connected peers.
	Peers() []Peer
	// Close releases discovery + network resources.
	Close() error
}
