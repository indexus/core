package protocol

import (
	"fmt"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// PlanRegionalAdd is ADD-only inside the smallest closed prefix that can
// separate the donor's load. Owners outside that prefix must not change.
func PlanRegionalAdd(in HotInput) (TopologyPlan, error) {
	if in.Current == nil || in.Donor.IsZero() {
		return TopologyPlan{}, fmt.Errorf("regional add needs donor and view")
	}
	owned := donorZones(in)
	for _, z := range owned {
		if atomicZone(in, z) {
			return TopologyPlan{}, ErrAtomicOverload
		}
	}
	write := in.WritePressure >= in.TargetWrite && in.WritePressure > 0
	idx := hottestReliefLeaf(owned, in.DrainRate, write || in.PreferWrites)
	if idx < 0 && len(owned) > 0 {
		idx = 0
	}
	if idx < 0 {
		return TopologyPlan{}, ErrSafeSplitInsufficient
	}
	z := owned[idx]
	b, ok := tree.FirstDiffFrom(in.Donor, z.ID(), 0)
	if !ok {
		return TopologyPlan{}, ErrAtomicOverload
	}
	side := tree.Bit(z.ID(), b)
	j, err := tree.ForkID(in.Donor, b, side, z.ID())
	if err != nil || j == in.Donor || j.IsZero() {
		return TopologyPlan{}, ErrSafeSplitInsufficient
	}
	name := tree.EncodeName(j)
	next, err := tree.Extend(in.Current, tree.VNodeDescriptor{ID: j, Name: name})
	if err != nil {
		return TopologyPlan{}, err
	}
	if in.ItemPressure >= in.TargetItems && in.ItemPressure > 0 && !(in.WritePressure >= in.TargetWrite && in.WritePressure > 0) && !(in.DiskPressure >= in.TargetDisk && in.DiskPressure > 0) {
		if _, useful := itemRelief(in, next); !useful {
			return TopologyPlan{}, ErrSafeSplitInsufficient
		}
	}
	cell, err := tree.Cell(next, name)
	if err != nil {
		return TopologyPlan{}, err
	}
	// A shared leading bit among all vnodes does not constrain XOR ownership.
	// Widen to the smallest prefix enclosing the entire new cell, including
	// unobserved keys, rather than checking only the sampled inventory.
	bits := uint8(0)
	for bits < b && tree.Bit(cell.Mask, bits) == 1 {
		bits++
	}
	region := tree.Prefix{ID: in.Donor, Bits: bits}

	return TopologyPlan{
		BaseDigest: in.Current.Digest,
		Region:     region,
		Donor:      in.Donor,
		Add:        []tree.VNodeDescriptor{{ID: j, Name: name}},
		Reason:     ReasonRegionalAdd,
	}, nil
}
