package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The lock watch names the holder of a lock held too long, and reports
// waiters when no holder is recorded.
func TestLockWatchReportsHolderAndOrphanWaits(t *testing.T) {
	old, oldHeld := debugLocks, lockWatchHeld
	debugLocks, lockWatchHeld = true, 10*time.Millisecond
	defer func() { debugLocks, lockWatchHeld = old, oldHeld }()
	s := &Store{path: filepath.Join(t.TempDir(), "n.causal.db")}
	s.locks.write.name = "writeMu"
	s.locks.write.wait()
	s.locks.write.took()
	time.Sleep(20 * time.Millisecond)
	s.checkLock(&s.locks.write)
	s.locks.write.released()
	s.locks.write.wait() // a caller waits; nobody is recorded as holder
	time.Sleep(20 * time.Millisecond)
	s.checkLock(&s.locks.write)
	files, _ := filepath.Glob(s.path + ".lockwatch-*.txt")
	var all string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all += string(b)
	}
	for _, want := range []string{"writeMu held for", "TestLockWatchReportsHolderAndOrphanWaits", "no holder is recorded"} {
		if !strings.Contains(all, want) {
			t.Fatalf("lock watch report lacks %q:\n%.400s", want, all)
		}
	}
}
