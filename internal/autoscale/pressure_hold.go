package autoscale

import "time"

// Four fixed clocks: memory rise, CPU rise, memory level, CPU level.
// Each cools independently. A selected signal must not erase another's age.
// The caller holds Controller.mu; this does constant work, never disk I/O.
func (a *Controller) holdLocked(res ResourceSample, memRise, cpuRise, projected float64, now time.Time) (string, time.Duration, time.Duration) {
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
		if mature < 0 && now.Sub(a.pressureSince[i]) >= c.hold {
			mature = i
		}
	}
	// Preserve priority among qualified signals, then show the first active
	// signal while warming. A fresh rise cannot hide a mature high level.
	if mature >= 0 {
		first = mature
	}
	if first < 0 {
		return "", 0, a.cfg.PressureHold
	}
	return candidates[first].name, now.Sub(a.pressureSince[first]), candidates[first].hold
}
