package daemon

import "testing"

// Dedupe implements SPEC §3 rule 1: an item's msg_id is processed at most once.
func TestDedupe_SeenTwice(t *testing.T) {
	d := NewDedupe(8)
	if d.Seen("m1") {
		t.Fatalf("first Seen(m1) = true, want false")
	}
	if !d.Seen("m1") {
		t.Fatalf("second Seen(m1) = false, want true (echo/loop suppression)")
	}
	// A different id is independent.
	if d.Seen("m2") {
		t.Fatalf("first Seen(m2) = true, want false")
	}
}

// Once capacity is exceeded, the oldest id is evicted (bounded memory). An
// evicted id is treated as new again — acceptable because ULIDs are
// monotonically increasing, so a re-delivered old id is not expected in
// practice.
func TestDedupe_EvictsOldest(t *testing.T) {
	d := NewDedupe(2)
	d.Seen("a") // {a}
	d.Seen("b") // {a,b}
	d.Seen("c") // evicts a -> {b,c}
	if d.Seen("b") == false {
		t.Errorf("b should still be present")
	}
	if d.Seen("c") == false {
		t.Errorf("c should still be present")
	}
	if d.Seen("a") == true {
		t.Errorf("a should have been evicted (treated as new)")
	}
}
