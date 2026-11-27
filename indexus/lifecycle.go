package indexus

func (n *Node) ClientReady() bool {
	return n != nil && n.mesh != nil && n.topologyView().HasName(n.Name())
}

// Joining reports that the vnode is still acquiring its cell: it is receiving
// transfers, which its machine pays for. The process sets it around the join.
func (n *Node) Joining() bool { return n != nil && n.joining.Load() }

// SetJoining records whether the initial join is still running.
func (n *Node) SetJoining(joining bool) { n.joining.Store(joining) }

// HoldMaintenance keeps, while held, the safe background jobs from starting
// work that changes rights: Observe's refresh on a new peer (sync, resume,
// rebalance) and the cold fold scheduled after a summary reconcile. The
// worker holds it from StartSafe to Start, while the first join attempt runs.
func (n *Node) HoldMaintenance(held bool) { n.maintenanceHeld.Store(held) }
