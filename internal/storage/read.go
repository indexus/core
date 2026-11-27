package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func dataView(tx *Tx) (*tree.View, error) {
	var t Topology
	if err := tx.readJSON(meta(viewKey), &t); err != nil {
		return nil, err
	}
	return t.View()
}

// stagedSnapshot reads a staged zone: its rows are in its staging bucket.
// receipts are its receipt rows as stored, which an install copies as they
// are.
func stagedSnapshot(tx *Tx, t zoneTable, z tree.ZoneKey) (snap Snapshot, receipts []stagedReceipt, err error) {
	snap = Snapshot{Zone: z, Seq: t.seq(tx), Records: []Record{}}
	err = t.each(tx, recordPrefix, "", func(_, raw []byte) error {
		var r Record
		recordDecodes.Add(1)
		err := json.Unmarshal(raw, &r)
		snap.Records = append(snap.Records, r)
		return err
	})
	if err != nil {
		return snap, nil, err
	}
	err = t.each(tx, receiptPrefix, "", func(k, raw []byte) error {
		var op RequestReceipt
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		snap.Requests = append(snap.Requests, op)
		receipts = append(receipts, stagedReceipt{key: k, value: bytes.Clone(raw)})
		return nil
	})
	return snap, receipts, err
}

// liveSnapshot reads a live zone: its sequence and receipts from the zone
// bucket, its rows from records.
func (s *Store) liveSnapshot(ctx context.Context, tx *Tx, z tree.ZoneKey, includeRequests bool) (Snapshot, error) {
	rows, err := rowsOf(tx, z, true)
	if err != nil {
		return Snapshot{}, err
	}
	return s.snapshotOfRows(ctx, tx, z, rows, includeRequests)
}

func (s *Store) snapshotOfRows(ctx context.Context, tx *Tx, z tree.ZoneKey, rows []*row, includeRequests bool) (Snapshot, error) {
	t := liveZone(z)
	snap := Snapshot{Zone: z, Seq: t.seq(tx), Records: make([]Record, 0, len(rows))}
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return snap, err
		}
		snap.Records = append(snap.Records, *r.record)
	}
	if !includeRequests {
		return snap, nil
	}
	return snap, appendRequests(ctx, tx, t, &snap)
}

func appendRequests(ctx context.Context, tx *Tx, t zoneTable, snap *Snapshot) error {
	return t.each(tx, receiptPrefix, "", func(_, raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var op RequestReceipt
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		snap.Requests = append(snap.Requests, op)
		return nil
	})
}

func (snap Snapshot) Seal() (ZoneSeal, error) {
	sealsComputed.Add(1)
	records := append([]Record(nil), snap.Records...)
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	var items int64
	for i, r := range records {
		z, err := recordZone(r)
		if err != nil {
			return ZoneSeal{}, err
		}
		if z != snap.Zone || i > 0 && records[i-1].ID == r.ID {
			return ZoneSeal{}, ErrConflict
		}
		if !r.Deleted {
			items++
		}
		// One canonical form: a record without metrics reads back as nil from
		// the memtable and as an empty list from a segment, and the two
		// marshal differently. A zone sealed at its freeze and again at the
		// region's, with a flush in between, got two digests, and its region
		// was refused for good (ErrConflict at stage).
		if len(r.Metrics) == 0 {
			records[i].Metrics = nil
		}
	}
	requests := append([]RequestReceipt(nil), snap.Requests...)
	sort.Slice(requests, func(i, j int) bool { return requests[i].RequestID < requests[j].RequestID })
	for i, op := range requests {
		z, err := tree.NewZoneKey(op.Collection, op.Location)
		if err != nil || z != snap.Zone || op.RequestID == "" || op.Seq == 0 || op.Seq > snap.Seq || op.Digest == ([32]byte{}) {
			return ZoneSeal{}, ErrConflict
		}
		if i > 0 && requests[i-1].RequestID == op.RequestID {
			return ZoneSeal{}, ErrConflict
		}
	}
	raw, err := json.Marshal(struct {
		Records  []Record
		Requests []RequestReceipt
	}{records, requests})
	if err != nil {
		return ZoneSeal{}, err
	}
	return ZoneSeal{Zone: snap.Zone, Seq: snap.Seq, Digest: sha256.Sum256(raw), Items: items, Bytes: int64(len(raw))}, nil
}
func (s *Store) Snapshot(z tree.ZoneKey) (Snapshot, error) {
	return read(s, func(tx *Tx) (Snapshot, error) { return s.liveSnapshot(context.Background(), tx, z, true) })
}

