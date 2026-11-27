package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
)

var localSeedSealed = []byte("local-seed-sealed")

// SeedLocalRecords builds a new, offline local dataset in atomic batches.
// It is not a client write: no client ACKs or retry receipts are fabricated.
// Existing different rows, any mutation history and sealed seeds are refused.
// Normal mutation writes and handoffs retain their existing durable path.
func (s *Store) SeedLocalRecords(records []Record) error {
	return s.update(func(tx *Tx) error {
		if tx.value(meta(localSeedSealed)) != nil {
			return fmt.Errorf("local seed already sealed")
		}
		if tx.any(key(historyBucket)) {
			return fmt.Errorf("cannot seed a node with mutation history")
		}
		if tx.any(key(operationsBucket)) {
			return fmt.Errorf("cannot seed a node with retry receipts")
		}
		usage, err := s.storedDataUsage(tx)
		if err != nil {
			return err
		}
		for _, r := range records {
			z, err := recordZone(r)
			if err != nil {
				return err
			}
			if err = s.causalWritable(tx, z); err != nil {
				return err
			}
			raw, err := json.Marshal(r)
			if err != nil {
				return err
			}
			zone := liveZone(z)
			fresh := tx.value(zone.row(sequenceKey)) == nil
			prior, err := tx.record(z, r.ID)
			if err != nil {
				return err
			}
			if prior != nil {
				old, err := json.Marshal(prior.record)
				if err != nil {
					return err
				}
				if !bytes.Equal(old, raw) {
					return ErrConflict
				}
				continue
			}
			_, delta, err := tx.putRecord(z, r)
			if err != nil {
				return err
			}
			usage.Owned += delta
			seq := number(zone.seq(tx) + 1)
			if err = tx.putCounted(zone.row(sequenceKey), sequenceKey, seq, &usage.Owned); err != nil {
				return err
			}
			if fresh {
				if err = s.noteMigrationZone(tx, z); err != nil {
					return err
				}
			}
		}
		if usage.PhysicalLoad() > s.capacity {
			return ErrDataCapacity
		}
		return tx.putJSON(meta(usageKey), usage)
	})
}
func (s *Store) SealLocalSeed() error {
	return s.update(func(tx *Tx) error { return tx.put(meta(localSeedSealed), []byte{1}) })
}
