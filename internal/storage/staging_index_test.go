package storage

import (
	"errors"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// assertStagingIndex fails when a session has staged rows and no index row,
// an index row and no staged row, or a row that does not count its staged
// rows; and when the sessions the writer keeps in memory are not the
// indexed ones. For the end of any test that staged or collected.
func assertStagingIndex(t testing.TB, s *Store) []string {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.view(checkStagingIndex); err != nil {
		t.Fatal(err)
	}
	indexed, err := read(s, indexedSessions)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indexed, s.staged) {
		t.Fatalf("staging sessions in memory %q, indexed %q", s.staged, indexed)
	}
	return indexed
}

// rawStagingUpdate writes control rows as a build without the index did:
// the update commits them and leaves the index alone.
func rawStagingUpdate(t *testing.T, s *Store, fn func(*Tx) error) {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx := newTx(s.db, true)
	if err := tx.commit(fn(tx)); err != nil {
		t.Fatal(err)
	}
}

// The check itself: a staged row without its index row, and an index row
// without a staged row, are both found.
func TestStagingIndexCheckFindsBothMismatches(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "index.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	operation := causal.TransferID{Actor: "disk", Sequence: 1}
	stageRowsForTest(t, store, operation, 3, causal.TransferMoving)
	if got := assertStagingIndex(t, store); !slices.Equal(got, []string{operation.String()}) {
		t.Fatalf("indexed sessions %q", got)
	}
	row := stagingIndexRow(operation.String())
	rawStagingUpdate(t, store, func(tx *Tx) error { return tx.del(row) })
	if err := store.view(checkStagingIndex); err == nil {
		t.Fatal("staged rows without an index row pass the check")
	}
	rawStagingUpdate(t, store, func(tx *Tx) error { return tx.put(row, number(2)) })
	if err := store.view(checkStagingIndex); err == nil {
		t.Fatal("an index row that miscounts the staged rows passes the check")
	}
	rawStagingUpdate(t, store, func(tx *Tx) error {
		if err := tx.put(row, number(3)); err != nil {
			return err
		}
		return tx.put(stagingIndexRow("ghost"), number(1))
	})
	if err := store.view(checkStagingIndex); err == nil {
		t.Fatal("an index row without staged rows passes the check")
	}
}

