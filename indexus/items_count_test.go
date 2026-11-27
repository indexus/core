package indexus

import (
	"testing"
	"time"
)

// Under sustained writes the advisory count still refreshes every
// itemsCountMaxAge, so an items-driven scale-up can fire during the load.
func TestItemsCountRefreshesUnderSustainedWrites(t *testing.T) {
	t.Parallel()
	n := &Node{mesh: &meshRuntime{}}
	now := time.Now()
	n.mesh.lastWrite.Store(now.UnixNano()) // writing right now: not quiet
	if !n.countDue(now) {
		t.Fatal("first count must run")
	}
	n.lastCountAt.Store(now.Add(-5 * time.Second).UnixNano())
	if n.countDue(now) {
		t.Fatal("a fresh count must wait while writes are hot")
	}
	n.lastCountAt.Store(now.Add(-itemsCountMaxAge).UnixNano())
	if !n.countDue(now) {
		t.Fatal("an old count must refresh even while writes are hot")
	}
	n.mesh.lastWrite.Store(now.Add(-2 * time.Second).UnixNano())
	n.lastCountAt.Store(now.Add(-5 * time.Second).UnixNano())
	if !n.countDue(now) {
		t.Fatal("quiet writes must allow a count")
	}
}
