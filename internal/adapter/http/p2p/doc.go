// Package p2p is the HTTP RPC between nodes.
//
// ROLE
//
//	Encode a request, carry it, decode it.
//
// MODEL
//
//	The bytes of a vnode id, a generation and a mutation sequence survive the hop.
//
// INVARIANTS
//
//	FWD-1 is decided by the kernel, not by this package. Residual bytes are
//	not an HTTP response body that this layer invents.
//
// NON-GOALS
//
//	No owner function.
//
// VERIFICATION
//
//	p2p_test.go.
package p2p
