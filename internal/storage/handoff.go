package storage

import (
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"slices"
)

// zoneSeal is one zone of a frozen region with the digest of its seal, the
// value its ZoneTransfer carries (DataDigest).
type zoneSeal struct {
	Zone   tree.ZoneKey
	Digest causal.Digest
}

// regionDigest is what a frozen region certificate commits to (DataDigest):
// each zone of its inventory with its seal digest, in canonical order. A
// receiver checks it from the seals of the snapshots it is sent and from the
// ZoneTransfers of the zones it already installed, so the region stage
// carries only the zones that did not flip one by one. It carried every
// zone's bytes: 32 MB and more for a 60 000-zone split, refused by the mesh.
func regionDigest(seals []zoneSeal) causal.Digest {
	slices.SortFunc(seals, func(a, b zoneSeal) int { return a.Zone.Compare(b.Zone) })
	return causal.Hash(seals)
}

// transferRows is the prefix of an operation's snapshot rows.
func transferRows(operation causal.TransferID) string {
	return key(transferBucket, []byte(operation.String()))
}

// transferSnapshot is one zone's frozen snapshot; ErrNotReady when the
// operation has none at all.
func transferSnapshot(tx *Tx, operation causal.TransferID, zone tree.ZoneKey) (Snapshot, error) {
	rows := transferRows(operation)
	if !tx.any(rows) {
		return Snapshot{}, ErrNotReady
	}
	var snap Snapshot
	err := tx.readJSON(rows+element(zoneBytes(zone)), &snap)
	recordDecodes.Add(int64(len(snap.Records)))
	return snap, err
}

// dropTransferSnapshots releases the rows once the bytes have been installed
// or collected.
func dropTransferSnapshots(tx *Tx, operation causal.TransferID) error {
	return tx.drop(transferRows(operation))
}

func (s *Store) outgoingTransfer(state CausalState, id causal.TransferID) (CausalTransfer, error) {
	t, ok := state.Transfers[id]
	if !ok || t.Phase == causal.TransferCancelled {
		return t, ErrNotReady
	}
	identity, err := s.Identity()
	if err != nil {
		return t, err
	}
	if t.Cession.Source.Owner != (causal.Owner{VNode: s.node, Storage: identity}) {
		return t, ErrNotOwner
	}
	return t, nil
}
func covers(c causal.Cession, id tree.ID) bool { return c.Region.Contains(id) }
func coversAll(c causal.Cession, snaps []Snapshot) bool {
	for _, s := range snaps {
		if !covers(c, s.Zone.ID) {
			return false
		}
	}
	return true
}
func allInstalled(state CausalState) bool {
	for _, t := range state.Transfers {
		if t.Phase != causal.TransferInstalled {
			return false
		}
	}
	return true
}
func (s *Store) localHolder(tx *Tx, h causal.Endpoint) bool {
	return h.VNode == s.node && h.Storage == storageID(tx)
}
func outgoingFrozen(tx *Tx, state CausalState, z tree.ZoneKey, disk string, permission causal.Digest) (bool, error) {
	for id, t := range state.Transfers {
		if permission != (causal.Digest{}) && t.Cession.Input.ID != permission {
			continue
		}
		if t.Cession.Source.Storage != disk || t.Phase >= causal.TransferInstalled || !t.Cession.Region.Contains(z.ID) {
			continue
		}
		st, ok, e := zoneStateOf(tx, state, id, z)
		if e != nil {
			return false, e
		}
		if ok && st.Frozen {
			return true, nil
		}
		if t.Phase >= causal.TransferSealed {
			return true, nil
		}
	}
	return false, nil
}
