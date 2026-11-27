package summary

import (
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type localReadiness struct {
	routing  causal.Digest
	revision uint64
	ready    bool
}

func (t *Tree) readinessKey() (localReadiness, bool) {
	routing, err := t.service.Store.CausalRouting()
	if err != nil {
		return localReadiness{}, false
	}
	return localReadiness{routing: routing.RoutingDigest(), revision: t.service.Store.LocalTransferRevision()}, true
}
func sameReadinessKey(a, b localReadiness) bool {
	return a.routing == b.routing && a.revision == b.revision
}

// LocalReady reports whether this disk's current native authority is exactly
// its advisory XOR cell. Off-placement records and retained source backups are
// real data, but the placement-derived summary cannot account for them alone.
// This gates only an accelerator; it never authorizes or delays a write.
func (t *Tree) LocalReady() bool {
	key, ok := t.readinessKey()
	if !ok {
		return false
	}
	if cached := t.readiness.Load(); cached != nil && sameReadinessKey(*cached, key) {
		return cached.ready
	}
	t.readinessMu.Lock()
	defer t.readinessMu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		key, ok = t.readinessKey()
		if !ok {
			return false
		}
		if cached := t.readiness.Load(); cached != nil && sameReadinessKey(*cached, key) {
			return cached.ready
		}
		key.ready = t.computeLocalReady()
		after, ok := t.readinessKey()
		if !ok {
			return false
		}
		if sameReadinessKey(key, after) {
			t.readiness.Store(&key)
			return key.ready
		}
	}
	return false // changing control is unfinished accelerator work
}

func (t *Tree) computeLocalReady() bool {
	state, err := t.service.Store.CausalState()
	if err != nil {
		return false
	}
	membership, err := state.MembershipReplica()
	if err != nil {
		return false
	}
	cell, err := tree.Cell(membership.Current(), t.name)
	if err != nil {
		return false
	}
	self := t.service.Self.Owner
	var coverage []tree.Region
	for _, right := range state.Permissions.Native {
		part, ok := right.Region.Intersect(cell)
		if !ok || part != right.Region || right.Owner != self {
			return false
		}
		coverage = append(coverage, right.Region)
	}
	for _, op := range state.Transfers {
		if op.Phase < causal.TransferInstalled {
			return false
		}
	}
	return tree.CoversRegions(cell, coverage)
}

// incompleteBelowLocked follows the summary tree, rather than requiring peers'
// derived caches to become ready simultaneously. A complete leaf can recover
// first, then its complete value repairs its ancestors on subsequent polls.
func (t *Tree) incompleteBelowLocked(collection, location string) bool {
	for _, key := range t.incomingBelowLocked(collection, location) {
		if t.incoming[key].incomplete {
			return true
		}
	}
	return false
}

func (t *Tree) peersAvailableLocked() bool {
	for _, unavailable := range t.peerUnavailable {
		if unavailable {
			return false
		}
	}
	return true
}

// ReadyFor is conservative about current placement and known unavailable
// contributors. Ordinary summaries remain asynchronous, not linearizable.
func (t *Tree) ReadyFor(collection, location string) bool {
	if !t.LocalReady() {
		return false
	}
	t.Mu.Lock()
	defer t.Mu.Unlock()
	return t.peersAvailableLocked() && !t.incompleteBelowLocked(collection, location)
}

func (t *Tree) exportIncompleteLocked(collection, location string) bool {
	return !t.LocalReady() || !t.peersAvailableLocked() || t.incompleteBelowLocked(collection, location)
}

// PollStatus precedes ExportPage in the same response. Fresh exported values
// are allocated after its revision; a delayed pre-invalidation push is older.
func (t *Tree) PollStatus() *mesh.SummaryStatus {
	unavailable := !t.LocalReady()
	holder, err := t.service.Store.Endpoint(t.service.Self.Host)
	if err != nil {
		unavailable = true
		holder = t.service.Self
	}
	t.Mu.Lock()
	defer t.Mu.Unlock()
	t.revision++
	return &mesh.SummaryStatus{Holder: holder, Epoch: t.epoch, Revision: t.revision, Unavailable: unavailable}
}

func newerSummaryStatus(old, next mesh.SummaryStatus) bool {
	return old.Holder.Owner != next.Holder.Owner || next.Epoch > old.Epoch || next.Epoch == old.Epoch && next.Revision > old.Revision
}
func (t *Tree) olderThanPollLocked(in mesh.Contribution) bool {
	floor, ok := t.peerFloor[in.Holder.Host]
	return ok && floor.Holder.Owner == in.Holder.Owner && (in.Epoch < floor.Epoch || in.Epoch == floor.Epoch && in.Revision <= floor.Revision)
}
