package indexus

import (
	"context"
	"fmt"
	"log/slog"
)

// planningState is one local advisory sample. It is reused for choice and
// validation; only a later durable intent/grant can authorize execution.
func (n *Node) planningState(reason string) (MachineState, error) {
	if n == nil || n.durableStore() == nil {
		return MachineState{}, ErrNotReady
	}
	routing, err := n.durableStore().CausalRouting()
	if err != nil {
		return MachineState{}, err
	}
	capacity, usage, err := n.durableStore().CapacityUsage()
	if err != nil {
		return MachineState{}, err
	}
	zones, err := n.zoneStats()
	if err != nil {
		return MachineState{}, err
	}
	st := MachineState{MachineID: MachineID(n.Name()), Capacity: capacity, Usage: usage, View: routing.Current(), Donor: FromSlice(n.ID()), Zones: zones}
	st.Pressure.Disk = float64(usage.PhysicalLoad())
	if n.machine != nil {
		st.WriteCap = n.machine.WriteCap
		st.WriteLoad = n.machine.WriteLoad
	}
	if st.WriteCap > 0 {
		for _, z := range zones {
			st.Pressure.Writes += z.WriteFast
		}
	}
	if n.autoscale != nil {
		st.ItemLimit = int64(n.autoscale.Config().ItemsLimit)
	}
	// Process resource pressure is an explicit request to divide owned load. It
	// is not a fabricated byte count or an exception to the geometric planner.
	st.Rebalance = reason != "" && reason != "items"
	st.PreferWrites = reason == "writes" || reason == "queue" || reason == "cpu" || reason == "host_pressure"
	return st, nil
}

func (n *Node) zoneStats() ([]ZoneStats, error) {
	if n == nil || n.durableStore() == nil {
		return nil, ErrNotReady
	}
	stats, zones, err := n.durableStore().PlanningSample(context.Background(), planSampleZones)
	if err != nil {
		return nil, err
	}
	out := make([]ZoneStats, 0, len(stats))
	for i, z := range stats {
		if z.Bytes <= 0 || z.Items < 0 {
			return nil, fmt.Errorf("invalid planning counters for %s/%s", z.Zone.Collection, z.Zone.Location)
		}
		// Use each rank slice's actual width. A common truncated weight
		// would represent only 512 of 1,023 zones with a 512-zone sample.
		stands := int64(1)
		if zones > len(stats) {
			stands = int64((i+1)*zones/len(stats) - i*zones/len(stats))
		}
		exact, heat := n.zoneHeatScores(Key{Collection: z.Zone.Collection, Location: z.Zone.Location})
		out = append(out, ZoneStats{Zone: z.Zone, Items: z.Items * stands, Bytes: z.Bytes * stands, WriteFast: float64(exact * stands), Heat: float64(heat * stands), Stands: stands})
	}
	return out, nil
}

// planSampleZones is how many zones a scale decision reads (PLAN-SAMPLE-1).
// Choosing where to split needs where the load is in the address space, not
// every zone: a vnode under pressure read all its zones for each decision,
// 40 000 of them on AWS on 2026-11-26. 512 evenly ranked zones guide an
// advisory choice; heterogeneous or concentrated load can be missed. A store
// with fewer zones is read in full.
const planSampleZones = 512

func (n *Node) preferNearsForDemand(reason string, spawnN int, _ string) []string {
	if n == nil || n.mesh == nil {
		return nil
	}
	names, err := n.mesh.service.PendingSpawnNames()
	if err != nil {
		return nil
	}
	if len(names) > 0 {
		return names
	}
	st, err := n.planningState(reason)
	if err != nil {
		slog.Warn("cannot sample scale state", "err", err)
		return nil
	}
	d := EvaluateMachine(st)
	if d.Kind != KindHotSplit && d.Kind != KindRegionalAdd {
		return nil
	}
	return n.namesFromScaleState(d, spawnN, st)
}

func (n *Node) namesFromScaleState(d Decision, spawnN int, st MachineState) []string {
	if spawnN < 1 {
		return nil
	}
	// Validation uses the same detached view/statistics as the decision, while
	// this fresh digest rejects intervening membership before durable reservation.
	current, err := n.durableStore().CausalRouting()
	if err != nil || current.Current().Digest != st.View.Digest {
		return nil
	}
	vp, err := ValidatePlan(st.View, d.Plan, st.Zones)
	if err != nil {
		slog.Warn("scale plan failed XOR validation", "err", err, "reason", d.Plan.Reason)
		return nil
	}
	// Reserve the whole validated decision. Planner order is deterministic;
	// local ticket handoffs decide independently when each piece can activate.
	if len(vp.Plan.Add) == 0 {
		return nil
	}
	if err := n.reserveScalePlan(vp.Plan); err != nil {
		slog.Warn("scale plan not durably reserved", "err", err)
		return nil
	}
	names := make([]string, len(vp.Plan.Add))
	for i, vnode := range vp.Plan.Add {
		names[i] = vnode.Name
	}
	return names
}

// StorageUsage reports capacity and owned bytes for GET /metrics.
func (n *Node) StorageUsage() (capacity, owned int64, err error) {
	if n == nil || n.durableStore() == nil {
		return 0, 0, ErrNotReady
	}
	// Monitoring only: the last usage read when the store is busy.
	capacity, usage := n.durableStore().MonitoredUsage()
	return capacity, usage.Owned, nil
}

// CommitStats is the store's group commit for GET /metrics: transactions,
// writes they carried, and power-of-two microsecond buckets of a commit and of
// one write's wait for it.
func (n *Node) CommitStats() map[string]any {
	if n == nil || n.durableStore() == nil {
		return nil
	}
	c := n.durableStore().CommitStats()
	return map[string]any{"commits": c.Commits, "writes": c.Writes, "commit_us_log2": c.CommitLog2, "wait_us_log2": c.WaitLog2, "stages": c.Stages, "by_kind": c.ByKind, "admission_refused": c.Refused, "pending": c.Pending, "pending_bytes": c.PendingBytes, "peak_pending": c.PeakPending}
}
