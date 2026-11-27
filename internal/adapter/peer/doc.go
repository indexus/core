// Package peer is the dialable contact adapter.
//
// ROLE
//
//	Name, address and vnode bytes of a remote process.
//
// MODEL
//
//	A contact is a route, not an owner.
//
// INVARIANTS
//
//	None from the protocol kernel. Encode and decode must preserve the
//	vnode id the node already computed.
//
// NON-GOALS
//
//	No XOR. No ValidatePlan.
//
// VERIFICATION
//
//	contact_test.go.
package peer
