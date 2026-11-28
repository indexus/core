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
	if err := ctx.Err(); err != nil {
		return region, 0, err
	}
	var first tree.ID
	var totalBytes, prefixBytes int64
	var zones int
	commonBits := uint8(tree.Size * 8)
	err := s.scanSpatialRegion(ctx, region, rowsMeta, func(tx *Tx, z tree.ZoneKey, rows []*row) error {
		if zones == 0 {
			first = z.ID
		}
		// IDs arrive in order. When a shorter common prefix appears, all
		// preceding rows are on the anchor's side of this new fork, and
		// all subsequent rows are outside that side.
		if bits := tree.CommonPrefixLen(first, z.ID); bits < commonBits {
			commonBits, prefixBytes = bits, totalBytes
		}
		zones++
		_, size, err := zoneBytesOf(tx, liveZone(z), rows)
		if err != nil {
			return err
		}
		if zones > maxZones || size > maxBytes-totalBytes {
			return errGrainSampleFull
		}
		totalBytes += size
		return nil
	})
	if !errors.Is(err, errGrainSampleFull) {
		if err != nil {
			return region, 0, err
		}
		return region, totalBytes, nil
	}
	if commonBits == tree.Size*8 {
		return tree.Point(first), 0, nil
	}
	// Preserve all constraints of the input, including non-prefix masks.
	for bit := uint8(0); bit <= commonBits; bit++ {
		region.Mask = tree.SetBit(region.Mask, bit, 1)
		region.Value = tree.SetBit(region.Value, bit, tree.Bit(first, bit))
	}
	return region, prefixBytes, nil
}

var errGrainSampleFull = errors.New("handoff grain sample full")
