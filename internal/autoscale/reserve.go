package autoscale

import "math"

// Below this fraction a fleet already has ample idle CPU. Individual joins
// still need an admissible remote host, including space for the copy.
const reuseCPUPercent = 30.0

// ReceiverOffer is one immediately admissible incoming join, not a slot count.
func ReceiverOffer(m MachineFacts) (ready, known bool) {
	if !m.CPUOK || m.Cores <= 0 || !m.MemOK || !m.DiskOK || (m.ResourceManaged || m.AddressSlots > 0) && !m.HostCPUOK {
		return false, false
	}
	return refusal(m) == "", true
}

// Reserve is the arithmetic of the ready reserve, in receiver offers: one
// machine supplies one offer, whatever its free slots.
type Reserve struct {
	// Floor is the spare offers derived from core usage above the fleet reuse threshold.
	Floor int `json:"floor"`
	// Pending is the concurrent offers the waiting source vnodes can use.
	Pending int `json:"pending"`
	// Ready is the admissible machines not promised to a spawn.
	Ready int `json:"ready"`
	// Warming is the empty machines that just registered and are not
	// admissible yet: supply to come, like a boot, counted once.
	Warming int `json:"warming"`
	// WaitingRemote records a request for which Place found no eligible
	// destination. A ready offer on its own donor cannot serve this request.
	WaitingRemote bool `json:"waiting_remote,omitempty"`
}

// Boots is how many machines must be booting or boot now: what the pending
// requests and the floor ask for, less what is ready or warming. The hook
// subtracts the boots it already accepted and applies the machine cap.
func (r Reserve) Boots() int {
	need := r.Floor + r.Pending - r.Ready
	if r.WaitingRemote {
		need = max(1, need)
	}
	return max(0, need-r.Warming)
}

// ReadyReserve counts the reserve from the machines' facts, with the same
// conservative receiver budget as admission. selected is the target promised
// to the current spawn, pending the concurrent offers needed by the backlog,
// and fresh the machines that registered lately. A machine is counted in one
// term at most: selected or still receiving, it is a destination and counts
// in none. It is unknown (false) when a fact is missing.
func ReadyReserve(machines []MachineFacts, selected string, pct float64, pending int, fresh map[string]bool) (Reserve, bool) {
	if len(machines) == 0 || math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 100 {
		return Reserve{}, false
	}
	reserve := Reserve{Pending: pending}
	excessCorePct := 0.0
	for _, m := range machines {
		ready, known := ReceiverOffer(m)
		if !known {
			return Reserve{}, false
		}
		excessCorePct += float64(m.Cores) * (m.CPUPct - reuseCPUPercent)
		switch {
		case m.Machine == selected:
		case ready:
			reserve.Ready++
		case fresh[m.Machine] && warming(m):
			reserve.Warming++
		}
	}
	// Idle headroom on a larger machine offsets more busy CPU elsewhere.
	// Subtract the threshold before summing so exactly 30% produces zero.
	excessCores := max(0, excessCorePct) / 100
	reserve.Floor = int(math.Ceil(excessCores * pct / 100 / receiverCores))
	return reserve, true
}

// warming reports an empty machine that only its CPU keeps from being a
// target: the CPU of its own start, which falls without anyone acting.
func warming(m MachineFacts) bool {
	return m.Running == 0 && m.Receiving == 0 && m.Free > 0 && !short(m)
}
