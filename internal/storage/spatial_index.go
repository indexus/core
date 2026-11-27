package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// This is a derived, persistent-in-memory search tree, not another authority
// index. Nodes are immutable: a reader retains its root with one DB snapshot.
// Hashed priorities bound expected search depth; AND/OR summaries prune arbitrary
// masked cubes, including non-prefix XOR cells. Arbitrary masks can still
// require linear visits; no universal sublinear bound is claimed. Handoff
// grains refine leading bits to avoid creating new scattered stripes.
// No payload enters this index.
type spatialNode struct {
	zone        tree.ZoneKey
	priority    uint64
	left, right *spatialNode
	and, or     tree.ID
	count       int // zones in this subtree: the rank of any zone in O(depth)
}
type spatialState struct{ root *spatialNode }
type spatialChange struct {
	zone    tree.ZoneKey
	present bool
}
type spatialCatalog struct {
	mu                sync.Mutex
	state             atomic.Pointer[spatialState]
	building          bool
	done              chan struct{}
	generation        uint64
	revision          uint64 // shape only; metadata and updates within a zone do not change it
	pending           map[tree.ZoneKey]bool
	buildRows, visits atomic.Uint64
}

func spatialCompare(a, b tree.ZoneKey) int {
	if n := bytes.Compare(a.ID[:], b.ID[:]); n != 0 {
		return n
	}
	return a.Compare(b)
}
func spatialPull(n *spatialNode) {
	n.and, n.or, n.count = n.zone.ID, n.zone.ID, 1
	for _, child := range []*spatialNode{n.left, n.right} {
		if child != nil {
			n.count += child.count
			for i := range n.and {
				n.and[i] &= child.and[i]
				n.or[i] |= child.or[i]
			}
		}
	}
}
func spatialEarlier(a, b *spatialNode) bool {
	return a.priority < b.priority || a.priority == b.priority && spatialCompare(a.zone, b.zone) < 0
}
func spatialMerge(a, b *spatialNode) *spatialNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if spatialEarlier(a, b) {
		n := *a
		n.right = spatialMerge(n.right, b)
		spatialPull(&n)
		return &n
	}
	n := *b
	n.left = spatialMerge(a, n.left)
	spatialPull(&n)
	return &n
}
func spatialSet(root *spatialNode, z tree.ZoneKey, present bool) *spatialNode {
	if root == nil {
		if !present {
			return nil
		}
		sum := sha256.Sum256([]byte(z.Collection + "\x00" + z.Location))
		return &spatialNode{zone: z, priority: binary.BigEndian.Uint64(sum[:8]), and: z.ID, or: z.ID, count: 1}
	}
	cmp := spatialCompare(z, root.zone)
	if cmp == 0 {
		if present {
			return root
		}
		return spatialMerge(root.left, root.right)
	}
	n := *root
	if cmp < 0 {
		n.left = spatialSet(root.left, z, present)
		if n.left == root.left {
			return root
		}
		if n.left != nil && spatialEarlier(n.left, &n) {
			top := *n.left
			n.left = top.right
			spatialPull(&n)
			top.right = &n
			spatialPull(&top)
			return &top
		}
	} else {
		n.right = spatialSet(root.right, z, present)
		if n.right == root.right {
			return root
		}
		if n.right != nil && spatialEarlier(n.right, &n) {
			top := *n.right
			n.right = top.left
			spatialPull(&n)
			top.left = &n
			spatialPull(&top)
			return &top
		}
	}
	spatialPull(&n)
	return &n
}

// spatialCount is how many zones the index holds.
func spatialCount(root *spatialNode) int {
	if root == nil {
		return 0
	}
	return root.count
}

// spatialAt is the zone of rank i (from 0) in the order of the index, which
// is the order of zone ids: ranks spread evenly are zones spread evenly over
// the address space.
func spatialAt(root *spatialNode, i int) (tree.ZoneKey, bool) {
	for root != nil {
		left := spatialCount(root.left)
		switch {
		case i < left:
			root = root.left
		case i == left:
			return root.zone, true
		default:
			i, root = i-left-1, root.right
		}
	}
	return tree.ZoneKey{}, false
}

func spatialHas(root *spatialNode, z tree.ZoneKey) bool {
	for root != nil {
		switch cmp := spatialCompare(z, root.zone); {
		case cmp == 0:
			return true
		case cmp < 0:
			root = root.left
		default:
			root = root.right
		}
	}
	return false
}
func spatialPossible(n *spatialNode, r tree.Region) bool {
	for i, mask := range r.Mask {
		if r.Value[i]&mask&^n.or[i] != 0 || ^r.Value[i]&mask&n.and[i] != 0 {
			return false
		}
	}
	return true
}

