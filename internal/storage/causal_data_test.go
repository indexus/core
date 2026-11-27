package storage

import (
	"context"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
)

func TestBatchedAppliesKeepReceiptsAndContiguousHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.db")
	store, err := OpenWithOptions(path, "D", "test", 1<<26, Options{NoSync: true, BatchWrites: true, BatchSize: 64, BatchDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	holder, err := store.Endpoint("D")
	if err != nil {
		t.Fatal(err)
	}
	root := tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}
	if err = store.InitCausal(root, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	const writes = 64
	var wg sync.WaitGroup
	errs := make(chan error, writes+3)
	for i := 0; i < writes; i++ {
		wg.Go(func() {
			record := Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprintf("row-%d", i)}
			_, _, e := store.Apply(fmt.Sprintf("request-%d", i), record)
			errs <- e
		})
	}
	wg.Go(func() {
		_, _, e := store.Apply("", Record{Collection: "Demo000000000000", Location: "aa", ID: "invalid"})
		errs <- e
	})
	for i := range 2 {
		wg.Go(func() {
			_, _, e := store.Apply("conflicting-request", Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprintf("conflict-%d", i)})
			errs <- e
		})
	}
	wg.Wait()
	close(errs)
	failures := 0
	for err := range errs {
		if err != nil {
			failures++
		}
	}
	if failures != 2 {
		t.Fatalf("failures=%d want empty request and one conflicting callback", failures)
	}
	zone, _ := recordZone(Record{Collection: "Demo000000000000", Location: "aa", ID: "probe"})
	snapshot, err := store.Snapshot(zone)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Seq != writes+1 || len(snapshot.Records) != writes+1 || len(snapshot.Requests) != writes+1 {
		t.Fatalf("batch lost atomic history: seq=%d records=%d requests=%d", snapshot.Seq, len(snapshot.Records), len(snapshot.Requests))
	}
	assertStoredUsage(t, store)
	if _, _, err = store.Apply("replace-row", Record{Collection: "Demo000000000000", Location: "aa", ID: "row-0", Metrics: []float64{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	assertStoredUsage(t, store)
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, "D", "test", 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < writes; i++ {
		record := Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprintf("row-%d", i)}
		mutation, applied, e := store.Apply(fmt.Sprintf("request-%d", i), record)
		if e != nil || applied || mutation.Seq == 0 {
			t.Fatalf("receipt %d not replayable: mutation=%+v applied=%v err=%v", i, mutation, applied, e)
		}
	}
}

func assertStoredUsage(t *testing.T, store *Store) {
	t.Helper()
	err := store.view(func(tx *Tx) error {
		stored, err := store.storedDataUsage(tx)
		if err != nil {
			return err
		}
		actual, err := store.dataUsage(tx)
		if err != nil {
			return err
		}
		if stored != actual {
			t.Fatalf("incremental usage=%+v scanned=%+v", stored, actual)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewZoneRegistrationKeepsCachedControlImmutable(t *testing.T) {
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "zones.db"), "D", "test", 1<<26, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	holder, err := store.Endpoint("D")
	if err != nil {
		t.Fatal(err)
	}
	g := causal.Genesis{Cluster: "test", Holder: holder}
	if err := store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, g); err != nil {
		t.Fatal(err)
	}
	op, beginErr := store.BeginCausal(context.Background(), g.Right().ID, causal.Endpoint{Owner: causal.Owner{VNode: "J", Storage: "disk-j"}, Host: "J"}, tree.Region{})
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	_, err = store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Collection: "0AABCDEFGH123456", Location: "W000000000000000", ID: "new-zone"}
	zone, err := recordZone(record)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("rollback registration")
	err = store.update(func(tx *Tx) error {
		if err := store.noteMigrationZone(tx, zone); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("rollback: %v", err)
	}
	if _, ok := zoneStates(t, store, op.Cession.ID)[sealKey(zone)]; ok {
		t.Fatal("rollback persisted the registration")
	}
	if _, applied, err := store.Apply("new-zone", record); err != nil || !applied {
		t.Fatalf("apply: %v applied=%v", err, applied)
	}
	if _, ok := zoneStates(t, store, op.Cession.ID)[sealKey(zone)]; !ok {
		t.Fatal("committed new zone was not registered with the warm handoff")
	}
}

func TestDefaultDataUsesDurableGroupCommit(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "default.db"), "D", "test", 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.writes == nil || store.writes.size != 256 || store.writes.delay != 2*time.Millisecond {
		t.Fatal("default storage must batch writes without disabling disk synchronization")
	}
}
