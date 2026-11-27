package storage

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// CompactReadDirectory forgets versions that no remaining local operation can
// replay into the directory. This is a routing frontier, not an authority floor.
// For every possible pending update v, rank(stamp) preserves v > stamp. Future
// admissions/operations allocate above the durable Sequence. Normalized equal
// siblings can therefore merge without retaining completed transfer history.
func (s *Store) CompactReadDirectory() error {
	var state CausalState
	var expectedControl, expectedDirectory []byte
	var directory []readDelegation
	var self causal.Owner
	if err := s.view(func(tx *Tx) error {
		var err error
		state, expectedControl, err = readCausalControlVersion(tx)
		if err != nil {
			return err
		}
		expectedControl = bytes.Clone(expectedControl)
		expectedDirectory = bytes.Clone(tx.value(meta(readDelegationsKey)))
		directory, err = readDelegations(tx)
		self = causal.Owner{VNode: s.node, Storage: storageID(tx)}
		return err
	}); err != nil || len(directory) == 0 {
		return err
	}
	var possible []uint64
	for _, op := range state.Transfers {
		if op.Cession.Source.Owner == self && op.Phase < causal.TransferIssued {
			possible = append(possible, op.Cession.ID.Sequence)
		} else if op.Cession.Target.Owner == self && op.Phase == causal.TransferReserved {
			possible = append(possible, op.Cession.Ticket)
		}
	}
	slices.Sort(possible)
	possible = slices.Compact(possible)
	for i := range directory {
		directory[i].Stamp = directoryVersionRank(directory[i].Stamp, possible)
	}
	directory = normalizeReadDelegations(directory)
	raw, err := json.Marshal(directory)
	if err != nil || bytes.Equal(raw, expectedDirectory) {
		return err
	}
	// Both inputs matter: directory imports can change hints without changing
	// a permission header. No planning, geometry merge or encoding holds writeMu.
	return s.update(func(tx *Tx) error {
		if !bytes.Equal(controlVersion(tx), expectedControl) || !bytes.Equal(tx.value(meta(readDelegationsKey)), expectedDirectory) {
			return ErrNotReady
		}
		return tx.put(meta(readDelegationsKey), raw)
	})
}

func directoryVersionRank(stamp uint64, possible []uint64) uint64 {
	i := sort.Search(len(possible), func(i int) bool { return possible[i] > stamp })
	if i == 0 {
		return 0
	}
	return possible[i-1]
}
