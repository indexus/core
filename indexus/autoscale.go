package indexus

import (
	"context"
	"log/slog"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// Process spawn remains the machine provisioner. PreferNear tags come only
// from a validated HotTrie or Regional ADD plan. No plan means no spawn.

// EnableAutoscale wires the process-spawn controller. Placement tags are
// taken from a validated topology plan when HotTrie/Regional ADD applies.
func (n *Node) EnableAutoscale(cfg autoscale.Config) {
	n.autoscale = autoscale.New(cfg)
	n.autoscale.SetNearBuilder(n.preferNearsForDemand)
}

func (n *Node) AutoscaleSnapshot() map[string]any {
	if n.autoscale == nil {
		return map[string]any{"enabled": false}
	}
	return n.autoscale.Snapshot()
}

// retainedOwnedMetrics reports the current stable ownership snapshot. Handoff
// rounds are synchronous under transferMu, so there is no pending-outbound set.
func (n *Node) retainedOwnedMetrics() (zones, items int) {
	// The zones the disk holds, from its zone index: OwnershipStats read
	// every row at every tick (14 % of a donor's CPU on AWS, 2026-11-26).
	if store := n.durableStore(); store != nil {
		zones, _ = store.PlanningZones(context.Background())
	}
	return zones, n.Items()
}

// writeDemandRate is the writes that arrived at this vnode as their holder,
// per second since the previous call (autoscale ticks only). It is counted
// on arrival: a store that stops serving does not lower it.
func (n *Node) writeDemandRate(now time.Time) float64 {
	if n.mesh == nil {
		return 0
	}
	m := n.mesh
	count := m.service.Writes.Arrived()
	rate := 0.0
	if elapsed := now.Sub(m.demandAt).Seconds(); !m.demandAt.IsZero() && elapsed > 0 {
		rate = max(0, float64(count-m.demandSeen)/elapsed)
	}
	m.demandAt, m.demandSeen = now, count
	return rate
}

// transferStallLimit is how long the open transfers of this disk may go
// without a committed step before autoscale stops waiting for them.
const transferStallLimit = 3 * time.Minute

// holdForTransfers decides whether autoscale waits for the transfers of this
// disk. While one is open, owned zones and items are moving, so a decision
// would count them twice: it waits as long as the transfers keep committing
// steps, however long they take. When no step was committed for
// transferStallLimit, or since they were first seen open, it stops waiting
// (stalled). It only asks for capacity: whether a new transfer may start is
// still decided by the transfer itself.
func holdForTransfers(open bool, openSince, lastStep, now time.Time) (hold, stalled bool) {
	if !open {
		return false, false
	}
	quietSince := openSince
	if lastStep.After(quietSince) {
		quietSince = lastStep
	}
	if now.Sub(quietSince) >= transferStallLimit {
		return false, true
	}
	return true, false
}

func (n *Node) holdAutoscaleForTransfers() bool {
	if n == nil {
		return false
	}
	store := n.durableStore()
	if store == nil {
		return false
	}
	snap, err := store.CausalRouting()
	if err != nil {
		return false
	}
	open, now := snap.HasOpenTransfer(), time.Now()
	if open {
		// Receiving the rest of a join must not prevent the first installed
		// native grain from helping another vnode. Only already pinned inputs
		// wait; the actual transfer still validates ownership and admission.
		if available, err := store.HasDelegatableRight(); err == nil && available {
			open = false
		}
	}
	if !open {
		n.openTransferSince.Store(0)
		n.transferStallWarned.Store(false)
		return false
	}
	n.openTransferSince.CompareAndSwap(0, now.UnixNano())
	hold, stalled := holdForTransfers(true, time.Unix(0, n.openTransferSince.Load()), store.LastTransferStep(), now)
	if !stalled {
		n.transferStallWarned.Store(false)
	} else if !n.transferStallWarned.Swap(true) {
		slog.Warn("open transfers committed no step; autoscale stops waiting for them",
			"quiet_limit", transferStallLimit, "last_step", store.LastTransferStep())
	}
	return hold
}

func (n *Node) AutoscaleTick() {
	if n.autoscale == nil {
		return
	}
	// Keep sampling through handoffs: averaging an entire multi-minute join
	// hides the current demand when the controller resumes.
	in := n.sampleAutoscaleDemand()
	in.Resources = n.autoscale.Sample()
	if n.mesh != nil {
		n.mesh.service.ObserveCopyPressure(in.Resources.CPUPct, n.autoscale.Config().CPULimitPct, in.Resources.CPUOK)
	}
	if n.holdAutoscaleForTransfers() {
		return
	}
	zones, items := n.retainedOwnedMetrics()
	in.OwnedZones, in.OwnedItems = zones, items
	if view := n.topologyView(); view != nil {
		in.Members = view.Active
	}
	n.autoscale.Tick(in, func(req autoscale.Request) error {
		return n.askForPeers(req, in, zones)
	})
}

// sampleAutoscaleDemand takes one coherent local sample, including when a
// handoff prevents the placement tick. The lock covers only in-memory counters.
func (n *Node) sampleAutoscaleDemand() autoscale.Input {
	in := autoscale.Input{SelfName: n.Name()}
	m := n.mesh
	if m == nil {
		return in
	}
	m.sampleMu.Lock()
	defer m.sampleMu.Unlock()
	now := time.Now()
	in.WriteRate = n.writeDemandRate(now)
	in.InboundLots = m.installedLots.Swap(0)
	in.ClientOps, in.ClientInFlight = m.clientActivity.Sample()
	if in.ClientOps > 0 || in.ClientInFlight > 0 || in.WriteRate > 0 {
		m.lastClientSample = now
	}
	cfg := n.autoscale.Config()
	lookback := max(cfg.PressureHold, cfg.RiseHold)
	in.ClientDemand = autoscale.ClientQuiet
	if now.Sub(m.lastClientSample) <= lookback || now.Sub(time.Unix(0, m.lastWrite.Load())) <= lookback {
		in.ClientDemand = autoscale.ClientActive
	}
	demand := &m.service.Writes
	in.Queue = int(max(0, demand.InFlight.Load()))
	in.OldestWait = demand.OldestWait(now)
	in.TransfersOut = m.service.OutgoingHandoffs()
	return in
}

// askForPeers sends one scale request to the provisioner and logs what
// fired it.
func (n *Node) askForPeers(req autoscale.Request, in autoscale.Input, zones int) error {
	inserts := n.autoscale.Inserts()
	req.SpawnCount = max(1, req.SpawnCount)
	slog.Info("under pressure, asking for peers",
		"reason", req.Reason,
		"signal", req.Signal,
		"transfers_in_flight", req.TransfersInFlight,
		"oldest_wait", in.OldestWait,
		"cpu_process_pct", in.Resources.ProcessCPUPct,
		"cpu_host_pct", in.Resources.HostCPUPct,
		"spawn_count", req.SpawnCount,
		"prefer_near", req.PreferNear,
		"prefer_nears", req.PreferNears,
		"queue", in.Queue,
		"inserts_window", inserts,
		"owned_items_retained", in.OwnedItems,
		"cpu_pct", in.Resources.CPUPct,
		"mem_pct", in.Resources.MemPct,
		"disk_free_pct", in.Resources.DiskFreePct,
	)
	return n.autoscale.PostRequest(n.autoscale.Config().ProvisionerURL, n.Name(), inserts, zones, req)
}
