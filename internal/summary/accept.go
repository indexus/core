package summary

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// foldMark says which view the summaries were last made ready for. The
// sums themselves are the store's (summary_store.go); in memory there are
// only the contributions of remote children and the boundaries to push.
type foldMark struct {
	digest [32]byte
}

// Owned copies the remote contributions under Tree.Mu, then reads the store
// without it, as the /sets answer does: no disk read nor snapshot close under
// the lock that AcceptBatch and the flushes take.
func (t *Tree) Owned(collection, location string) *wire.Abelian {
	t.Mu.Lock()
	if !t.OwnsHere(collection, location) {
		t.Mu.Unlock()
		return wire.NewAbelian(0, nil)
	}
	incoming := t.snapshotIncomingLocked(collection, location)
	t.Mu.Unlock()
	value, _, err := t.valueWithIncoming(collection, location, nil, incoming)
	if err != nil {
		return wire.NewAbelian(0, nil)
	}
	return value
}
func (t *Tree) refreshZone(collection, location string) {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	t.queueAboveLocked(collection, location)
}

// OnLocalWrite queues a committed record using its current local placement cell.
// It needs neither a global view refresh nor the inventory/summary mutexes.
func (t *Tree) OnLocalWrite(record storage.Record, previous *storage.Record, cell tree.Region) {
	if record.Deleted && (previous == nil || previous.Deleted) {
		return
	}
	if key, ok := boundaryIn(cell, record.Collection, record.Location); ok {
		t.markDirty(key)
	}
}

// markDirty notes a boundary written since the last flush, without Mu.
func (t *Tree) markDirty(key tree.Key) {
	t.dirtyMu.Lock()
	defer t.dirtyMu.Unlock()
	if t.dirty == nil {
		t.dirty = map[tree.Key]struct{}{}
	}
	t.dirty[key] = struct{}{}
	if t.dirtyTimer == nil {
		var timer *time.Timer
		timer = time.AfterFunc(summaryRemoteWindow, func() {
			t.dirtyMu.Lock()
			due := t.takeDirtyTimerLocked(timer)
			t.dirtyMu.Unlock()
			if due {
				t.sendEdge()
			}
		})
		t.dirtyTimer = timer
	}
}

// takeDirtyTimerLocked ignores callbacks already retired by a concurrent flush.
// Stop alone cannot cancel an expired callback waiting for dirtyMu.
func (t *Tree) takeDirtyTimerLocked(timer *time.Timer) bool {
	if t.dirtyTimer != timer {
		return false
	}
	t.dirtyTimer = nil
	return true
}

// drainDirtyLocked queues the boundaries written since the last flush.
func (t *Tree) drainDirtyLocked() {
	t.dirtyMu.Lock()
	dirty := t.dirty
	t.dirty = nil
	if t.dirtyTimer != nil {
		t.dirtyTimer.Stop()
		t.dirtyTimer = nil
	}
	t.dirtyMu.Unlock()
	for key := range dirty {
		t.queueEdgeLocked(key)
	}
}
func (t *Tree) AcceptWire(in mesh.Contribution) error {
	_, err := t.acceptWireChanged(in)
	return err
}
func (t *Tree) acceptWireChanged(in mesh.Contribution) (bool, error) {
	changed, parent, err := t.acceptChild(in)
	if err == nil && changed {
		t.refreshZone(in.Collection, parent)
	}
	return changed, err
}
func (t *Tree) acceptChild(in mesh.Contribution) (bool, string, error) {
	parent := tree.BASE64.Parent(in.Location)
	if in.Collection == "" || parent == "" {
		return false, parent, nil
	}
	if !t.OwnsHere(in.Collection, parent) {
		t.stats.droppedNotOwner.Add(1)
		return false, parent, storage.ErrNotOwner
	}
	if in.Holder.Owner == t.service.Self.Owner {
		// A boundary moved to this node. Canonical local data, not an old wire
		// value, supplies the now-local subtree.
		_, err := t.Fold(context.Background(), true)
		return false, parent, err
	}
	if ok, err := t.checkDescription(in); err != nil || !ok {
		return false, parent, err
	}
	// This check only spares Tree.Mu for messages already stale. The one that
	// decides is repeated under Tree.Mu, which refreshLocked also holds: a
	// contribution is stored only while its holder is current, and evicting it
	// after a later change of holder stays refreshLocked's job.
	if !t.ChildHolderCurrent(in.Collection, in.Location, in.Holder) {
		t.stats.droppedStaleHolder.Add(1)
		return false, parent, nil
	}
	if t.beforeAcceptLock != nil {
		t.beforeAcceptLock(in)
	}
	t.Mu.Lock()
	defer t.Mu.Unlock()
	if !t.ChildHolderCurrent(in.Collection, in.Location, in.Holder) {
		t.stats.droppedStaleHolder.Add(1)
		return false, parent, nil
	}
	if t.olderThanPollLocked(in) {
		t.stats.droppedStaleHolder.Add(1)
		return false, parent, nil
	}
	key := tree.Key{Collection: in.Collection, Location: in.Location}
	value := contributionFromWire(in)
	if old, ok := t.incoming[key]; ok && !newerContribution(old, value) {
		return false, parent, nil
	}
	if t.incoming == nil {
		t.incoming = map[tree.Key]summaryContribution{}
	}
	if _, ok := t.incoming[key]; !ok {
		t.indexIncomingLocked(key, true)
	}
	t.incoming[key] = value
	t.stats.accepted.Add(1)
	return true, parent, nil
}
func (t *Tree) AcceptBatch(contributions []mesh.Contribution) error {
	dirty := map[tree.Key]struct{}{}
	notOwner := false
	for _, in := range contributions {
		changed, parent, err := t.acceptChild(in)
		if errors.Is(err, storage.ErrNotOwner) {
			notOwner = true
			continue
		}
		if err != nil {
			return err
		}
		if changed {
			dirty[tree.Key{Collection: in.Collection, Location: parent}] = struct{}{}
		}
	}
	for key := range dirty {
		t.refreshZone(key.Collection, key.Location)
	}
	if notOwner {
		return storage.ErrNotOwner
	}
	return nil
}

