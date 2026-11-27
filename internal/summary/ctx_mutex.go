package summary

import (
	"context"
	"sync"
)

// ctxMutex is a mutex whose wait can be abandoned with a context: a one-slot
// channel. A waiter sleeps until the holder unlocks or its context ends,
// instead of polling TryLock on a ticker. The zero value is unlocked.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *ctxMutex) slot() chan struct{} {
	m.once.Do(func() { m.ch = make(chan struct{}, 1) })
	return m.ch
}

func (m *ctxMutex) Lock() { m.slot() <- struct{}{} }

func (m *ctxMutex) Unlock() {
	select {
	case <-m.slot():
	default:
		panic("summary: unlock of unlocked ctxMutex")
	}
}

// LockContext takes the lock, or returns ctx.Err() once ctx ends first. A
// free lock is taken even when ctx has ended, as a TryLock loop did.
func (m *ctxMutex) LockContext(ctx context.Context) error {
	ch := m.slot()
	select {
	case ch <- struct{}{}:
		return nil
	default:
	}
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
