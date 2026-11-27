package storage

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Read delegations are local forwarding hints, never authority. Emission writes
// one in the same transaction as the irreversible cession. Installation removes
// the acquired region in the same transaction as mounting it. Unlike completed
// operation receipts, these current successors survive authority compaction.
var readDelegationsKey = []byte("read-delegations")

type readDelegation struct {
	Region tree.Region
	Holder causal.Endpoint
	Stamp  uint64 `json:",omitempty"`
}

func readDelegations(tx *Tx) ([]readDelegation, error) {
	var out []readDelegation
	if raw := tx.value(meta(readDelegationsKey)); len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// replaceReadDelegation overlays a region, preserving its disjoint remainder.
// A nil holder means this disk has acquired the region. Siblings pointing to
// the same successor are merged: repeated full-region round trips retain no
// per-transfer history. Size follows the current geometric fragmentation.
func replaceReadDelegation(tx *Tx, region tree.Region, holder *causal.Endpoint, stamp uint64) error {
	old, err := readDelegations(tx)
	if err != nil {
		return err
	}
	var next []readDelegation
	pending := []tree.Region{region}
	for _, d := range old {
		if holder != nil && d.Stamp >= stamp {
			next = append(next, d)
			var remaining []tree.Region
			for _, part := range pending {
				remaining = append(remaining, part.Subtract(d.Region)...)
			}
			pending = remaining
			continue
		}
		for _, rest := range d.Region.Subtract(region) {
			next = append(next, readDelegation{Region: rest, Holder: d.Holder, Stamp: d.Stamp})
		}
	}
	if holder != nil {
		for _, part := range pending {
			next = append(next, readDelegation{Region: part, Holder: *holder, Stamp: stamp})
		}
	}
	next = normalizeReadDelegations(next)
	if len(next) == 0 {
		return tx.del(meta(readDelegationsKey))
	}
	return tx.putJSON(meta(readDelegationsKey), next)
}

// normalizeReadDelegations is pure; maintenance uses it before taking the
// writer lock, and a single overlay uses it on its current local frontier.
func normalizeReadDelegations(next []readDelegation) []readDelegation {
	merged := map[tree.Region]readDelegation{}
	for _, d := range next {
		for {
			joined := false
			for i, mask := range d.Region.Mask {
				for bit := byte(128); bit != 0; bit >>= 1 {
					if mask&bit == 0 {
						continue
					}
					sibling := d.Region
					sibling.Value[i] ^= bit
					if h, ok := merged[sibling]; ok && h.Holder == d.Holder && h.Stamp == d.Stamp {
						delete(merged, sibling)
						d.Region.Mask[i] &^= bit
						d.Region.Value[i] &^= bit
						joined = true
						break
					}
				}
				if joined {
					break
				}
			}
			if !joined {
				break
			}
		}
		merged[d.Region] = d
	}
	next = next[:0]
	for _, d := range merged {
		next = append(next, d)
	}
	slices.SortFunc(next, func(a, b readDelegation) int {
		if c := bytes.Compare(a.Region.Value[:], b.Region.Value[:]); c != 0 {
			return c
		}
		return bytes.Compare(a.Region.Mask[:], b.Region.Mask[:])
	})
	return next
}

// ReadDirectory moves routing exceptions with physical responsibility. These
// pointers cannot grant authority and never replace installed local coverage.
func (s *Store) ReadDirectory() ([]RegionReferral, error) {
	return read(s, func(tx *Tx) ([]RegionReferral, error) {
		ds, e := readDelegations(tx)
		if e != nil {
			return nil, e
		}
		var out []RegionReferral
		for _, d := range ds {
			out = append(out, RegionReferral{Region: d.Region, Endpoint: d.Holder})
		}
		return out, nil
	})
}
func (s *Store) MergeDirectory(refs []RegionReferral) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		self := causal.Owner{VNode: s.node, Storage: storageID(tx)}
		existing, e := readDelegations(tx)
		if e != nil {
			return e
		}
		for _, ref := range refs {
			if !ref.Region.Valid() || !ref.Endpoint.Valid() {
				return ErrConflict
			}
			if ref.Endpoint.Owner == self {
				continue
			}
			parts := []tree.Region{ref.Region}
			remove := func(scope tree.Region) {
				var next []tree.Region
				for _, part := range parts {
					next = append(next, part.Subtract(scope)...)
				}
				parts = next
			}
			for _, r := range state.Permissions.Native {
				remove(r.Region)
			}
			for _, d := range existing {
				remove(d.Region)
			}
			for _, part := range parts {
				endpoint := ref.Endpoint
				if e = replaceReadDelegation(tx, part, &endpoint, 0); e != nil {
					return e
				}
			}
		}
		return nil
	})
}
