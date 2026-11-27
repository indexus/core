package convergent

import (
	"slices"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// modelReplicas delegates positive replica-set checks to the runtime. The
// negative control deliberately omits machine deduplication, yielding the
// closest n vnodes instead of the closest n distinct machines.
func modelReplicas(zone tree.ID, active []tree.VNodeDescriptor, machine func(tree.VNodeID) string, n int) []tree.VNodeID {
	if injected != NoMachineCheck {
		return Replicas(zone, active, machine, n)
	}
	ids := make([]tree.VNodeID, len(active))
	for i, d := range active {
		ids[i] = d.ID
	}
	slices.SortFunc(ids, func(a, b tree.ID) int { return tree.XORCompare(zone, a, b) })
	return ids[:min(n, len(ids))]
}
