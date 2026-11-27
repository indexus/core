package storage

import (
	"encoding/json"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// ZoneMutations is one zone's mirror backlog, so a handoff round can carry many
// zones in one request and one receiver transaction.
type ZoneMutations struct {
	Zone      tree.ZoneKey
	Mutations []Mutation
}

// MutationRange asks for a zone's history after a sequence, through another.
type MutationRange struct {
	Zone           tree.ZoneKey
	After, Through uint64
}

// MutationsBatch reads several zones' backlogs in one read transaction and
// omits zones with nothing pending. Diffuse data puts one record per zone: a
// handoff that opened a transaction per zone paid for the whole region twice.
func (s *Store) MutationsBatch(ranges []MutationRange) ([]ZoneMutations, error) {
	var out []ZoneMutations
	err := s.view(func(tx *Tx) error {
		for _, r := range ranges {
			mutations, err := mutationsTx(tx, r.Zone, r.After, r.Through)
			if err != nil {
				return err
			}
			if len(mutations) > 0 {
				out = append(out, ZoneMutations{Zone: r.Zone, Mutations: mutations})
			}
		}
		return nil
	})
	return out, err
}

func mutationsTx(tx *Tx, z tree.ZoneKey, after, through uint64) ([]Mutation, error) {
	var out []Mutation
	if after > through {
		return nil, ErrSequence
	}
	log := key(historyBucket, zoneBytes(z))
	for seq := after + 1; seq <= through && seq != 0; seq++ {
		raw := tx.value(log + element(number(seq)))
		if raw == nil {
			return nil, ErrSequence
		}
		var m Mutation
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
