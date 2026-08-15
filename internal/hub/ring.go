package hub

import "encoding/json"

// ring is a bounded transcript buffer.
//
// The whole persistence story of glance is this type: a browser that attaches
// mid-session, or reloads, sees the last `capacity` frames and nothing older.
// Frames are kept as the raw bytes that arrived, so replay is byte-identical to
// what the terminal saw and costs no re-encoding.
//
// Bounded and in-memory is a deliberate choice, not a shortcut. A control plane
// that durably recorded everything an agent ever said -- file contents, diffs,
// command output -- would be a far larger secret to keep than the one this
// server is built to keep.
type ring struct {
	frames  []json.RawMessage
	start   int
	size    int
	dropped int
}

func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}
	return &ring{frames: make([]json.RawMessage, capacity)}
}

func (r *ring) push(frame json.RawMessage) {
	n := len(r.frames)
	if r.size < n {
		r.frames[(r.start+r.size)%n] = frame
		r.size++
		return
	}
	// Full: overwrite the oldest and remember that history was lost, so the UI
	// can say "history truncated" rather than implying the session began here.
	r.frames[r.start] = frame
	r.start = (r.start + 1) % n
	r.dropped++
}

// snapshot returns the buffered frames oldest-first.
//
// The slice is fresh but the frames are shared: they are never mutated after
// being pushed, so readers may hold them without copying.
func (r *ring) snapshot() []json.RawMessage {
	out := make([]json.RawMessage, 0, r.size)
	n := len(r.frames)
	for i := 0; i < r.size; i++ {
		out = append(out, r.frames[(r.start+i)%n])
	}
	return out
}

func (r *ring) reset() {
	r.start = 0
	r.size = 0
	r.dropped = 0
	for i := range r.frames {
		r.frames[i] = nil
	}
}
