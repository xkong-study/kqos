package sysadvisor

import (
	"math"
	"sort"
	"time"
)

// window is a fixed-capacity ring of timestamped observations. It is the only
// state the advisor keeps, and it is deliberately small: sizing the reclaimed
// tier off a five-minute window means kqos reacts to a genuine traffic shift
// within one window but ignores a single scrape blip.
type window struct {
	values   []float64
	stamps   []time.Time
	next     int
	filled   bool
	capacity int
	horizon  time.Duration
}

func newWindow(capacity int, horizon time.Duration) *window {
	if capacity < 1 {
		capacity = 1
	}
	return &window{
		values:   make([]float64, capacity),
		stamps:   make([]time.Time, capacity),
		capacity: capacity,
		horizon:  horizon,
	}
}

// push records an observation, overwriting the oldest when full.
func (w *window) push(v float64, at time.Time) {
	w.values[w.next] = v
	w.stamps[w.next] = at
	w.next = (w.next + 1) % w.capacity
	if w.next == 0 {
		w.filled = true
	}
}

// live returns the observations inside the horizon, oldest first. Entries
// older than the horizon are skipped rather than deleted: the ring overwrites
// them soon enough, and skipping keeps push allocation-free.
func (w *window) live(now time.Time) []float64 {
	n := w.capacity
	if !w.filled {
		n = w.next
	}
	if n == 0 {
		return nil
	}
	cutoff := now.Add(-w.horizon)
	out := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		idx := i
		if w.filled {
			idx = (w.next + i) % w.capacity
		}
		if w.stamps[idx].Before(cutoff) {
			continue
		}
		out = append(out, w.values[idx])
	}
	return out
}

// count is how many live observations back the current aggregate.
func (w *window) count(now time.Time) int { return len(w.live(now)) }

// full reports whether the window has been running long enough for its
// percentiles to mean anything. Recommendations made before this are marked
// low-confidence rather than suppressed, because a fresh node still needs a
// number.
func (w *window) full() bool { return w.filled }

// percentile returns the p-th percentile (0-100) using nearest-rank, which is
// the right choice here: with a few dozen samples, interpolating between two
// observations invents a value the node never actually exhibited.
func (w *window) percentile(p float64, now time.Time) float64 {
	vals := w.live(now)
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	if p <= 0 {
		return vals[0]
	}
	if p >= 100 {
		return vals[len(vals)-1]
	}
	rank := int(math.Ceil(p / 100 * float64(len(vals))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(vals) {
		rank = len(vals)
	}
	return vals[rank-1]
}

// max returns the largest live observation.
func (w *window) max(now time.Time) float64 {
	vals := w.live(now)
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// mean returns the arithmetic mean of live observations.
func (w *window) mean(now time.Time) float64 {
	vals := w.live(now)
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// ewma applies exponential smoothing over the live observations, oldest first.
// Unlike a percentile it never fully forgets an old spike, which makes it the
// safer aggregate for memory.
func (w *window) ewma(alpha float64, now time.Time) float64 {
	vals := w.live(now)
	if len(vals) == 0 {
		return 0
	}
	if alpha <= 0 || alpha > 1 {
		alpha = 0.3
	}
	acc := vals[0]
	for _, v := range vals[1:] {
		acc = alpha*v + (1-alpha)*acc
	}
	return acc
}

// aggregate collapses the window according to the configured algorithm.
func (w *window) aggregate(algorithm string, alpha float64, now time.Time) float64 {
	switch algorithm {
	case "p99":
		return w.percentile(99, now)
	case "max":
		return w.max(now)
	case "ewma":
		return w.ewma(alpha, now)
	case "mean":
		return w.mean(now)
	default:
		return w.percentile(95, now)
	}
}
