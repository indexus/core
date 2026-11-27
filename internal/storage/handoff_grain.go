package storage

import (
	"context"
	"errors"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// HandoffGrain selects a subregion from row metadata, including its empty
// space so future inserts reach the new receiver. Zone and byte budgets are
// advisory: concurrent writes still enter BeginCausal's migration set.
// Identical point IDs cannot be separated geometrically and are left intact.
func (s *Store) HandoffGrain(ctx context.Context, region tree.Region, maxZones int, maxBytes int64) (tree.Region, error) {
	grain, _, err := s.HandoffGrainBytes(ctx, region, maxZones, maxBytes)
	return grain, err
}

// HandoffGrainBytes is HandoffGrain with the bytes the grain holds, as its
// rows' metadata counts them (0 for an indivisible point over the budget).
func (s *Store) HandoffGrainBytes(ctx context.Context, region tree.Region, maxZones int, maxBytes int64) (tree.Region, int64, error) {
	if !region.Valid() || maxZones < 1 || maxBytes < 1 {
		return region, 0, ErrConflict
	}
	for {
		if err := ctx.Err(); err != nil {
			return region, 0, err
		}
		var sample []tree.ID
		var totalBytes int64
		oversized := false
		err := s.scanSpatialRegion(ctx, region, rowsMeta, func(tx *Tx, z tree.ZoneKey, rows []*row) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, size, err := zoneBytesOf(tx, liveZone(z), rows)
			if err != nil {
				return err
			}
			sample = append(sample, z.ID)
			if size > maxBytes-totalBytes {
				oversized = true
			} else {
				totalBytes += size
			}
			if len(sample) > maxZones || oversized {
				return errGrainSampleFull
			}
			return nil
		})
		if err != nil && !errors.Is(err, errGrainSampleFull) {
			return region, 0, err
		}
		if len(sample) <= maxZones && !oversized {
			return region, totalBytes, nil
		}
		// The index yields sorted IDs. Keep the anchor's contiguous prefix
		// through the first varying bit, rather than creating new low-bit
		// stripes by maximizing sample balance. Existing region constraints
		// are retained; regions with arbitrary masks remain exact.
		first, last := sample[0], sample[len(sample)-1]
		differing := false
		for i := range region.Mask {
			for bit := byte(128); bit != 0; bit >>= 1 {
				region.Mask[i] |= bit
				region.Value[i] = (region.Value[i] &^ bit) | (first[i] & bit)
				if (first[i]^last[i])&bit != 0 {
					differing = true
					break
				}
			}
			if differing {
				break
			}
		}
		if !differing {
			return tree.Point(first), 0, nil
		}

	}
}

var errGrainSampleFull = errors.New("handoff grain sample full")
