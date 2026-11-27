// Package convergent is the transport-free core of convergent replication
// (docs/design/convergent-replication.md): dots, per-ID versions, the merge
// rule and the replica set R(z). The transport-free historical replica model
// lives in test fixtures. This runtime imports no storage or network code.
package convergent

import "bytes"

// Incarnation names one store lifetime: a disk incarnation. A store that
// loses its disk comes back with a new one.
type Incarnation string

// Dot is the unique identity of one write: the writer's incarnation and a
// counter that increases by one per write on that store.
type Dot struct {
	Incarnation Incarnation
	Counter     uint64
}

// Version is one stored version of a record ID (section 4 of the design).
type Version struct {
	ID      string
	Ver     uint64 // zone Lamport clock of the writer, plus one
	Deleted bool
	Value   int64    // counted in aggregates when live
	Digest  [32]byte // of ID, Deleted and Value
	Dot     Dot
}

// Wins reports whether a beats b, two versions of the same ID. The order is
// total: higher Ver, then deleted, then smaller digest, then smaller dot. It
// reads no clock.
func Wins(a, b Version) bool {
	if a.Ver != b.Ver {
		return a.Ver > b.Ver
	}
	if a.Deleted != b.Deleted {
		return a.Deleted
	}
	if c := bytes.Compare(a.Digest[:], b.Digest[:]); c != 0 {
		return c < 0
	}
	if a.Dot.Incarnation != b.Dot.Incarnation {
		return a.Dot.Incarnation < b.Dot.Incarnation
	}
	return a.Dot.Counter < b.Dot.Counter
}
