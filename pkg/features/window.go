// Package features turns a per-pod stream of ScoreEvents into a flat
// FeatureVector that the policy engine evaluates each tick. Two pipelines
// feed the vector:
//
//   - spatial: the most recent ScoreEvent's multi-horizon arrays
//     (p50_horizons / tail_horizons / horizon_ms from the extended proto).
//   - temporal: a rolling window of the last N events, used to derive
//     slope / acceleration / variance / duration-above-HI over time.
//
// Both pipelines degrade gracefully: with a single-horizon predictor the
// spatial fields are zero and policy authors lean on temporal-only rules;
// with a cold window the temporal fields are zero too.
package features

import (
	"sync"
	"time"

	pb "github.com/coding-workspace/simple-mitigation-1/gen/go/contentionpb"
)

// Sample is a window entry. The full *pb.ScoreEvent is retained verbatim so
// later code can mine fields we don't currently use (e.g. tail_trend_label
// for tail-based temporal rules).
type Sample struct {
	Event    *pb.ScoreEvent
	Received time.Time
}

// Window is a per-(target,pod) bounded ring buffer of the most recent N
// samples. Safe for concurrent Push / Snapshot from different goroutines.
type Window struct {
	mu   sync.Mutex
	cap  int
	ring []Sample
	// next is the index of the slot to write to next. Once ring is full this
	// wraps around; Snapshot reconstructs in chronological order.
	next int
	// full reports whether the ring has wrapped at least once. Until then
	// only ring[:next] is populated.
	full bool
	// lastSeenSampleID is used by Push to drop duplicate events arriving from
	// reconnects. Sample IDs are monotone within a producer stream.
	lastSeenSampleID int64
}

// NewWindow returns a Window of capacity n. n must be >= 1.
func NewWindow(n int) *Window {
	if n < 1 {
		n = 1
	}
	return &Window{
		cap:  n,
		ring: make([]Sample, n),
	}
}

// Cap returns the configured capacity.
func (w *Window) Cap() int { return w.cap }

// Push records ev at receivedAt. Duplicate sample_ids are ignored so stream
// reconnect storms don't poison temporal slopes with replayed data.
func (w *Window) Push(ev *pb.ScoreEvent, receivedAt time.Time) {
	if ev == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if ev.SampleId != 0 && ev.SampleId == w.lastSeenSampleID {
		return
	}
	w.lastSeenSampleID = ev.SampleId
	w.ring[w.next] = Sample{Event: ev, Received: receivedAt}
	w.next++
	if w.next == w.cap {
		w.next = 0
		w.full = true
	}
}

// Snapshot returns the buffered samples in chronological order (oldest
// first). Returns nil if the window is empty.
func (w *Window) Snapshot() []Sample {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.len()
	if n == 0 {
		return nil
	}
	out := make([]Sample, 0, n)
	if w.full {
		// Oldest entry is at w.next, wrap around to w.next-1.
		for i := 0; i < w.cap; i++ {
			idx := (w.next + i) % w.cap
			out = append(out, w.ring[idx])
		}
		return out
	}
	for i := 0; i < w.next; i++ {
		out = append(out, w.ring[i])
	}
	return out
}

// Len returns the current number of samples in the window.
func (w *Window) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.len()
}

func (w *Window) len() int {
	if w.full {
		return w.cap
	}
	return w.next
}

// Reset clears the window. Used when a pod becomes unhealthy so the next
// reconnect doesn't blend stale samples with fresh ones.
func (w *Window) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next = 0
	w.full = false
	w.lastSeenSampleID = 0
}
