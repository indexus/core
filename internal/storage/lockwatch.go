package storage

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"time"
)

// Lock watch, for incidents only (REM-121). With INDEXUS_DEBUG_LOCKS=1 the
// store records the stack of whoever takes writeMu, and clears it on release.
// A watchdog writes <path>.lockwatch-<unixnano>.txt with that stack and every
// goroutine when a lock is held for longer than lockWatchHeld, or when callers
// wait while no holder is recorded: a lock released by no one. Without the
// variable it costs one branch per update.
var debugLocks = os.Getenv("INDEXUS_DEBUG_LOCKS") == "1"

var lockWatchHeld = 30 * time.Second

type lockHolder struct {
	since time.Time
	stack []byte
}

type lockSlot struct {
	name    string
	holder  atomic.Pointer[lockHolder]
	waiting atomic.Int64
	// waitSince is when the oldest current waiter started, in UnixNano.
	waitSince atomic.Int64
	reported  atomic.Pointer[lockHolder]
}

type lockWatch struct {
	write lockSlot
}

func (l *lockSlot) wait() {
	if debugLocks && l.waiting.Add(1) == 1 {
		l.waitSince.Store(time.Now().UnixNano())
	}
}

func (l *lockSlot) took() {
	if !debugLocks {
		return
	}
	if l.waiting.Add(-1) == 0 {
		l.waitSince.Store(0)
	}
	buf := make([]byte, 32<<10)
	l.holder.Store(&lockHolder{since: time.Now(), stack: buf[:runtime.Stack(buf, false)]})
}

func (l *lockSlot) released() {
	if debugLocks {
		l.holder.Store(nil)
	}
}

func (s *Store) startLockWatch() {
	if !debugLocks {
		return
	}
	s.locks.write.name = "writeMu"
	go func() {
		for range time.Tick(5 * time.Second) {
			s.checkLock(&s.locks.write)
		}
	}()
}

func (s *Store) checkLock(l *lockSlot) {
	h := l.holder.Load()
	waitSince := l.waitSince.Load()
	var why string
	switch {
	case h != nil && time.Since(h.since) > lockWatchHeld && l.reported.Load() != h:
		why = fmt.Sprintf("%s held for %s", l.name, time.Since(h.since).Round(time.Second))
		l.reported.Store(h)
	case h == nil && waitSince != 0 && time.Since(time.Unix(0, waitSince)) > lockWatchHeld:
		why = fmt.Sprintf("%d callers wait for %s for %s and no holder is recorded", l.waiting.Load(), l.name, time.Since(time.Unix(0, waitSince)).Round(time.Second))
		l.waitSince.Store(time.Now().UnixNano()) // report again only after another period
	default:
		return
	}
	f, err := os.Create(fmt.Sprintf("%s.lockwatch-%d.txt", s.path, time.Now().UnixNano()))
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n\n", why)
	if h != nil {
		fmt.Fprintf(f, "holder since %s:\n%s\n\n", h.since.Format(time.RFC3339Nano), h.stack)
	}
	_ = pprof.Lookup("goroutine").WriteTo(f, 2)
}
