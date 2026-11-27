package storage

import "github.com/indexus/go-indexus-core/internal/causal"

// HasDelegatableRight is an advisory autoscale sample, never permission to
// cede a right. BeginCausal rechecks the actual input transactionally. An
// incoming transfer does not pin unrelated installed native pieces: a new
// vnode may already delegate them while the rest of its join is still copying.
func (s *Store) HasDelegatableRight() (bool, error) {
	return read(s, func(tx *Tx) (bool, error) {
		cached, err := s.causalSnapshot(tx)
		if err != nil {
			return false, err
		}
		if cached.state.Moving != nil || coldBusy(tx) {
			return false, nil
		}
		pinned := make(map[causal.Digest]bool)
		for _, op := range cached.state.Transfers {
			if op.Cession.Source.Storage == cached.storageID && op.Phase < causal.TransferInstalled {
				pinned[op.Cession.Input.ID] = true
			}
		}
		for _, right := range cached.state.Permissions.Native {
			if !pinned[right.ID] {
				return true, nil
			}
		}
		return false, nil
	})
}
