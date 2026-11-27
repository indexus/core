package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func waitDiskBatch(t *testing.T, b *diskBatch, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		ready := predicate()
		b.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("batch did not reach the expected state")
}

func TestDiskBatchCoalescesBehindBusyCommitAndDrainsOnClose(t *testing.T) {
	for _, size := range []int{16, 256} {
		t.Run(fmt.Sprintf("max=%d", size), func(t *testing.T) { testBusyDiskBatch(t, size) })
	}
}

func testBusyDiskBatch(t *testing.T, size int) {
	path := filepath.Join(t.TempDir(), "disk.db")
	db, err := openTestKV(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := newDiskBatchWithLimits(db.Update, size, time.Millisecond, 0, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	results := make(chan error, 65)
	txIDs := make(chan int, 65)
	go func() {
		results <- b.apply(func(tx *Tx) error {
			close(entered)
			<-release
			txIDs <- tx.ID()
			return tx.put(key([]byte("first")), nil)
		})
	}()
	<-entered
	for i := range 64 {
		go func() {
			results <- b.apply(func(tx *Tx) error {
				txIDs <- tx.ID()
				return tx.put(key([]byte("rows"), []byte(fmt.Sprint(i))), []byte("durable"))
			})
		}()
	}
	waitDiskBatch(t, b, func() bool { return len(b.pending) == 64 })
	closed := make(chan struct{})
	go func() { b.close(); close(closed) }()
	waitDiskBatch(t, b, func() bool { return b.closed })
	if err := b.apply(func(*Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("submission after close = %v", err)
	}
	if len(results) != 0 {
		t.Fatal("ACK escaped before the blocked transaction committed")
	}
	select {
	case <-closed:
		t.Fatal("close discarded pending writes")
	default:
	}
	close(release)
	for range 65 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	<-closed
	commits := map[int]bool{}
	for range 65 {
		commits[<-txIDs] = true
	}
	if want := 1 + (64+size-1)/size; len(commits) != want {
		t.Fatalf("pending writes fragmented into %d commits, want %d", len(commits), want)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openTestKV(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *Tx) error {
		got := 0
		tx.each(key([]byte("rows")), "", func(string, []byte) bool { got++; return true })
		if got != 64 {
			return fmt.Errorf("reopened rows=%d want 64", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDiskBatchCallbackFailureKeepsSiblingCommits(t *testing.T) {
	db, err := openTestKV(t, filepath.Join(t.TempDir(), "failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := newDiskBatchWithLimits(db.Update, 256, 10*time.Millisecond, 0, 0)
	defer b.close()
	results := make(chan error, 3)
	for _, key := range []string{"before", "bad", "after"} {
		go func() {
			results <- b.apply(func(tx *Tx) error {
				if err := tx.put(rowsKey(key), []byte("value")); err != nil {
					return err
				}
				if key == "bad" {
					return ErrConflict
				}
				return nil
			})
		}()
	}
	failures := 0
	for range 3 {
		err := <-results
		if errors.Is(err, ErrConflict) {
			failures++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if failures != 1 {
		t.Fatalf("failures=%d", failures)
	}
	if err := db.View(func(tx *Tx) error {
		if tx.value(rowsKey("bad")) != nil || tx.value(rowsKey("before")) == nil || tx.value(rowsKey("after")) == nil {
			return fmt.Errorf("rollback lost siblings or retained the rejected mutation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func rowsKey(name string) string { return key([]byte("rows"), []byte(name)) }

// A slow commit shows in both histograms: its own time, and the wait of every
// write queued behind it.
func TestDiskBatchTimesCommitsAndWaits(t *testing.T) {
	db, err := openTestKV(t, filepath.Join(t.TempDir(), "disk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := newDiskBatchWithLimits(db.Update, 256, time.Millisecond, 0, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 9)
	go func() {
		done <- b.submit(context.Background(), 1, func(tx *Tx) error {
			close(entered)
			<-release
			return tx.put(key([]byte("slow")), nil)
		})
	}()
	<-entered
	for i := range 8 {
		go func() {
			done <- b.submit(context.Background(), 1, func(tx *Tx) error {
				return tx.put(key([]byte("rows"), []byte(fmt.Sprint(i))), nil)
			})
		}()
	}
	waitDiskBatch(t, b, func() bool { return len(b.pending) == 8 })
	time.Sleep(40 * time.Millisecond)
	close(release)
	for range 9 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	c := b.stats.snapshot()
	sum := func(h []int64) (n, slow int64) {
		for i, v := range h {
			n += v
			if i >= 16 { // 2^15 µs = 33 ms and more
				slow += v
			}
		}
		return n, slow
	}
	commits, slowCommits := sum(c.CommitLog2)
	waits, slowWaits := sum(c.WaitLog2)
	if c.Writes != 9 || commits != c.Commits || c.Commits < 2 || waits != 9 {
		t.Fatalf("counts: %+v", c)
	}
	if slowCommits < 1 || slowWaits != 9 {
		t.Fatalf("the slow commit is not seen: %d slow commits, %d slow waits of 9: %+v", slowCommits, slowWaits, c)
	}
}
