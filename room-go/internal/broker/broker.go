// Package broker is the in-memory hub that connects live SSH sessions in a
// room and fans out envelopes between them. It is a direct adaptation of the
// sshbbs chat broker (internal/chat/broker.go): the same map[key][]*Session
// shape and the same self-send discipline, but keyed by room name and
// delivering opaque framed envelopes to per-session outbound channels instead
// of tea.Program.Send.
//
// Self-send guard (inherited from sshbbs, see its
// pitfalls/water-balloon-self-send-hangs-server.md): the originator of an
// envelope must never receive its own message back. sshbbs enforced this
// because a self-targeted tea.Program.Send from inside Update deadlocks on an
// unbuffered channel; here the reason is protocol-level (a client must not
// echo its own send), and Broadcast excludes the sender explicitly.
package broker

import "sync"

// Session is one live SSH connection that has joined a room. The server's
// relay handler creates it, registers it, and drains Out to the SSH channel.
type Session struct {
	Room string
	// FP is the client's SSH public-key fingerprint (its stable identity per
	// PROTOCOL §3). Advisory here; carried in the envelope's Sender field.
	FP string
	// Out is the session's outbound queue. The relay handler's writer
	// goroutine drains it to the SSH channel. Buffered so a momentarily slow
	// client doesn't stall the broadcaster.
	Out chan []byte
}

// Broker maps a room name to the set of sessions currently in it.
type Broker struct {
	mu    sync.RWMutex
	rooms map[string]map[*Session]struct{}
}

func New() *Broker {
	return &Broker{rooms: make(map[string]map[*Session]struct{})}
}

// Register adds s to its room.
func (b *Broker) Register(s *Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.rooms[s.Room]
	if set == nil {
		set = make(map[*Session]struct{})
		b.rooms[s.Room] = set
	}
	set[s] = struct{}{}
}

// Unregister removes s from its room, deleting the room when it empties.
func (b *Broker) Unregister(s *Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.rooms[s.Room]
	if set == nil {
		return
	}
	delete(set, s)
	if len(set) == 0 {
		delete(b.rooms, s.Room)
	}
}

// Broadcast delivers frame to every session in room except the sender. It
// returns the number of recipients that accepted it. Recipients are snapshotted
// under the read lock and delivered to outside it, so a slow client cannot hold
// the lock; a recipient whose Out buffer is full is skipped (dropped) rather
// than blocking the whole fan-out.
func (b *Broker) Broadcast(room string, from *Session, frame []byte) int {
	b.mu.RLock()
	set := b.rooms[room]
	targets := make([]*Session, 0, len(set))
	for s := range set {
		if s == from { // self-send guard: never echo to the originator
			continue
		}
		targets = append(targets, s)
	}
	b.mu.RUnlock()

	n := 0
	for _, s := range targets {
		select {
		case s.Out <- frame:
			n++
		default:
			// Out buffer full — drop for this recipient rather than stall
			// the broadcast. Phase 0: buffers are generous and localhost is
			// fast, so this is defensive only.
		}
	}
	return n
}

// RoomCount returns the number of sessions currently in room (peer-count
// visibility for `status`).
func (b *Broker) RoomCount(room string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.rooms[room])
}
