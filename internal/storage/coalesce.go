package storage

import (
	"bytes"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"slices"
)

// CoalesceLocalRights prepares the whole geometric plan without the disk's
// writer lock. Two actual sibling rights become one fresh local right; no
// receipt history or global membership validation is involved. Only one short
// metadata commit is attempted. Concurrent control progress makes maintenance
// retry on its next tick, never blocks a handoff waiting for convergence.
//
// Steps, in order:
//  1. read the control state and its version;
//  2. queue the native rights that no unfinished transfer of this disk pins;
//  3. merge sibling rights until none is left, a merged right being queued
//     to merge again with its own sibling;
//  4. commit the merged permissions, unless the control state has moved.
func (s *Store) CoalesceLocalRights() error {
	var state CausalState
	var expected []byte
	var self causal.Owner
	if e := s.view(func(tx *Tx) error {
		var e error
		state, expected, e = readCausalControlVersion(tx)
		expected = bytes.Clone(expected)
		self = causal.Owner{VNode: s.node, Storage: storageID(tx)}
		return e
	}); e != nil {
		return e
	}
	plan := newCoalescePlan(state, self)
	for cursor := 0; cursor < len(plan.queue); cursor++ {
		if e := plan.mergeSibling(plan.queue[cursor], s.cluster, self); e != nil {
			return e
		}
	}
	if !plan.changed {
		return nil
	}
	return s.updateCausal(func(tx *Tx, current CausalState) error {
		if !bytes.Equal(controlVersion(tx), expected) {
			return ErrNotReady
		}
		current.Permissions = plan.permissions
		return writeCausal(tx, current)
	})
}

// coalescePlan is the merge of sibling rights being prepared: the rights
// still available by region, the queue of rights to try (merged rights are
// appended to it) and the permissions after the merges done so far.
type coalescePlan struct {
	available   map[tree.Region]causal.Right
	queue       []causal.Right
	permissions causal.LocalPermissions
	changed     bool
}

// newCoalescePlan queues, in region order, the native rights of the state. A
// right that is the input of a transfer this disk is still the source of is
// pinned: it stays out of the plan.
func newCoalescePlan(state CausalState, self causal.Owner) *coalescePlan {
	pinned := map[causal.Digest]bool{}
	for _, t := range state.Transfers {
		if t.Cession.Source.Owner == self && t.Phase < causal.TransferInstalled {
			pinned[t.Cession.Input.ID] = true
		}
	}
	plan := &coalescePlan{
		available:   map[tree.Region]causal.Right{},
		queue:       make([]causal.Right, 0, len(state.Permissions.Native)),
		permissions: state.Permissions,
	}
	for _, r := range state.Permissions.Native {
		if !pinned[r.ID] {
			plan.available[r.Region] = r
			plan.queue = append(plan.queue, r)
		}
	}
	slices.SortFunc(plan.queue, func(a, b causal.Right) int {
		if c := bytes.Compare(a.Region.Mask[:], b.Region.Mask[:]); c != 0 {
			return c
		}
		return bytes.Compare(a.Region.Value[:], b.Region.Value[:])
	})
	return plan
}

// mergeSibling merges r with the first of its siblings that is available,
// and queues the merged right. A right already merged away, or without an
// available sibling, is left as it is.
func (p *coalescePlan) mergeSibling(r causal.Right, cluster string, self causal.Owner) error {
	if current, ok := p.available[r.Region]; !ok || current.ID != r.ID {
		return nil
	}
	sibling, other, ok := p.availableSibling(r.Region)
	if !ok {
		return nil
	}
	permissions, merged, e := p.permissions.Coalesce(cluster, r.ID, other.ID, self)
	if e != nil {
		return e
	}
	p.permissions = permissions
	delete(p.available, r.Region)
	delete(p.available, sibling)
	p.available[merged.Region] = merged
	p.queue = append(p.queue, merged)
	p.changed = true
	return nil
}

// availableSibling is the first available right whose region differs from
// region by one bit of its mask, taking the bits from the most significant.
func (p *coalescePlan) availableSibling(region tree.Region) (tree.Region, causal.Right, bool) {
	for i, mask := range region.Mask {
		for bit := byte(128); bit != 0; bit >>= 1 {
			if mask&bit == 0 {
				continue
			}
			sibling := region
			sibling.Value[i] ^= bit
			if other, ok := p.available[sibling]; ok {
				return sibling, other, true
			}
		}
	}
	return tree.Region{}, causal.Right{}, false
}
