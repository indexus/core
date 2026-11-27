package main

import (
	"os"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// Machine-wide shares of elapsed CPU ticks. Guest time is included in user.
type hostCPUPercent struct {
	UserNice   float64 `json:"user_nice_pct"`
	System     float64 `json:"system_pct"`
	IRQSoftIRQ float64 `json:"irq_softirq_pct"`
	IOWait     float64 `json:"iowait_pct"`
	Steal      float64 `json:"steal_pct"`
	Idle       float64 `json:"idle_pct"`
}

type hostCPUObservation struct {
	Status     string          `json:"status"`
	IntervalMS float64         `json:"interval_ms,omitempty"`
	EndUnixMS  int64           `json:"end_unix_ms,omitempty"`
	Ticks      uint64          `json:"ticks,omitempty"`
	Breakdown  *hostCPUPercent `json:"breakdown,omitempty"`
	busyPct    float64
	valid      bool
}

type hostCPUMeter struct {
	mu    sync.Mutex
	at    time.Time
	ticks autoscale.HostCPUTicks
	last  hostCPUObservation
}

func (m *hostCPUMeter) sample() hostCPUObservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return hostCPUObservation{Status: "unavailable"}
	}
	return m.observe(string(raw), time.Now())
}

// Repeated polls in the same scheduler tick must preserve the interval whose
// counters produced host_cpu_pct. A new poll does not extend its end time.
func (m *hostCPUMeter) observe(raw string, at time.Time) hostCPUObservation {
	ticks, ok := autoscale.ParseHostCPUTicks(raw)
	if !ok {
		return hostCPUObservation{Status: "unavailable"}
	}
	start, previous := m.at, m.ticks
	if !start.IsZero() && ticks == previous {
		out := m.last
		if out.valid {
			out.Status = "reused"
		}
		return out
	}
	m.at, m.ticks = at, ticks
	if start.IsZero() {
		m.last = hostCPUObservation{Status: "baseline"}
	} else {
		m.last = cpuInterval(previous, ticks, start, at)
	}
	return m.last
}

func cpuInterval(previous, ticks autoscale.HostCPUTicks, start, end time.Time) hostCPUObservation {
	total, prevTotal := ticks.Total(), previous.Total()
	if total <= prevTotal || ticks.Idle < previous.Idle || ticks.Idle-previous.Idle > total-prevTotal {
		return hostCPUObservation{Status: "counter_reset"}
	}
	delta, idle := total-prevTotal, ticks.Idle-previous.Idle
	pct := func(n uint64) float64 { return 100 * float64(n) / float64(delta) }
	out := hostCPUObservation{
		Status: "partial", valid: true, busyPct: pct(delta - idle),
		IntervalMS: float64(end.Sub(start)) / float64(time.Millisecond),
		EndUnixMS:  end.UnixMilli(), Ticks: delta,
	}
	// Linux iowait can decrease. The aggregate can remain usable even
	// when a component delta cannot be represented as a percentage.
	if !ticks.Complete || !previous.Complete || ticks.IOWait < previous.IOWait ||
		ticks.UserNice < previous.UserNice || ticks.System < previous.System ||
		ticks.IRQSoftIRQ < previous.IRQSoftIRQ || ticks.Steal < previous.Steal {
		return out
	}
	out.Status = "measured"
	out.Breakdown = &hostCPUPercent{
		UserNice: pct(ticks.UserNice - previous.UserNice), System: pct(ticks.System - previous.System),
		IRQSoftIRQ: pct(ticks.IRQSoftIRQ - previous.IRQSoftIRQ), IOWait: pct(ticks.IOWait - previous.IOWait),
		Steal: pct(ticks.Steal - previous.Steal), Idle: pct(idle),
	}
	return out
}
