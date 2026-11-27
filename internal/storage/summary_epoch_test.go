package storage

import (
	"errors"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

func TestSummaryEpochConcurrentAttachmentsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	store, err := Open(path, "node", "summary-epoch", 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	originalHolder, err := store.Endpoint("same-host")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 32
	results := make(chan uint64, callers)
	failures := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			epoch, err := store.NextSummaryEpoch()
			if err != nil {
				failures <- err
				return
			}
			results <- epoch
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var epochs []uint64
	for epoch := range results {
		epochs = append(epochs, epoch)
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	if len(epochs) != callers {
		t.Fatalf("epochs=%d want %d", len(epochs), callers)
	}
	for i, epoch := range epochs {
		if epoch != uint64(i+1) {
			t.Fatalf("epoch=%d want %d", epoch, i+1)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, "node", "summary-epoch", 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	reopenedHolder, err := store.Endpoint("same-host")
	if err != nil || reopenedHolder != originalHolder {
		t.Fatalf("summary incarnation changed durable holder: %v %v", reopenedHolder, err)
	}
	next, err := store.NextSummaryEpoch()
	if err != nil || next != callers+1 {
		t.Fatalf("after reopen epoch=%d err=%v", next, err)
	}
}

func TestSummaryEpochRollbackDoesNotPublishOrConsumeIncarnation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), "node", "summary-epoch", 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.NextSummaryEpoch()
	if err != nil || first != 1 {
		t.Fatal(first, err)
	}
	aborted := errors.New("injected transaction rollback")
	err = store.update(func(tx *Tx) error {
		next, err := advanceSummaryEpoch(tx)
		if err != nil {
			return err
		}
		if next != 2 {
			t.Fatalf("next=%d", next)
		}
		return aborted
	})
	if !errors.Is(err, aborted) {
		t.Fatal(err)
	}
	next, err := store.NextSummaryEpoch()
	if err != nil || next != 2 {
		t.Fatalf("rollback consumed epoch: %d %v", next, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if epoch, err := store.NextSummaryEpoch(); epoch != 0 || err == nil {
		t.Fatalf("closed commit exposed epoch=%d err=%v", epoch, err)
	}
}

func TestSummaryEpochCorruptionAndOverflowFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{{"corrupt", []byte{1, 2, 3}}, {"overflow", number(math.MaxUint64)}} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "data.db"), "node", "summary-epoch", 1<<24)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.update(func(tx *Tx) error { return tx.put(meta(summaryEpochKey), tc.raw) }); err != nil {
				t.Fatal(err)
			}
			epoch, err := store.NextSummaryEpoch()
			if epoch != 0 || !errors.Is(err, causal.ErrConflict) {
				t.Fatalf("epoch=%d err=%v", epoch, err)
			}
			if err = store.view(func(tx *Tx) error {
				raw := tx.value(meta(summaryEpochKey))
				if string(raw) != string(tc.raw) {
					t.Fatal("failed allocation changed counter")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
