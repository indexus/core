package protocol

import (
	"fmt"
	"sort"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

type HotInput struct {
	PreferWrites  bool
	Donor         tree.VNodeID
	Zones         []ZoneStats
	DiskPressure  float64
	WritePressure float64
	TargetDisk    float64
	TargetWrite   float64
	ItemPressure  float64
	TargetItems   float64
	Current       *tree.View
	DrainRate     float64
	AtomicDisk    int64
	AtomicWrite   float64
}

type ReliefEstimate struct {
	ItemsRemoved  int64
	WritesRemoved float64
	Heat          float64
	SnapshotBytes int64
	MirrorRho     float64
	TimeToReady   time.Duration
}

func Guard(view *tree.View, donor tree.VNodeID) uint8 {
	if view == nil {
		return 0
	}
	others := make([]tree.ID, 0, view.Len())
	for _, d := range view.Active {
		others = append(others, d.ID)
	}
	return tree.GuardBit(donor, others)
}

func EstimateRelief(z ZoneStats, drain float64) ReliefEstimate {
	bytes := z.Bytes
	if bytes == 0 {
		bytes = z.Items
	}
	rho := 0.0
	if drain > 0 {
		rho = z.WriteFast / drain
	}
	tt := z.ETA
	if tt <= 0 {
		tt = time.Millisecond * time.Duration(bytes+1)
		if rho >= 1 {
			tt = time.Hour
		} else if rho > 0 {
			tt = time.Duration(float64(tt) / (1 - rho))
		}
	}
	return ReliefEstimate{
		ItemsRemoved:  z.Items,
		WritesRemoved: z.WriteFast,
		Heat:          z.Heat,
		SnapshotBytes: bytes,
		MirrorRho:     rho,
		TimeToReady:   tt,
	}
}

func (e ReliefEstimate) score(write bool) float64 {
	sec := e.TimeToReady.Seconds()
	if sec < 1e-9 {
		sec = 1e-9
	}
	if write {
		return max(e.WritesRemoved, e.Heat) / sec
	}
	return float64(e.ItemsRemoved) / sec
}

func donorZones(in HotInput) []ZoneStats {
	var out []ZoneStats
	if in.Current == nil {
		return out
	}
	for _, z := range in.Zones {
		if in.Current.Owner(z.ID()) != in.Donor {
			continue
		}
		out = append(out, z)
	}
	return out
}

// Bytes are the runtime disk/transfer weight. Items is a compatibility weight
// only for pure model inputs which do not supply Bytes.
func zoneDiskWeight(z ZoneStats) int64 {
	if z.Bytes > 0 {
		return z.Bytes
	}
	return z.Items
}

// Cardinality/process relief must actually divide the donor's load. Moving its
// only indivisible address to a fresh vnode would repeat the same pressure and
// manufacture an unbounded chain of empty donors.
func itemRelief(in HotInput, next *tree.View) (remaining float64, useful bool) {
	total := float64(0)
	for _, z := range donorZones(in) {
		total += float64(z.Items)
		if next.Owner(z.ID()) == in.Donor {
			remaining += float64(z.Items)
		}
	}
	return remaining, remaining > 0 && remaining < total
}

// atomicZone says one zone alone exceeds what a machine can hold or write:
// no split can relieve it. An entry that stands for several zones is judged
// by one of them.
func atomicZone(in HotInput, z ZoneStats) bool {
	stands := max(z.Stands, 1)
	if in.AtomicDisk > 0 && zoneDiskWeight(z)/stands > in.AtomicDisk {
		return true
	}
	if in.AtomicWrite > 0 && z.WriteFast/float64(stands) > in.AtomicWrite {
		return true
	}
	return false
}

func forkForZone(donor tree.VNodeID, g uint8, z ZoneStats) (tree.VNodeDescriptor, bool) {
	b, ok := tree.FirstDiffFrom(donor, z.ID(), g)
	if !ok {
		return tree.VNodeDescriptor{}, false
	}
	j, err := tree.HotSplitAddress(donor, b)
	if err != nil || j == donor || j.IsZero() {
		return tree.VNodeDescriptor{}, false
	}
	return tree.VNodeDescriptor{ID: j, Name: tree.EncodeName(j)}, true
}

// PlanHotSplit enforces donor-only ADD in in.Current: newOwner≠oldOwner implies oldOwner=D.
// It does not certify that this supplied membership includes hidden competitors.
func PlanHotSplit(in HotInput) (TopologyPlan, error) {
	if in.Current == nil || in.Donor.IsZero() {
		return TopologyPlan{}, fmt.Errorf("hot split needs donor and view")
	}
	owned := donorZones(in)
	for _, z := range owned {
		if atomicZone(in, z) {
			return TopologyPlan{}, ErrAtomicOverload
		}
	}
	writeNeed := in.WritePressure >= in.TargetWrite && in.WritePressure > 0
	diskNeed := in.DiskPressure >= in.TargetDisk && in.DiskPressure > 0
	itemNeed := in.ItemPressure >= in.TargetItems && in.ItemPressure > 0
	needW, needD, needI := in.WritePressure, in.DiskPressure, in.ItemPressure
	if !writeNeed && !diskNeed && !itemNeed {
		return TopologyPlan{BaseDigest: in.Current.Digest, Donor: in.Donor, Reason: ReasonHotSplit}, nil
	}
	g := Guard(in.Current, in.Donor)
	// HOT_SPLIT is exactly one canonical ADD (ValidatePlan checks this). Rank
	// candidates once rather than repeatedly constructing all remaining leaves;
	// execution still transfers changed children before their parents.
	sort.SliceStable(owned, func(i, j int) bool {
		a, b := EstimateRelief(owned[i], in.DrainRate).score(writeNeed || in.PreferWrites), EstimateRelief(owned[j], in.DrainRate).score(writeNeed || in.PreferWrites)
		if a != b {
			return a > b
		}
		return owned[i].Zone.Location > owned[j].Zone.Location
	})
	seen := map[tree.VNodeID]bool{}
	for _, z := range owned {
		desc, ok := forkForZone(in.Donor, g, z)
		for ok && in.Current.IsRetiredID(desc.ID) {
			bit, _ := tree.SingleDiffBit(in.Donor, desc.ID)
			if bit+1 >= tree.Size*8 {
				ok = false
				break
			}
			desc, ok = forkForZone(in.Donor, bit+1, z)
		}
		if !ok || seen[desc.ID] {
			continue
		}
		seen[desc.ID] = true
		next, err := tree.Extend(in.Current, desc)
		if err != nil {
			return TopologyPlan{}, err
		}
		// Several canonical zones can share an ID. Compute relief from the actual
		// next owners, not just the candidate that supplied the fork.
		needW, needD, needI = in.WritePressure, in.DiskPressure, in.ItemPressure
		for _, zone := range owned {
			if next.Owner(zone.ID()) != in.Donor {
				needW -= zone.WriteFast
				needD -= float64(zoneDiskWeight(zone))
				needI -= float64(zone.Items)
			}
		}
		if (writeNeed && needW > in.TargetWrite) || (diskNeed && needD > in.TargetDisk) || (itemNeed && needI > in.TargetItems) {
			continue
		}
		if itemNeed && !diskNeed && !writeNeed {
			if _, useful := itemRelief(in, next); !useful {
				continue
			}
		}
		return TopologyPlan{BaseDigest: in.Current.Digest, Donor: in.Donor, Add: []tree.VNodeDescriptor{desc}, Reason: ReasonHotSplit}, nil
	}
	return TopologyPlan{}, ErrSafeSplitInsufficient
}

// hottestReliefLeaf is the leaf zone whose move relieves the donor most: a
// zone no other zone of the list lies below. Ties go to the greater location.
func hottestReliefLeaf(zones []ZoneStats, drain float64, write bool) int {
	best := -1
	bestScore := -1.0
	inner := innerZones(zones)
	for i, z := range zones {
		if inner.has(z.Zone) {
			continue
		}
		s := EstimateRelief(z, drain).score(write)
		if best < 0 || s > bestScore || (s == bestScore && z.Zone.Location > zones[best].Zone.Location) {
			best, bestScore = i, s
		}
	}
	return best
}

// zoneAt names a zone by collection and location.
type zoneAt struct{ collection, location string }

// inner is the zones of a list that have another zone of the list below
// them (tree.Covers): every strict prefix of a location in its collection,
// and the root of a collection that holds any other location.
type inner struct {
	prefixes map[zoneAt]struct{}
	// first is one location of each collection; several says the collection
	// holds at least two distinct ones, so its root covers another zone.
	first   map[string]string
	several map[string]bool
}

// innerZones reads the list once. A vnode asks for the hottest leaf when it
// is under pressure, with every zone it owns: comparing each zone with every
// other took 12 s for 40 000 zones (AWS 2026-11-26: 36 to 38 % of the CPU of
// the vnodes asking for help). Marking the prefixes of each location stops
// at the first one already marked, whose own prefixes are marked too, so the
// work is linear in the zones.
func innerZones(zones []ZoneStats) inner {
	in := inner{prefixes: make(map[zoneAt]struct{}, len(zones)), first: map[string]string{}, several: map[string]bool{}}
	root := tree.BASE64.Root()
	for _, z := range zones {
		collection, location := z.Zone.Collection, z.Zone.Location
		if first, seen := in.first[collection]; !seen {
			in.first[collection] = location
		} else if first != location {
			in.several[collection] = true
		}
		if location == root {
			continue // the root is below no location
		}
		for n := len(location) - 1; n > 0; n-- {
			at := zoneAt{collection, location[:n]}
			if _, marked := in.prefixes[at]; marked {
				break
			}
			in.prefixes[at] = struct{}{}
		}
	}
	return in
}

func (in inner) has(z tree.ZoneKey) bool {
	if z.Location == tree.BASE64.Root() || z.Location == "" {
		return in.several[z.Collection]
	}
	_, marked := in.prefixes[zoneAt{z.Collection, z.Location}]
	return marked
}
