package autoscale

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"time"
)

// Placement of a new vnode on a machine. The XOR address of the vnode is
// decided elsewhere (PreferNear); this only chooses the host that pays for
// the join. See docs/design/placement-and-resources.md.

// MachineFacts is what one machine agent reports about its host.
type MachineFacts struct {
	// Machine names the host for whoever applies the decision (agent URL).
	Machine         string `json:"machine"`
	Running         int    `json:"running"` // vnode processes alive
	Free            int    `json:"free"`    // unoccupied address slots
	ResourceManaged bool   `json:"resource_managed,omitempty"`
	AddressSlots    int    `json:"address_slots,omitempty"` // resource-admitted agent, total port positions
	// Receiving counts the vnodes still joining: each is receiving a transfer.
	Receiving int `json:"receiving"`
	// CopySlots is the configured shared source-copy budget. Without a
	// positive known limit each waiting requester is counted individually.
	CopySlots int `json:"copy_slots,omitempty"`
	// ReceiverReserveCores is this agent's forecast for the next join.
	ReceiverReserveCores float64 `json:"receiver_reserve_cores,omitempty"`
	ReceiverRefusal      string  `json:"receiver_refusal,omitempty"`
	// VNodes are the names of the vnodes it hosts, to find the donor's machine.
	VNodes []string `json:"vnodes"`
	// Items is the advisory owned item count of each vnode it hosts, by name.
	// Under uniform writes a machine's write load follows the items its vnodes
	// own (AWS, 2026-11-26: write share = item share within one point).
	// Absent for a vnode that did not report: the relief rule then is off.
	Items map[string]int64 `json:"items,omitempty"`
	// AppliedRate covers every live vnode, using applied writes measured at its destination.
	AppliedRate *AppliedRate `json:"applied_rate,omitempty"`
	Cores       int          `json:"cores"`
	// CPUPct is the maximum of summed vnode CPU and aggregate host CPU
	// when available, over a short window, in percent of all its cores. CPUOK false means the
	// agent could not say (an old agent): the slot-count rule then applies.
	CPUPct float64 `json:"cpu_pct"`
	CPUOK  bool    `json:"cpu_ok"`
	// HostCPU is the separately measured whole-machine scope, when available.
	HostCPUPct    float64 `json:"host_cpu_pct"`
	HostCPUOK     bool    `json:"host_cpu_ok"`
	ReceiverReady *bool   `json:"receiver_ready,omitempty"` // nil: resource admission unknown
	// Memory and disk left on the host. A reading that is not OK is not
	// checked (macOS has no /proc/meminfo).
	MemFreePct  float64 `json:"mem_free_pct"`
	MemOK       bool    `json:"mem_ok"`
	DiskFreePct float64 `json:"disk_free_pct"`
	DiskOK      bool    `json:"disk_ok"`
}

// AppliedRate describes a recent host-wide observation. AsOf is the oldest
// endpoint of the contributing vnode windows; a missing observation is unknown.
type AppliedRate struct {
	PerSecond float64   `json:"per_second"`
	AsOf      time.Time `json:"as_of"`
}

// AppliedRateMaxAge is the lifetime of a 10-second rate estimate.
const AppliedRateMaxAge = 10 * time.Second

func (r *AppliedRate) fresh(now time.Time) bool {
	if r == nil || r.PerSecond < 0 || math.IsNaN(r.PerSecond) || math.IsInf(r.PerSecond, 0) {
		return false
	}
	age := now.Sub(r.AsOf)
	return age >= 0 && age <= AppliedRateMaxAge
}

// ItemsCountMaxAge bounds the advisory count used for placement and the
// node recount cadence. Older observations are unknown, never empty.
const ItemsCountMaxAge = 30 * time.Second

const (
	// Keep forty percent headroom for skew, joins and short bursts. These are
	// admission/scale targets, not hard instantaneous resource caps.
	defaultCPUTargetPct   = 60.0
	defaultDiskMinFreePct = 40.0
	// PlaceLocal: a free slot of the donor's own machine.
	PlaceLocal = "local"
	// PlaceExisting: a free slot of another machine that already runs vnodes.
	PlaceExisting = "existing"
	// PlaceNew: an empty machine. With no Machine, none is up: boot one and
	// let the donor ask again.
	PlaceNew = "new"
	// PlaceSlots: resource facts are missing, the machine with the most free
	// slots (the rule before resources were reported).
	PlaceSlots = "slots"
)

