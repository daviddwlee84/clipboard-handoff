package broker

import (
	"sync"
	"testing"
)

// drain reads up to n frames from a session's Out without blocking, returning
// what was buffered.
func drain(s *Session) [][]byte {
	var out [][]byte
	for {
		select {
		case f := <-s.Out:
			out = append(out, f)
		default:
			return out
		}
	}
}

func newSession(room string) *Session {
	return &Session{Room: room, Out: make(chan []byte, 16)}
}

// Fan-out reaches every other member of the room but never the sender
// (self-send guard inherited from sshbbs).
func TestBroadcast_ExcludesSender(t *testing.T) {
	b := New()
	a := newSession("r1")
	c := newSession("r1")
	d := newSession("r1")
	b.Register(a)
	b.Register(c)
	b.Register(d)

	n := b.Broadcast("r1", a, []byte("hello"))
	if n != 2 {
		t.Fatalf("Broadcast delivered to %d, want 2", n)
	}
	if got := drain(a); len(got) != 0 {
		t.Errorf("sender received its own message: %v", got)
	}
	for name, s := range map[string]*Session{"c": c, "d": d} {
		got := drain(s)
		if len(got) != 1 || string(got[0]) != "hello" {
			t.Errorf("session %s got %v, want [\"hello\"]", name, got)
		}
	}
}

// Rooms are isolated: a broadcast in one room never reaches another.
func TestBroadcast_RoomIsolation(t *testing.T) {
	b := New()
	a := newSession("r1")
	other := newSession("r2")
	b.Register(a)
	b.Register(other)

	b.Broadcast("r1", a, []byte("x"))
	if got := drain(other); len(got) != 0 {
		t.Errorf("cross-room leak: r2 received %v", got)
	}
}

func TestUnregister_RemovesAndCleansRoom(t *testing.T) {
	b := New()
	a := newSession("r1")
	c := newSession("r1")
	b.Register(a)
	b.Register(c)
	if got := b.RoomCount("r1"); got != 2 {
		t.Fatalf("RoomCount = %d, want 2", got)
	}
	b.Unregister(a)
	if got := b.RoomCount("r1"); got != 1 {
		t.Fatalf("RoomCount after 1 unregister = %d, want 1", got)
	}
	// Broadcasting now reaches only c, and skipping the removed sender is safe.
	if n := b.Broadcast("r1", a, []byte("y")); n != 1 {
		t.Fatalf("Broadcast reached %d, want 1", n)
	}
	b.Unregister(c)
	if got := b.RoomCount("r1"); got != 0 {
		t.Fatalf("RoomCount after all unregister = %d, want 0", got)
	}
}

// Concurrency stress (run with -race): register/broadcast/unregister together.
func TestBroker_Concurrency(t *testing.T) {
	b := New()
	const workers = 16
	const iters = 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := newSession("shared")
			for i := 0; i < iters; i++ {
				b.Register(s)
				b.Broadcast("shared", s, []byte("x"))
				drain(s)
				b.Unregister(s)
			}
		}()
	}
	wg.Wait()
	if got := b.RoomCount("shared"); got != 0 {
		t.Errorf("RoomCount not zero after all unregisters: %d", got)
	}
}