// Snapshots reads a prefix of zones in one read transaction so copyLot does not
// re-acquire rowsMu.RLock once per zone behind competing write fsyncs. When
// maxBytes > 0, it stops once the accumulated lower-bound payload exceeds
// maxBytes so large zones never force reading the rest of the unit.
func (s *Store) Snapshots(ctx context.Context, zones []tree.ZoneKey, maxBytes int) ([]Snapshot, error) {
	out := make([]Snapshot, 0, len(zones))
	err := s.view(func(tx *Tx) error {
		approx := 0
		for _, z := range zones {
			if err := ctx.Err(); err != nil {
				return err
			}
			snap, err := s.liveSnapshot(ctx, tx, z, true)
			if err != nil {
				return err
			}
			out = append(out, snap)
			if maxBytes > 0 {
				for _, r := range snap.Records {
					approx += len(r.Payload) + len(r.ID) + 48
				}
				approx += len(snap.Requests) * 96
				if approx > maxBytes {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

// ZoneSeqs returns the live sequence of each zone in one read transaction,
// without decoding records or replay receipts.
func (s *Store) ZoneSeqs(zones []tree.ZoneKey) ([]uint64, error) {
	out := make([]uint64, len(zones))
	err := s.view(func(tx *Tx) error {
		for i, z := range zones {
			out[i] = liveZone(z).seq(tx)
		}
		return nil
	})
	return out, err
}
func (s *Store) Inventory(includeRequests bool) ([]Snapshot, error) {
	return s.InventoryContext(context.Background(), includeRequests)
}
func (s *Store) InventoryContext(ctx context.Context, includeRequests bool) ([]Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return read(s, func(tx *Tx) ([]Snapshot, error) { return s.inventoryScan(ctx, tx, includeRequests, nil, 0, nil) })
}

// InventoryRegion lists writable zones inside region in bounded MVCC snapshots.
// Each zone is read whole from one version; different pages may see different
// versions. It is an advisory inventory, not a strict paginated client read.
func (s *Store) InventoryRegion(region tree.Region, includeRequests bool) ([]Snapshot, error) {
	var out []Snapshot
	err := s.scanZonePages(zonePages{size: inventoryPage, rows: rowsWithPayloads, within: region.Contains, between: inventoryPaged}, func(tx *Tx, z tree.ZoneKey, rows []*row) error {
		snap, err := s.snapshotOfRows(context.Background(), tx, z, rows, includeRequests)
		if err == nil {
			out = append(out, snap)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// inventoryPage bounds the zones one read of InventoryRegion visits.
var inventoryPage = 256

// inventoryPaged, when set, runs between two pages (tests).
var inventoryPaged func()

// zonePages configures advisory scans that may span committed versions.
// Strict client inventories retain their own watermark validation.
type zonePages struct {
	size, rows int
	within     func(tree.ID) bool
	begin      func(*Tx) bool
	between    func()
}

var errZonePageFull = errors.New("zone page full")

func (s *Store) scanZonePages(p zonePages, visit func(*Tx, tree.ZoneKey, []*row) error) error {
	var from *tree.ZoneKey
	for {
		done := false
		err := s.view(func(tx *Tx) error {
			if p.begin != nil && !p.begin(tx) {
				done = true
				return nil
			}
			n := 0
			err := s.eachInventoryZoneFrom(context.Background(), tx, nil, from, 0, p.within, p.rows, func(z tree.ZoneKey, rows []*row) error {
				if from != nil && z == *from {
					return nil // the last zone of the previous page
				}
				if n == p.size {
					return errZonePageFull
				}
				if err := visit(tx, z, rows); err != nil {
					return err
				}
				n++
				zone := z
				from = &zone
				return nil
			})
			if errors.Is(err, errZonePageFull) {
				return nil
			}
			done = err == nil
			return err
		})
		if err != nil || done {
			return err
		}
		if p.between != nil {
			p.between()
		}
	}
}

func (s *Store) inventoryScan(ctx context.Context, tx *Tx, includeRequests bool, query *tree.ZoneKey, generation uint64, within func(tree.ID) bool) ([]Snapshot, error) {
	var out []Snapshot
	err := s.eachGatedZoneFrom(ctx, tx, query, nil, generation, within, rowsWithPayloads, s.readableIn, func(z tree.ZoneKey, rows []*row) error {
		snap, err := s.snapshotOfRows(ctx, tx, z, rows, includeRequests)
		if err != nil {
			return err
		}
		out = append(out, snap)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// What a walk over zones reads of their rows.
const (
	rowsNone = iota
	rowsMeta
	rowsWithPayloads
)

// eachInventoryZone centralizes selection of writable canonical zones. Derived
// planning metadata and full inventories use the same transaction-local gate.
// visit gets, unless want is rowsNone, the zone's rows.
func (s *Store) eachInventoryZone(ctx context.Context, tx *Tx, query *tree.ZoneKey, generation uint64, within func(tree.ID) bool, want int, visit func(tree.ZoneKey, []*row) error) error {
	return s.eachInventoryZoneFrom(ctx, tx, query, nil, generation, within, want, visit)
}

// zoneGate decides whether a scan includes a zone: writableIn for what moves
// or counts load (handoffs, planning, shadows), readableIn for client reads.
type zoneGate func(*Tx, *causalSnapshot, tree.ZoneKey) error

// eachInventoryZoneFrom is eachInventoryZone resuming at zone from (visited
// again first): a paged read continues where its last page stopped.
func (s *Store) eachInventoryZoneFrom(ctx context.Context, tx *Tx, query, from *tree.ZoneKey, generation uint64, within func(tree.ID) bool, want int, visit func(tree.ZoneKey, []*row) error) error {
	return s.eachGatedZoneFrom(ctx, tx, query, from, generation, within, want, s.writableIn, visit)
}

func (s *Store) eachGatedZoneFrom(ctx context.Context, tx *Tx, query, from *tree.ZoneKey, generation uint64, within func(tree.ID) bool, want int, gate zoneGate, visit func(tree.ZoneKey, []*row) error) error {
	v, err := dataView(tx)
	if err != nil {
		return err
	}
	if generation != 0 && generation != v.Generation {
		return ErrNotOwner
	}
	control, err := s.causalSnapshot(tx)
	if err != nil {
		return err
	}
	keep := func(z tree.ZoneKey) (bool, error) {
		if within != nil && !within(z.ID) {
			return false, nil
		}
		if err := gate(tx, control, z); err != nil {
			if errors.Is(err, ErrNotOwner) || errors.Is(err, ErrFrozen) || errors.Is(err, ErrNotReady) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	return s.eachZoneFrom(ctx, tx, query, from, want, keep, visit)
}

func (s *Store) eachZoneFrom(ctx context.Context, tx *Tx, query, from *tree.ZoneKey, want int, keep func(tree.ZoneKey) (bool, error), visit func(tree.ZoneKey, []*row) error) error {
	var current *tree.ZoneKey
	var rows []*row
	flush := func() error {
		if current == nil {
			return nil
		}
		z, zoneRows := *current, rows
		current, rows = nil, nil
		return visit(z, zoneRows)
	}
	err := tx.walk(query, from, "", want == rowsWithPayloads, func(z tree.ZoneKey, r *row) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if r != nil {
			rows = append(rows, r)
			return true, nil
		}
		if err := flush(); err != nil {
			return false, err
		}
		ok, err := keep(z)
		if err != nil {
			return false, err
		}
		if !ok {
			return true, errSkipZone
		}
		current = &z
		if want == rowsNone {
			if err := flush(); err != nil {
				return false, err
			}
			return true, errSkipZone
		}
		return true, nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}
