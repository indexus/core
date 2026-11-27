package storage

import (
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

type faultReader struct {
	zonelog.Reader
	getErr, scanErr error
}

func (r faultReader) Get(collection, location, id string) (zonelog.Record, bool, error) {
	if r.getErr != nil {
		return zonelog.Record{}, false, r.getErr
	}
	return r.Reader.Get(collection, location, id)
}

func (r faultReader) ScanWith(collection, location string, after *zonelog.Record, ids *zonelog.IDRange, load func(zonelog.Record) bool, visit func(zonelog.Record, int) bool) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return r.Reader.ScanWith(collection, location, after, ids, load, visit)
}

func TestTransactionReadErrorAbortsPendingWrites(t *testing.T) {
	t.Parallel()
	for _, scan := range []bool{false, true} {
		name := "value"
		if scan {
			name = "exists"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := routingStore(t)
			failure := errors.New("injected disk read failure")
			path := meta([]byte("must-not-commit"))
			before := s.db.Seq()
			err := s.update(func(tx *Tx) error {
				if err := tx.put(path, []byte("pending")); err != nil {
					return err
				}
				f := faultReader{Reader: tx.reader.Reader, getErr: failure}
				if scan {
					f.getErr, f.scanErr = nil, failure
				}
				tx.reader.Reader = f
				if scan {
					_ = tx.any(key(metaBucket, []byte("unreadable-prefix")))
				} else {
					_ = tx.value(meta([]byte("unreadable")))
				}
				// A caller may mistake the missing value for a refusal. The I/O
				// error must win, without making a failed write appear retry-safe.
				return ErrNotOwner
			})
			if !errors.Is(err, failure) || errors.Is(err, ErrNotOwner) || s.db.Seq() != before {
				t.Fatalf("failed read committed or became a refusal: err=%v seq=%d/%d", err, s.db.Seq(), before)
			}
			if err := s.view(func(tx *Tx) error {
				if tx.value(path) != nil {
					t.Fatal("pending row survived the failed transaction")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadBoundariesReturnIgnoredIOErrors(t *testing.T) {
	t.Parallel()
	s, _ := routingStore(t)
	failure := errors.New("injected metadata read failure")
	if err := s.shadowUpdate(func(tx *Tx) error { return tx.put(meta([]byte("present")), nil) }); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(func(*Tx) error) error{s.view, s.peek, s.shadowView} {
		err := read(func(tx *Tx) error {
			tx.reader.Reader = faultReader{Reader: tx.reader.Reader, getErr: failure}
			_ = tx.value(meta([]byte("unreadable")))
			return nil
		})
		if !errors.Is(err, failure) {
			t.Fatalf("read returned success after I/O failure: %v", err)
		}
	}
}

func TestScanDoesNotClearItsCallbacksReadError(t *testing.T) {
	t.Parallel()
	s, _ := routingStore(t)
	failure := errors.New("injected nested read failure")
	err := s.view(func(tx *Tx) error {
		tx.reader.Reader = faultReader{Reader: tx.reader.Reader, getErr: failure}
		return tx.reader.ScanWith(kvCollection, "@", nil, nil, func(zonelog.Record) bool { return false }, func(zonelog.Record, int) bool {
			_ = tx.value(meta([]byte("unreadable")))
			return false
		})
	})
	if !errors.Is(err, failure) {
		t.Fatalf("outer scan cleared its callback's failure: %v", err)
	}
}

func TestCommitRejectsReadErrorDuringUsageAccounting(t *testing.T) {
	t.Parallel()
	s, _ := routingStore(t)
	tx := newTx(s.db, true)
	failure := errors.New("injected usage read failure")
	tx.reader.Reader = faultReader{Reader: tx.reader.Reader, getErr: failure}
	tx.delta = 1 // commit reads storedRowBytes only after entering commit
	before := s.db.Seq()
	if err := tx.commit(nil); !errors.Is(err, failure) || s.db.Seq() != before {
		t.Fatalf("late read error was committed: err=%v seq=%d/%d", err, s.db.Seq(), before)
	}
}
