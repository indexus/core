package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestConcurrentCollectionSharesOnlyAnActiveTransfer(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"failure", errors.New("collection failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var store Store
				first := causal.TransferID{Actor: "disk", Sequence: 1}
				second := causal.TransferID{Actor: "disk", Sequence: 2}
				release := make(chan struct{})
				results := make(chan error, 3)
				go func() {
					results <- store.collectOnce(first, func(causal.TransferID) error {
						<-release
						return tc.err
					})
				}()
				synctest.Wait()
				for range 2 {
					go func() {
						results <- store.CollectTransferred(first)
					}()
				}
				synctest.Wait()
				if len(results) != 0 {
					t.Fatal("a caller returned before the first collection finished")
				}
				if err := store.collectOnce(second, func(causal.TransferID) error { return nil }); err != nil {
					t.Fatalf("independent transfer: %v", err)
				}
				close(release)
				for range 3 {
					if err := <-results; err != tc.err {
						t.Fatalf("got %v, want shared result %v", err, tc.err)
					}
				}
				later := errors.New("new attempt")
				if err := store.collectOnce(first, func(causal.TransferID) error { return later }); err != later {
					t.Fatalf("later attempt returned %v", err)
				}
			})
		})
	}
}

// Maintenance asks to collect every installed transfer on each tick. With no
// staged bytes left that must not commit: a commit every tick voids the caches
// keyed by the transaction id and made an idle node rescan all its zones.
func TestCollectTransferredWithoutStagedBytesDoesNotCommit(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "idle.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	txid := func() int {
		var id int
		_ = store.view(func(tx *Tx) error { id = tx.ID(); return nil })
		return id
	}
	before := txid()
	for range 3 {
		if err := store.CollectTransferred(causal.TransferID{Actor: "old-disk", Sequence: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if after := txid(); after != before {
		t.Fatalf("collecting nothing committed %d transactions", after-before)
	}
}

// When an installed transfer holds an exact multiple of collectBatchZones
// (256) rows, CollectTransferred must still run the final last=true step to
// drop transfer snapshots and compact the transfer entry.
func TestCollectTransferredExactBatchMultipleRunsFinalStep(t *testing.T) {
	source, target, g := ticketStores(t)
	for i := range 256 {
		record := Record{Collection: "aaaaaaaaaaaaaaaa", Location: fmt.Sprintf("A%04d", i), ID: fmt.Sprint(i)}
		if _, e := source.Write(fmt.Sprint("w", i), record); e != nil {
			t.Fatal(e)
		}
	}
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	ticketFinish(t, source, target, op.Cession.ID)
	if e := source.CollectTransferred(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	state, e := source.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := state.Transfers[op.Cession.ID]; ok {
		t.Fatal("exact256 final collection skipped")
	}
}

// A restart between the last batch and the final step leaves an installed
// transfer with no rows and its fence flags still set. The next collection
// must finish it from the durable state, then commit nothing more.
func TestCollectTransferredFinishesAfterRestartBeforeFinalStep(t *testing.T) {
	source, target, g := ticketStores(t)
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	ticketFinish(t, source, target, op.Cession.ID)
	path := source.path
	if e := source.Close(); e != nil {
		t.Fatal(e)
	}
	source = openTestData(t, path, "A")
	defer source.Close()
	if e := source.CollectTransferred(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	before := 0
	_ = source.view(func(tx *Tx) error { before = tx.ID(); return nil })
	for range 3 {
		if e := source.CollectTransferred(op.Cession.ID); e != nil {
			t.Fatal(e)
		}
	}
	_ = source.view(func(tx *Tx) error {
		if tx.ID() != before {
			t.Fatal("idle collection committed")
		}
		return nil
	})
}

// LastTransferStep moves on an updateCausal commit that writes, and not on
// one that changes nothing: a stalled transfer polled every tick must not
// look alive to autoscale.
func TestLastTransferStepCountsOnlyCommitsThatWrite(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "step.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	before := store.LastTransferStep()
	if err := store.updateCausal(func(*Tx, CausalState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := store.LastTransferStep(); !got.Equal(before) {
		t.Fatalf("an empty updateCausal moved LastTransferStep from %v to %v", before, got)
	}
	op := causal.TransferID{Actor: "disk", Sequence: 1}
	if err := store.updateCausal(func(tx *Tx, state CausalState) error {
		tr := CausalTransfer{}
		tr.Cession.ID = op
		state.Transfers[op] = tr
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
	if got := store.LastTransferStep(); !got.After(before) {
		t.Fatalf("a writing updateCausal left LastTransferStep at %v", got)
	}
}
