// Package monitoring exposes process health over HTTP.
//
// ROLE
//
//	Observe a node. Do not steer it.
//
// MODEL
//
//	None.
//
// INVARIANTS
//
//	A metrics or health response must not change ownership or capacity.
//
// NON-GOALS
//
//	No XOR. No handoff.
//
// VERIFICATION
//
//	The handlers are exercised with the node binary. This package has no
//	protocol proof of its own.
package monitoring
