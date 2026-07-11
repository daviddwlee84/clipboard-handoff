// Package meshtransport is the libp2p-mesh probe transport (BAKEOFF question:
// "iroh vs libp2p at the same topology — config surface, hole-punch reliability,
// relay/rendezvous needs"). It builds a LAN gossip mesh from go-libp2p:
//
//   - identity: a persisted libp2p private key; the PeerId is the device
//     identity (PROTOCOL §3).
//   - discovery: libp2p mDNS (p2p/discovery/mdns) with a service tag derived
//     from the room, so only same-room peers find and dial each other on the LAN.
//   - transport/topology: gossipsub (go-libp2p-pubsub). Envelopes are published
//     to a topic derived from the room; images ride inline in blob_data for
//     Phase 0.
//
// Same-host note: two instances listen on distinct ephemeral TCP ports, so two
// daemons on one machine discover over mDNS and mesh together.
package meshtransport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/config"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/transport"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// Transport implements transport.Transport over go-libp2p gossipsub + mDNS.
type Transport struct {
	room       string
	deviceName string

	host       host.Host
	topicName  string
	serviceTag string

	ps    *pubsub.PubSub
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	mdns  mdns.Service

	recvFn transport.ReceiveFunc

	mu         sync.Mutex
	connecting map[peer.ID]bool
	ctx        context.Context
	cancel     context.CancelFunc
}

// New builds the transport, loading (or creating) the persistent libp2p
// identity key and the host (listening on an ephemeral TCP port).
func New(cfg *config.Store, room, deviceName string) (*Transport, error) {
	priv, err := loadOrCreateKey(cfg)
	if err != nil {
		return nil, err
	}
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"),
	)
	if err != nil {
		return nil, fmt.Errorf("libp2p host: %w", err)
	}
	sum := sha256.Sum256([]byte(room))
	h16 := hex.EncodeToString(sum[:])[:16]
	t := &Transport{
		room:       room,
		deviceName: deviceName,
		host:       h,
		connecting: make(map[peer.ID]bool),
		// Topic and mDNS tag are both derived from the room so cross-room
		// traffic and discovery stay isolated (PROTOCOL §3).
		topicName:  "clip-exp/" + hex.EncodeToString(sum[:]),
		serviceTag: "clipexp" + h16,
	}
	return t, nil
}

func (t *Transport) Identity() string                   { return t.host.ID().String() }
func (t *Transport) OnReceive(fn transport.ReceiveFunc) { t.recvFn = fn }

// Start brings up gossipsub, joins the room topic and starts mDNS discovery.
func (t *Transport) Start(ctx context.Context) error {
	t.ctx, t.cancel = context.WithCancel(ctx)

	ps, err := pubsub.NewGossipSub(t.ctx, t.host)
	if err != nil {
		return fmt.Errorf("gossipsub: %w", err)
	}
	t.ps = ps

	topic, err := ps.Join(t.topicName)
	if err != nil {
		return fmt.Errorf("join topic: %w", err)
	}
	t.topic = topic

	sub, err := topic.Subscribe()
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	t.sub = sub
	go t.readLoop(sub)

	svc := mdns.NewMdnsService(t.host, t.serviceTag, &notifee{t: t})
	if err := svc.Start(); err != nil {
		return fmt.Errorf("mdns start: %w", err)
	}
	t.mdns = svc

	log.Printf("mesh transport up: peer=%s room=%q topic=%s tag=%s addrs=%v",
		t.host.ID(), t.room, t.topicName, t.serviceTag, t.host.Addrs())
	return nil
}

// notifee connects to peers discovered over mDNS in the same room. To avoid
// simultaneous-connect TLS handshake collisions on the LAN (both peers dialing
// each other at once), only the peer with the lexicographically smaller PeerId
// initiates; the other waits to be dialed and accepts.
type notifee struct{ t *Transport }

func (n *notifee) HandlePeerFound(pi peer.AddrInfo) {
	t := n.t
	if pi.ID == t.host.ID() {
		return
	}
	if t.host.ID().String() >= pi.ID.String() {
		return // the peer with the smaller id dials us
	}
	t.mu.Lock()
	if t.connecting[pi.ID] {
		t.mu.Unlock()
		return
	}
	t.connecting[pi.ID] = true
	t.mu.Unlock()
	go t.connectPeer(pi)
}

// connectPeer dials a discovered peer, retrying briefly since the peer's
// listener may not be up yet and libp2p mDNS does not re-query on a fixed
// interval once it has a response.
func (t *Transport) connectPeer(pi peer.AddrInfo) {
	defer func() {
		t.mu.Lock()
		delete(t.connecting, pi.ID)
		t.mu.Unlock()
	}()
	for attempt := 0; attempt < 8; attempt++ {
		if t.host.Network().Connectedness(pi.ID) == network.Connected {
			return
		}
		ctx, cancel := context.WithTimeout(t.ctx, 8*time.Second)
		err := t.host.Connect(ctx, pi)
		cancel()
		if err == nil {
			log.Printf("mesh: connected to peer %s", pi.ID)
			return
		}
		log.Printf("mesh: connect %s attempt %d failed: %v", pi.ID, attempt+1, err)
		select {
		case <-t.ctx.Done():
			return
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func (t *Transport) readLoop(sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(t.ctx)
		if err != nil {
			return // ctx cancelled / sub closed
		}
		if msg.GetFrom() == t.host.ID() {
			continue // our own published message
		}
		env, err := wire.Unmarshal(msg.Data)
		if err != nil {
			log.Printf("mesh: decode envelope: %v", err)
			continue
		}
		if t.recvFn != nil {
			t.recvFn(env)
		}
	}
}

// Broadcast publishes the envelope to the room topic (gossipsub floods it to
// mesh peers; small images ride inline in blob_data for Phase 0).
func (t *Transport) Broadcast(env *wire.Envelope) error {
	if t.topic == nil {
		return fmt.Errorf("topic not ready")
	}
	frame, err := wire.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
	defer cancel()
	return t.topic.Publish(ctx, frame)
}

func (t *Transport) Peers() []transport.Peer {
	if t.topic == nil {
		return nil
	}
	ids := t.topic.ListPeers()
	out := make([]transport.Peer, 0, len(ids))
	for _, id := range ids {
		addr := ""
		if conns := t.host.Network().ConnsToPeer(id); len(conns) > 0 {
			addr = conns[0].RemoteMultiaddr().String()
		}
		out = append(out, transport.Peer{ID: id.String(), Addr: addr})
	}
	return out
}

func (t *Transport) Close() error {
	if t.cancel != nil {
		t.cancel()
	}
	if t.mdns != nil {
		_ = t.mdns.Close()
	}
	if t.sub != nil {
		t.sub.Cancel()
	}
	if t.topic != nil {
		_ = t.topic.Close()
	}
	return t.host.Close()
}

// loadOrCreateKey loads a persisted libp2p private key from the config dir, or
// generates and persists an Ed25519 key on first run. Its PeerId is identity.
func loadOrCreateKey(cfg *config.Store) (crypto.PrivKey, error) {
	path := cfg.Path("libp2p.key")
	if b, err := os.ReadFile(path); err == nil {
		priv, perr := crypto.UnmarshalPrivateKey(b)
		if perr != nil {
			return nil, fmt.Errorf("parse libp2p key: %w", perr)
		}
		return priv, nil
	}
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, err
	}
	b, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}
