package tree

import "sort"

// A compressed binary trie keeps only branching bits and leaves: 2*N-1 nodes,
// at most 96 decisions. At the highest differing bit, matching the query beats
// every ID in the other branch, regardless of all following bits.
type xorNode struct {
	child [2]int32
	id    ID
	bit   uint8
}

func buildXORIndex(active []VNodeDescriptor) []xorNode {
	if len(active) < 16 {
		return nil
	}
	// active already has the canonical ID order used by the membership digest.
	nodes := make([]xorNode, 0, 2*len(active)-1)
	var build func([]VNodeDescriptor) int32
	build = func(active []VNodeDescriptor) int32 {
		at := int32(len(nodes))
		nodes = append(nodes, xorNode{bit: Size * 8})
		if len(active) == 1 {
			nodes[at].id = active[0].ID
			return at
		}
		bit := CommonPrefixLen(active[0].ID, active[len(active)-1].ID)
		pivot := sort.Search(len(active), func(i int) bool { return Bit(active[i].ID, bit) == 1 })
		left, right := build(active[:pivot]), build(active[pivot:])
		nodes[at].bit, nodes[at].child = bit, [2]int32{left, right}
		return at
	}
	build(active)
	return nodes
}

func indexedOwner(nodes []xorNode, zone ID) ID {
	at := int32(0)
	for nodes[at].bit < Size*8 {
		at = nodes[at].child[Bit(zone, nodes[at].bit)]
	}
	return nodes[at].id
}
