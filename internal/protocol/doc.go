// Package protocol implements the correctness kernel of Indexus.
//
// ROLE
//
//	Scaling plans and the bottom-up order of zone moves, built on the
//	address algebra of internal/tree. The rules hold without a node, a
//	network or a disk.
//
// MODEL
//
//	owner(z) = argmin_v XOR(z, v), v in V_active (tree.View.Owner).
//
// INVARIANTS
//
//	PLAN-1 HOT-1 HOT-2 REG-1
//	HND-3 ACT-2 ZONE-2
//	AUTO-1 AUTO-2
//
//	The normative text is docs/protocol/invariants.md.
//	What is actually proved today is docs/protocol/verification.md.
//
// NON-GOALS
//
//	No HTTP. No filesystem. No EC2. No peer.Contact. No global cluster oracle.
//	No skeleton refinement and no cold balloon planner.
//
// VERIFICATION
//
//	protocol_contract_test.go is the short reading of the contract.
//	invariants_test.go names AUTO-1; internal/tree names ID-1 and TOPO-1.
//	Property and fuzz tests in this package explore the same rules.
//	internal/modelcheck searches for an illegal state. It does not measure cost.
//	INDEXUS_LONG=1 explores more space. It adds no rule.
package protocol
