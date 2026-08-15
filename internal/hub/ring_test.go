package hub

import (
	"encoding/json"
	"fmt"
	"testing"
)

func frames(r *ring) []string {
	out := make([]string, 0, r.size)
	for _, f := range r.snapshot() {
		out = append(out, string(f))
	}
	return out
}

func TestRingKeepsTheMostRecentFrames(t *testing.T) {
	r := newRing(3)
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("empty ring snapshot = %v", got)
	}

	for i := 1; i <= 5; i++ {
		r.push(json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
	}

	// A browser opening mid-session wants the end of the transcript, not the
	// beginning, so the oldest frames are what fall off.
	want := []string{`{"n":3}`, `{"n":4}`, `{"n":5}`}
	got := frames(r)
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("snapshot = %v, want %v", got, want)
		}
	}

	// The count of dropped frames is shown in the UI, so a viewer knows the
	// transcript starts mid-stream rather than at the beginning of the session.
	if r.dropped != 2 {
		t.Fatalf("dropped = %d, want 2", r.dropped)
	}
	if r.size != 3 {
		t.Fatalf("size = %d, want 3", r.size)
	}
}

func TestRingSnapshotIsOrderedAcrossTheWrap(t *testing.T) {
	r := newRing(4)
	for i := 1; i <= 4; i++ {
		r.push(json.RawMessage(fmt.Sprintf(`%d`, i)))
	}
	// Exactly full: no wrap yet.
	if got := frames(r); got[0] != "1" || got[3] != "4" {
		t.Fatalf("full ring = %v", got)
	}

	r.push(json.RawMessage(`5`))
	got := frames(r)
	// Replay is only useful if it is in order; an off-by-one at the wrap point
	// would show the transcript scrambled rather than truncated.
	for i, want := range []string{"2", "3", "4", "5"} {
		if got[i] != want {
			t.Fatalf("after wrap = %v, want [2 3 4 5]", got)
		}
	}
}

func TestRingReset(t *testing.T) {
	r := newRing(2)
	r.push(json.RawMessage(`1`))
	r.push(json.RawMessage(`2`))
	r.push(json.RawMessage(`3`))
	r.reset()
	if r.size != 0 || r.dropped != 0 || len(r.snapshot()) != 0 {
		t.Fatalf("reset left size=%d dropped=%d len=%d", r.size, r.dropped, len(r.snapshot()))
	}
}
