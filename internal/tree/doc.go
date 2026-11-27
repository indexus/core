// Package tree is the address algebra of Indexus: layer 1 of the node.
//
// ROLE
//
//	Where something is in the one binary tree. 96-bit IDs and XOR distance,
//	zone keys id(z) = location bits || collection suffix, regions (mask,
//	value), the membership View and its Owner, the XOR trie behind it, the
//	peer registry trie and the Base alphabets that print addresses.
//
// MODEL
//
//	owner(z) = argmin_v XOR(z, v), v in V_active.
//
// INVARIANTS
//
//	ID-1 OWN-1 TOPO-1 (docs/protocol/invariants.md).
//
// NON-GOALS
//
//	No I/O and no other internal package: the standard library only. Plans,
//	capacity and authority build on this package, never the reverse.
package tree
