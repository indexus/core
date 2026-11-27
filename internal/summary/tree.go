// Package summary keeps the derived zone summaries of a node (SUM-1, SUM-2):
// the sums of the zones it holds, read from its store and corrected by the
// contributions of children held elsewhere, the boundaries it pushes to the
// owners of their parents, and the polls that repair a lost push. A summary
// is a read accelerator: it never grants permission to write.
package summary

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Hooks are what the summaries ask of the node.
type Hooks struct {
	// LastWrite is when the node last saw a client write (unix nanoseconds).
	LastWrite func() int64
	// Quiet reports that no write arrived lately: a periodic inventory fold
	// waits for it.
	Quiet func() bool
	// RefreshView reloads the node's view when its topology changed.
	RefreshView func() error
	// Reconciled runs after each reconciliation that was not cancelled.
	Reconciled func()
}

// topology keeps advisory membership and physical placement from one
// durable snapshot together. It never grants permission to write.
type topology struct {
	view  *tree.View
	local bool
}

// Tree is the summary state of one vnode.
type Tree struct {
	service *mesh.Service
	name    string
	epoch   uint64
	hooks   Hooks
	// beforeAcceptLock is a test seam (SetBeforeAcceptLockForTest).
	beforeAcceptLock func(mesh.Contribution)
	// betweenLocalReads is a test seam (SetBetweenLocalReadsForTest).
	betweenLocalReads func()

	topology atomic.Pointer[topology]

	// ExportCacheBytes bounds the shared boundary inventory (default 1 MiB).
	// Set before use; zero disables retention. It stores keys, never totals.
	ExportCacheBytes int
	exportMu         sync.Mutex
	exportKeys       *exportKeys // guarded by exportMu, never needed by local writes

	// Mu guards the fields below it. Exported, with LastReconcile,
	// PollEvery, LastFold, Edges and Summaries, for the facade's tests.
	Mu sync.Mutex
	// RefreshMu serialises reconciliations, InventoryMu inventory folds.
	RefreshMu   ctxMutex
	InventoryMu ctxMutex
	pushMu      sync.Mutex
	// pushing / pushAgain coalesce concurrent sendEdge triggers into one
	// in-flight pass plus one follow-up, so a slow flush does not serialize a
	// stack of identical full-edge scans behind pushMu.
	pushing         bool
	pushAgain       bool
	Summaries       *foldMark
	incoming        map[tree.Key]summaryContribution
	peerUnavailable map[string]bool
	peerFloor       map[string]mesh.SummaryStatus
	readinessMu     sync.Mutex
	readiness       atomic.Pointer[localReadiness]
	// incomingSorted holds the locations of incoming per collection, sorted,
	// so that the children below a zone are a binary search, not a walk of
	// every contribution (incomingBelowLocked).
	// storeGen counts refreshLocked: rows that reached the store without a
	// local write (a bulk, an install) or a change of view. A flush drops
	// the zone sums it read before one and reads a newer version (zoneSums).
	storeGen       uint64
	incomingSorted map[string][]string
	incomingCount  int
	revision       uint64
	Edges          map[tree.Key]*Edge
	// dirty holds the boundaries written since the last flush. A write only
	// adds its boundary here, under dirtyMu, and never waits for Mu: the
	// mark is idempotent and the value is read from the store when the
	// flush sends it, so a late mark changes nothing that converges
	// (2026-11-20: writes waited seconds behind Mu held by a flush).
	dirtyMu    sync.Mutex
	dirty      map[tree.Key]struct{}
	dirtyTimer *time.Timer // guarded by dirtyMu; retired when a flush drains dirty
	// pendingCount is PendingEdges of Stats as last counted: /metrics does
	// not wait for Mu either.
	pendingCount atomic.Int64
	// pendingEdges are the keys of edges holding a value to send; a flush
	// walks them, not every boundary. One timer serves them all.
	pendingEdges     map[tree.Key]struct{}
	flushTimer       *time.Timer
	flushAt          time.Time
	lastDigest       causal.Digest
	LastReconcile    time.Time
	lastFoldDigest   causal.Digest
	LastFold         time.Time
	lastFoldRevision uint64
	// Only the most recently started fold may publish its completion.
	foldAttempt uint64
	// fullTicks keeps the boundary reconciliation running for a few ticks
	// after an authority change: peers merge the change at their own pace and
	// the first poll after it often sees their pre-change views.
	fullTicks int
	// PollEvery is the current boundary poll cadence; quietSince holds the
	// counters seen by the last poll (see activeLocked).
	PollEvery  time.Duration
	quietSince struct{ write, accepted, pushFailures int64 }
	// pollRound rotates the steady-state boundary poll over remote hosts.
	pollRound atomic.Uint64
	stats     stats
}

// New is the summary state of the vnode of service, whose summaries carry
// epoch: a new epoch for each incarnation of its store.
func New(service *mesh.Service, epoch uint64, hooks Hooks) *Tree {
	return &Tree{service: service, name: service.VNode.Name, epoch: epoch, hooks: hooks, ExportCacheBytes: 1 << 20, Edges: map[tree.Key]*Edge{}}
}

