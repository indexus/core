package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWriteAdmissionBoundsQueuedAndInFlight(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		count                int
		bytes, first, second int64
	}{{"count", 2, 100, 10, 10}, {"bytes", 10, 10, 6, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := openTestKV(t, filepath.Join(t.TempDir(), "limit.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			b := newDiskBatchWithLimits(db.Update, 1, 0, tc.count, tc.bytes)
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 2)
			go func() {
				done <- b.submit(ctx, tc.first, func(tx *Tx) error {
					close(entered)
					<-release
					return tx.put(key([]byte("first")), nil)
				})
			}()
			<-entered
			go func() {
				done <- b.submit(context.Background(), tc.second, func(tx *Tx) error { return tx.put(key([]byte("committed"), []byte("row")), []byte("value")) })
			}()
			waitDiskBatch(t, b, func() bool { return len(b.pending) == 1 })
			called := false
			err = b.submit(context.Background(), 1, func(*Tx) error { called = true; return nil })
			var overload *WriteAdmissionError
			if !errors.Is(err, ErrWriteOverload) || !errors.As(err, &overload) || called {
				t.Fatalf("overload=%v callback=%v", err, called)
			}
			cancel()
			select {
			case e := <-done:
				t.Fatalf("accepted call returned before commit: %v", e)
			default:
			}
			rejected, cancelRejected := context.WithCancel(context.Background())
			cancelRejected()
			if err = b.submit(rejected, 1, func(*Tx) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
				t.Fatalf("canceled admission: %v", err)
			}
			close(release)
			for range 2 {
				if e := <-done; e != nil {
					t.Fatal(e)
				}
			}
			waitDiskBatch(t, b, func() bool { return b.admitted == 0 && b.admittedBytes == 0 })
			// A failed callback must also release both budgets, including its isolated replay.
			if e := b.submit(context.Background(), tc.first, func(*Tx) error { return ErrConflict }); !errors.Is(e, ErrConflict) {
				t.Fatal(e)
			}
			if e := b.submit(context.Background(), tc.first, func(*Tx) error { return nil }); e != nil {
				t.Fatal(e)
			}
			b.close()
			if err = b.submit(context.Background(), 1, func(*Tx) error { return nil }); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
			if e := db.View(func(tx *Tx) error {
				if string(tx.value(key([]byte("committed"), []byte("row")))) != "value" {
					t.Fatal("accepted write missing")
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestWriteAdmissionOversizedAndCanceledNeverRuns(t *testing.T) {
	db, err := openTestKV(t, filepath.Join(t.TempDir(), "oversized.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := newDiskBatchWithLimits(db.Update, 128, time.Millisecond, 10, 32)
	defer b.close()
	if err = b.submit(context.Background(), 33, func(*Tx) error { t.Fatal("oversized callback ran"); return nil }); !errors.Is(err, ErrWriteOverload) {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running || b.admitted != 0 || b.admittedBytes != 0 {
		t.Fatal("refused request consumed budget")
	}
}

func TestWriteAdmissionSojournTimePreventsCongestionCollapse(t *testing.T) {
	db, err := openTestKV(t, filepath.Join(t.TempDir(), "codel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	b := newDiskBatchWithLimits(func(fn func(*Tx) error) error {
		blockOnce.Do(func() {
			close(entered)
			<-release
		})
		return db.Update(fn)
	}, 1, 0, DefaultMaxPendingWrites, DefaultMaxPendingBytes)
	b.maxQueueWait = 30 * time.Millisecond
	defer b.close()

	// 1st write enters the stalled commit; 2nd write sits at the head of b.pending.
	done1 := make(chan error, 1)
	go func() { done1 <- b.submit(context.Background(), 16, func(*Tx) error { return nil }) }()
	<-entered
	done2 := make(chan error, 1)
	go func() { done2 <- b.submit(context.Background(), 16, func(*Tx) error { return nil }) }()
	time.Sleep(50 * time.Millisecond)

	// 3rd write sees head-of-queue sojourn > 30ms and is shed immediately even
	// though pending=2 << 4096.
	var overload *WriteAdmissionError
	if err := b.submit(context.Background(), 16, func(*Tx) error { t.Fatal("shed write ran"); return nil }); !errors.Is(err, ErrWriteOverload) || !errors.As(err, &overload) || overload.QueueWait <= b.maxQueueWait {
		t.Fatalf("expected sojourn ErrWriteOverload, got %v", err)
	}

	// Unblock disk: queued writes finish and new writes are admitted again.
	close(release)
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
	if err := b.submit(context.Background(), 16, func(*Tx) error { return nil }); err != nil {
		t.Fatalf("post-recovery write failed: %v", err)
	}
}
