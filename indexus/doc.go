// Package indexus is the concrete runtime of one Node.
//
// ROLE
//
//	Items, collections, zones, reads, writes, membership, maintenance and
//	process autoscale. It turns a node operation into a call on the kernel.
//
// MODEL
//
//	The ownership model is not restated here. It is internal/protocol:
//	owner(z) = argmin XOR(z, V_active).
//
// INVARIANTS
//
//	WRITE-1  a write follows tree.View.Owner, never the GPS tree.
//	READ-1   a read follows the current owner, never residual bytes.
//	NODE-TOPO-1  Owner reads the durable causal view. The peer registry is only a route.
//	SCALE-WIRE-1  EvaluateMachine → ValidatePlan → activation, on the node.
//
// NON-GOALS
//
//	This package does not re-prove XOR. A second owner function is a bug.
//	Child markers are not ownership.
//	Node delegates durable decisions to internal/storage and internal/mesh.
//	Collections are derived read projections, never write authorities.
//
// VERIFICATION
//
//	protocol_gates_test.go and the node tests are wiring tests.
//	Kernel proofs live in internal/protocol.
package indexus
