// Package issuer spawns and retires node processes.
//
// ROLE
//
//	Turn a scale decision into a process. The decision itself is EvaluateMachine.
//
// MODEL
//
//	A spawn request with the same identity must not be treated as a second vnode.
//
// INVARIANTS
//
//	EXP-1 stays in the kernel. This package does not implement W1 or balloons.
//
// NON-GOALS
//
//	No XOR. No handoff state.
//
// VERIFICATION
//
//	server_scale_test.go.
package issuer
