package indexus

import (
	"github.com/indexus/go-indexus-core/internal/autoscale"
	"time"
)

func (n *Node) Routing() ([]Contact, error) {
	return n.traverseRouting(true), nil
}

func (n *Node) Acknowledged() ([]Contact, error) {
	return n.traverseAcknowledged(true), nil
}

func (n *Node) Registered() ([]Contact, error) {
	return n.traverseRegistered(true), nil
}

func (n *Node) Ownership() (map[string]map[string]map[string]any, error) {
	if n.mesh == nil {
		return nil, ErrNotReady
	}
	inventory, err := n.durableStore().PlanningStats()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]map[string]any{}
	for _, snap := range inventory {
		if out[snap.Zone.Collection] == nil {
			out[snap.Zone.Collection] = map[string]map[string]any{}
		}
		out[snap.Zone.Collection][snap.Zone.Location] = map[string]any{}
	}
	return out, nil
}

func (n *Node) OwnershipStats() (collections, zones int) {
	if n.mesh == nil {
		return 0, 0
	}
	totals, err := n.durableStore().PlanningTotals()
	if err != nil {
		return 0, 0
	}
	return totals.Collections, totals.Zones
}

// measuredOwnership is advisory, published by the same scan that measures items.
type measuredOwnership struct{ collections, zones int }

// MeasuredOwnershipStats never opens a disk transaction. Monitoring may be
// briefly stale; ownership decisions continue to use durable authority.
func (n *Node) MeasuredOwnershipStats() (collections, zones int) {
	if stats := n.measuredOwnership.Load(); stats != nil {
		return stats.collections, stats.zones
	}
	return 0, 0
}

func (n *Node) Count() (int, error) {
	if n.mesh == nil {
		return 0, ErrNotReady
	}
	totals, err := n.mesh.service.Store.PlanningTotals()
	if err != nil {
		return 0, err
	}
	n.measuredOwnership.Store(&measuredOwnership{collections: totals.Collections, zones: totals.Zones})
	n.items.Store(totals.Items)
	return int(totals.Items), nil
}

func (n *Node) Items() int {
	return int(n.items.Load())
}

// Advisory inventory scans must not pin Bolt pages during sustained writes.
// Exact Count calls and authority changes remain independent of this quiet gate.
func (n *Node) inventoryQuiet() bool {
	return n.mesh == nil || time.Since(time.Unix(0, n.mesh.lastWrite.Load())) >= time.Second
}

// itemsCountMaxAge bounds how stale the advisory item count may get under
// sustained writes. The quiet gate alone never recounted during a load, so the
// autoscaler's items trigger could not fire in the very load that needs it.
const itemsCountMaxAge = autoscale.ItemsCountMaxAge

// countDue: on start, when writes are quiet, or when the count is too old.
func (n *Node) countDue(now time.Time) bool {
	last := n.lastCountAt.Load()
	return last == 0 || n.inventoryQuiet() || now.Sub(time.Unix(0, last)) >= itemsCountMaxAge
}

func (n *Node) MeasureItems() {
	if _, err := n.Count(); err != nil {
		return
	}
	n.lastCountAt.Store(time.Now().UnixNano())
}

// ItemsAge is how old the advisory item count is, false before the first
// count: a vnode that just received rights shows 0 items until it counts
// (multi-process run of 2026-11-26), and a reader has to tell that 0 from an
// empty vnode.
func (n *Node) ItemsAge() (time.Duration, bool) {
	last := n.lastCountAt.Load()
	if last == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, last)), true
}

// FrozenTransfers is the monitoring view of Store.FrozenOutgoing.
func (n *Node) FrozenTransfers() []string {
	ids, _ := n.ObserveFrozenTransfers()
	return ids
}

// ObserveFrozenTransfers reports a failed storage read to settlement probes.
func (n *Node) ObserveFrozenTransfers() ([]string, error) {
	store := n.durableStore()
	if store == nil {
		return nil, ErrNotReady
	}
	out, err := store.FrozenOutgoing()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(out))
	for i, id := range out {
		ids[i] = id.String()
	}
	return ids, nil
}

// WriteDemand is the monitoring view of write demand at this vnode, counted
// on arrival (mesh.WriteDemand). Counters are cumulative; in_flight,
// oldest_wait_ms and transfers_out are gauges.
func (n *Node) WriteDemand() map[string]any {
	if n.mesh == nil {
		return nil
	}
	service := n.mesh.service
	d := service.Writes.Sample(time.Now())
	return map[string]any{
		"arrived": d.Arrived, "received": d.Received, "forwarded": d.Forwarded, "retried": d.Retried,
		"in_flight": d.InFlight, "oldest_wait_ms": d.OldestWait.Milliseconds(),
		"applied": d.Applied, "acknowledged": d.Acknowledged,
		"refused": d.Refused, "refused_by": d.RefusedBy,
		"transfers_out": service.OutgoingHandoffs(),
	}
}

func (n *Node) NativeAddressShare() (float64, bool) {
	if store := n.durableStore(); store != nil {
		return store.NativeAddressShare()
	}
	return 0, false
}
