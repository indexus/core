package storage

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// scanStarts records where each scan of the control rows starts.
type scanStarts struct {
	zonelog.Reader
	after []string
}

func (r *scanStarts) ScanWith(collection, zone string, after *zonelog.Record, ids *zonelog.IDRange, load func(zonelog.Record) bool, visit func(zonelog.Record, int) bool) error {
	if collection == kvCollection {
		at := ""
		if ids != nil {
			at = max(ids.From, ids.Prefix)
		}
		if after != nil {
			at = after.ID
		}
		r.after = append(r.after, at)
	}
	return r.Reader.ScanWith(collection, zone, after, ids, load, visit)
}

func stageRowsForTest(t *testing.T, store *Store, operation causal.TransferID, rows int, phase causal.TransferPhase) {
	t.Helper()
	z, err := tree.NewZoneKey("compactstaging00", "Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateCausal(func(tx *Tx, state CausalState) error {
		transfer := state.Transfers[operation]
		transfer.Cession.ID, transfer.Phase = operation, phase
		if state.Transfers == nil {
			state.Transfers = map[causal.TransferID]CausalTransfer{}
		}
		state.Transfers[operation] = transfer
		staged := stagedTable(operation.String(), z)
		for i := range rows {
			r := Record{Collection: z.Collection, Location: z.Location, ID: fmt.Sprintf("r%05d", i)}
			if err := staged.putJSON(tx, recordPrefix, []byte(r.ID), r); err != nil {
				return err
			}
		}
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
}

// A batch of compactStaging scans from where the batch before it stopped,
// not from the start of the prefix through the rows already deleted: under
// the writer, that made the collection of a large install quadratic.
func TestDropStagingBatchResumesAfterTheLastRow(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "staging.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	operation := causal.TransferID{Actor: "disk", Sequence: 1}
	stageRowsForTest(t, store, operation, 2*stagingBatchRows+7, causal.TransferInstalled)

	reader := &scanStarts{Reader: store.db}
	tx := store.transaction(reader, true)
	first, err := dropStagingBatch(tx, operation.String(), "")
	if err != nil || first == "" || len(tx.pending) != stagingBatchRows {
		t.Fatalf("first batch: last %q, %d deletes, %v", first, len(tx.pending), err)
	}
	reader.after = nil
	second, err := dropStagingBatch(tx, operation.String(), first)
	if err != nil || second <= first || len(tx.pending) != 2*stagingBatchRows {
		t.Fatalf("second batch: last %q after %q, %d deletes, %v", second, first, len(tx.pending), err)
	}
	// The row at the cursor is the last deleted one: nothing before it is read.
	for _, after := range reader.after {
		if after < first[:len(first)-1] {
			t.Fatalf("the second batch scanned from %q, before the cursor %q", after, first)
		}
	}
	if len(reader.after) == 0 {
		t.Fatal("the second batch scanned nothing")
	}
}

// Every staged row goes, batch boundaries included, the usage stays exact
// (every commit of a test compares it with a scan), and a transfer still
// moving keeps its staging.
func TestCompactStagingDropsEveryBatch(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "staging.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	operation := causal.TransferID{Actor: "disk", Sequence: 1}
	const rows = 3*stagingBatchRows + 41
	left := func() (n int) {
		_ = store.view(func(tx *Tx) error {
			return tx.each(stagingRows(operation.String()), "", func(string, []byte) bool { n++; return true })
		})
		return n
	}
	stageRowsForTest(t, store, operation, rows, causal.TransferMoving)
	if err := store.compactStaging(operation.String()); err != nil || left() != rows {
		t.Fatalf("staging of a moving transfer: %d rows left of %d, %v", left(), rows, err)
	}
	stageRowsForTest(t, store, operation, rows, causal.TransferInstalled)
	if err := store.compactStaging(operation.String()); err != nil || left() != 0 {
		t.Fatalf("%d staged rows left after compaction, %v", left(), err)
	}
	_, usage, err := store.CapacityUsage()
	if err != nil || usage.Staging != 0 {
		t.Fatalf("staging usage %d after compaction, %v", usage.Staging, err)
	}
	if sessions := assertStagingIndex(t, store); len(sessions) != 0 {
		t.Fatalf("staging sessions %q indexed after compaction", sessions)
	}
}

// BenchmarkCompactStaging collects the staging of one installed transfer of
// 16,384 staged rows (64 batches).
func BenchmarkCompactStaging(b *testing.B) {
	for i := range b.N {
		b.StopTimer()
		store, err := OpenWithOptions(filepath.Join(b.TempDir(), fmt.Sprint("staging", i)), "D", "test", 1<<30, Options{NoSync: true})
		if err != nil {
			b.Fatal(err)
		}
		holder, _ := store.Endpoint("D")
		if err := store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
			b.Fatal(err)
		}
		operation := causal.TransferID{Actor: "disk", Sequence: 1}
		z, _ := tree.NewZoneKey("compactstaging00", "Z")
		if err := store.updateCausal(func(tx *Tx, state CausalState) error {
			state.Transfers = map[causal.TransferID]CausalTransfer{operation: {}}
			transfer := state.Transfers[operation]
			transfer.Cession.ID, transfer.Phase = operation, causal.TransferInstalled
			state.Transfers[operation] = transfer
			staged := stagedTable(operation.String(), z)
			for r := range 64 * stagingBatchRows {
				if err := staged.put(tx, recordPrefix, []byte(fmt.Sprintf("r%06d", r)), []byte(`{"ID":"x"}`), nil); err != nil {
					return err
				}
			}
			return writeCausal(tx, state)
		}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := store.compactStaging(operation.String()); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		store.Close()
	}
}
