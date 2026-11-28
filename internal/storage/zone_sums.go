package storage

import (
	"context"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// ZoneSum is what the records at or below a zone add up to, as zonelog keeps
// it in its segments: no record is read. Only is the one location holding
// them all, or "".
type ZoneSum = zonelog.Sum

// ZoneChildren is [0] the records placed exactly at a zone and [1+c] those
// at or below its child c. It reads every row the disk holds, whatever its
// authority: zones handed away and not yet collected count until they are.
func (s *Store) ZoneChildren(collection, location string) ([65]ZoneSum, error) {
	return s.db.Children(collection, location)
}

// ZoneTotal is what every record at or below a zone adds up to.
func (s *Store) ZoneTotal(collection, location string) (ZoneSum, error) {
	return s.db.Total(collection, location)
}

// Collections lists the collections this store holds rows of.
func (s *Store) Collections() []string { return s.db.Collections() }

// ZoneSums reads zone sums from one committed version (SUM-4). A total and
// the totals subtracted from it to correct it, read through one ZoneSums,
// see the same rows: a row a concurrent commit adds or deletes is in all of
// them or in none. Close it when done; it pins a zonelog snapshot.
type ZoneSums struct{ sn *zonelog.Snapshot }

// OpenZoneSums pins the current committed version for sum reads.
func (s *Store) OpenZoneSums() *ZoneSums { return &ZoneSums{sn: s.db.Snapshot()} }

// Children is ZoneChildren at this version.
func (z *ZoneSums) Children(collection, location string) ([65]ZoneSum, error) {
	return z.sn.Children(collection, location)
}

// Total is ZoneTotal at this version.
func (z *ZoneSums) Total(collection, location string) (ZoneSum, error) {
	return z.sn.Total(collection, location)
}

// Close releases the version.
func (z *ZoneSums) Close() { z.sn.Close() }

// ObserveChildPresence returns two routing-alphabet masks for this disk.
// A seen child has a counted local witness, including uncollected handoff
// copies. An unknown child has an inconclusive probe. Neither mask confers
// authority or describes other peers. No summary is materialized.
func (s *Store) ObserveChildPresence(ctx context.Context, collection, location string) (seen, unknown uint64, err error) {
	return s.db.ObserveChildPresence(ctx, collection, location)
}
