package indexus

// SummaryCacheSizes describes derived, rebuildable state, never user data or
// authority. It uses constant-time counters so diagnostics do not scan the tree.
type SummaryCacheSizes struct {
	Zones         int  `json:"zones"`
	DirectZones   int  `json:"direct_zones"`
	BoundaryKeys  int  `json:"boundary_keys"`
	PendingWrites int  `json:"pending_writes"`
	Ready         bool `json:"ready"`
}

func (n *Node) SummaryCacheSizes() SummaryCacheSizes {
	if n.mesh == nil {
		return SummaryCacheSizes{}
	}
	// The sums are on disk: what is in memory is the remote contributions
	// and the boundaries this node pushes.
	zones, boundaries, ready := n.mesh.summary.Sizes()
	return SummaryCacheSizes{Zones: zones, BoundaryKeys: boundaries, Ready: ready}
}
