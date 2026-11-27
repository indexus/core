package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"sync/atomic"
)

var (
	stageHeaderKey  = []byte("header")
	stageReplaysKey = []byte("replays")
)

type stageHeader struct {
	Source         string
	SnapshotDigest [32]byte
	SnapshotSeq    uint64
	Certified      *ZoneSeal
}

// stagingRows is the prefix of a transaction's staged copies. Both handoff
// levels stage under the operation; the region install drops them all.
func stagingRows(transaction string) string { return key(stagingBucket, []byte(transaction)) }

func stagedTable(transaction string, z tree.ZoneKey) zoneTable {
	return zoneTable(stagingRows(transaction) + element(zoneBytes(z)))
}

// stagedZone is a staged copy and its header; ErrNotReady when there is none.
func stagedZone(tx *Tx, transaction string, z tree.ZoneKey) (zoneTable, stageHeader, error) {
	t := stagedTable(transaction, z)
	var header stageHeader
	raw := tx.value(t.row(stageHeaderKey))
	if raw == nil {
		return t, header, ErrNotReady
	}
	return t, header, json.Unmarshal(raw, &header)
}

// stagedFrom is the staged copy source made; ErrNotReady when there is none,
// ErrConflict when another source made it.
func stagedFrom(tx *Tx, transaction string, z tree.ZoneKey, source string) (zoneTable, stageHeader, error) {
	t, header, err := stagedZone(tx, transaction, z)
	if err == nil && header.Source != source {
		err = ErrConflict
	}
	return t, header, err
}

// stageOnce stages snap unless source already staged its zone: that copy and
// its mirror progress then stay. staged says which.
func stageOnce(tx *Tx, transaction, source string, snap Snapshot) (t zoneTable, staged bool, err error) {
	t, _, err = stagedFrom(tx, transaction, snap.Zone, source)
	if err == ErrNotReady {
		return t, true, stageSnapshot(tx, transaction, source, snap)
	}
	return t, false, err
}

// receiverClosed is whether this disk refuses incoming copies: it is moving
// to another host or draining in a cold fold. StageZones and StageCausal
// still acknowledge a copy already installed.
func receiverClosed(tx *Tx, state CausalState) bool {
	return state.Moving != nil || tx.value(meta(coldDrainKey)) != nil
}

// prepareSnapshots stages the snapshots a preload brings. It does not check
// the capacity: its caller does, once, when the update has made all its
// writes (PreloadCausal: writeCausalChecked). The check that stood here ran
// the same accounting a second time in the same transaction, 2.12 s and
// 2.08 s of CPU in the receiver profile of 2026-11-26.
func (s *Store) prepareSnapshots(tx *Tx, transaction, source string, snapshots []Snapshot, progress map[string]uint64) error {
	// Reserve empty address space too, atomically with receiver admission.
	if err := tx.put(stagingRows(transaction), nil); err != nil {
		return err
	}
	for _, snap := range snapshots {
		t, staged, err := stageOnce(tx, transaction, source, snap)
		if err != nil {
			return err
		}
		if staged {
			progress[sealKey(snap.Zone)] = snap.Seq
		} else {
			progress[sealKey(snap.Zone)] = t.seq(tx)
		}
	}
	return nil
}

func stageSnapshot(tx *Tx, transaction, source string, snap Snapshot) error {
	seal, err := snap.Seal()
	if err != nil {
		return err
	}
	if transaction == "" || source == "" {
		return ErrConflict
	}
	t, old, err := stagedZone(tx, transaction, snap.Zone)
	if err == nil {
		if old.Source != source || old.SnapshotDigest != seal.Digest || old.SnapshotSeq != snap.Seq {
			return ErrConflict
		}
		return nil
	} else if err != ErrNotReady {
		return err
	}
	for _, r := range snap.Records {
		if err := t.putJSON(tx, recordPrefix, []byte(r.ID), r); err != nil {
			return err
		}
	}
	for _, op := range snap.Requests {
		if err := t.putJSON(tx, receiptPrefix, []byte(op.RequestID), op); err != nil {
			return err
		}
	}
	header := stageHeader{Source: source, SnapshotDigest: seal.Digest, SnapshotSeq: snap.Seq}
	if err := tx.putJSON(t.row(stageHeaderKey), header); err != nil {
		return err
	}
	return tx.put(t.row(sequenceKey), number(snap.Seq))
}

func (s *Store) Mirror(transaction string, z tree.ZoneKey, mutations []Mutation) error {
	return s.MirrorBatch(transaction, []ZoneMutations{{Zone: z, Mutations: mutations}})
}

