package storage

import (
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

func openTestData(t testing.TB, path, node string) *Store {
	t.Helper()
	s, err := Open(path, node, "test", 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func initTestCausal(t *testing.T, store *Store) {
	t.Helper()
	holder, err := store.Endpoint(store.node)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.InitCausal(tree.VNodeDescriptor{Name: store.node, ID: tree.ID{1}}, causal.Genesis{Cluster: store.cluster, Holder: holder}); err != nil {
		t.Fatal(err)
	}
}

func TestOmitHistoryKeepsTheLiveRecordOnly(t *testing.T) {
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "quiet.db"), "D", "test", 1<<24, Options{OmitHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initTestCausal(t, store)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "row"}
	if _, err = store.Write("once", record); err != nil {
		t.Fatal(err)
	}
	zone, err := recordZone(record)
	if err != nil {
		t.Fatal(err)
	}
	err = store.view(func(tx *Tx) error {
		if tx.any(key(historyBucket, zoneBytes(zone))) {
			t.Fatal("mutation log was written")
		}
		if r, err := tx.record(zone, record.ID); r == nil || err != nil {
			t.Fatal("live record missing")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCapacityIncludesRetryHistoryAndRollsBackRejectedWrite(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "bounded.db"), "D", "test", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initTestCausal(t, store)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "same-row"}
	accepted := uint64(0)
	for i := 0; i < 100; i++ {
		request := fmt.Sprintf("request-%d", i)
		m, err := store.Write(request, record)
		if errors.Is(err, ErrDataCapacity) {
			z, _ := recordZone(record)
			snap, err := store.Snapshot(z)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Seq != accepted || len(snap.Requests) != int(accepted) {
				t.Fatal("rejected write entered durable state")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		accepted = m.Seq
	}
	t.Fatal("retry history escaped the capacity bound")
}

func stagedProgress(store *Store, operation string, zone tree.ZoneKey) (uint64, error) {
	var seq uint64
	err := store.view(func(tx *Tx) error {
		t, _, err := stagedZone(tx, operation, zone)
		if err != nil {
			return err
		}
		seq = t.seq(tx)
		return nil
	})
	return seq, err
}

// A materialized topology is routing information, never write authority.
func TestUninitializedStoreCannotWriteFromTopologyAlone(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "uninitialized.db"), "D")
	defer store.Close()
	view, err := tree.NewViewFrom(1, []tree.VNodeDescriptor{{Name: "D", ID: tree.ID{1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.update(func(tx *Tx) error { return tx.putJSON(meta(viewKey), encodeView(view)) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Apply("attempt", Record{Collection: "Demo000000000000", Location: "aa", ID: "row"}); err == nil {
		t.Fatal("topology alone authorized a write")
	}
	if err = store.view(func(tx *Tx) error {
		if tx.any(key(liveBucket)) || tx.any(key(operationsBucket)) {
			t.Fatal("refused write changed durable data")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
