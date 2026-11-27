package storage

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/indexus/go-indexus-core/internal/tree"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// Per-zone transfer states under their own keys (XFER-KEYS-1,
// docs/protocol/transfer-state-keys.md). One row per (operation, zone), so a
// batch of a split reads and writes its own zones only. The states lived in
// the control document, one entry per zone of the operation, re-encoded,
// hashed and cloned by every freeze and emit: a 60 000-zone split spent 83 %
// of its time there (docs/benchmarks/elasticity.md, 2026-11-18 evening).
//
// The rows also replace the warm discovery journal: a zone born during a warm
// transfer gets its row, with no flag set, in the transaction of its first
// write (noteMigrationZone).
var (
	zoneStatesBucket   = []byte("zone-states")
	zoneStateVerBucket = []byte("zone-state-ver")
)

const closurePageZones = 512

var errZoneStatesChanged = errors.New("zone states changed during paged scan")

func zoneStatesRows(operation causal.TransferID) string {
	return key(metaBucket, zoneStatesBucket, []byte(operation.String()))
}

func zoneStateVerPath(operation causal.TransferID) string {
	return key(metaBucket, zoneStateVerBucket, []byte(operation.String()))
}

func zoneStatesVersion(tx *Tx, operation causal.TransferID) uint64 {
	raw := tx.value(zoneStateVerPath(operation))
	if len(raw) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

func zoneStatePath(operation causal.TransferID, z tree.ZoneKey) string {
	return zoneStatesRows(operation) + element(zoneBytes(z))
}

// zoneStateOf is the state of z in operation, if it has one. With tx nil
// (a caller that checked no transfer is in progress, such as a bulk ingest)
// there is none.
func zoneStateOf(tx *Tx, state CausalState, operation causal.TransferID, z tree.ZoneKey) (ZoneState, bool, error) {
	if tx == nil {
		return ZoneState{}, false, nil
	}
	raw := tx.value(zoneStatePath(operation, z))
	if raw == nil {
		return ZoneState{}, false, nil
	}
	var st ZoneState
	err := json.Unmarshal(raw, &st)
	return st, err == nil, err
}

func putZoneState(tx *Tx, operation causal.TransferID, st ZoneState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := tx.put(zoneStatePath(operation, st.Zone), raw); err != nil {
		return err
	}
	return tx.put(zoneStateVerPath(operation), number(zoneStatesVersion(tx, operation)+1))
}

// eachZoneStatePaged scans operation's ZoneState rows in bounded read-lock
// pages so concurrent writers on this store never stall behind an O(zones)
// lock hold. It returns the version observed across every page, or
// errZoneStatesChanged if any ZoneState row changed between pages.
func (s *Store) eachZoneStatePaged(operation causal.TransferID, visit func(tx *Tx, state CausalState, st ZoneState) (bool, error)) (uint64, error) {
	prefix := zoneStatesRows(operation)
	from := ""
	var ver uint64
	first := true
	for {
		var stopped bool
		count := 0
		err := s.view(func(tx *Tx) error {
			cached, err := s.causalSnapshot(tx)
			if err != nil {
				return err
			}
			cur := zoneStatesVersion(tx, operation)
			if first {
				ver = cur
				first = false
			} else if cur != ver {
				return errZoneStatesChanged
			}
			var failed error
			err = tx.each(prefix, from, func(path string, raw []byte) bool {
				if path == from {
					return true
				}
				var st ZoneState
				if failed = json.Unmarshal(raw, &st); failed != nil {
					return false
				}
				from = path
				count++
				if st.Acked, failed = zoneAcked(tx, operation, st); failed != nil {
					return false
				}
				keep, err := visit(tx, cached.state, st)
				if err != nil {
					failed = err
					return false
				}
				if !keep {
					stopped = true
					return false
				}
				return count < closurePageZones
			})
			return errors.Join(err, failed)
		})
		if err != nil || stopped || count < closurePageZones {
			return ver, err
		}
	}
}

// eachZoneState visits the states of operation until visit returns false.
func eachZoneState(tx *Tx, state CausalState, operation causal.TransferID, visit func(ZoneState) bool) error {
	return eachZoneStateBelow(tx, state, operation, zoneStatesRows(operation), visit)
}

func eachZoneStateBelow(tx *Tx, state CausalState, operation causal.TransferID, prefix string, visit func(ZoneState) bool) error {
	var failed error
	stopped := false
	err := tx.each(prefix, "", func(_ string, raw []byte) bool {
		var st ZoneState
		if failed = json.Unmarshal(raw, &st); failed != nil {
			return false
		}
		stopped = !visit(st)
		return !stopped
	})
	if err = errors.Join(err, failed); err != nil || stopped {
		return err
	}
	return nil
}

// zoneReady answers ZONE-2 for parent: every zone of the operation below it
// has flipped. Below is IsAncestorOf (tree.Covers): a strict prefix of the
// location, and for a root location every other location of the collection.
// The scan reads parent's subtree only: a leaf reads its own row.
func zoneReady(tx *Tx, state CausalState, operation causal.TransferID, parent tree.ZoneKey) (bool, error) {
	below := parent.Collection + "\x00"
	if !isRootLocation(parent.Location) {
		below += parent.Location
	}
	ready := true
	err := eachZoneStateBelow(tx, state, operation, zoneStatesRows(operation)+escape([]byte(below)), func(st ZoneState) bool {
		if st.Zone.Collection == parent.Collection && st.Zone.Location != parent.Location && !st.Transferred {
			ready = false
		}
		return ready
	})
	return ready, err
}

// compactBatchZones bounds the deletes of one compaction transaction: it
// holds the writer lock; MVCC readers remain independent.
const compactBatchZones = 256

// compactZoneRows drops the per-zone rows of an installed operation, its
// zone states and ZoneTransfers, a batch per commit: they override nothing
// once its region certificate is installed (zoneHolder, writableIn). An idle
// store reads, and does not write. Each batch is read in an MVCC snapshot,
// from where the last one stopped; only its deletes take the exclusive lock.
// On AWS (2026-11-20)
// every batch read from the start inside the writer transaction, through the
// deleted rows of the batches before it: a node installing a large region
// stopped every read and write for 10 to 28 s.
func (s *Store) compactZoneRows(operation causal.TransferID) error {
	for from := ""; ; {
		states, rows, err := s.zoneRowBatch(operation, from)
		if err != nil || len(states) == 0 && len(rows) == 0 {
			return err
		}
		if len(states) > 0 {
			from = states[len(states)-1]
		}
		if err = s.deleteZoneRows(operation, append(rows, states...)); err != nil {
			return err
		}
	}
}

// zoneRowBatch reads, in an MVCC snapshot, every row one batch deletes: up
// to compactBatchZones zone states after from, the ZoneTransfers that exist
// for them and, with the last batch, the version row. The update then
// deletes them without reading them again.
func (s *Store) zoneRowBatch(operation causal.TransferID, from string) (states, rows []string, err error) {
	err = s.view(func(tx *Tx) error {
		var failed error
		err := tx.each(zoneStatesRows(operation), from, func(path string, raw []byte) bool {
			if path == from {
				return true
			}
			states = append(states, path)
			var st ZoneState
			if json.Unmarshal(raw, &st) != nil || st.Lot != 0 {
				return len(states) < compactBatchZones // no ZoneTransfer row of its own
			}
			transfer := zoneTransferPath(st.Zone, operation)
			var payload []byte
			if payload, failed = tx.get(transfer); failed != nil {
				return false
			}
			if payload != nil {
				rows = append(rows, transfer)
			}
			return len(states) < compactBatchZones
		})
		if err != nil || failed != nil {
			return errors.Join(err, failed)
		}
		if len(states) == compactBatchZones {
			return nil
		}
		// The states are done: the lot rows, then the version row.
		lots, err := zoneLotPage(tx, operation, compactBatchZones-len(states))
		if err != nil {
			return err
		}
		rows = append(rows, lots...)
		if len(states)+len(lots) < compactBatchZones && tx.value(zoneStateVerPath(operation)) != nil {
			rows = append(rows, zoneStateVerPath(operation))
		}
		return nil
	})
	return states, rows, err
}

// deleteZoneRows deletes the given rows of a transfer that is installed,
// cancelled or gone.
func (s *Store) deleteZoneRows(operation causal.TransferID, rows []string) error {
	return s.updateCausalKind(commitCollect, func(tx *Tx, state CausalState) error {
		if tr, ok := state.Transfers[operation]; ok && tr.Phase != causal.TransferInstalled && tr.Phase != causal.TransferCancelled {
			return ErrNotReady
		}
		for _, path := range rows {
			if err := tx.set(path, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// AckZoneInstalls records on the source that the receiver acknowledged the
// install of zones: one row per lot they cover, one per zone otherwise,
// nothing else.
func (s *Store) AckZoneInstalls(operation causal.TransferID, zones []tree.ZoneKey) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		var inLots []ZoneState
		for _, z := range zones {
			st, ok, err := zoneStateOf(tx, state, operation, z)
			if err != nil || !ok || !st.Transferred {
				return errors.Join(err, ErrNotReady)
			}
			if st.Acked {
				continue
			}
			if st.Lot != 0 {
				inLots = append(inLots, st)
				continue
			}
			st.Acked = true
			if err := putZoneState(tx, operation, st); err != nil {
				return err
			}
		}
		return ackLots(tx, operation, inLots)
	})
}

// TransferZones are the zone states of operation on this disk.
func (s *Store) TransferZones(operation causal.TransferID) ([]ZoneState, error) {
	return read(s, func(tx *Tx) ([]ZoneState, error) {
		state, err := readCausal(tx)
		if err != nil {
			return nil, err
		}
		var out []ZoneState
		var failed error
		err = eachZoneState(tx, state, operation, func(st ZoneState) bool {
			st.Acked, failed = zoneAcked(tx, operation, st)
			out = append(out, st)
			return failed == nil
		})
		return out, errors.Join(err, failed)
	})
}

// receivedRegionDigestPaged computes the region digest a receiver holds for
// operation in bounded read-lock pages, hashing outside any store lock, and
// returns the ZoneStates version observed across every page.
func (s *Store) receivedRegionDigestPaged(operation causal.TransferID) (causal.Digest, uint64, error) {
	seals := make([]zoneSeal, 0)
	ver, err := s.eachZoneStatePaged(operation, func(tx *Tx, state CausalState, st ZoneState) (bool, error) {
		if !st.Installed {
			return true, nil
		}
		zt, ok, err := zoneTransferOf(tx, state, operation, st.Zone)
		if err != nil || !ok {
			return false, errors.Join(err, ErrConflict)
		}
		seals = append(seals, zoneSeal{Zone: st.Zone, Digest: zt.DataDigest})
		return true, nil
	})
	if err != nil {
		return causal.Digest{}, 0, err
	}
	return regionDigest(seals), ver, nil
}

// FrozenOutgoing lists the outgoing transfers of this disk that are not
// installed and hold frozen zones.
func (s *Store) FrozenOutgoing() ([]causal.TransferID, error) {
	return read(s, func(tx *Tx) ([]causal.TransferID, error) {
		cached, err := s.causalSnapshot(tx)
		if err != nil {
			return nil, err
		}
		var out []causal.TransferID
		for op, t := range cached.state.Transfers {
			if t.Phase >= causal.TransferMoving && t.Phase < causal.TransferInstalled && t.Cession.Source.Storage == cached.storageID {
				out = append(out, op)
			}
		}
		return out, nil
	})
}
