package storage

import (
	"context"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// ZoneInventory is an immutable snapshot of canonical zone identities, including
// tombstone-only zones. It is a conservative inventory, not authority or sums.
// It pins no database view and owns no second copy of records or index nodes.
type ZoneInventory struct {
	root     *spatialNode
	revision uint64
}

// ZoneInventory reuses the catalog used by spatial reads and transfers. Cold
// construction reads record headers once; hot capture takes only the index lock.
func (s *Store) ZoneInventory(ctx context.Context) (ZoneInventory, error) {
	for {
		if err := s.ensureSpatial(ctx); err != nil {
			return ZoneInventory{}, err
		}
		s.spatial.mu.Lock()
		state := s.spatial.state.Load()
		revision := s.spatial.revision
		s.spatial.mu.Unlock()
		if state != nil {
			return ZoneInventory{state.root, revision}, nil
		}
	}
}

// Revision changes on zone creation, removal or catalog replacement, not on
// unrelated control commits. It is process-local: consumers discard it on reopen.
func (v ZoneInventory) Revision() uint64 { return v.revision }

// Each visits zone identities without reading values or calculating aggregates.
func (v ZoneInventory) Each(ctx context.Context, visit func(tree.ZoneKey) bool) error {
	stopped := false
	var walk func(*spatialNode)
	walk = func(n *spatialNode) {
		if n == nil || stopped || ctx.Err() != nil {
			return
		}
		walk(n.left)
		if stopped || ctx.Err() != nil {
			return
		}
		stopped = !visit(n.zone)
		if !stopped {
			walk(n.right)
		}
	}
	walk(v.root)
	return ctx.Err()
}

// Since visits the zones v holds and old does not, and reports whether v
// holds every zone of old: with a removed zone the caller starts over.
// Two inventories of one catalog share every subtree a commit did not
// touch; shared subtrees are skipped whole, so the walk costs the changes
// and their paths, not the catalog. Unrelated inventories cost both of them.
func (v ZoneInventory) Since(old ZoneInventory, visit func(tree.ZoneKey)) bool {
	was, is := spatialWalk{}.push(old.root), spatialWalk{}.push(v.root)
	for len(is) > 0 || len(was) > 0 {
		switch a, b := was.top(), is.top(); {
		case len(was) == 0 && b.whole:
			is = is.expand()
		case len(was) == 0:
			visit(b.node.zone)
			is = is.pop()
		case len(is) == 0:
			return false
		case a.whole && b.whole && a.node == b.node:
			was, is = was.pop(), is.pop()
		case a.whole && (!b.whole || a.node.count >= b.node.count):
			was = was.expand()
		case b.whole:
			is = is.expand()
		default:
			switch n := spatialCompare(a.node.zone, b.node.zone); {
			case n < 0:
				return false
			case n == 0:
				was, is = was.pop(), is.pop()
			default:
				visit(b.node.zone)
				is = is.pop()
			}
		}
	}
	return true
}

// spatialWalk is what an in-order walk still has to visit, next on top: a
// whole subtree, or the zone of one node.
type spatialWalk []spatialPart

type spatialPart struct {
	node  *spatialNode
	whole bool
}

func (w spatialWalk) top() spatialPart {
	if len(w) == 0 {
		return spatialPart{}
	}
	return w[len(w)-1]
}

func (w spatialWalk) pop() spatialWalk { return w[:len(w)-1] }

func (w spatialWalk) push(subtree *spatialNode) spatialWalk {
	if subtree == nil {
		return w
	}
	return append(w, spatialPart{subtree, true})
}

// expand replaces the subtree on top by its left subtree, its node, its
// right subtree.
func (w spatialWalk) expand() spatialWalk {
	sinceExpands.Add(1)
	n := w.top().node
	w = w.pop().push(n.right)
	w = append(w, spatialPart{n, false})
	return w.push(n.left)
}