// Placement is where to start the vnode and the one-line reason. An empty
// Kind places nothing: there is no slot.
type Placement struct {
	Kind    string `json:"kind"`
	Machine string `json:"machine"`
	Reason  string `json:"reason"`
}

const (
	// receiverCores is the CPU budgeted on the target for one joining vnode,
	// in cores, for the whole join: applying the transfer, its writer and
	// disk time, and the writes it starts to serve. Measured once (AWS,
	// 2026-11-26, i4i.xlarge): two vnodes receiving on one 4 vCPU machine
	// used 87-89 % of it, about 1.75 cores each.
	receiverCores = 1.75
	// Initial estimate for an instrumented machine until it samples a join.
	MinReceiverCores = 0.75

	// targetCPUMaxPct: a machine is a target only if its CPU plus the
	// receiver budget stays at or under the steady-state target. The receiver
	// budget is a measured estimate, not a bound on every future burst.
	targetCPUMaxPct = defaultCPUTargetPct

	// targetDiskMinFreePct: below this free disk a machine is no target. It
	// is the floor under which a vnode asks for a scale-up itself
	// (Config.DiskMinFreePct): a vnode placed lower would ask at once.
	targetDiskMinFreePct = defaultDiskMinFreePct

	// targetMemMinFreePct: below this free memory a machine is no target.
	// 100 minus Config.MemRefusePct: lower, the new vnode would refuse writes.
	targetMemMinFreePct = 40.0

	// maxReceiversPerMachine: joins a machine pays for at the same time. Two
	// at once is what saturated the machines of the 2026-11-26 run.
	maxReceiversPerMachine = 1
)

// Place chooses the host of a vnode asked for by donorVNode. Prefer known
// remote relief, then an empty machine (new), then admissible remote capacity
// with incomplete load information. The donor's own machine comes last, only when nothing
// can boot: a split on the donor's machine moves no load off it. When no
// machine qualifies it asks for a new one while canBoot. A machine cap or a
// resource shortage refuses the spawn instead of bypassing admission.
func Place(machines []MachineFacts, donorVNode string, canBoot bool) Placement {
	if len(machines) == 0 {
		return Placement{Reason: "no machine reported"}
	}
	// Keep the slots-only compatibility path for fleets with no telemetry.
	// One unknown host must never disable the admission checks of its peers.
	if !slices.ContainsFunc(machines, func(m MachineFacts) bool {
		return m.ResourceManaged || m.AddressSlots > 0 || m.CPUOK || m.DiskOK || m.MemOK || m.Receiving > 0
	}) {
		return placeBySlots(machines, "no machine reports resources")
	}
	donor := MachineOf(machines, donorVNode)
	relieves, donorKnown := reliefRule(machines, donor, donorVNode)
	eligible := func(m MachineFacts) bool {
		return m.Running > 0 && m.Machine != donor && ReceiverRefusal(m) == "" && relieves(m)
	}
	preferred := pickByLoad(machines, func(m MachineFacts) bool { _, known := m.load(); return donorKnown && known && eligible(m) }, true)
	// Pack existing remote capacity only with complete load observations. This
	// moves work off the donor without consuming a spare machine per vnode.
	if preferred != nil {
		return Placement{PlaceExisting, preferred.Machine, "known-load remote machine with headroom: " + headroom(*preferred)}
	}
	if m := pick(machines, func(m MachineFacts) bool { return m.Running == 0 && ReceiverRefusal(m) == "" }); m != nil {
		return Placement{PlaceNew, m.Machine, "empty machine: " + headroom(*m)}
	}
	if existing := pick(machines, eligible); existing != nil {
		return Placement{PlaceExisting, existing.Machine, "least loaded machine with headroom: " + headroom(*existing)}
	}
	if canBoot {
		// A budget/quota cap never authorizes overloading a reported machine.
		// If an empty machine is already short, booting an identical one may
		// not help; report that explicitly instead of placing into it anyway.
		if pick(machines, func(m MachineFacts) bool { return m.Running == 0 && short(m) }) != nil {
			return Placement{Reason: "an empty machine is already short of disk or memory"}
		}
		return Placement{Kind: PlaceNew, Reason: "no machine with headroom that the split relieves: wait for a new machine"}
	}
	// Nothing will boot. Dividing the vnode still helps a process-bound donor,
	// so take any machine with headroom, the donor's own included.
	if m := pick(machines, func(m MachineFacts) bool { return m.Machine == donor && ReceiverRefusal(m) == "" }); m != nil {
		return Placement{PlaceLocal, m.Machine, "machine cap reached, donor machine keeps headroom: " + headroom(*m)}
	}
	if m := pick(machines, func(m MachineFacts) bool { return ReceiverRefusal(m) == "" }); m != nil {
		return Placement{PlaceExisting, m.Machine, "machine cap reached, least loaded machine with headroom: " + headroom(*m)}
	}
	return Placement{Reason: "machine cap reached: no free slot with resource headroom"}
}

