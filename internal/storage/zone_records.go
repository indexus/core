package storage

import "github.com/indexus/go-indexus-core/internal/zonelog"

// ZoneRecord is a live record without its payload, as a typed fold reads it.
type ZoneRecord struct {
	Location, ID string
	Weight       float64
	Metrics      []float64
}

// ZoneRecords visits the live, counted records at or below a zone, every row
// the disk holds whatever its authority (as ZoneTotal sums them), newest
// version per key, in key order, until visit returns false.
//
// It serves the collections whose columns are not all sums: zonelog adds
// rows across segments with retractions, a group inverse that min, max,
// first, last and affine columns do not have, so their zone values are folded
// from the records themselves (summary/typed.go).
func (s *Store) ZoneRecords(collection, location string, visit func(ZoneRecord) bool) error {
	return scanPaged(s.db, collection, location, nil, nil, loader(false), func(r zonelog.Record, _ int) bool {
		if r.Uncounted {
			return true
		}
		return visit(ZoneRecord{Location: r.Location, ID: r.ID, Weight: r.Weight, Metrics: r.Metrics})
	})
}
