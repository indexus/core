// Package logging configures the process logger.
//
// ROLE
//
//	INDEXUS_LOG_LEVEL and INDEXUS_LOG_FORMAT.
//
// MODEL
//
//	None.
//
// INVARIANTS
//
//	Logging must not change a protocol decision.
//
// NON-GOALS
//
//	No correctness rule lives here.
//
// VERIFICATION
//
//	No protocol test. A wrong log line is not an OWN-1 failure.
package logging
