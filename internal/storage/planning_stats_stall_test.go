package storage

import (
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlanningStatsContentionOnWriters(t *testing.T) {
	store, _ := routingStore(t)

	// Seed 12,000 distinct zones (typical hot vnode before split).
	const totalZones = 12000
	const batch = 250
	for base := 0; base < totalZones; base += batch {
		if err := store.update(func(tx *Tx) error {
			for i := 0; i < batch; i++ {
				id := base + i
				col := fmt.Sprintf("u%015d", id%64)
				loc := fmt.Sprintf("A%015d", id)
				zk, err := tree.NewZoneKey(col, loc)
				if err != nil {
					return err
				}
				rec := Record{
					Collection: col,
					Location:   loc,
					ID:         fmt.Sprintf("w%d", id),
					Metrics:    []float64{1},
				}
				if _, _, err := tx.putRecord(zk, rec); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Measure one PlanningStats call alone + concurrent writer stall.
	var stop atomic.Bool
	var maxWriteWait atomic.Int64
	doneWriter := make(chan struct{})
	go func() {
		defer close(doneWriter)
		i := 0
		for !stop.Load() {
			started := time.Now()
			_ = store.update(func(tx *Tx) error {
				return tx.put(rowsKey(fmt.Sprintf("tick-%d", i)), []byte("x"))
			})
			d := time.Since(started)
			for {
				cur := maxWriteWait.Load()
				if int64(d) <= cur || maxWriteWait.CompareAndSwap(cur, int64(d)) {
					break
				}
			}
			i++
			time.Sleep(5 * time.Millisecond)
		}
	}()

	t0 := time.Now()
	stats, err := store.PlanningStats()
	scanDur := time.Since(t0)
	stop.Store(true)
	<-doneWriter
	if err != nil {
		t.Fatal(err)
	}
	cs := store.CommitStats()
	maxWait := time.Duration(maxWriteWait.Load())
	t.Logf("zones=%d PlanningStats=%v maxWriteWait=%v write_lock_max=%v",
		len(stats), scanDur.Round(time.Millisecond),
		maxWait.Round(time.Millisecond),
		time.Duration(cs.Stages["write_lock"].MaxNanos).Round(time.Millisecond))
	if len(stats) != totalZones {
		t.Fatalf("PlanningStats returned %d zones, want %d", len(stats), totalZones)
	}
	if scanDur >= 20*time.Millisecond && maxWait > scanDur/2 {
		t.Fatalf("PlanningStats stalled concurrent writes for %v (total scan %v); expected lock-free MVCC reads", maxWait, scanDur)
	}
}

// TestMVCCViewDoesNotBlockUpdates verifies that holding an open read
// transaction (s.view) never blocks concurrent s.update commits, and that
// the read transaction stays isolated at its snapshot commit.
func TestMVCCViewDoesNotBlockUpdates(t *testing.T) {
	store, _ := routingStore(t)
	if err := store.update(func(tx *Tx) error {
		return tx.put(rowsKey("k"), []byte("v1"))
	}); err != nil {
		t.Fatal(err)
	}

	viewStarted := make(chan struct{})
	releaseView := make(chan struct{})
	viewErr := make(chan error, 1)
	go func() {
		viewErr <- store.view(func(tx *Tx) error {
			if got := string(tx.value(rowsKey("k"))); got != "v1" {
				return fmt.Errorf("before update: got %q, want v1", got)
			}
			close(viewStarted)
			<-releaseView
			if got := string(tx.value(rowsKey("k"))); got != "v1" {
				return fmt.Errorf("after concurrent update: got %q, want v1", got)
			}
			return nil
		})
	}()

	<-viewStarted
	// A write transaction must complete immediately while the read view is held open.
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.update(func(tx *Tx) error {
			return tx.put(rowsKey("k"), []byte("v2"))
		})
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("concurrent update failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(releaseView)
		t.Fatal("s.update blocked behind an open s.view")
	}
	close(releaseView)
	if err := <-viewErr; err != nil {
		t.Fatal(err)
	}
	// A new view sees the committed v2.
	if err := store.view(func(tx *Tx) error {
		if got := string(tx.value(rowsKey("k"))); got != "v2" {
			return fmt.Errorf("new view: got %q, want v2", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