// The index is about staged content, not about open transfers: a mounted,
// closed transfer keeps its row until the collection deletes its last
// staged row.
func TestStagingIndexFollowsContentNotTransfers(t *testing.T) {
	lot := fencedWarmLot(t)
	session := []string{lot.id.String()}
	if got := assertStagingIndex(t, lot.target); !slices.Equal(got, session) {
		t.Fatalf("after the preload: indexed sessions %q", got)
	}
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); err != nil {
		t.Fatal(err)
	}
	if got := assertStagingIndex(t, lot.target); !slices.Equal(got, session) {
		t.Fatalf("after the mount, before the collection: indexed sessions %q", got)
	}
	if err := lot.target.update(func(tx *Tx) error {
		sessions, err := stagingSessions(tx)
		if err == nil && !slices.Equal(sessions, session) {
			t.Errorf("the writer sees sessions %q", sessions)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := lot.target.updateCausal(func(tx *Tx, state CausalState) error {
		transfer := state.Transfers[lot.id]
		transfer.Phase = causal.TransferInstalled
		state.Transfers[lot.id] = transfer
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
	if err := lot.target.compactStaging(lot.id.String()); err != nil {
		t.Fatal(err)
	}
	if got := assertStagingIndex(t, lot.target); len(got) != 0 {
		t.Fatalf("after the collection: indexed sessions %q", got)
	}
}

// An update that stages and unstages in one transaction, and one that reads
// the sessions between two staging writes, leave the index exact.
func TestStagingIndexWithinOneUpdate(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "index.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	z, _ := tree.NewZoneKey("compactstaging00", "Z")
	held := stagedTable("held", z)
	if err := store.update(func(tx *Tx) error { return held.put(tx, recordPrefix, []byte("a"), []byte(`{}`), nil) }); err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(tx *Tx) error {
		gone := stagedTable("gone", z)
		if err := gone.put(tx, recordPrefix, []byte("a"), []byte(`{}`), nil); err != nil {
			return err
		}
		if sessions, err := stagingSessions(tx); err != nil || !slices.Equal(sessions, []string{"gone", "held"}) {
			t.Errorf("sessions after the first write %q, %v", sessions, err)
		}
		if err := tx.drop(stagingRows("gone")); err != nil {
			return err
		}
		if err := tx.drop(stagingRows("held")); err != nil {
			return err
		}
		if err := stagedTable("new", z).put(tx, recordPrefix, []byte("a"), []byte(`{}`), nil); err != nil {
			return err
		}
		sessions, err := stagingSessions(tx)
		if err == nil && !slices.Equal(sessions, []string{"new"}) {
			t.Errorf("sessions after the drops %q", sessions)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := assertStagingIndex(t, store); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("indexed sessions %q", got)
	}
	// A refused update leaves the index, and the sessions in memory, alone.
	refused := errors.New("refused")
	if err := store.update(func(tx *Tx) error {
		if err := tx.drop(stagingRows("new")); err != nil {
			return err
		}
		if _, err := stagingSessions(tx); err != nil {
			return err
		}
		return refused
	}); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if got := assertStagingIndex(t, store); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("indexed sessions after a refused update %q", got)
	}
}

// A store written by a build without the index has staged rows and no index
// row: the first open with this build counts them, once, and the staging
// then behaves as on a store that always had the index.
func TestStagingIndexBackfillsAStoreOfAnOlderBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	store := openTestData(t, path, "D")
	initTestCausal(t, store)
	moving := causal.TransferID{Actor: "disk", Sequence: 1}
	installed := causal.TransferID{Actor: "disk", Sequence: 2}
	stageRowsForTest(t, store, moving, 5, causal.TransferMoving)
	stageRowsForTest(t, store, installed, stagingBatchRows+3, causal.TransferInstalled)
	// Open refuses the transfers of this fixture, which are no valid offers:
	// they leave the control document for the time the store is closed.
	forget := func(s *Store) {
		t.Helper()
		if err := s.updateCausal(func(tx *Tx, state CausalState) error {
			state.Transfers = nil
			return writeCausal(tx, state)
		}); err != nil {
			t.Fatal(err)
		}
	}
	forget(store)
	// What the older build left: the staged rows, and nothing of the index.
	rawStagingUpdate(t, store, func(tx *Tx) error { return tx.drop(key(stagingIndexBucket)) })
	if err := store.view(checkStagingIndex); err == nil {
		t.Fatal("the fixture is not a store without an index")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestData(t, path, "D")
	want := []string{moving.String(), installed.String()}
	if got := assertStagingIndex(t, store); !slices.Equal(got, want) {
		t.Fatalf("backfilled sessions %q, want %q", got, want)
	}
	// The collection of the installed transfer and a capacity check of new
	// staging both run on the backfilled index (and are compared with a
	// scan, as every commit of a test is).
	stageRowsForTest(t, store, installed, 0, causal.TransferInstalled)
	if err := store.compactStaging(installed.String()); err != nil {
		t.Fatal(err)
	}
	stageRowsForTest(t, store, moving, 7, causal.TransferMoving)
	if err := store.update(store.checkCapacity); err != nil {
		t.Fatal(err)
	}
	if got := assertStagingIndex(t, store); !slices.Equal(got, want[:1]) {
		t.Fatalf("sessions after the collection %q", got)
	}
	forget(store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The backfill ran once: a later open does not scan the staging rows.
	store = openTestData(t, path, "D")
	rawStagingUpdate(t, store, func(tx *Tx) error { return tx.del(stagingIndexRow(moving.String())) })
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "D", "test", 1<<24); err == nil {
		t.Fatal("an open after the backfill rebuilt a damaged index instead of reporting it")
	}
}

// Power cuts at every point of a staging update, of a partial collection
// and of its end: the store that reopens has an exact index, and the same
// sessions whether or not the cut update survived.
func TestStagingIndexSurvivesPowerLoss(t *testing.T) {
	rng := rand.New(rand.NewSource(20261126))
	fs := testkit.NewPowerFS()
	path := filepath.Join(t.TempDir(), "power.db")
	open := func() *Store {
		s, err := OpenWithOptions(path, "D", "test", 1<<30, Options{FS: handoffDisk{fs}, Entropy: rng})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// cut runs step with the power going off after ops more disk operations
	// (0: after the step), and reopens the disk found afterwards.
	cut := func(s *Store, ops int, step func(*Store) error) *Store {
		if ops > 0 {
			fs.FailAt = fs.Ops + ops
		}
		err := step(s)
		if ops == 0 && err != nil {
			t.Fatal(err)
		}
		after := fs.Crash(rng, true)
		fs.FailAt = fs.Ops + 1 // Close adds nothing to the image
		_ = s.Close()
		fs = after
		s = open()
		assertStagingIndex(t, s)
		return s
	}
	// The staged rows of a session and their collection, batch by batch,
	// without a transfer in the control document: Open would refuse the
	// made-up transfers of the other staging tests.
	session := causal.TransferID{Actor: "disk", Sequence: 1}.String()
	stage := func(s *Store) error {
		return s.update(func(tx *Tx) error {
			z, _ := tree.NewZoneKey("compactstaging00", "Z")
			// More than the five batches the cuts below can commit.
			for i := range 6*stagingBatchRows + 9 {
				if err := stagedTable(session, z).put(tx, recordPrefix, number(uint64(i)), []byte(`{}`), nil); err != nil {
					return err
				}
			}
			return tx.put(stagingRows(session), nil)
		})
	}
	left := true
	batch := func(s *Store) error {
		return s.updateCausalKind(commitCollect, func(tx *Tx, _ CausalState) error {
			last, err := dropStagingBatch(tx, session, "")
			left = last != ""
			return err
		})
	}
	store := open()
	initTestCausal(t, store)
	for ops := 1; ops <= 4; ops++ {
		store = cut(store, ops, stage)
	}
	store = cut(store, 0, stage)
	if got := assertStagingIndex(t, store); !slices.Equal(got, []string{session}) {
		t.Fatalf("sessions after the staging %q", got)
	}
	for ops := 1; ops <= 4; ops++ {
		store = cut(store, ops, batch)
	}
	store = cut(store, 0, batch)
	if got := assertStagingIndex(t, store); !slices.Equal(got, []string{session}) {
		t.Fatalf("sessions after a partial collection %q", got)
	}
	for left {
		store = cut(store, 0, batch)
	}
	defer store.Close()
	if got := assertStagingIndex(t, store); len(got) != 0 {
		t.Fatalf("sessions after the collection %q", got)
	}
}
