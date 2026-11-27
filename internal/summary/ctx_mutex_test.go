package summary

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCtxMutex(t *testing.T) {
	var m ctxMutex
	if err := m.LockContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A held lock: the wait ends with the context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.LockContext(ctx); err != context.DeadlineExceeded {
		t.Fatalf("wait on a held lock: %v", err)
	}
	// A waiter gets the lock once it is released.
	got := make(chan error)
	go func() { got <- m.LockContext(context.Background()) }()
	time.Sleep(5 * time.Millisecond)
	m.Unlock()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	m.Unlock()
	// A free lock is taken even with an ended context, as TryLock was.
	done, stop := context.WithCancel(context.Background())
	stop()
	if err := m.LockContext(done); err != nil {
		t.Fatalf("free lock with an ended context: %v", err)
	}
	m.Unlock()
	// Mutual exclusion.
	var wg sync.WaitGroup
	counter := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				m.Lock()
				counter++
				m.Unlock()
			}
		}()
	}
	wg.Wait()
	if counter != 8000 {
		t.Fatalf("counter %d", counter)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unlock of an unlocked ctxMutex did not panic")
		}
	}()
	m.Unlock()
}

// BenchmarkSummaryLockHandoff: how long a waiter takes to get the lock once
// the holder releases it. ticker is the TryLock + 1 ms ticker loop the
// summary paths used; ctxMutex wakes the waiter on release.
func BenchmarkSummaryLockHandoff(b *testing.B) {
	ctx := context.Background()
	b.Run("ticker", func(b *testing.B) {
		var mu sync.Mutex
		for b.Loop() {
			mu.Lock()
			got := make(chan struct{})
			go func() {
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for !mu.TryLock() {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
				mu.Unlock()
				close(got)
			}()
			time.Sleep(50 * time.Microsecond)
			mu.Unlock()
			<-got
		}
	})
	b.Run("ctxMutex", func(b *testing.B) {
		var mu ctxMutex
		for b.Loop() {
			mu.Lock()
			got := make(chan struct{})
			go func() {
				if mu.LockContext(ctx) == nil {
					mu.Unlock()
				}
				close(got)
			}()
			time.Sleep(50 * time.Microsecond)
			mu.Unlock()
			<-got
		}
	})
}
