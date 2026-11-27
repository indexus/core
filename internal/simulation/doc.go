// Package mockup exercises cluster convergence over a controlled network.
//
// ROLE
//
//	Many nodes, a fake network, joins, partitions and convergence.
//	The directory is internal/simulation. The package name stays mockup.
//
// MODEL
//
//	Convergence tests use real indexus.Node, causal mesh and zonelog-backed stores.
//	The network is controlled; partition/drop injection does not fake durability.
//	Fault injection includes both region and per-zone handoff messages.
//	This transport does not model HTTP encoding, size limits or WAN latency.
//	Nodes share one Go runtime and physical machine. There is no per-host
//	CPU/disk contention model, machine boot/reserve controller or loader queue.
//	Passing convergence checks does not predict multi-process AWS p99.
//
// INVARIANTS
//
//	A simulation is not a proof. If an invariant fails, the run must stop.
//	Cost figures (bytes copied, time-to-relief, churn) are performance,
//	not correctness.
//
// NON-GOALS
//
//	Not part of the default go test. The whole package skips unless
//	INDEXUS_LONG=1. Long mode adds no protocol rule.
//
// VERIFICATION
//
//	Cluster scenarios in this directory. Kernel proofs stay in internal/protocol.
//	State-space search stays in internal/modelcheck.
package mockup
