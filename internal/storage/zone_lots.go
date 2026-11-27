package storage

import (
	"encoding/json"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Per-lot transfer rows (docs/design/lot-rows.md). A fence and a mount commit
// one row for their lot: the zones it moves, each with the sequence and the
// digest of its seal. That row replaces the ZoneTransfer row each zone had on
// both disks, and on the source it takes the acknowledgement of the lot's
// install, which rewrote each zone's state row. A zone's state row names its
// lot (ZoneState.Lot), so a zone is still looked up, fenced and resumed on
// its own: the lot row is read for its proof and its acknowledgement only.
//
// A store written before lot rows holds ZoneTransfer rows and no lot: a
// state without a lot reads them as before, and a transfer may hold both.
var zoneLotsBucket = []byte("zone-lots")

// lotZone is what a ZoneTransfer holds that its operation does not.
type lotZone struct {
	Zone       tree.ZoneKey
	Seq        uint64
	DataDigest causal.Digest
}

// zoneLot is the row of one lot. Acked, on the source, is that the receiver
// acknowledged the install of every zone of the lot.
type zoneLot struct {
	Zones []lotZone
	Acked bool `json:",omitempty"`
}

func zoneLotsRows(operation causal.TransferID) string {
	return key(metaBucket, zoneLotsBucket, []byte(operation.String()))
}

func zoneLotPath(operation causal.TransferID, lot uint64) string {
	return zoneLotsRows(operation) + element(number(lot))
}

// nextZoneLot is the number of the lot this update is about to write: the
// states version it will pass, which no earlier lot of the operation has.
func nextZoneLot(tx *Tx, operation causal.TransferID) uint64 {
	return zoneStatesVersion(tx, operation) + 1
}

// zoneLotOf is lot of operation, decoded once per transaction. A state that
// names a missing lot is corrupt: an error, never a zone without a fence.
func zoneLotOf(tx *Tx, operation causal.TransferID, lot uint64) (zoneLot, error) {
	path := zoneLotPath(operation, lot)
	if row, ok := tx.lots[path]; ok {
		return row, nil
	}
	raw := tx.value(path)
	if raw == nil {
		return zoneLot{}, ErrConflict
	}
	var row zoneLot
	if err := json.Unmarshal(raw, &row); err != nil {
		return zoneLot{}, err
	}
	tx.keepLot(path, row)
	return row, nil
}

func (tx *Tx) keepLot(path string, row zoneLot) {
	if tx.lots == nil {
		tx.lots = map[string]zoneLot{}
	}
	tx.lots[path] = row
}

// putZoneLot writes a lot row. It changes what the zone states of operation
// mean (their proofs, their acknowledgement), so it moves their version too:
// the paged scans of the region fence and seal are checked against it.
func putZoneLot(tx *Tx, operation causal.TransferID, lot uint64, row zoneLot) error {
	path := zoneLotPath(operation, lot)
	if err := tx.putJSON(path, row); err != nil {
		return err
	}
	tx.keepLot(path, row)
	return tx.put(zoneStateVerPath(operation), number(zoneStatesVersion(tx, operation)+1))
}

// lotTransfer is the ZoneTransfer of z in lot: the fields a row per zone
// held are those of the operation's cession, checked equal at the install.
func lotTransfer(tx *Tx, state CausalState, operation causal.TransferID, lot uint64, z tree.ZoneKey) (causal.ZoneTransfer, bool, error) {
	transfer, ok := state.Transfers[operation]
	if !ok {
		return causal.ZoneTransfer{}, false, nil
	}
	row, err := zoneLotOf(tx, operation, lot)
	if err != nil {
		return causal.ZoneTransfer{}, false, err
	}
	for _, moved := range row.Zones {
		if moved.Zone == z {
			c := transfer.Cession
			return causal.ZoneTransfer{Parent: c.Input.ID, Operation: operation, Ticket: c.Ticket, Zone: z, From: c.Source, To: c.Target, Seq: moved.Seq, DataDigest: moved.DataDigest}, true, nil
		}
	}
	return causal.ZoneTransfer{}, false, ErrConflict
}

// zoneAcked is whether the source knows st's zone installed: by its lot's
// row, or by its own (a zone fenced before lot rows, or a lot acknowledged
// in parts by a resume that cut it differently).
func zoneAcked(tx *Tx, operation causal.TransferID, st ZoneState) (bool, error) {
	if st.Acked || st.Lot == 0 {
		return st.Acked, nil
	}
	row, err := zoneLotOf(tx, operation, st.Lot)
	return row.Acked, err
}

// ackLots records the acknowledged installs of states, zones that name a lot
// and are not acknowledged yet: one row for a lot they cover, their own rows
// for a lot they cover in part.
func ackLots(tx *Tx, operation causal.TransferID, states []ZoneState) error {
	byLot := map[uint64]map[tree.ZoneKey]ZoneState{}
	for _, st := range states {
		if byLot[st.Lot] == nil {
			byLot[st.Lot] = map[tree.ZoneKey]ZoneState{}
		}
		byLot[st.Lot][st.Zone] = st
	}
	for lot, acked := range byLot {
		row, err := zoneLotOf(tx, operation, lot)
		if err != nil {
			return err
		}
		if row.Acked {
			continue
		}
		if len(acked) == len(row.Zones) {
			row.Acked = true
			if err := putZoneLot(tx, operation, lot, row); err != nil {
				return err
			}
			continue
		}
		for _, st := range acked {
			st.Acked = true
			if err := putZoneState(tx, operation, st); err != nil {
				return err
			}
		}
	}
	return nil
}

// validZoneTransfer refuses a proof that names no operation, ticket, parent
// right, content or disk.
func validZoneTransfer(p causal.ZoneTransfer) error {
	if !p.Operation.Valid() || p.Ticket == 0 || p.Parent == (causal.Digest{}) || p.DataDigest == (causal.Digest{}) || !p.From.Valid() || !p.To.Valid() {
		return ErrConflict
	}
	return nil
}

// zoneLotPage is up to limit lot rows of operation, for their deletion.
func zoneLotPage(tx *Tx, operation causal.TransferID, limit int) (paths []string, err error) {
	prefix := zoneLotsRows(operation)
	err = tx.scanRows(prefix, "", false, func(path string, _ []byte) bool {
		if path != prefix {
			paths = append(paths, path)
		}
		return len(paths) < limit
	})
	return paths, err
}