// MirrorBatch applies several zones' backlogs in one transaction. Each zone is
// checked exactly as by Mirror; a failure rolls back the whole batch.
func (s *Store) MirrorBatch(transaction string, batch []ZoneMutations) error {
	return s.update(func(tx *Tx) error {
		var id causal.TransferID
		if id.UnmarshalText([]byte(transaction)) == nil {
			state, e := readCausal(tx)
			if e != nil {
				return e
			}
			op, ok := state.Transfers[id]
			if !ok || op.Phase < causal.TransferReady || op.Phase >= causal.TransferInstalled {
				return ErrNotReady
			}
		}
		for _, part := range batch {
			if err := mirrorZone(tx, transaction, part.Zone, part.Mutations); err != nil {
				return err
			}
		}
		return s.checkCapacity(tx)
	})
}

func mirrorZone(tx *Tx, transaction string, z tree.ZoneKey, mutations []Mutation) error {
	t, header, err := stagedZone(tx, transaction, z)
	if err != nil {
		return err
	}
	replays := t.row(stageReplaysKey)
	for _, m := range mutations {
		actual, err := recordZone(m.Record)
		if err != nil {
			return err
		}
		if actual != z {
			return ErrConflict
		}
		hash := digest(m)
		replay := replays + element(number(m.Seq))
		if m.Seq <= t.seq(tx) {
			if !bytes.Equal(tx.value(replay), hash[:]) {
				// A retry may overlap the initial snapshot, whose request
				// receipts are durable even though mirror hashes start at S+1.
				var op RequestReceipt
				raw := t.get(tx, receiptPrefix, []byte(m.RequestID))
				if m.Seq > header.SnapshotSeq || raw == nil || json.Unmarshal(raw, &op) != nil || op.RequestID != m.RequestID || op.Seq != m.Seq || op.Collection != z.Collection || op.Location != z.Location || op.Digest != digest(m.Record) {
					return ErrSequence
				}
			}
			continue
		}
		if header.Certified != nil || m.Seq != t.seq(tx)+1 {
			return ErrSequence
		}
		if m.RequestID == "" || t.get(tx, receiptPrefix, []byte(m.RequestID)) != nil {
			return ErrConflict
		}
		if err := t.putJSON(tx, receiptPrefix, []byte(m.RequestID), requestReceipt(m, digest(m.Record))); err != nil {
			return err
		}
		if err := t.putJSON(tx, recordPrefix, []byte(m.Record.ID), m.Record); err != nil {
			return err
		}
		if err := tx.put(t.row(sequenceKey), number(m.Seq)); err != nil {
			return err
		}
		if err := tx.put(replay, hash[:]); err != nil {
			return err
		}
	}
	return nil
}

// certifySnapshot binds the staged copy of snap's zone to snap's seal.
func certifySnapshot(tx *Tx, transaction, source string, snap Snapshot) error {
	seal, err := snap.Seal()
	if err != nil {
		return err
	}
	return certifyZone(tx, transaction, source, seal)
}

func certifyZone(tx *Tx, transaction, source string, seal ZoneSeal) error {
	t, header, err := stagedFrom(tx, transaction, seal.Zone, source)
	if err != nil {
		return err
	}
	snap, receipts, err := stagedSnapshot(tx, t, seal.Zone)
	if err != nil {
		return err
	}
	actual, err := snap.Seal()
	if err != nil {
		return err
	}
	if actual != seal {
		return fmt.Errorf("seal mismatch: actual seq=%d items=%d bytes=%d; expected seq=%d items=%d bytes=%d: %w", actual.Seq, actual.Items, actual.Bytes, seal.Seq, seal.Items, seal.Bytes, ErrSequence)
	}
	if tx.certified != nil {
		tx.certified[seal.Zone] = certifiedCopy{seal: seal, records: snap.Records, receipts: receipts}
	}
	header.Certified = &seal
	return tx.putJSON(t.row(stageHeaderKey), header)
}

// certifiedCopy is the content of a staged copy as its certification read
// it, decoded and found equal to seal. A mount prepares it outside the
// writer (prepareMount) and installs it under the writer without reading
// the staged rows again: mountPreparedTx first checks that the copy's
// header and sequence are the ones the preparation read, and a staged copy
// only changes through one of them (stagingVersion).
type certifiedCopy struct {
	seal     ZoneSeal
	records  []Record
	receipts []stagedReceipt
}

// stagedReceipt is a receipt row of a staged copy, as stored.
type stagedReceipt struct{ key, value []byte }

// stagedReadBacks counts the staged record and receipt rows an install read
// under the writer, for tests.
var stagedReadBacks atomic.Int64