// spatialPage counts actual metadata nodes examined, not just returned matches.
func spatialPage(root *spatialNode, r tree.Region, after *tree.ZoneKey, limit int) ([]tree.ZoneKey, uint64) {
	var keys []tree.ZoneKey
	var visited uint64
	high := r.Value
	for i := range high {
		high[i] |= ^r.Mask[i]
	}
	var walk func(*spatialNode)
	walk = func(n *spatialNode) {
		if n == nil || len(keys) >= limit {
			return
		}
		visited++
		if !spatialPossible(n, r) {
			return
		}
		if after != nil && spatialCompare(n.zone, *after) <= 0 {
			walk(n.right)
			return
		}
		if bytes.Compare(n.zone.ID[:], r.Value[:]) < 0 {
			walk(n.right)
			return
		}
		if bytes.Compare(n.zone.ID[:], high[:]) > 0 {
			walk(n.left)
			return
		}
		walk(n.left)
		if len(keys) >= limit {
			return
		}
		if r.Contains(n.zone.ID) {
			keys = append(keys, n.zone)
		}
		walk(n.right)
	}
	walk(root)
	return keys, visited
}

// Only zone membership changes need a coupled index/DB publication. Ordinary
// updates of an existing zone and all metadata-only commits take no index lock.
func spatialChanges(tx *Tx) []spatialChange {
	changes := make([]spatialChange, 0, len(tx.records))
	for _, p := range tx.records {
		present := false
		for _, record := range p.rows {
			if record != nil {
				present = true
				break
			}
		}
		changes = append(changes, spatialChange{p.zone, present})
	}
	return changes
}
func (s *Store) commitSpatial(tx *Tx, err error) error {
	if err != nil {
		return tx.commit(err)
	}
	changes := spatialChanges(tx)
	if len(changes) == 0 {
		return tx.commit(nil)
	}
	state := s.spatial.state.Load()
	changed := state == nil
	if !changed {
		for _, c := range changes {
			if spatialHas(state.root, c.zone) != c.present {
				changed = true
				break
			}
		}
	}
	if !changed {
		return tx.commit(nil)
	}
	s.spatial.mu.Lock()
	defer s.spatial.mu.Unlock()
	if err = tx.commit(nil); err != nil {
		s.invalidateSpatialLocked()
		return err
	}
	if state = s.spatial.state.Load(); state != nil {
		root := state.root
		for _, c := range changes {
			root = spatialSet(root, c.zone, c.present)
		}
		s.spatial.state.Store(&spatialState{root})
		s.spatial.revision++
	} else if s.spatial.building {
		if s.spatial.pending == nil {
			s.spatial.pending = make(map[tree.ZoneKey]bool)
		}
		// The builder needs only the latest presence of each zone, not a
		// history of all writes that reached it while headers were scanned.
		for _, change := range changes {
			s.spatial.pending[change.zone] = change.present
		}
	}
	return nil
}
func (s *Store) invalidateSpatialLocked() {
	s.spatial.state.Store(nil)
	s.spatial.generation++
	s.spatial.revision++
}

// ensureSpatial returns once the spatial index is built. One caller builds
// it from a snapshot, outside the index lock; the others wait for it. Zone
// changes committed during the scan are queued in pending and applied before
// the index is published; an invalidation during the build starts it again.
func (s *Store) ensureSpatial(ctx context.Context) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		snapshot, generation, wait, ready := s.claimSpatialBuild()
		if ready {
			return nil
		}
		if wait != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wait:
			}
			continue
		}
		root, err := s.scanSpatial(ctx, snapshot)
		published, err := s.publishSpatial(ctx, generation, root, err)
		if err != nil || published {
			return err
		}
	}
}

// claimSpatialBuild says the index is ready, or gives the channel of the
// build in progress, or makes the caller the builder of this generation.
func (s *Store) claimSpatialBuild() (snapshot *zonelog.Snapshot, generation uint64, wait <-chan struct{}, ready bool) {
	if s.spatial.state.Load() != nil {
		return nil, 0, nil, true
	}
	s.spatial.mu.Lock()
	defer s.spatial.mu.Unlock()
	if s.spatial.state.Load() != nil {
		return nil, 0, nil, true
	}
	if s.spatial.building {
		return nil, 0, s.spatial.done, false
	}
	s.spatial.building = true
	s.spatial.done = make(chan struct{})
	s.spatial.pending = nil
	return s.db.Snapshot(), s.spatial.generation, nil, false
}

// scanSpatial builds the tree of the zones that have rows in the snapshot,
// and closes the snapshot.
func (s *Store) scanSpatial(ctx context.Context, snapshot *zonelog.Snapshot) (root *spatialNode, err error) {
	defer snapshot.Close()
	var rows uint64
	defer func() { s.spatial.buildRows.Add(rows) }()
	err = s.transaction(snapshot, false).run(func(tx *Tx) error {
		for _, collection := range tx.reader.Collections() {
			var e error
			if root, e = spatialCollection(ctx, tx, collection, root, &rows); e != nil {
				return e
			}
		}
		return nil
	})
	return root, err
}

