package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// slowMergeFS delays only the first merge output (four flushed segments),
// leaving log sync and subsequent flush outputs at the device's real speed.
type slowMergeFS struct {
	zonelog.FS
	entered chan struct{}
}

func (f *slowMergeFS) Create(name string) (zonelog.File, error) {
	if name == "00000005.seg" {
		close(f.entered)
		time.Sleep(time.Second)
	}
	return f.FS.Create(name)
}

// Opt-in open-loop load, with real fsync. Run on an otherwise idle machine:
// INDEXUS_STORAGE_LOAD=1 go test ./internal/storage -run TestStorageSustainedLoad -count=1 -v
func TestStorageSustainedLoad(t *testing.T) {
	if os.Getenv("INDEXUS_STORAGE_LOAD") == "" {
		t.Skip("30 second storage load")
	}
	dir := t.TempDir()
	opt := zonelog.Options{MemoryBytes: 1 << 20}
	var slow *slowMergeFS
	if os.Getenv("INDEXUS_STORAGE_LOAD") == "merge" {
		slow = &slowMergeFS{FS: zonelog.Dir(dir), entered: make(chan struct{})}
		opt.FS = slow
	}
	db, err := zonelog.Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	s.writes = newDiskBatchWithLimits(s.update, 256, time.Millisecond, 0, 0)
	defer s.Close()
	if slow != nil {
		for i := range 4 {
			rows := make([]zonelog.Record, 100)
			for j := range rows {
				rows[j] = zonelog.Record{Collection: "0123456789ABCDEF", Location: "@", ID: fmt.Sprintf("seed-%d-%d", i, j), Payload: make([]byte, 800)}
			}
			if err := db.Apply(rows); err != nil {
				t.Fatal(err)
			}
			if i < 3 {
				if err := db.Flush(); err != nil {
					t.Fatal(err)
				}
			}
		}
		flushed := make(chan error, 1)
		go func() { flushed <- db.Flush() }()
		select {
		case <-slow.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("merge did not start")
		}
		defer func() {
			if err := <-flushed; err != nil {
				t.Error(err)
			}
		}()
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var waits []time.Duration
	var failures int
	var peak int
	start := time.Now()
	// Five arrivals every 4 ms: 1,250 writes/s; no coordinated omission.
	for i := 0; time.Since(start) < 30*time.Second; i++ {
		target := start.Add(time.Duration(i) * 4 * time.Millisecond)
		if d := time.Until(target); d > 0 {
			time.Sleep(d)
		}
		for j := 0; j < 5; j++ {
			id := i*5 + j
			wg.Add(1)
			go func() {
				defer wg.Done()
				submitted := time.Now()
				err := s.writes.submit(context.Background(), 256, func(tx *Tx) error {
					mu.Lock()
					waits = append(waits, time.Since(submitted))
					mu.Unlock()
					return tx.put(rowsKey(fmt.Sprint(id)), make([]byte, 200))
				})
				if err != nil {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}()
		}
		s.writes.mu.Lock()
		peak = max(peak, s.writes.admitted)
		s.writes.mu.Unlock()
	}
	wg.Wait()
	slices.Sort(waits)
	if len(waits) == 0 {
		t.Fatal("no writes")
	}
	raw, _ := json.Marshal(s.CommitStats())
	t.Logf("duration=%v writes=%d refused=%d peak_pending=%d precommit p50=%v p99=%v max=%v metrics=%s", time.Since(start), len(waits), failures, peak, waits[len(waits)/2], waits[len(waits)*99/100], waits[len(waits)-1], raw)
	if failures != 0 {
		t.Fatalf("%d failed writes", failures)
	}
}
