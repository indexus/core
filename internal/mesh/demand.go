package mesh

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// WriteDemand measures write demand where it arrives, before the store has
// served it: a blocked store slows the acknowledgements, never the arrivals
// nor the count of writes still waiting.
//
// Every update is an atomic operation on a fixed array: no lock, no
// allocation per write.
//
//   - Received: write requests that entered this vnode (a retry of a request
//     this vnode just refused at a cutover is counted in Retried instead).
//   - Forwarded: requests this vnode passed on to another holder. It is net:
//     a request that comes back for a local attempt is taken out again.
//   - Arrived (Received - Forwarded): writes that reached this vnode as their
//     holder. A relay hop is therefore not an arrival at the relay.
//   - InFlight: arrived, not answered yet (waiting at a cutover, at the
//     store, or for a replica).
//   - Applied, Acknowledged, Refused: how the writes served here ended.
type WriteDemand struct {
	Received, Forwarded, Retried atomic.Int64
	InFlight                     atomic.Int64
	Applied, Acknowledged        atomic.Int64
	Refused                      [refusalKinds]atomic.Int64

	// waiting holds the arrival time (unix nanoseconds, 0 when free) of the
	// in-flight writes it tracks, for OldestWait.
	waitSeq atomic.Uint32
	waiting [waitSlots]atomic.Int64
	// refusedIDs remembers the request ids just refused at a cutover: the
	// relay retries those on the same endpoint (callWrite).
	refusedIDs [retrySlots]atomic.Uint64
}

const (
	// waitSlots bounds the writes whose arrival time is tracked; waitProbes
	// bounds the cost of finding a free slot when they are all taken.
	waitSlots  = 256
	waitProbes = 4
	retrySlots = 1024
)

const (
	refusedOverload = iota
	refusedFrozen
	refusedNotOwner
	refusedNotReady
	refusedCapacity
	refusedCanceled
	refusedOther
	refusalKinds
)

var refusalNames = [refusalKinds]string{"overload", "frozen", "not_owner", "not_ready", "capacity", "canceled", "other"}

func refusalKind(err error) int {
	switch {
	case errors.Is(err, storage.ErrWriteOverload):
		return refusedOverload
	case errors.Is(err, storage.ErrFrozen):
		return refusedFrozen
	case errors.Is(err, storage.ErrNotOwner):
		return refusedNotOwner
	case errors.Is(err, storage.ErrNotReady):
		return refusedNotReady
	case errors.Is(err, storage.ErrDataCapacity):
		return refusedCapacity
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return refusedCanceled
	}
	return refusedOther
}

// writeVisit follows one write request through this vnode. It lives on the
// stack of handleWrite.
type writeVisit struct {
	demand *WriteDemand
	id     uint64 // hash of the request id, never 0
	slot   int    // index in waiting, -1 when not tracked
	here   bool   // counted in flight at this vnode
}

// requestHash is FNV-1a over the request id; 0 is kept for "no id".
func requestHash(id string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(id); i++ {
		h = (h ^ uint64(id[i])) * 1099511628211
	}
	if h == 0 {
		return 1
	}
	return h
}

// arrive counts a write request entering this vnode.
func (d *WriteDemand) arrive(requestID string, now int64) writeVisit {
	v := writeVisit{demand: d, id: requestHash(requestID)}
	if d.refusedIDs[v.id%retrySlots].CompareAndSwap(v.id, 0) {
		d.Retried.Add(1)
	} else {
		d.Received.Add(1)
	}
	v.enter(now)
	return v
}

func (v *writeVisit) enter(now int64) {
	v.here = true
	v.demand.InFlight.Add(1)
	v.slot = v.demand.track(now)
}

func (v *writeVisit) leave() {
	v.here = false
	v.demand.InFlight.Add(-1)
	if v.slot >= 0 {
		v.demand.waiting[v.slot].Store(0)
	}
}

// track takes a free slot for an arrival time. With every probed slot taken
// the write stays untracked: the tracked ones arrived before it.
func (d *WriteDemand) track(now int64) int {
	start := d.waitSeq.Add(1)
	for i := uint32(0); i < waitProbes; i++ {
		slot := int((start + i) % waitSlots)
		if d.waiting[slot].CompareAndSwap(0, now) {
			return slot
		}
	}
	return -1
}

// forward: the request leaves for another holder; it did not arrive here.
func (v *writeVisit) forward() {
	if !v.here {
		return
	}
	v.leave()
	v.demand.Forwarded.Add(1)
}

// comeBack: the forward failed and this vnode attempts the write again.
func (v *writeVisit) comeBack(now int64) {
	if v.here {
		return
	}
	v.demand.Forwarded.Add(-1)
	v.enter(now)
}

// finish records how a write served here ended. The outcome of a forwarded
// request belongs to the vnode that answered it.
func (v *writeVisit) finish(err error) {
	if !v.here {
		return
	}
	v.leave()
	if err == nil {
		v.demand.Acknowledged.Add(1)
		return
	}
	v.demand.Refused[refusalKind(err)].Add(1)
	if cutoverRefusal(err) {
		v.demand.refusedIDs[v.id%retrySlots].Store(v.id)
	}
}

// Arrived is the number of writes that reached this vnode as their holder.
func (d *WriteDemand) Arrived() int64 {
	return max(0, d.Received.Load()-d.Forwarded.Load())
}

// OldestWait is the age of the oldest tracked in-flight write. It is exact
// while at most waitSlots writes are in flight. Beyond that the first to
// arrive keep their slots, so a store that stays blocked is still seen; a
// write that could not take a slot is not, which makes it a lower bound.
func (d *WriteDemand) OldestWait(now time.Time) time.Duration {
	oldest := int64(0)
	for i := range d.waiting {
		if at := d.waiting[i].Load(); at != 0 && (oldest == 0 || at < oldest) {
			oldest = at
		}
	}
	if oldest == 0 {
		return 0
	}
	return max(0, now.Sub(time.Unix(0, oldest)))
}

// DemandSample is a read of WriteDemand: counters are cumulative, InFlight
// and OldestWait are gauges.
type DemandSample struct {
	Arrived, Received, Forwarded, Retried int64
	InFlight                              int64
	OldestWait                            time.Duration
	Applied, Acknowledged, Refused        int64
	RefusedBy                             map[string]int64
}

// Sample reads the counters. They are not read under one lock, so two of
// them may differ by the writes that moved in between.
func (d *WriteDemand) Sample(now time.Time) DemandSample {
	s := DemandSample{
		Arrived: d.Arrived(), Received: d.Received.Load(), Forwarded: d.Forwarded.Load(), Retried: d.Retried.Load(),
		InFlight: max(0, d.InFlight.Load()), OldestWait: d.OldestWait(now),
		Applied: d.Applied.Load(), Acknowledged: d.Acknowledged.Load(),
		RefusedBy: make(map[string]int64, refusalKinds),
	}
	for kind, name := range refusalNames {
		n := d.Refused[kind].Load()
		s.RefusedBy[name] = n
		s.Refused += n
	}
	return s
}