func (s *Store) installStagedZone(tx *Tx, operation string, receipt Receipt) error {
	zone := receipt.Seal.Zone
	staged, header, err := stagedZone(tx, operation, zone)
	if err != nil {
		return err
	}
	if header.Certified == nil || *header.Certified != receipt.Seal || header.Source != receipt.Source {
		return ErrNotReady
	}
	if receipt.Seal.Items < 0 {
		return ErrConflict
	}
	if err := tx.clearZone(zone); err != nil {
		return err
	}
	dest := liveZone(zone)
	if carried, ok := tx.certified[zone]; ok && carried.seal == receipt.Seal {
		err = installCarried(tx, zone, dest, carried)
	} else {
		err = installRead(tx, zone, dest, staged)
	}
	if err != nil {
		return err
	}
	if err := tx.put(dest.row(sequenceKey), number(receipt.Seal.Seq)); err != nil {
		return err
	}
	// Installation can introduce a zone during another warm migration. Keep
	// its discovery checkpoint complete, just as Apply and local seeding do.
	return s.noteMigrationZone(tx, zone)
}

// installCarried writes the content a mount preparation certified: nothing
// of the staged copy is read.
func installCarried(tx *Tx, zone tree.ZoneKey, dest zoneTable, carried certifiedCopy) error {
	for _, record := range carried.records {
		if err := tx.putCleared(zone, record); err != nil {
			return err
		}
	}
	for _, r := range carried.receipts {
		if err := installReceipt(tx, dest, r.key, r.value); err != nil {
			return err
		}
	}
	return nil
}

// installRead writes the content of the staged copy, read and decoded here,
// under the writer: an install without a preparation, or of a zone its
// preparation did not certify.
func installRead(tx *Tx, zone tree.ZoneKey, dest, staged zoneTable) error {
	if err := staged.each(tx, recordPrefix, "", func(_, raw []byte) error {
		var record Record
		stagedReadBacks.Add(1)
		recordDecodes.Add(1)
		err := json.Unmarshal(raw, &record)
		if err == nil {
			err = tx.putCleared(zone, record)
		}
		return err
	}); err != nil {
		return err
	}
	return staged.each(tx, receiptPrefix, "", func(k, v []byte) error {
		stagedReadBacks.Add(1)
		return installReceipt(tx, dest, k, v)
	})
}

// installReceipt copies a staged receipt to the ledger and to the live zone.
func installReceipt(tx *Tx, dest zoneTable, k, v []byte) error {
	global := key(operationsBucket, k)
	if prior := tx.value(global); prior != nil && !bytes.Equal(prior, v) {
		return ErrConflict
	}
	if err := tx.put(global, v); err != nil {
		return err
	}
	return dest.put(tx, receiptPrefix, k, v, nil)
}

// stagingBatchRows bounds one transaction of compactStaging.
const stagingBatchRows = 256

// compactStaging drops the staged copies of an installed transfer, a batch
// per transaction, so unrelated writers wait for one bounded batch at most.
// Each batch scans from where the last one stopped, as compactZoneRows does:
// a scan from the start of the prefix went, under the writer, through every
// row the batches before it had deleted. A pass that finds nothing while
// rows are left starts again from the prefix.
func (s *Store) compactStaging(operation string) error {
	var id causal.TransferID
	if err := id.UnmarshalText([]byte(operation)); err != nil {
		return err
	}
	for from := ""; ; {
		var left bool
		if err := s.view(func(tx *Tx) error {
			state, err := readCausal(tx)
			left = err == nil && stagingCollectable(state.Transfers[id].Phase) && tx.any(stagingRows(operation))
			return err
		}); err != nil || !left {
			return err
		}
		if err := s.updateCausalKind(commitCollect, func(tx *Tx, state CausalState) (err error) {
			if !stagingCollectable(state.Transfers[id].Phase) {
				return ErrNotReady
			}
			from, err = dropStagingBatch(tx, operation, from)
			return err
		}); err != nil {
			return err
		}
	}
}

// stagingCollectable is whether a transfer in this phase no longer needs its
// staged copies.
func stagingCollectable(phase causal.TransferPhase) bool {
	return phase == causal.TransferInstalled || phase == causal.TransferCancelled || phase == causal.TransferReserved
}

// dropStagingBatch deletes up to stagingBatchRows staged rows of operation
// after from, and returns the last one: where the next batch starts. It
// returns "" when it found no row after from.
func dropStagingBatch(tx *Tx, operation, from string) (last string, err error) {
	var paths []string
	if err := tx.scanRows(stagingRows(operation), from, false, func(path string, _ []byte) bool {
		if path != from {
			paths = append(paths, path)
		}
		return len(paths) < stagingBatchRows
	}); err != nil {
		return "", err
	}
	for _, path := range paths {
		if err := tx.del(path); err != nil {
			return "", err
		}
		last = path
	}
	return last, nil
}
