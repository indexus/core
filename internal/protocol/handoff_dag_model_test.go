// Historical activation-component model. The runtime reserves whole validated
// plans and activates installed pieces through local tickets; it does not build
// this graph. These types remain only for the original geometric model tests.
package protocol

import (
	"fmt"
	"sort"

	"github.com/indexus/go-indexus-core/internal/tree"
)

type ComponentID string

// ActivationComponent is one membership mutation: many zone Moves, one activation.
type ActivationComponent struct {
	ID      ComponentID
	OldView *tree.View
	NewView *tree.View
	Delta   tree.Delta
	Moves   map[tree.Key]*Move
}

func NewComponent(id ComponentID, old *tree.View, delta tree.Delta) (*ActivationComponent, error) {
	next, err := tree.Apply(old, delta)
	if err != nil {
		return nil, err
	}
	return &ActivationComponent{
		ID:      id,
		OldView: old,
		NewView: next,
		Delta:   delta,
		Moves:   make(map[tree.Key]*Move),
	}, nil
}

func (c *ActivationComponent) EnsureMove(z tree.ZoneKey, donor, receiver string) *Move {
	if c.Moves == nil {
		c.Moves = make(map[tree.Key]*Move)
	}
	if m, ok := c.Moves[z.Key()]; ok {
		return m
	}
	m := NewMove(z, donor, receiver)
	c.Moves[z.Key()] = m
	return m
}

func (c *ActivationComponent) Reconcile(zones []tree.ZoneKey) {
	if c == nil || c.OldView == nil || c.NewView == nil {
		return
	}
	for _, z := range zones {
		old := c.OldView.Owner(z.ID)
		neu := c.NewView.Owner(z.ID)
		if old == neu {
			continue
		}
		c.EnsureMove(z, c.OldView.NameOf(old), c.NewView.NameOf(neu))
	}
}

// BuildActivationComponents partitions new vnodes so each activation is independent.
//
// Two added vnodes share a component when activating one without the other
// would assign a zone to an owner that is not owner(z) in the full new view.
// Independent leaves (J1 vs J3) become separate components and may commit as
// soon as their own Moves are done — the controller does not wait to peel
// the whole subtree. Geometrically nested IDs (J1 then J2) share one activation.
func BuildActivationComponents(old, neu *tree.View, zones []tree.ZoneKey) ([]*ActivationComponent, error) {
	if old == nil || neu == nil {
		return nil, fmt.Errorf("nil view")
	}
	added := neu.AddedSince(old)
	if len(added) == 0 {
		c, err := NewComponent("residual", old, tree.Delta{BaseDigest: old.Digest})
		if err != nil {
			return nil, err
		}
		c.Reconcile(zones)
		return []*ActivationComponent{c}, nil
	}
	parent := make([]int, len(added))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		pa, pb := find(a), find(b)
		if pa != pb {
			parent[pb] = pa
		}
	}
	index := make(map[tree.VNodeID]int, len(added))
	for i, d := range added {
		index[d.ID] = i
	}
	zoneIDs := make([]tree.ID, len(zones))
	for i, z := range zones {
		zoneIDs[i] = z.ID
	}
	for i := 0; i < len(added); i++ {
		for j := i + 1; j < len(added); j++ {
			if interferesAdd(old, neu, added[i], added[j], zoneIDs) || interferesAdd(old, neu, added[j], added[i], zoneIDs) {
				union(i, j)
			}
		}
	}
	repairCoupling(old, neu, added, index, zoneIDs, find, union)
	groups := map[int][]tree.VNodeDescriptor{}
	for i, d := range added {
		r := find(i)
		groups[r] = append(groups[r], d)
	}
	type keyed struct {
		id   string
		desc []tree.VNodeDescriptor
	}
	var list []keyed
	for _, desc := range groups {
		sort.Slice(desc, func(a, b int) bool { return desc[a].Name < desc[b].Name })
		id := desc[0].Name
		for i := 1; i < len(desc); i++ {
			id += "+" + desc[i].Name
		}
		list = append(list, keyed{id: id, desc: desc})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].id < list[j].id })
	out := make([]*ActivationComponent, 0, len(list))
	for _, g := range list {
		c, err := NewComponent(ComponentID(g.id), old, tree.Delta{
			BaseDigest: old.Digest,
			Add:        g.desc,
		})
		if err != nil {
			return nil, err
		}
		c.Reconcile(zones)
		out = append(out, c)
	}
	return out, nil
}

// interferesAdd is true when Ja alone already stole a zone, but not to the
// full-new owner, and adding Jb corrects that zone. Independent leaves that
// only steal their own zones do not interfere.
func interferesAdd(old, full *tree.View, ja, jb tree.VNodeDescriptor, zones []tree.ID) bool {
	va, err := tree.Extend(old, ja)
	if err != nil {
		return true
	}
	vab, err := tree.Extend(old, ja, jb)
	if err != nil {
		return true
	}
	for _, z := range zones {
		oOld := old.Owner(z)
		oA := va.Owner(z)
		oAB := vab.Owner(z)
		oFull := full.Owner(z)
		if oA != oOld && oA != oFull && oAB == oFull {
			return true
		}
	}
	return false
}

func repairCoupling(old, neu *tree.View, added []tree.VNodeDescriptor, index map[tree.VNodeID]int, zones []tree.ID, find func(int) int, union func(int, int)) {
	for {
		merged := false
		groups := map[int][]int{}
		for i := range added {
			groups[find(i)] = append(groups[find(i)], i)
		}
		for root, idxs := range groups {
			desc := make([]tree.VNodeDescriptor, len(idxs))
			for k, i := range idxs {
				desc[k] = added[i]
			}
			vs, err := tree.Extend(old, desc...)
			if err != nil {
				continue
			}
			for _, z := range zones {
				oS := vs.Owner(z)
				if oS == old.Owner(z) || oS == neu.Owner(z) {
					continue
				}
				for _, id := range []tree.VNodeID{neu.Owner(z), oS} {
					j, ok := index[id]
					if !ok {
						continue
					}
					if find(j) != find(root) {
						union(root, j)
						merged = true
					}
				}
			}
		}
		if !merged {
			return
		}
	}
}

// Move is one zone transfer from donor to receiver. It does not change
// V_active. The mesh flips deeper zones first, so children freeze before
// their parent (FLIP-UNIT).
type Move struct {
	Zone     tree.ZoneKey
	Donor    string
	Receiver string
}

func NewMove(zone tree.ZoneKey, donor, receiver string) *Move {
	return &Move{Zone: zone, Donor: donor, Receiver: receiver}
}
