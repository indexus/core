package convergent

import (
	"cmp"
	"maps"
	"slices"
)

// State is the durable state of one zone on one replica: the winning version
// of every ID, receipts, the version vector and aggregates kept by exact
// delta. For every dot the vector covers, Records holds that dot's version or
// one that wins over it.
type State struct {
	Records  map[string]Version
	Receipts map[string]Receipt
	Vector   Vector

	// Clock is the highest Ver this replica has seen in the zone, pruned
	// versions included: a zone Lamport clock. A new write takes Clock+1, so
	// it beats every version its writer has seen, even after pruning.
	Clock uint64

	Live int64 // records not deleted
	Sum  int64 // sum of Value over live records

	// AwaitRetired lists retired members that may still hold versions: a
	// retirement can be wrong (the node was only cut off). While one is
	// listed, deletes are not pruned, so a version it brings back cannot
	// resurrect a deleted record. It leaves the list when its handover
	// arrives or when an operator confirms its disk is destroyed (Forget).
	AwaitRetired map[Member]bool

	Overwrites       uint64 // versions dropped against a concurrent version
	RequestConflicts uint64 // one request ID, two commands
}

func NewState() *State {
	return &State{Records: map[string]Version{}, Receipts: map[string]Receipt{}, Vector: NewVector(), AwaitRetired: map[Member]bool{}}
}

// Apply merges one version. It reports whether the visible record changed.
func (s *State) Apply(v Version) bool {
	if s.Vector.Covers(v.Dot) {
		return false
	}
	s.Vector.Add(v.Dot)
	if v.Ver > s.Clock {
		s.Clock = v.Ver
	}
	cur, ok := s.Records[v.ID]
	if ok && !modelWins(v, cur) {
		if cur.Ver >= v.Ver && cur != v {
			s.Overwrites++
		}
		return false
	}
	if ok {
		if cur.Ver >= v.Ver {
			s.Overwrites++
		}
		if !cur.Deleted {
			s.Live--
			s.Sum -= cur.Value
		}
	}
	if !v.Deleted {
		s.Live++
		s.Sum += v.Value
	}
	s.Records[v.ID] = v
	return true
}

// ApplyReceipt merges one receipt.
func (s *State) ApplyReceipt(r Receipt) {
	cur, ok := s.Receipts[r.RequestID]
	if ok && cur.Command != r.Command {
		s.RequestConflicts++
	}
	if !ok || receiptWins(r, cur) {
		s.Receipts[r.RequestID] = r
	}
}

// Delta is what a replica sends so that another, whose vector is known,
// receives every effect it misses.
type Delta struct {
	Versions []Version
	Receipts []Receipt
	Vector   Vector
}

// DeltaSince returns the versions and receipts whose dots known does not
// cover, in a deterministic order, with a copy of this replica's vector.
func (s *State) DeltaSince(known Vector) Delta {
	d := Delta{Vector: s.Vector.Clone()}
	for _, id := range slices.Sorted(maps.Keys(s.Records)) {
		if v := s.Records[id]; !known.Covers(v.Dot) {
			d.Versions = append(d.Versions, v)
		}
	}
	if injected != NoReceiptReplication {
		for _, rid := range slices.Sorted(maps.Keys(s.Receipts)) {
			if r := s.Receipts[rid]; !known.Covers(r.Result.Dot) {
				d.Receipts = append(d.Receipts, r)
			}
		}
	}
	return d
}

// Merge applies a delta, then extends the vector. The order matters: the
// vector may only cover what the state already holds.
func (s *State) Merge(d Delta) {
	for _, v := range d.Versions {
		s.Apply(v)
	}
	for _, r := range d.Receipts {
		s.ApplyReceipt(r)
	}
	s.Vector.Merge(d.Vector)
}

// Recount recomputes the aggregates from the records (audit, check A3).
func (s *State) Recount() (live, sum int64) {
	for _, v := range s.Records {
		if !v.Deleted {
			live++
			sum += v.Value
		}
	}
	return live, sum
}

// SameData reports whether two states hold the same records, receipts and
// vector (check A2). Counters are local and not compared.
func (s *State) SameData(o *State) bool {
	return maps.Equal(s.Records, o.Records) && maps.Equal(s.Receipts, o.Receipts) && s.Vector.Equal(o.Vector)
}

// Clone copies the durable state, for a crash that keeps the disk.
func (s *State) Clone() *State {
	c := *s
	c.Records = maps.Clone(s.Records)
	c.Receipts = maps.Clone(s.Receipts)
	c.Vector = s.Vector.Clone()
	c.AwaitRetired = maps.Clone(s.AwaitRetired)
	return &c
}

// Prune drops deleted versions whose dot p covers. Receipts stay.
func (s *State) Prune(p Vector) {
	for id, v := range s.Records {
		if v.Deleted && p.Covers(v.Dot) {
			delete(s.Records, id)
		}
	}
}

// Has reports whether the state holds v or a version that wins over it.
// Pruning only drops deletes, so an ID without a record whose vector covers
// v's dot received v and then a delete that won and was pruned.
func (s *State) Has(v Version) bool {
	cur, ok := s.Records[v.ID]
	if !ok {
		return s.Vector.Covers(v.Dot)
	}
	return cur == v || modelWins(cur, v)
}

func sortedMembers(m map[Member]*peer) []Member {
	return slices.SortedFunc(maps.Keys(m), func(a, b Member) int { return cmp.Compare(a, b) })
}
