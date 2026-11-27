package storage

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

const (
	DefaultMaxPendingWrites       = 4096
	DefaultMaxPendingBytes  int64 = 64 << 20
	// DefaultMaxQueueWait is the CoDel-style sojourn-time bound on the commit
	// queue: absorbs transient handoff/fsync spikes without shedding clients,
	// while capping queue growth well before multi-second congestion collapse.
	DefaultMaxQueueWait = 2 * time.Second
)

// ErrWriteOverload is a transient local admission refusal, distinct from disk
// capacity and ownership. A refused call never enters the commit queue.
var ErrWriteOverload = errors.New("durable write admission overloaded")

type WriteAdmissionError struct {
	Pending, MaxPending     int
	Bytes, MaxBytes         int64
	QueueWait, MaxQueueWait time.Duration
}

func (e *WriteAdmissionError) Error() string {
	if e.MaxQueueWait > 0 && e.QueueWait > e.MaxQueueWait {
		return fmt.Sprintf("%v: queue_wait=%s/%s pending=%d/%d", ErrWriteOverload, e.QueueWait.Round(time.Millisecond), e.MaxQueueWait, e.Pending, e.MaxPending)
	}
	return fmt.Sprintf("%v: pending=%d/%d bytes=%d/%d", ErrWriteOverload, e.Pending, e.MaxPending, e.Bytes, e.MaxBytes)
}
func (e *WriteAdmissionError) Is(target error) bool { return target == ErrWriteOverload }

type diskWrite struct {
	apply     func(*Tx) error
	done      chan error
	bytes     int64
	submitted time.Time
}

// diskBatch keeps one group commit in flight per store. While it waits for
// the storage writer or fsync, arrivals accumulate for the next commit instead of
// forming many timer-triggered batches that each wait for the same writer.
// Callbacks must reset their outputs on replay after a sibling fails.
type diskBatch struct {
	update  func(func(*Tx) error) error
	size    int
	delay   time.Duration
	mu      sync.Mutex
	pending []diskWrite
	// Admission includes the group already waiting for Bolt/fsync, not just
	// the next batch. BatchSize is a scheduling bound, not a backlog bound.
	peakPending   int
	admitted      int
	admittedBytes int64
	maxPending    int
	maxBytes      int64
	maxQueueWait  time.Duration
	running       bool
	closed        bool
	idle          chan struct{}
	stats         commitStats
}

// commitStats times group commits and queue/admission stages for GET /metrics.
// Buckets are powers of two in microseconds.
type commitStats struct {
	queue, admissionLock zonelog.Duration
	refused              atomic.Int64
	commits, writes      atomic.Int64
	commit, wait         [40]atomic.Int64
}

func observe(h *[40]atomic.Int64, d time.Duration) {
	h[min(bits.Len64(uint64(d/time.Microsecond)), len(h)-1)].Add(1)
}

// CommitStats is the store's group commit: Commit times one transaction and
// its fsync, Wait one write from submission to its commit's end.
type CommitStats struct {
	Pending, PeakPending int
	PendingBytes         int64
	Stages               map[string]zonelog.DurationSnapshot
	ByKind               map[string]CommitKindStats
	Refused              int64
	Commits, Writes      int64
	CommitLog2, WaitLog2 []int64 // power-of-two microsecond buckets
}

// CommitStats reports the group commit's counters since the store opened.
func (s *Store) CommitStats() CommitStats {
	out := s.writes.stats.snapshot()
	out.Stages = s.db.Timings()
	out.ByKind = s.commitKindStats()
	out.Stages["queue"] = s.writes.stats.queue.Snapshot()
	out.Stages["admission_lock"] = s.writes.stats.admissionLock.Snapshot()
	out.Stages["write_lock"] = s.writeLockTiming.Snapshot()
	out.Stages["validation"] = s.validationTiming.Snapshot()
	out.Refused = s.writes.stats.refused.Load()
	s.writes.mu.Lock()
	out.Pending, out.PendingBytes, out.PeakPending = s.writes.admitted, s.writes.admittedBytes, s.writes.peakPending
	s.writes.mu.Unlock()
	return out
}

func (c *commitStats) snapshot() CommitStats {
	load := func(h *[40]atomic.Int64) []int64 {
		out, last := make([]int64, len(h)), 0
		for i := range h {
			if out[i] = h[i].Load(); out[i] != 0 {
				last = i + 1
			}
		}
		return out[:last]
	}
	return CommitStats{Commits: c.commits.Load(), Writes: c.writes.Load(), CommitLog2: load(&c.commit), WaitLog2: load(&c.wait)}
}

