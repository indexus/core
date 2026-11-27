package zonelog

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// Duration records a fixed storage stage using atomics. It lives at the
// storage engine boundary so neither engine nor storage depends on the
// process-wide HTTP metrics package.
type Duration struct {
	buckets        [40]atomic.Int64
	nanos, maximum atomic.Int64
}

type DurationSnapshot struct {
	Log2     []int64 `json:"us_log2"`
	Nanos    int64   `json:"nanos"`
	MaxNanos int64   `json:"max_nanos"`
}

func (d *Duration) Observe(t time.Duration) {
	d.buckets[min(bits.Len64(uint64(t/time.Microsecond)), len(d.buckets)-1)].Add(1)
	d.nanos.Add(int64(t))
	for old := d.maximum.Load(); int64(t) > old; old = d.maximum.Load() {
		if d.maximum.CompareAndSwap(old, int64(t)) {
			break
		}
	}
}
func (d *Duration) Snapshot() DurationSnapshot {
	out, last := make([]int64, len(d.buckets)), 0
	for i := range d.buckets {
		if out[i] = d.buckets[i].Load(); out[i] != 0 {
			last = i + 1
		}
	}
	return DurationSnapshot{Log2: out[:last], Nanos: d.nanos.Load(), MaxNanos: d.maximum.Load()}
}

type timings struct {
	lock, memtable, syncLock, flush, merge, publish Duration
}

// Timings separates foreground waits from background work. Background times
// overlap foreground times and must not be added to them.
func (s *Store) Timings() map[string]DurationSnapshot {
	return map[string]DurationSnapshot{
		"zonelog_lock":  s.timing.lock.Snapshot(),
		"memtable_full": s.timing.memtable.Snapshot(),
		"sync_lock":     s.timing.syncLock.Snapshot(),
		"flush":         s.timing.flush.Snapshot(),
		"merge":         s.timing.merge.Snapshot(),
		"publish":       s.timing.publish.Snapshot(),
	}
}
