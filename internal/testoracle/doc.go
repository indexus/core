// Package testoracle is a slow reference that is allowed to disagree with production.
//
// ROLE
//
//	Independent XOR argmin. It exists to contradict internal/protocol.
//
// MODEL
//
//	For each vnode, materialise z⊕v and compare the distances.
//	Generic over a 12-byte array, so it does not import protocol or indexus.
//
// INVARIANTS
//
//	None of its own. Callers use it to check OWN-1 and PLAN-1.
//
// NON-GOALS
//
//	Does not call View.Owner, XORCompare, HotTrie or ValidatePlan.
//	Production packages must not import it.
//
// VERIFICATION
//
//	oracle_test.go checks the reference against itself.
package testoracle