// View is the node's current view, nil before the first SetView.
func (t *Tree) View() *tree.View {
	if cached := t.topology.Load(); cached != nil {
		return cached.view
	}
	return nil
}

// ViewIs reports whether the cached view has this digest and placement.
func (t *Tree) ViewIs(digest causal.Digest, local bool) bool {
	cached := t.topology.Load()
	return cached != nil && causal.Digest(cached.view.Digest) == digest && cached.local == local
}

// SetView caches a view and whether this vnode's placement is on this disk.
// With repush, a changed view queues every owned boundary again: a topology
// change moves parents between vnodes, and the children, which hold the
// truth and did not change, would never push to the new parent owner. Polls
// remain the safety net for what this misses.
func (t *Tree) SetView(view *tree.View, local, repush bool) {
	cached := t.topology.Swap(&topology{view: view, local: local})
	if repush && (cached == nil || cached.view.Digest != view.Digest) {
		t.repush(view)
	}
}

// stats counts the derived-summary protocol's outcomes so an operator or a
// benchmark can tell a stalled repair from a slow one.
type stats struct {
	accepted, droppedNotOwner, droppedStaleHolder atomic.Int64
	// droppedDescription counts contributions whose description digest is
	// not the one this disk holds (DESC-4): a forged collision or a bug.
	droppedDescription        atomic.Int64
	pushBatches, pushFailures atomic.Int64
	pollCalls, pollFailures   atomic.Int64
	// pollChanged counts contributions a poll brought in that changed a
	// parent: repairs the push path did not deliver.
	pollChanged atomic.Int64
	// The boundary inventory of an export: built from the store, reused at
	// the same revision, reused by a later page of the same poll, and built
	// but too large to keep (ExportCacheBytes), which rebuilds at every page.
	exportKeysBuilt, exportKeysExtended, exportKeysReused, exportKeysContinued, exportKeysOversized atomic.Int64
	// flushes counts sendEdge passes; flushMaxMS is the slowest pass and
	// flushLastMS the latest, so a stalled push path shows as a large pass
	// rather than a mysteriously late root.
	flushes, flushMaxMS, flushLastMS atomic.Int64
	// valueMS and pushMS sum the time flushes spent valuing edges and
	// pushing them, and valued the edges valued: which of the two makes a
	// slow flush (2026-11-20: 10 to 48 s flushes of 20 000 to 35 000 edges).
	valueMS, pushMS, valued atomic.Int64
	// unchanged counts edges not sent because their owner holds the value.
	unchanged atomic.Int64
}

// Stats is a snapshot of the derived-summary protocol counters:
// contributions accepted, dropped because this vnode does not own the parent,
// dropped because the sender is no longer the child's owner, push batches sent
// and failed, reconciliation polls sent and failed.
type Stats struct {
	Accepted, DroppedNotOwner, DroppedStaleHolder int64
	PushBatches, PushFailures                     int64
	PollCalls, PollFailures, PollChanged          int64
	Flushes, FlushMaxMS, FlushLastMS              int64
	ValueMS, PushMS, Valued                       int64
	Unchanged                                     int64
	// Boundary inventories of exports: built from the store, extended by the zones added since, reused
	// at the same revision, reused by a later page of a poll, too large to keep.
	ExportKeysBuilt, ExportKeysExtended, ExportKeysReused, ExportKeysContinued, ExportKeysOversized int64
	// PendingEdges is the number of boundary edges holding a value not yet
	// acknowledged by their parent's owner.
	PendingEdges int
}

func (t *Tree) Stats() Stats {
	s := &t.stats
	out := Stats{
		Accepted: s.accepted.Load(), DroppedNotOwner: s.droppedNotOwner.Load(), DroppedStaleHolder: s.droppedStaleHolder.Load(),
		PushBatches: s.pushBatches.Load(), PushFailures: s.pushFailures.Load(),
		PollCalls: s.pollCalls.Load(), PollFailures: s.pollFailures.Load(), PollChanged: s.pollChanged.Load(),
		Flushes: s.flushes.Load(), FlushMaxMS: s.flushMaxMS.Load(), FlushLastMS: s.flushLastMS.Load(),
		ValueMS: s.valueMS.Load(), PushMS: s.pushMS.Load(), Valued: s.valued.Load(),
		Unchanged:       s.unchanged.Load(),
		ExportKeysBuilt: s.exportKeysBuilt.Load(), ExportKeysExtended: s.exportKeysExtended.Load(), ExportKeysReused: s.exportKeysReused.Load(),
		ExportKeysContinued: s.exportKeysContinued.Load(), ExportKeysOversized: s.exportKeysOversized.Load(),
	}
	if t.Mu.TryLock() {
		pending := int64(0)
		for _, edge := range t.Edges {
			if edge != nil && edge.Pending != nil {
				pending++
			}
		}
		t.Mu.Unlock()
		t.pendingCount.Store(pending)
	}
	out.PendingEdges = int(t.pendingCount.Load())
	return out
}

// Sizes is what the summaries keep in memory: the contributions of remote
// children and the boundaries this node pushes, and whether they are ready.
func (t *Tree) Sizes() (zones, boundaries int, ready bool) {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	return len(t.incoming), len(t.Edges), t.Summaries != nil && !t.LastFold.IsZero()
}
