package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

func (s *Store) Certify(transaction, source string, seal ZoneSeal) (Receipt, error) {
	receipts, err := s.CertifyBatch(transaction, source, []ZoneSeal{seal})
	if err != nil {
		return Receipt{}, err
	}
	return receipts[0], nil
}

// zoneStates are the zone states of operation, by sealKey.
func zoneStates(t testing.TB, s *Store, operation causal.TransferID) map[string]ZoneState {
	t.Helper()
	list, err := s.TransferZones(operation)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ZoneState{}
	for _, st := range list {
		out[sealKey(st.Zone)] = st
	}
	return out
}

func (s *Store) CertifyBatch(transaction, source string, seals []ZoneSeal) ([]Receipt, error) {
	receipts := make([]Receipt, 0, len(seals))
	err := s.update(func(tx *Tx) error {
		for _, seal := range seals {
			if err := certifyZone(tx, transaction, source, seal); err != nil {
				return err
			}
			receipts = append(receipts, Receipt{Source: source, Target: s.node, Seal: seal})
		}
		return s.checkCapacity(tx)
	})
	if err != nil {
		return nil, err
	}
	return receipts, nil
}

func (s *Store) Mutations(z tree.ZoneKey, after, through uint64) ([]Mutation, error) {
	var out []Mutation
	err := s.view(func(tx *Tx) error {
		var err error
		out, err = mutationsTx(tx, z, after, through)
		return err
	})
	return out, err
}

// BulkReceipts lists the installed batches.
func (s *Store) BulkReceipts() ([]BulkReceipt, error) {
	var out []BulkReceipt
	err := s.view(func(tx *Tx) error {
		var failed error
		err := tx.each(key(metaBucket, bulkBucket), "", func(_ string, raw []byte) bool {
			var receipt BulkReceipt
			failed = json.Unmarshal(raw, &receipt)
			out = append(out, receipt)
			return failed == nil
		})
		return errors.Join(err, failed)
	})
	return out, err
}

func (b *diskBatch) apply(fn func(*Tx) error) error {
	return b.submit(context.Background(), 1, fn)
}