// Under uniform traffic, a half split moves half the vnode's load off its
// donor. A remote receiver relieves the pair when its projected items/core
// stay strictly below the donor's original items/core. An incomplete count
// makes only that pair unknown.
func reliefRule(machines []MachineFacts, donor, donorVNode string) (func(MachineFacts) bool, bool) {
	all := func(MachineFacts) bool { return true }
	if donor == "" {
		return all, false
	}
	for _, m := range machines {
		if m.Machine != donor {
			continue
		}
		d, ok := m.load()
		if !ok || m.Cores <= 0 {
			return all, false
		}
		v := uint64(m.Items[donorVNode])
		// Budget the larger half for an odd count. The sum of the target's
		// int64 count and this half fits uint64.
		moved := (v + 1) / 2
		return func(target MachineFacts) bool {
			t, known := target.load()
			if !known || target.Cores <= 0 {
				return true
			}
			if moved == 0 {
				return false
			}
			// Compare (t+moved)/target.Cores < d/m.Cores exactly.
			targetHi, targetLo := bits.Mul64(uint64(t)+moved, uint64(m.Cores))
			donorHi, donorLo := bits.Mul64(uint64(d), uint64(target.Cores))
			return targetHi < donorHi || targetHi == donorHi && targetLo < donorLo
		}, true
	}
	return all, false
}

// load is the items owned by the vnodes of the machine; ok is false when one
// of them did not report its count.
func (m MachineFacts) load() (int64, bool) {
	if len(m.VNodes) != m.Running {
		return 0, false
	}
	var n int64
	for _, name := range m.VNodes {
		items, ok := m.Items[name]
		if !ok || items < 0 || n > math.MaxInt64-items {
			return 0, false
		}
		n += items
	}
	return n, true
}

// refusal is why a machine is not a target, or "" when it is one.
func ReceiverRefusal(m MachineFacts) string {
	switch {
	case (m.ResourceManaged || m.AddressSlots > 0) && (!m.HostCPUOK || !m.MemOK || !m.DiskOK || m.Cores <= 0):
		return "waiting for machine resource measurements"
	case !m.CPUOK:
		return "unknown machine CPU"
	case m.Free <= 0:
		return "no free slot"
	case m.DiskOK && m.DiskFreePct < targetDiskMinFreePct:
		return fmt.Sprintf("disk free %.0f%% < %.0f%%", m.DiskFreePct, targetDiskMinFreePct)
	case m.MemOK && m.MemFreePct < targetMemMinFreePct:
		return fmt.Sprintf("memory free %.0f%% < %.0f%%", m.MemFreePct, targetMemMinFreePct)
	case m.Receiving >= maxReceiversPerMachine:
		return fmt.Sprintf("%d vnode still joining", m.Receiving)
	case projectedCPU(m) > targetCPUMaxPct:
		return fmt.Sprintf("cpu %.0f%% + receiver %.0f%% > %.0f%%", m.CPUPct, receiverCost(m), targetCPUMaxPct)
	}
	return ""
}

