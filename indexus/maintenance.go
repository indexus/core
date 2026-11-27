package indexus

import (
	"context"
	"time"
)

func (n *Node) Refresh() error { return n.RefreshContext(context.Background()) }

func (n *Node) RefreshContext(ctx context.Context) error {
	if n.mesh == nil {
		return ErrNotReady
	}
	return n.meshMaintainContext(ctx)
}

func (n *Node) zoneWriteHeat(key Key) int64 {
	_, heat := n.zoneHeatScores(key)
	return heat
}

func (n *Node) zoneHeatScores(key Key) (int64, int64) {
	if n == nil {
		return 0, 0
	}
	n.heatMu.Lock()
	defer n.heatMu.Unlock()
	now := time.Now()
	return n.zoneHeat.score(key, now), n.zoneHeat.relief(key, now)
}

// Update explicitly reconciles remote boundaries. Periodic workers use
// UpdateContext and its bounded reconciliation cadence.
func (n *Node) Update() error {
	if n.mesh == nil {
		return ErrNotReady
	}
	return n.mesh.summary.Reconcile(context.Background(), true)
}

func (n *Node) UpdateContext(ctx context.Context) error {
	if n.mesh == nil {
		return ErrNotReady
	}
	return n.mesh.summary.Reconcile(ctx, false)
}
