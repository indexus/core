package wire

import (
	"encoding/json"
	"sync"
)

// Set is the answer to a read of one location: its children, each with
// its aggregate, and their running total.
type Set struct {
	list map[string]*Abelian
	// agg is the running sum of list entries; nil means empty.
	agg *Abelian
	mu  sync.Mutex
}

func NewSet() *Set {
	return &Set{
		list: make(map[string]*Abelian),
	}
}

func (s *Set) List() map[string]*Abelian {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.snapshot()
}

// MarshalJSON snapshots under the lock then encodes unlocked, so /set responses
// do not hold Set.mu across json.Marshal.
func (s *Set) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	s.mu.Lock()
	snap := s.snapshot()
	s.mu.Unlock()
	return json.Marshal(snap)
}

// snapshot copies the values, not just the map: handing the stored pointers
// out and reading them after the lock is released could show a torn
// aggregate on the path that answers reads. Callers hold s.mu.
func (s *Set) snapshot() map[string]*Abelian {
	out := make(map[string]*Abelian, len(s.list))
	for key, value := range s.list {
		if value == nil {
			out[key] = nil
			continue
		}
		out[key] = value.Clone()
	}
	return out
}

func (s *Set) Traverse(process func(string, *Abelian)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, value := range s.list {
		process(key, value)
	}
}

func (s *Set) Get(value string) (*Abelian, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	abelian, ok := s.list[value]
	return abelian, ok
}

func (s *Set) Put(value string, abelian *Abelian) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old, ok := s.list[value]; ok && old != nil && s.agg != nil {
		s.agg.Substract(old)
	}
	s.list[value] = abelian
	if abelian == nil {
		return
	}
	if s.agg == nil {
		s.agg = abelian.Clone()
		return
	}
	s.agg.Sum(abelian)
}

// SetTotal replaces the running total once every entry is in: the total of
// a typed collection's set is the fold of its entries in cell order, not
// their sum (summary/typed.go). A later Put adds to it as a sum again.
func (s *Set) SetTotal(total *Abelian) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agg = total
}

func (s *Set) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.agg == nil {
		return 0
	}
	return s.agg.Count()
}

func (s *Set) Abelian() *Abelian {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.agg == nil {
		return NewAbelian(0, nil)
	}
	return s.agg.Clone()
}

// Clone preserves the exact aggregate (including compensated arithmetic) and
// owns every value; size is what the copy weighs. A set heavier than limit
// is not copied (nil).
func (s *Set) Clone(limit int) (*Set, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size := 256
	if s.agg != nil {
		size += 8 * (len(s.agg.metrics) + len(s.agg.compensation))
	}
	for key, value := range s.list {
		size += len(key) + 128
		if value != nil {
			size += 8 * (len(value.metrics) + len(value.compensation))
		}
	}
	if size > limit {
		return nil, size
	}
	out := &Set{list: s.snapshot()}
	if s.agg != nil {
		out.agg = s.agg.Clone()
	}
	return out, size
}

// size is a capacity hint: the list may change before Traverse runs.
func (s *Set) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}