func (t *Tree) Export(collection string, locations []string, parentOwner string) []mesh.Contribution {
	out, _ := t.ExportPage(collection, locations, parentOwner, nil)
	return out
}

// exportSumParents bounds the parents whose children one export keeps: an
// unpaged export of every boundary must not hold the sums of them all.
const exportSumParents = 256

// ExportPage is Export for one page of a boundary poll: only
// keys after the cursor, at most page.Limit contributions, and whether more
// remain. A single response for all boundaries outgrew the 32 MiB body cap of
// the mesh transport (about 95 000 boundaries) and every poll failed.
func (t *Tree) ExportPage(collection string, locations []string, parentOwner string, page *mesh.SummaryPage) ([]mesh.Contribution, bool) {
	var keys boundaryRange
	var built *foldMark
	var packed bool
	var cursor tree.Key
	if page != nil {
		cursor = tree.Key{Collection: page.Collection, Location: page.Location}
	}
	if len(locations) == 0 {
		var inventory *exportKeys
		inventory, built = t.exportInventory(cursor != (tree.Key{}))
		keys = inventory.after(collection, cursor)
		packed = len(keys.groups) > 0
	} else {
		explicit := make([]tree.Key, 0, len(locations))
		for _, location := range locations {
			explicit = append(explicit, tree.Key{Collection: collection, Location: location})
		}
		slices.SortFunc(explicit, tree.Key.Compare)
		keys.keys = afterCursor(explicit, cursor)
	}
	if len(locations) == 0 && parentOwner != "" && parentOwner != t.name {
		// Only resolve parents after the cursor, outside Mu.
		view := t.View()
		var matches []tree.Key
		for key, ok := keys.next(); ok; key, ok = keys.next() {
			parent, err := tree.NewZoneKey(key.Collection, tree.BASE64.Parent(key.Location))
			if err != nil {
				continue
			}
			if owner, _ := view.OwnerName(parent.ID); owner == parentOwner {
				matches = append(matches, key)
			}
		}
		keys = boundaryRange{keys: matches}
	}
	// One version of the sums for the page, as a flush reads its edges: the
	// keys are sorted, so siblings follow each other and share one read of
	// their parent's children. A version per key read 65 sums to use one
	// (2026-11-26: 145 s of 813 s CPU during joins). Closed after Mu is
	// released: closing a snapshot can reclaim segments.
	sums := t.openZoneSums()
	defer sums.close()
	t.Mu.Lock()
	defer t.Mu.Unlock()
	fold := t.Summaries
	if fold == nil || fold.digest != t.View().Digest || len(locations) == 0 && fold != built {
		return nil, false
	}
	var out []mesh.Contribution
	for key, ok := keys.next(); ok; key, ok = keys.next() {
		if page != nil && page.Limit > 0 && len(out) == page.Limit {
			return out, true
		}
		if !t.OwnsHere(key.Collection, key.Location) {
			continue
		}
		if len(sums.children) >= exportSumParents {
			sums.releaseChildren()
		}
		if packed {
			// An exported contribution can outlive this snapshot (e.g. a local
			// peer). Two small strings must not keep an entire old arena alive.
			key.Collection, key.Location = strings.Clone(key.Collection), strings.Clone(key.Location)
		}
		if wire, err := t.wireWithLocked(key, sums); err == nil && (wire.Count > 0 || wire.Incomplete || len(locations) > 0) {
			out = append(out, wire)
		}
	}
	return out, false
}

func (t *Tree) repush(current *tree.View) {
	if current == nil {
		return
	}
	t.Mu.Lock()
	defer t.Mu.Unlock()
	t.Summaries = &foldMark{digest: current.Digest}
	t.refreshLocked(current)
}

func (t *Tree) FoldLocal(ctx context.Context, view *tree.View) (bool, error) {
	// The store keeps the sums: nothing to read, every boundary to push.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	t.Mu.Lock()
	defer t.Mu.Unlock()
	if t.View().Digest != view.Digest {
		return false, nil
	}
	t.Summaries = &foldMark{digest: view.Digest}
	t.revision++
	t.refreshLocked(view)
	return true, nil
}

// SetBeforeAcceptLockForTest runs f between the unlocked holder check of a
// contribution and Tree.Mu, so a test can move the holder in that window.
// Set it before contributions arrive; nil removes it.
func (t *Tree) SetBeforeAcceptLockForTest(f func(mesh.Contribution)) { t.beforeAcceptLock = f }

// SetBetweenLocalReadsForTest runs f in a zone's total between reading its
// local rows and reading the rows below its remote children, so a test can
// commit to the store in that window. nil removes it.
func (t *Tree) SetBetweenLocalReadsForTest(f func()) { t.betweenLocalReads = f }
