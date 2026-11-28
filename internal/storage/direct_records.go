package storage

import (
	"context"
	"errors"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

var ErrDirectRecordBudget = errors.New("direct record or metadata budget exceeded")

// DirectRecords reads one physical (collection,location) key range on this
// disk. The right to serve that point and its rows belong to the same snapshot.
// A frozen source and an uninstalled destination both refuse the read.
func (s *Store) DirectRecords(ctx context.Context, zone tree.ZoneKey, maxRecords, maxBytes int) ([]Record, error) {
	if maxRecords < 1 || maxBytes < 1 {
		return nil, ErrDirectRecordBudget
	}
	canonical, err := tree.NewZoneKey(zone.Collection, zone.Location)
	if err != nil {
		return nil, err
	}
	if zone != canonical {
		return nil, errors.New("noncanonical direct record location")
	}
	rows := make([]Record, 0)
	err = s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.causalWritable(tx, zone); err != nil {
			return err
		}
		remaining := maxBytes
		var visitErr error
		// Uncounted zonelog rows in a user collection are application
		// tombstones. An opaque, counted row stores its metadata in its body.
		load := func(zonelog.Record) bool { return false }
		err := tx.reader.ScanWith(zone.Collection, zone.Location, nil, &zonelog.IDRange{}, load, func(z zonelog.Record, payloadBytes int) bool {
			if visitErr = ctx.Err(); visitErr != nil {
				return false
			}
			remaining -= 128 + len(z.Collection) + len(z.Location) + len(z.ID) + 8*len(z.Metrics)
			if remaining < 0 {
				visitErr = ErrDirectRecordBudget
				return false
			}
			if z.Uncounted {
				return true
			}
			if z.Opaque {
				// Only this older representation needs its body to decode the
				// projection. The physical scan provides its length without
				// fetching it. A point read on this snapshot sees the same row.
				if payloadBytes > remaining {
					visitErr = ErrDirectRecordBudget
					return false
				}
				remaining -= payloadBytes
				full, ok, err := tx.reader.Get(z.Collection, z.Location, z.ID)
				if err != nil {
					visitErr = err
					return false
				}
				if !ok {
					visitErr = errors.New("opaque record missing from pinned snapshot")
					return false
				}
				z = full
			}
			row, err := fromLog(z)
			if err != nil {
				visitErr = err
				return false
			}
			if row.Deleted {
				return true
			}
			if z.Opaque {
				remaining -= 8 * len(row.Metrics)
			}
			if remaining < 0 || len(rows) == maxRecords {
				visitErr = ErrDirectRecordBudget
				return false
			}
			row.Payload = nil
			row.Binary = false
			rows = append(rows, row)
			return true
		})
		if err != nil {
			return err
		}
		if visitErr != nil {
			return visitErr
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
