// Command node parses configuration, builds adapters and runs one indexus.Node.
//
// ROLE
//
//	Process entry. No protocol rule is implemented here.
//
// VERIFICATION
//
//	Real HTTP/TCP Raft/Bolt startup, join, restart and missing-disk tests.
//	Kernel proofs are internal/protocol.
package node
