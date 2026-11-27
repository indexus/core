package protocol

import (
	"errors"
	"fmt"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

type PlanReason uint8

const (
	ReasonNone PlanReason = iota
	ReasonMoveHost
	ReasonHotSplit
	ReasonRegionalAdd
	// Values are persisted in JoinIntent.Plan; retired values are not reused.
	ReasonColdBalloon PlanReason = 6
)

func (r PlanReason) String() string {
	switch r {
	case ReasonMoveHost:
		return "MOVE_HOST"
	case ReasonHotSplit:
		return "HOT_SPLIT"
	case ReasonRegionalAdd:
		return "REGIONAL_ADD"
	case ReasonColdBalloon:
		return "COLD_BALLOON"
	default:
		return "NONE"
	}
}

// TopologyPlan is the only value a planner may produce. It has no Start()
// and must not mutate V_active. Changes are a prediction: ValidatePlan
// overwrites them from actual XOR on a copy of the current View.
type TopologyPlan struct {
	BaseDigest [32]byte
	Region     tree.Prefix
	Donor      tree.VNodeID
	Add        []tree.VNodeDescriptor
	Remove     []string
	Changes    []ZoneChange
	Reason     PlanReason
}

func (p TopologyPlan) Delta() tree.Delta {
	return tree.Delta{BaseDigest: p.BaseDigest, Add: p.Add, Remove: p.Remove}
}

type ZoneChange struct {
	Zone      tree.ZoneKey
	OldOwner  tree.VNodeID
	NewOwner  tree.VNodeID
	Items     int64
	Bytes     int64
	WriteFast float64
	WriteSlow float64
}

type ZoneStats struct {
	Zone  tree.ZoneKey
	Items int64
	Bytes int64
	// Stands is how many zones this entry stands for when the planner is
	// given a sample (PLAN-SAMPLE-1): its Items, Bytes and writes are then
	// those of all of them. 0 or 1: the zone itself.
	// Heat is an advisory ranking weight, not a write-rate or capacity claim.
	Heat      float64
	Stands    int64
	WriteFast float64
	WriteSlow float64
	ETA       time.Duration
}

func (z ZoneStats) ID() tree.ID {
	if !z.Zone.ID.IsZero() {
		return z.Zone.ID
	}
	id, _ := tree.ZoneID(z.Zone.Collection, z.Zone.Location)
	return id
}

var (
	// ErrRhoDiverged reports a warm mirror that cannot catch up; authority has not moved.
	ErrRhoDiverged = errors.New("mirror ρ ≥ 1")

	ErrStalePlan             = errors.New("stale topology plan")
	ErrExternalSteal         = errors.New("plan would steal from a vnode outside its guarantee")
	ErrSafeSplitInsufficient = errors.New("donor-only safe split cannot meet target")
	ErrAtomicOverload        = errors.New("atomic zone overload")

	// Cold compaction (mesh.Service.Compact) reports these; callers treat both
	// as "nothing to do now" rather than as failures.
	ErrNoBetterConfiguration = errors.New("no better cold configuration")
	ErrColdPlanPreempted     = errors.New("cold reconfiguration preempted by hot work")
)

// ValidatedPlan is TopologyPlan after XOR recomputation. The planner's
// predicted Changes are never trusted.
type ValidatedPlan struct {
	Plan    TopologyPlan
	Current *tree.View
	Next    *tree.View
	Changes []ZoneChange
}

func ValidatePlan(current *tree.View, plan TopologyPlan, zones []ZoneStats) (*ValidatedPlan, error) {
	if current == nil {
		return nil, fmt.Errorf("nil view")
	}
	if plan.BaseDigest != [32]byte{} && plan.BaseDigest != current.Digest {
		return nil, fmt.Errorf("%w: digest mismatch", ErrStalePlan)
	}
	plan.BaseDigest = current.Digest
	next, err := tree.Apply(current, plan.Delta())
	if err != nil {
		return nil, err
	}
	var changes []ZoneChange
	for _, stats := range zones {
		z := stats.Zone
		if z.ID.IsZero() {
			z.ID = stats.ID()
		}
		old := current.Owner(z.ID)
		neu := next.Owner(z.ID)
		if old == neu {
			continue
		}
		ch := ZoneChange{Zone: z, OldOwner: old, NewOwner: neu, Items: stats.Items, Bytes: stats.Bytes, WriteFast: stats.WriteFast, WriteSlow: stats.WriteSlow}
		changes = append(changes, ch)
	}
	plan.Changes = changes
	if err := checkGuarantee(plan, current, next); err != nil {
		return nil, err
	}
	return &ValidatedPlan{Plan: plan, Current: current, Next: next, Changes: changes}, nil
}

// checkGuarantee proves the domain on the complete key space. Existing-row
// checks would miss future zones and duplicate what cell containment proves.
func checkGuarantee(plan TopologyPlan, current, next *tree.View) error {
	var allowed tree.Region
	switch plan.Reason {
	case ReasonHotSplit:
		if len(plan.Add) != 1 {
			return fmt.Errorf("%w: hot split adds exactly one vnode", ErrExternalSteal)
		}
		forkBit, ok := tree.SingleDiffBit(plan.Donor, plan.Add[0].ID)
		if !ok {
			return fmt.Errorf("%w: hot split address must flip exactly one donor bit", ErrExternalSteal)
		}
		guard := Guard(current, plan.Donor)
		if forkBit < guard {
			return fmt.Errorf("%w: fork bit %d is below guard %d", ErrExternalSteal, forkBit, guard)
		}
		canonical, canonicalErr := tree.HotSplitAddress(plan.Donor, forkBit)
		if canonicalErr != nil || canonical != plan.Add[0].ID {
			return fmt.Errorf("%w: non-canonical hot split address", ErrExternalSteal)
		}
		var err error
		allowed, err = tree.Cell(current, current.NameOf(plan.Donor))
		if err != nil {
			return fmt.Errorf("%w: donor absent", ErrExternalSteal)
		}
	case ReasonRegionalAdd:
		if plan.Region.Bits > tree.Size*8 {
			return ErrExternalSteal
		}
		closed := false
		for _, node := range current.Active {
			closed = closed || plan.Region.Contains(node.ID)
		}
		if !closed {
			return fmt.Errorf("%w: region has no existing owner", ErrExternalSteal)
		}
		for bit := uint8(0); bit < plan.Region.Bits; bit++ {
			allowed.Mask = tree.SetBit(allowed.Mask, bit, 1)
			allowed.Value = tree.SetBit(allowed.Value, bit, tree.Bit(plan.Region.ID, bit))
		}
	default:
		return nil
	}
	if len(plan.Remove) != 0 {
		return ErrExternalSteal
	}
	for _, added := range plan.Add {
		cell, err := tree.Cell(next, added.Name)
		if err != nil {
			return err
		}
		inside, ok := allowed.Intersect(cell)
		if !ok || inside != cell {
			return fmt.Errorf("%w: cell of %s exceeds planned region", ErrExternalSteal, added.Name)
		}
	}
	return nil
}

// BestDiskHost is the naive baseline: roomiest disk, residual donors skipped.
func BestDiskHost(offers []Offer) (Offer, bool) {
	return BestDiskOffer(offers)
}
