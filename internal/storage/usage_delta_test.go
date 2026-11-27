package storage

import (
	"encoding/json"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// REM-87: a capacity check counts what its update changes, without scanning
// the store, and lands on the scanned numbers. Each step below changes rows
// of a different shape: a new session, receipts that reserve the ledger, a
// receipt already in the ledger, a nested row under an existing row, a
// replaced value, deletions down to an empty session.
func TestCapacityCheckCountsOnlyItsUpdate(t *testing.T) {
	s, holder := newPlanningStore(t, filepath.Join(t.TempDir(), "usage.db"), "D")
	defer s.Close()
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "planning", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		r := Record{Collection: "0000000000000001", Location: fmt.Sprintf("%02x", i), ID: fmt.Sprint("item-", i), Metrics: []float64{1}}
		if _, _, applied, err := s.ApplyRecord(fmt.Sprint("request-", i), r); err != nil || !applied {
			t.Fatalf("apply %d: %v %v", i, applied, err)
		}
	}
	saved := verifyUsage
	verifyUsage = false
	defer func() { verifyUsage = saved }()

	zone := func(loc string) []byte {
		z, _ := tree.NewZoneKey("0000000000000001", loc)
		return zoneBytes(z)
	}
	receipt := func(id, loc string) []byte {
		raw, _ := json.Marshal(RequestReceipt{RequestID: id, Seq: 1, Collection: "0000000000000001", Location: loc})
		return raw
	}
	staged := func(session, loc, id string) string {
		return key(stagingBucket, []byte(session), zone(loc), append([]byte{receiptPrefix}, id...))
	}
	steps := []func(tx *Tx) error{
		func(tx *Tx) error { return tx.put(stagingRows("op-a"), nil) }, // a reservation row
		func(tx *Tx) error {
			for i := range 3 {
				id := fmt.Sprint("incoming-", i)
				if err := tx.put(staged("op-a", "zz", id), receipt(id, "zz")); err != nil {
					return err
				}
			}
			return tx.put(key(stagingBucket, []byte("op-a"), zone("zz"), stageHeaderKey), []byte(`{"Source":"S"}`))
		},
		func(tx *Tx) error { // the same requests in a second session: reserved once
			for i := range 3 {
				id := fmt.Sprint("incoming-", i)
				if err := tx.put(staged("op-b", "zz", id), receipt(id, "zz")); err != nil {
					return err
				}
			}
			return nil
		},
		func(tx *Tx) error { // a receipt the ledger already holds reserves nothing
			id := "request-3"
			v := tx.value(key(operationsBucket, []byte(id)))
			return tx.put(staged("op-a", "03", id), v)
		},
		func(tx *Tx) error { // the ledger learns an incoming request
			return tx.put(key(operationsBucket, []byte("incoming-0")), receipt("incoming-0", "zz"))
		},
		func(tx *Tx) error { // a replaced value
			return tx.put(key(stagingBucket, []byte("op-a"), zone("zz"), stageHeaderKey), []byte(`{"Source":"S2"}`))
		},
		func(tx *Tx) error { return tx.put(key(transferBucket, []byte("op-a"), zone("zz")), []byte("snapshot")) },
		func(tx *Tx) error { return tx.drop(stagingRows("op-b")) },
		func(tx *Tx) error {
			if err := tx.drop(stagingRows("op-a")); err != nil {
				return err
			}
			return tx.drop(key(transferBucket))
		},
	}
	for i, step := range steps {
		before := usageScans.Load()
		err := s.update(func(tx *Tx) error {
			if err := step(tx); err != nil {
				return err
			}
			return s.checkCapacity(tx)
		})
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if n := usageScans.Load() - before; n != 0 {
			t.Fatalf("step %d scanned the usage %d times", i, n)
		}
		assertStoredUsage(t, s)
	}
}

// Conflicting copies of one request are refused, as the scan refuses them.
func TestCapacityCheckRefusesConflictingReceipts(t *testing.T) {
	s, holder := newPlanningStore(t, filepath.Join(t.TempDir(), "conflict.db"), "D")
	defer s.Close()
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "planning", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	if _, _, applied, err := s.ApplyRecord("request-0", Record{Collection: "0000000000000001", Location: "00", ID: "item", Metrics: []float64{1}}); err != nil || !applied {
		t.Fatal(applied, err)
	}
	saved := verifyUsage
	verifyUsage = false
	defer func() { verifyUsage = saved }()
	z, _ := tree.NewZoneKey("0000000000000001", "00")
	err := s.update(func(tx *Tx) error {
		other, _ := json.Marshal(RequestReceipt{RequestID: "request-0", Seq: 9, Collection: z.Collection, Location: z.Location})
		if err := tx.put(key(stagingBucket, []byte("op"), zoneBytes(z), append([]byte{receiptPrefix}, "request-0"...)), other); err != nil {
			return err
		}
		return s.checkCapacity(tx)
	})
	if err != ErrConflict {
		t.Fatalf("conflicting staged receipt: %v, want ErrConflict", err)
	}
}