// spatialCollection adds to root every zone of one collection that has rows.
func spatialCollection(ctx context.Context, tx *Tx, collection string, root *spatialNode, rows *uint64) (*spatialNode, error) {
	last := ""
	var failed error
	skip := func(zonelog.Record) bool { return false }
	e := scanPaged(tx.reader, collection, "@", nil, nil, skip, func(rec zonelog.Record, _ int) bool {
		*rows++
		if failed = ctx.Err(); failed != nil {
			return false
		}
		if rec.Location == last {
			return true
		}
		var z tree.ZoneKey
		if z, failed = tree.NewZoneKey(collection, rec.Location); failed != nil {
			return false
		}
		root, last = spatialSet(root, z, true), rec.Location
		return true
	})
	if e == nil {
		e = failed
	}
	return root, e
}

// publishSpatial applies the changes queued during the scan until none is
// left, then publishes the index. It reports false without an error when the
// index was invalidated meanwhile: the build starts again.
func (s *Store) publishSpatial(ctx context.Context, generation uint64, root *spatialNode, err error) (bool, error) {
	for {
		pending, outcome := s.settleSpatialBuild(generation, root, err)
		if outcome != spatialBuilding {
			return outcome == spatialPublished, err
		}
		for zone, present := range pending {
			root = spatialSet(root, zone, present)
		}
		if e := ctx.Err(); e != nil {
			err = e
		}
	}
}

type spatialOutcome int

const (
	spatialBuilding  spatialOutcome = iota // changes were queued: apply them
	spatialPublished                       // root is the index
	spatialAbandoned                       // error, or invalidated meanwhile
)

// settleSpatialBuild ends the build, by publishing root or by abandoning it,
// unless changes were queued during the scan: it then hands them to the
// builder, which stays the builder.
func (s *Store) settleSpatialBuild(generation uint64, root *spatialNode, err error) (map[tree.ZoneKey]bool, spatialOutcome) {
	s.spatial.mu.Lock()
	defer s.spatial.mu.Unlock()
	pending := s.spatial.pending
	s.spatial.pending = nil
	outcome := spatialPublished
	switch {
	case err != nil || generation != s.spatial.generation:
		outcome = spatialAbandoned
	case len(pending) > 0:
		return pending, spatialBuilding
	default:
		s.spatial.state.Store(&spatialState{root})
		s.spatial.revision++
	}
	s.spatial.building = false
	close(s.spatial.done)
	return nil, outcome
}

// spatialView captures the immutable derived keys with the exact MVCC data
// snapshot. No scan or payload read takes place while the index lock is held.
func (s *Store) spatialView(ctx context.Context, visit func(*Tx, *spatialNode) error) error {
	for {
		if e := s.ensureSpatial(ctx); e != nil {
			return e
		}
		s.spatial.mu.Lock()
		state := s.spatial.state.Load()
		if state == nil {
			s.spatial.mu.Unlock()
			continue
		}
		snapshot := s.db.Snapshot()
		s.spatial.mu.Unlock()
		defer snapshot.Close()
		return s.transaction(snapshot, false).run(func(tx *Tx) error { return visit(tx, state.root) })
	}
}
func (s *Store) spatialZonePage(ctx context.Context, region tree.Region, after *tree.ZoneKey, limit int, want int, visit func(*Tx, tree.ZoneKey, []*row) error) (*tree.ZoneKey, bool, error) {
	var cursor *tree.ZoneKey
	done := false
	err := s.spatialView(ctx, func(tx *Tx, root *spatialNode) error {
		keys, visited := spatialPage(root, region, after, limit+1)
		s.spatial.visits.Add(visited)
		done = len(keys) <= limit
		if !done {
			keys = keys[:limit]
		}
		cached, e := s.causalSnapshot(tx)
		if e != nil {
			return e
		}
		for _, z := range keys {
			if e = ctx.Err(); e != nil {
				return e
			}
			key := z
			cursor = &key
			if e = s.writableIn(tx, cached, z); e != nil {
				if errors.Is(e, ErrNotOwner) || errors.Is(e, ErrFrozen) || errors.Is(e, ErrNotReady) {
					continue
				}
				return e
			}
			var rows []*row
			if want != rowsNone {
				rows, e = rowsOf(tx, z, want == rowsWithPayloads)
				if e != nil {
					return e
				}
			}
			if e = visit(tx, z, rows); e != nil {
				return e
			}
		}
		return nil
	})
	return cursor, done, err
}

func (s *Store) scanSpatialRegion(ctx context.Context, region tree.Region, want int, visit func(*Tx, tree.ZoneKey, []*row) error) error {
	var from *tree.ZoneKey
	for {
		next, done, e := s.spatialZonePage(ctx, region, from, 512, want, visit)
		if e != nil {
			return e
		}
		if done {
			return nil
		}
		from = next
	}
}
