package storage

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// ZonePlanningStats is advisory input to a planner: a zone's live records
// and the bytes its rows and receipts take, read without the payloads.
type ZonePlanningStats struct {
	Zone  tree.ZoneKey
	Items int64
	Bytes int64
}

// zoneBytesOf counts a zone the way Snapshot.Seal encodes it: its rows,
// then its receipts.
func zoneBytesOf(tx *Tx, zone zoneTable, rows []*row) (items, size int64, err error) {
	arrays := func(n, bytes int64) int64 {
		if n == 0 {
			return 4 // Seal copies into a nil slice: JSON null, not [].
		}
		return bytes + n - 1 + 2
	}
	var records, recordBytes, requests, requestBytes int64
	for _, r := range rows {
		records++
		recordBytes += int64(r.size)
		if r.record != nil && !r.record.Deleted {
			items++
		}
	}
	err = zone.each(tx, receiptPrefix, "", func(_, raw []byte) error {
		requests++
		requestBytes += int64(len(raw))
		return nil
	})
	return items, int64(len(`{"Records":,"Requests":}`)) + arrays(records, recordBytes) + arrays(requests, requestBytes), err
}

// PlanningStats reads advisory per-zone counts and sizes in bounded MVCC
// snapshots. Different pages may observe different committed versions.
func (s *Store) PlanningStats() ([]ZonePlanningStats, error) {
	var out []ZonePlanningStats
	err := s.scanZonePages(zonePages{size: planningPage, rows: rowsMeta, between: planningPaged}, func(tx *Tx, z tree.ZoneKey, rows []*row) error {
		items, size, err := zoneBytesOf(tx, liveZone(z), rows)
		if err == nil {
			out = append(out, ZonePlanningStats{Zone: z, Items: items, Bytes: size})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PlanningTotal is a monitoring projection, not an inventory or authority cache.
type PlanningTotal struct {
	Items              int64
	Collections, Zones int
}

// PlanningSample is PlanningStats bounded: the statistics of at most limit
// zones, taken at even ranks of the zone index, with the number of zones the
// store holds. A store of at most limit zones answers with all of them. The
// index orders zones by id, so the sample covers the address space in
// proportion to where the zones are, and each sampled zone stands for
// zones/len(sample) of them. The cost is limit zone reads and limit rank
// lookups, whatever the size of the store (PLAN-SAMPLE-1).
func (s *Store) PlanningSample(ctx context.Context, limit int) (sample []ZonePlanningStats, zones int, err error) {
	if limit < 1 {
		return nil, 0, ErrConflict
	}
	err = s.spatialView(ctx, func(tx *Tx, root *spatialNode) error {
		zones = spatialCount(root)
		picked := min(zones, limit)
		sample = make([]ZonePlanningStats, 0, picked)
		for i := range picked {
			// The middle of each of picked equal slices of the ranks.
			z, ok := spatialAt(root, (2*i+1)*zones/(2*picked))
			if !ok {
				continue
			}
			rows, e := rowsOf(tx, z, false)
			if e != nil {
				return e
			}
			items, size, e := zoneBytesOf(tx, liveZone(z), rows)
			if e != nil {
				return e
			}
			sample = append(sample, ZonePlanningStats{Zone: z, Items: items, Bytes: size})
		}
		return nil
	})
	return sample, zones, err
}

// PlanningZones is how many zones have rows on this disk, read from the zone
// index: no row is read, whatever the size of the store, once the index is
// built. It is advisory and is not PlanningTotals().Zones: the index counts
// the zones the disk holds, so a zone fenced for a transfer, or ceded and
// not collected yet, is still in it, while PlanningTotals counts the zones
// the disk may write. An autoscale tick asks this one: PlanningTotals read
// every row at every tick, 14 % of a donor's CPU on AWS on 2026-11-26.
func (s *Store) PlanningZones(ctx context.Context) (int, error) {
	for {
		if err := s.ensureSpatial(ctx); err != nil {
			return 0, err
		}
		if state := s.spatial.state.Load(); state != nil {
			return spatialCount(state.root), nil
		}
	}
}

// planningTotalsSnapshot retains only three advisory counters from an exact
// committed read version. It is never used to authorize data operations.
type planningTotalsSnapshot struct {
	txID   int
	totals PlanningTotal
}

// planningPage bounds the work and snapshot retention of each advisory page.
var planningPage = 1024

// planningPaged, when set, runs between two pages (tests).
var planningPaged func()

// PlanningTotals counts the zones and live items a page of zones at a time.
// When no commit lands between the first and the last page, the counters are
// those of one committed version and are kept for it: an unchanged version
// reuses them. Under writes they are advisory, as their callers expect.
//
// It reads every row of the store. Its callers need what only the rows
// say, the items of the zones this disk may write: the item limit of a cold
// fold (PinColdSurvivor, mesh cold drain) and the background item count
// (Node.Count, at most every 30 s under writes). A caller that needs the
// number of zones at every tick asks PlanningZones.
func (s *Store) PlanningTotals() (PlanningTotal, error) {
	var out PlanningTotal
	collections := map[string]struct{}{}
	first, last := -1, -1
	cached := false
	scan := zonePages{size: planningPage, rows: rowsMeta, between: planningPaged}
	scan.begin = func(tx *Tx) bool {
		last = tx.ID()
		if first < 0 {
			first = last
			if prior := s.planningTotalsCache.Load(); prior != nil && prior.txID == first {
				out, cached = prior.totals, true
				return false
			}
		}
		return true
	}
	err := s.scanZonePages(scan, func(_ *Tx, z tree.ZoneKey, rows []*row) error {
		for _, r := range rows {
			if r.record != nil && !r.record.Deleted {
				out.Items++
			}
		}
		out.Zones++
		collections[z.Collection] = struct{}{}
		return nil
	})
	if err != nil {
		return PlanningTotal{}, err
	}
	if !cached {
		out.Collections = len(collections)
		if first == last {
			// Only counters read from one committed version are kept for it.
			s.planningTotalsCache.Store(&planningTotalsSnapshot{txID: first, totals: out})
		}
	}
	return out, nil
}
