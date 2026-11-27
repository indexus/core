package summary

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// A write marks its boundary without waiting for Mu, however long a flush
// or a fold holds it; the next flush queues the edge as stale, once per
// boundary however many writes marked it.
func TestWriteMarkDoesNotWaitForTheTreeLock(t *testing.T) {
	tr := &Tree{}
	key := tree.Key{Collection: "aaaaaaaaaaaaaaaa", Location: "AB"}
	tr.Mu.Lock()
	done := make(chan struct{})
	go func() {
		for range 1000 {
			tr.markDirty(key)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a write mark waited for the tree lock")
	}
	if len(tr.pendingEdges) != 0 {
		t.Fatal("the mark queued an edge without the lock")
	}
	tr.drainDirtyLocked()
	edge := tr.Edges[key]
	_, queued := tr.pendingEdges[key]
	stale := edge != nil && edge.Stale
	// Nothing left for the timers this armed: the tree has no store.
	tr.pendingEdges, tr.Edges = nil, nil
	tr.Mu.Unlock()
	tr.dirtyMu.Lock()
	marksLeft := len(tr.dirty)
	tr.dirtyMu.Unlock()
	if !queued || !stale || marksLeft != 0 {
		t.Fatalf("after the drain: queued %v, stale %v, %d marks left", queued, stale, marksLeft)
	}
}

// A flush can drain the dirty set before its one-second timer runs. That
// callback must neither flush the next wave early nor replace its timer.
func TestDrainedDirtyTimerCannotFlushTheNextWave(t *testing.T) {
	tr := &Tree{pushing: true}
	first := tree.Key{Collection: "aaaaaaaaaaaaaaaa", Location: "AB"}
	second := tree.Key{Collection: "aaaaaaaaaaaaaaaa", Location: "AC"}
	tr.markDirty(first)
	tr.dirtyMu.Lock()
	retired := tr.dirtyTimer
	tr.dirtyMu.Unlock()
	tr.Mu.Lock()
	tr.drainDirtyLocked()
	tr.StopFlushLocked()
	tr.Mu.Unlock()
	tr.dirtyMu.Lock()
	if tr.dirtyTimer != nil {
		t.Fatal("draining dirty left its timer armed")
	}
	tr.dirtyMu.Unlock()

	tr.markDirty(second)
	tr.dirtyMu.Lock()
	current := tr.dirtyTimer
	tr.dirtyMu.Unlock()
	deliver := func(timer *time.Timer) {
		tr.dirtyMu.Lock()
		due := tr.takeDirtyTimerLocked(timer)
		tr.dirtyMu.Unlock()
		if due {
			tr.sendEdge()
		}
	}
	deliver(retired) // deterministically deliver the expired old callback
	tr.pushMu.Lock()
	pushed := tr.pushAgain
	tr.pushMu.Unlock()
	tr.dirtyMu.Lock()
	preserved := tr.dirtyTimer == current && len(tr.dirty) == 1
	tr.dirtyMu.Unlock()
	if pushed || !preserved {
		t.Fatalf("retired callback flushed next wave: pushed=%v preserved=%v", pushed, preserved)
	}
	deliver(current)
	tr.pushMu.Lock()
	pushed = tr.pushAgain
	tr.pushMu.Unlock()
	if !pushed {
		t.Fatal("current dirty timer did not request its flush")
	}
	tr.Mu.Lock()
	tr.drainDirtyLocked()
	tr.StopFlushLocked()
	tr.Mu.Unlock()
}
