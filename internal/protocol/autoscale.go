package protocol

import (
	"errors"
	"math"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

type Pressure struct {
	Disk        float64
	Writes      float64
	Reads       float64
	Network     float64
	DiskGrowth  float64
	WriteGrowth float64
}

type MachineState struct {
	MachineID   MachineID
	Capacity    int64
	WriteCap    int64
	Usage       Usage
	WriteLoad   int64
	Pressure    Pressure
	Offers      []Offer
	View        *tree.View
	Donor       tree.VNodeID
	Zones       []ZoneStats
	TargetDisk  float64
	TargetWrite float64
	DrainRate   float64
	ScaleTime   time.Duration
	// ItemLimit is a soft cardinality trigger; Rebalance requests relief for
	// process pressure when no write-throughput capacity has been measured.
	// Neither field changes physical capacity or authorizes an activation.
	ItemLimit    int64
	Rebalance    bool
	PreferWrites bool // rank recent write relief, rather than stored cardinality
}

type Kind uint8

const (
	KindNone Kind = iota
	KindMove
	KindHotSplit
	KindRegionalAdd
	KindAtomic
)

type HostMove struct {
	VNode tree.VNodeID
	From  MachineID
	To    MachineID
	Load  int64
}

type Decision struct {
	Kind Kind
	Plan TopologyPlan
	Move *HostMove
	Err  error
}

const (
	DefaultDiskTrigger  = 0.90
	DefaultWriteTrigger = 0.70
	exhaustSafety       = 2.0
)

func defaultScaleTime(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return 30 * time.Second
}

func underPressure(m MachineState) (disk, write bool) {
	td, tw := m.TargetDisk, m.TargetWrite
	if td == 0 && m.Capacity > 0 {
		td = DefaultDiskTrigger * float64(m.Capacity)
	}
	if tw == 0 && m.WriteCap > 0 {
		tw = DefaultWriteTrigger * float64(m.WriteCap)
	}
	disk = td > 0 && m.Pressure.Disk >= td
	write = tw > 0 && m.Pressure.Writes >= tw
	tScale := defaultScaleTime(m.ScaleTime)
	if m.Pressure.DiskGrowth > 0 && m.Capacity > 0 {
		head := float64(m.Capacity) - m.Pressure.Disk
		if head < 0 {
			head = 0
		}
		if head/m.Pressure.DiskGrowth < exhaustSafety*tScale.Seconds() {
			disk = true
		}
	}
	if m.Pressure.WriteGrowth > 0 && m.WriteCap > 0 {
		head := float64(m.WriteCap) - m.Pressure.Writes
		if head < 0 {
			head = 0
		}
		if head/m.Pressure.WriteGrowth < exhaustSafety*tScale.Seconds() {
			write = true
		}
	}
	return disk, write
}

func pickMove(m MachineState) *HostMove {
	load := m.Usage.Owned
	if load <= 0 {
		load = int64(m.Pressure.Disk)
	}
	best, ok := BestDiskHost(m.Offers)
	if !ok || best.MachineID == "" || best.MachineID == m.MachineID {
		return nil
	}
	if best.AvailableDisk < load {
		return nil
	}
	if m.WriteCap > 0 && best.AvailableWrite < m.WriteLoad && m.WriteLoad > 0 {
		return nil
	}
	return &HostMove{VNode: m.Donor, From: m.MachineID, To: best.MachineID, Load: load}
}

// EvaluateMachine chooses MOVE, then HotTrie, then Regional ADD.
// It does not invent vnode IDs itself and never mutates V_active.
func EvaluateMachine(m MachineState) Decision {
	disk, write := underPressure(m)
	var items int64
	for _, z := range m.Zones {
		if m.View != nil && m.View.Owner(z.ID()) == m.Donor {
			items += z.Items
		}
	}
	itemNeed := (m.ItemLimit > 0 && items >= m.ItemLimit) || m.Rebalance
	if !disk && !write && !itemNeed {
		return Decision{Kind: KindNone}
	}
	if disk && !write && !itemNeed {
		if mv := pickMove(m); mv != nil {
			return Decision{
				Kind: KindMove,
				Move: mv,
				Plan: TopologyPlan{BaseDigest: digestOf(m.View), Donor: m.Donor, Reason: ReasonMoveHost},
			}
		}
	}
	in := HotInput{
		Donor:         m.Donor,
		Zones:         m.Zones,
		DiskPressure:  m.Pressure.Disk,
		WritePressure: m.Pressure.Writes,
		TargetDisk:    m.TargetDisk,
		TargetWrite:   m.TargetWrite,
		Current:       m.View,
		DrainRate:     m.DrainRate,
		PreferWrites:  m.PreferWrites,
		AtomicDisk:    m.Capacity,
		AtomicWrite:   float64(m.WriteCap),
	}
	if itemNeed {
		in.ItemPressure = float64(items)
		in.TargetItems = math.Max(1, float64(items)/2)
		if m.PreferWrites && !(m.ItemLimit > 0 && items >= m.ItemLimit) {
			// Process relief may move a small hot area, but must not leave an
			// empty donor chain around a single indivisible zone.
			in.TargetItems = math.Max(1, float64(items)-1)
		}
		if m.ItemLimit > 0 {
			in.TargetItems = math.Max(1, math.Min(in.TargetItems, .7*float64(m.ItemLimit)))
		}
	}
	if in.TargetDisk == 0 && m.Capacity > 0 {
		in.TargetDisk = DefaultDiskTrigger * float64(m.Capacity)
	}
	if in.TargetWrite == 0 && m.WriteCap > 0 {
		in.TargetWrite = DefaultWriteTrigger * float64(m.WriteCap)
	}
	plan, err := PlanHotSplit(in)
	if err == nil && len(plan.Add) > 0 {
		return Decision{Kind: KindHotSplit, Plan: plan}
	}
	if errors.Is(err, ErrAtomicOverload) {
		return Decision{Kind: KindAtomic, Err: err}
	}
	if err != nil && !errors.Is(err, ErrSafeSplitInsufficient) {
		return Decision{Kind: KindNone, Err: err}
	}
	plan, err = PlanRegionalAdd(in)
	if err == nil {
		return Decision{Kind: KindRegionalAdd, Plan: plan}
	}
	if errors.Is(err, ErrAtomicOverload) {
		return Decision{Kind: KindAtomic, Err: err}
	}
	return Decision{Kind: KindNone, Err: err}
}

func digestOf(v *tree.View) [32]byte {
	if v == nil {
		return [32]byte{}
	}
	return v.Digest
}

// RateEWMA is the growth estimator used by time-to-exhaustion. A single
// spike must not pin the estimate forever: α∈(0,1) forgets.
type RateEWMA struct {
	Value  float64
	Alpha  float64
	inited bool
}

func (e *RateEWMA) Observe(x float64) float64 {
	if e == nil {
		return x
	}
	if e.Alpha <= 0 || e.Alpha >= 1 {
		e.Alpha = 0.2
	}
	if !e.inited {
		e.Value = x
		e.inited = true
		return e.Value
	}
	e.Value = e.Alpha*x + (1-e.Alpha)*e.Value
	return e.Value
}