// receiverCost is receiverCores in percent of the machine. An unknown core
// count is taken as one core: such a machine is never a target by choice.
func receiverCost(m MachineFacts) float64 {
	cores := m.ReceiverReserveCores
	if !(cores >= MinReceiverCores) || math.IsInf(cores, 0) {
		cores = receiverCores
	}
	return 100 * cores / float64(max(m.Cores, 1))
}

// projectedCPU is the machine's CPU once it also pays for one more receiver.
func projectedCPU(m MachineFacts) float64 { return m.CPUPct + receiverCost(m) }

func headroom(m MachineFacts) string {
	return fmt.Sprintf("cpu %.0f%% + receiver %.0f%% <= %.0f%%", m.CPUPct, receiverCost(m), targetCPUMaxPct)
}

// pick is the least loaded machine among those ok accepts, nil if none. A
// machine short of disk or memory comes last, then the one with more joins
// in progress, then the one owning more items when both report them, then
// the higher CPU; ties go to more free slots, then to the name so that the
// choice is deterministic.
func pick(machines []MachineFacts, ok func(MachineFacts) bool) *MachineFacts {
	return pickByLoad(machines, ok, false)
}

func pickByLoad(machines []MachineFacts, ok func(MachineFacts) bool, rankTraffic bool) *MachineFacts {
	var kept []MachineFacts
	for _, m := range machines {
		if ok(m) {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	// Pairwise fallback from item order to CPU order can create a comparison
	// cycle when one machine has no count. Use one order for the whole set.
	preferUnderused := rankTraffic
	useItems := true
	now := time.Now()
	for _, m := range kept {
		if rankTraffic && !m.AppliedRate.fresh(now) {
			rankTraffic = false
		}
		if _, ok := m.load(); !ok {
			useItems = false
		}
	}
	receiverBudgets := func(m MachineFacts) int {
		return min(m.Free, int((targetCPUMaxPct-m.CPUPct)/receiverCost(m)))
	}
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		switch {
		case short(a) != short(b):
			return !short(a)
		case a.Receiving != b.Receiving:
			return a.Receiving < b.Receiving
		}
		// Prefer lightly used, already running machines within the cohort.
		// Admission above has already budgeted the transient copy cost.
		if preferUnderused && (a.CPUPct < reuseCPUPercent) != (b.CPUPct < reuseCPUPercent) {
			return a.CPUPct < reuseCPUPercent
		}
		// Compare traffic only between qualified remote targets. Finite slots and
		// CPU headroom limit how many receiver budgets each could still support.
		if rankTraffic {
			if ac, bc := receiverBudgets(a), receiverBudgets(b); ac != bc {
				return ac > bc
			}
			if a.AppliedRate.PerSecond != b.AppliedRate.PerSecond {
				return a.AppliedRate.PerSecond < b.AppliedRate.PerSecond
			}
		}
		// Fall back to owned items for incomplete or equally busy observations.
		if useItems {
			la, _ := a.load()
			lb, _ := b.load()
			if la != lb {
				return la < lb
			}
		}
		switch {
		case a.CPUPct != b.CPUPct:
			return a.CPUPct < b.CPUPct
		case a.Free != b.Free:
			return a.Free > b.Free
		}
		return a.Machine < b.Machine
	})
	return &kept[0]
}

// short reports a machine under its disk or memory floor.
func short(m MachineFacts) bool {
	return m.DiskOK && m.DiskFreePct < targetDiskMinFreePct || m.MemOK && m.MemFreePct < targetMemMinFreePct
}

// MachineOf is the machine hosting the vnode, "" when no agent reports it.
func MachineOf(machines []MachineFacts, vnode string) string {
	if vnode == "" {
		return ""
	}
	for _, m := range machines {
		for _, name := range m.VNodes {
			if name == vnode {
				return m.Machine
			}
		}
	}
	return ""
}

// placeBySlots is the rule used before machines reported resources: the
// machine with the most free slots, the first one on a tie.
func placeBySlots(machines []MachineFacts, why string) Placement {
	best := -1
	for i, m := range machines {
		if m.Free > 0 && (best < 0 || m.Free > machines[best].Free) {
			best = i
		}
	}
	if best < 0 {
		return Placement{Reason: why + ": no free slot"}
	}
	return Placement{PlaceSlots, machines[best].Machine, why + ": most free slots"}
}
