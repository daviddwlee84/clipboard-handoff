package daemon

import "sync"

// Dedupe is the msg_id dedupe set (SPEC §3 echo/loop suppression rule 1:
// "never process the same item twice"). Bounded so a long-lived daemon does not
// grow without limit; oldest ids are evicted first. On send the daemon marks
// its own msg_id seen so a transport that echoes published messages back (e.g.
// gossipsub self-delivery) is suppressed.
type Dedupe struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	cap   int
}

func NewDedupe(capN int) *Dedupe {
	if capN <= 0 {
		capN = 1024
	}
	return &Dedupe{seen: make(map[string]struct{}), cap: capN}
}

// Seen reports whether id was already processed, marking it seen. The first call
// for an id returns false; subsequent calls return true.
func (d *Dedupe) Seen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; ok {
		return true
	}
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > d.cap {
		old := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, old)
	}
	return false
}