func newDiskBatchWithLimits(update func(func(*Tx) error) error, size int, delay time.Duration, pending int, bytes int64) *diskBatch {
	if pending <= 0 {
		pending = DefaultMaxPendingWrites
	}
	if bytes <= 0 {
		bytes = DefaultMaxPendingBytes
	}
	idle := make(chan struct{})
	close(idle)
	return &diskBatch{update: update, size: max(size, 1), delay: max(delay, 0), idle: idle, maxPending: pending, maxBytes: bytes, maxQueueWait: DefaultMaxQueueWait}
}

// Context bounds admission only. Once accepted, cancellation cannot claim that
// a durable commit was rolled back: the caller waits for its actual outcome.
func (b *diskBatch) submit(ctx context.Context, bytes int64, fn func(*Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	call := diskWrite{apply: fn, done: make(chan error, 1), bytes: max(bytes, 1), submitted: started}
	b.mu.Lock()
	b.stats.admissionLock.Observe(time.Since(started))
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		b.mu.Unlock()
		return err
	}
	var queueWait time.Duration
	if len(b.pending) > 0 {
		queueWait = started.Sub(b.pending[0].submitted)
	}
	if b.admitted >= b.maxPending || call.bytes > b.maxBytes-b.admittedBytes || (b.maxQueueWait > 0 && queueWait > b.maxQueueWait) {
		b.stats.refused.Add(1)
		err := &WriteAdmissionError{Pending: b.admitted, MaxPending: b.maxPending, Bytes: b.admittedBytes, MaxBytes: b.maxBytes, QueueWait: queueWait, MaxQueueWait: b.maxQueueWait}
		b.mu.Unlock()
		return err
	}
	b.admitted++
	b.peakPending = max(b.peakPending, b.admitted)
	b.admittedBytes += call.bytes
	b.pending = append(b.pending, call)
	if !b.running {
		b.running = true
		b.idle = make(chan struct{})
		go b.run()
	}
	b.mu.Unlock()
	err := <-call.done
	observe(&b.stats.wait, time.Since(started))
	return err
}

func (b *diskBatch) run() {
	for {
		b.mu.Lock()
		if len(b.pending) == 0 {
			b.running = false
			close(b.idle)
			b.mu.Unlock()
			return
		}
		// A full backlog can go straight to disk. A partial group gets one
		// short collection window, including callers waking from the last ACK.
		if len(b.pending) < b.size {
			b.mu.Unlock()
			time.Sleep(b.delay)
			b.mu.Lock()
		}
		n := min(len(b.pending), b.size)
		calls := append([]diskWrite(nil), b.pending[:n]...)
		clear(b.pending[:n])
		b.pending = b.pending[n:]
		b.mu.Unlock()
		b.commit(calls)
	}
}

func (b *diskBatch) commit(calls []diskWrite) {
	for _, call := range calls {
		b.stats.queue.Observe(time.Since(call.submitted))
	}
	var retry []diskWrite
	for len(calls) > 0 {
		failed := -1
		started := time.Now()
		err := b.update(func(tx *Tx) error {
			for i, call := range calls {
				if err := call.apply(tx); err != nil {
					failed = i
					return err
				}
			}
			return nil
		})
		b.stats.commits.Add(1)
		b.stats.writes.Add(int64(len(calls)))
		observe(&b.stats.commit, time.Since(started))
		if failed >= 0 {
			// Rollback is complete. Remove the failing callback, retry its
			// siblings as a group, then run this callback in isolation.
			retry = append(retry, calls[failed])
			calls = append(calls[:failed], calls[failed+1:]...)
			continue
		}
		for _, call := range calls {
			b.complete(call, err)
		}
		break
	}
	for _, call := range retry {
		b.complete(call, b.update(call.apply))
	}
}

func (b *diskBatch) complete(call diskWrite, err error) {
	b.mu.Lock()
	b.admitted--
	b.admittedBytes -= call.bytes
	b.mu.Unlock()
	call.done <- err
}

// close rejects new submissions and drains accepted writes before DB.Close.
func (b *diskBatch) close() {
	b.mu.Lock()
	b.closed = true
	idle := b.idle
	b.mu.Unlock()
	<-idle
}
