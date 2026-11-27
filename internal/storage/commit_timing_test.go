package storage

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCommitTimingSeparatesChangesNoopsAndFailures(t *testing.T) {
	db, err := openTestKV(t, filepath.Join(t.TempDir(), "disk"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := db.s
	failure := errors.New("reject")
	for _, kind := range []commitKind{commitFence, commitMount, commitCollect} {
		before := s.db.Seq()
		changed, err := s.updateWroteKind(kind, func(tx *Tx) error {
			return tx.put(meta([]byte{byte(kind)}), []byte("committed"))
		})
		if err != nil || !changed || s.db.Seq() != before+1 {
			t.Fatalf("kind %d: changed=%v err=%v seq=%d before=%d", kind, changed, err, s.db.Seq(), before)
		}
		if changed, err = s.updateWroteKind(kind, func(*Tx) error { return nil }); err != nil || changed {
			t.Fatalf("no-op: changed=%v err=%v", changed, err)
		}
		if changed, err = s.updateWroteKind(kind, func(tx *Tx) error {
			if err := tx.put(meta([]byte("aborted")), []byte("discard")); err != nil {
				return err
			}
			return failure
		}); !errors.Is(err, failure) || changed {
			t.Fatalf("failure: changed=%v err=%v", changed, err)
		}
		stats := s.commitKindStats()[commitKindNames[kind]]
		if stats.Attempts != 3 || stats.Commits != 1 || stats.Errors != 1 || s.db.Seq() != before+1 {
			t.Fatalf("kind %d: %+v; metrics/no-op/failure changed sequence: %d", kind, stats, s.db.Seq())
		}
		for _, histogram := range [][]int64{stats.Wait.Log2, stats.Hold.Log2} {
			var observations int64
			for _, count := range histogram {
				observations += count
			}
			if observations != stats.Attempts {
				t.Fatalf("timings=%d attempts=%d", observations, stats.Attempts)
			}
		}
	}
}

func TestDiskBatchUsesWriteCommitTiming(t *testing.T) {
	s, err := OpenWithOptions(filepath.Join(t.TempDir(), "disk"), "node", "cluster", 1<<30, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := s.CommitStats().ByKind
	if err := s.writes.apply(func(tx *Tx) error {
		return tx.put(meta([]byte("client-write")), []byte("durable"))
	}); err != nil {
		t.Fatal(err)
	}
	after := s.CommitStats().ByKind
	if after["write"].Commits != before["write"].Commits+1 || after["control"].Attempts != before["control"].Attempts {
		t.Fatalf("write classified incorrectly: before=%+v after=%+v", before, after)
	}
}
