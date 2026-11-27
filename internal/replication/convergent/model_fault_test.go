package convergent

// Fault selects a deliberately wrong rule, so tests can show they detect it.
// Only the test model reads these switches; runtime decisions have no fault hook.
type Fault uint8

const (
	NoFault              Fault = iota
	NoDigestRule               // merge rule 3 removed: equal versions do not order
	NoDeleteWins               // merge rule 2 removed
	VectorTakesMax             // a vector covers every counter up to the highest seen
	NoReceiptReplication       // receipts stay on the replica that wrote them
	NoMachineCheck             // R(z) may hold two vnodes of one machine
	CleanWithoutFresh          // a zone is clean without a fresh heartbeat reply
	PruneWithoutAll            // deletes are dropped once one replica covers them
	CleanIgnoresLeavers        // a zone is clean while a former member's data is awaited
)

var injected = NoFault
