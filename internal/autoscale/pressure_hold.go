package autoscale

import "time"

// Four fixed clocks: memory rise, CPU rise, memory level, CPU level.
// Each cools independently. A selected signal must not erase another's age.
// The caller holds Controller.mu; this does constant work, never disk I/O.
func (a *Controller) holdLocked(in Input, memRise, cpuRise, projected float64, now time.Time) (string, time.Duration, time.Duration, bool) {
	res := in.Resources
	if in.InboundLots == 0 {
		a.inboundCPUUntil = time.Time{}
	}
	deferred := false
	candidates := [...]struct {
		name   string
		active bool
		hold   time.Duration
	}{
		{"mem_rise", a.cfg.MemRisePct > 0 && memRise >= a.cfg.MemRisePct, a.cfg.RiseHold},
		{"cpu_rise", a.cfg.CPURisePct > 0 && res.CPUOK && res.CPUPct >= a.cfg.CPUFloorPct && cpuRise >= a.cfg.CPURisePct, a.cfg.RiseHold},
		{"mem_filling", a.cfg.MemLimitPct > 0 && res.MemPct >= a.cfg.MemFloorPct && projected >= a.cfg.MemLimitPct, a.cfg.PressureHold},
		{"cpu", a.cfg.CPULimitPct > 0 && res.CPUOK && res.CPUPct >= a.cfg.CPUFloorPct && res.CPUPct >= a.cfg.CPULimitPct, a.cfg.PressureHold},
	}
	a.risingFast = candidates[0].active || candidates[1].active
	first, mature := -1, -1
	for i, c := range candidates {
		if !c.active {
			a.pressureSince[i] = time.Time{}
			continue
		}
		if a.pressureSince[i].IsZero() {
			a.pressureSince[i] = now
		}
		if first < 0 {
			first = i
		}
		if mature >= 0 || now.Sub(a.pressureSince[i]) < c.hold {
			continue
		}
		cpu := c.name == "cpu" || c.name == "cpu_rise"
		if cpu && in.InboundLots > 0 && in.WriteRate == 0 && in.Queue == 0 && in.OldestWait == 0 && a.hostDriven(c.name, res) {
			// A receiving disk briefly amplifies shared CPU. Extend the first
			// mature signal once per run of samples with successful installs.
			// Continued copies cannot renew the deadline for a read-heavy host.
			if a.inboundCPUUntil.IsZero() {
				a.inboundCPUUntil = now.Add(min(c.hold, a.cfg.PressureHold))
			}
			if now.Before(a.inboundCPUUntil) {
				deferred = true
				continue
			}
		}
		mature = i
	}
	// Preserve priority among qualified signals, then show the first active
	// signal while warming. A fresh rise cannot hide a mature high level.
	if mature >= 0 {
		first = mature
	}
	if first < 0 || mature < 0 && deferred {
		return "", 0, a.cfg.PressureHold, deferred
	}
	return candidates[first].name, now.Sub(a.pressureSince[first]), candidates[first].hold, deferred
}
