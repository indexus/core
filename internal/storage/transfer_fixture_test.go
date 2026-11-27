package storage

import (
	"encoding/json"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
)

// loadTransfer is the transfer of state with its snapshot rows.
func loadTransfer(tx *Tx, state CausalState, operation causal.TransferID) (CausalTransfer, bool, error) {
	transfer, ok := state.Transfers[operation]
	if !ok {
		return transfer, false, nil
	}
	transfer.Snapshots = nil
	var failed error
	err := tx.each(transferRows(operation), "", func(_ string, raw []byte) bool {
		var snap Snapshot
		failed = json.Unmarshal(raw, &snap)
		transfer.Snapshots = append(transfer.Snapshots, snap)
		return failed == nil
	})
	return transfer, true, errors.Join(err, failed)
}

func (s *Store) LoadCausalTransfer(operation causal.TransferID) (CausalTransfer, bool, error) {
	var transfer CausalTransfer
	var ok bool
	err := s.view(func(tx *Tx) error {
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		transfer, ok, err = loadTransfer(tx, state, operation)
		return err
	})
	return transfer, ok, err
}
