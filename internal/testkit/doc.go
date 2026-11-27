// Package testkit is shared test support. It holds no protocol rule.
//
// ROLE
//
//	RequireLong, RequireSoak, Seed, DumpSeedOnFailure, Eventually.
//
// MODEL
//
//	None.
//
// INVARIANTS
//
//	None. INDEXUS_LONG=1 and INDEXUS_SOAK=1 add cases, never rules.
//	A random failure must log seed= and step=.
//
// NON-GOALS
//
//	Production packages must not import it.
//
// VERIFICATION
//
//	Used by indexus, protocol, modelcheck and simulation tests.
package testkit
