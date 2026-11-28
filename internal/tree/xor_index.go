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

// FoldSibling finds the one live peer that would inherit this whole cell.
// At the last branching bit on the source path, the opposite branch must
// be a leaf. If it branches again, the source cell contains keys on both
// sides of that deeper bit and no single survivor can cover it.
func (v *View) FoldSibling(source ID) (VNodeDescriptor, bool) {
	if !v.HasID(source) || len(v.Active) < 2 {
		return VNodeDescriptor{}, false
	}
	if nodes := v.xorIndex; len(nodes) != 0 {
		node := nodes[0]
		for {
			side := Bit(source, node.bit)
			child := nodes[node.child[side]]
			if child.bit == Size*8 {
				sibling := nodes[node.child[1-side]]
				if sibling.bit != Size*8 {
					return VNodeDescriptor{}, false
				}
				return VNodeDescriptor{ID: sibling.id, Name: v.NameOf(sibling.id)}, true
			}
			node = child
		}
	}
	// In a small or incrementally built view, the opposite branch contains
	// precisely the peers tied for the longest shared prefix with source.
	best := -1
	var peer VNodeDescriptor
	unique := false
	for _, candidate := range v.Active {
		if candidate.ID == source {
			continue
		}
		depth := int(CommonPrefixLen(source, candidate.ID))
		if depth > best {
			best, peer, unique = depth, candidate, true
		} else if depth == best {
			unique = false
		}
	}
	if !unique {
		return VNodeDescriptor{}, false
	}
	return peer, true
}

// VisitBucketPeers emits one peer per occupied XOR bucket, in increasing
// common-prefix length. Offset advances the rotation within every bucket.
// Compressed subtrees occupy contiguous ranks in xorOrder. Only the path
// towards self is visited, even if self has not joined this view yet.
func (v *View) VisitBucketPeers(self ID, offset uint64, visit func(VNodeDescriptor)) {
	peers := v.xorOrder
	if peers == nil {
		peers = canonicalActive(v.Active)
	}
	selectPeer := func(bucket []VNodeDescriptor, depth uint8) {
		visit(bucket[(offset+uint64(depth))%uint64(len(bucket))])
	}
	var at int32
	for len(peers) > 0 {
		depth := CommonPrefixLen(self, peers[0].ID)
		if len(peers) == 1 {
			if depth < Size*8 {
				selectPeer(peers, depth)
			}
			return
		}
		var bit uint8
		var pivot int
		if len(v.xorIndex) > 0 {
			node := v.xorIndex[at]
			bit = node.bit
			// Preorder encodes the left subtree in 2*leaves-1 nodes.
			pivot = int(node.child[1]-node.child[0]+1) / 2
			at = node.child[Bit(self, bit)]
		} else {
			bit = CommonPrefixLen(peers[0].ID, peers[len(peers)-1].ID)
			pivot = sort.Search(len(peers), func(i int) bool { return Bit(peers[i].ID, bit) == 1 })
		}
		if depth < bit {
			selectPeer(peers, depth)
			return
		}
		if Bit(self, bit) == 0 {
			selectPeer(peers[pivot:], bit)
			peers = peers[:pivot]
		} else {
			selectPeer(peers[:pivot], bit)
			peers = peers[pivot:]
		}
	}
}
