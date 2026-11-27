package convergent

import (
	"slices"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// ReplicaCount is the size of R(z).
const ReplicaCount = 3

// Replicas returns R(z): up to n active vnodes ordered by XOR distance to the
// zone, keeping only the closest vnode of each machine. R(z)[0] is owner(z).
// It is a pure function of the active set and of machine; heat, capacity and
// placement play no part. machine must be defined for every active vnode.
func Replicas(zone tree.ID, active []tree.VNodeDescriptor, machine func(tree.VNodeID) string, n int) []tree.VNodeID {
	ids := make([]tree.VNodeID, len(active))
	for i, d := range active {
		ids[i] = d.ID
	}
	slices.SortFunc(ids, func(a, b tree.ID) int { return tree.XORCompare(zone, a, b) })
	out := make([]tree.VNodeID, 0, n)
	used := map[string]bool{}
	for _, id := range ids {
		if len(out) == n {
			break
		}
		m := machine(id)
		if used[m] {
			continue
		}
		used[m] = true
		out = append(out, id)
	}
	return out
}
