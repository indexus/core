package storage

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Measures deletion planning plus exact usage accounting over flushed values.
// Setup skips fsync; the measured loop does not commit and is not a freeze bound.
func BenchmarkDiscardFlushedHistory(b *testing.B) {
	s, err := OpenWithOptions(filepath.Join(b.TempDir(), "cleanup"), "root", "bench", 1<<30, Options{NoSync: true, MemoryBytes: 2 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	prefix := key(historyBucket, []byte("old-transfer"))
	value := []byte(strings.Repeat("discarded-snapshot-data", 3200))
	if err := s.update(func(tx *Tx) error {
		for i := 0; i < 128; i++ {
			if err := tx.put(prefix+element([]byte(fmt.Sprint(i))), value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	if err := s.db.Flush(); err != nil {
		b.Fatal(err)
	}
	snapshot := s.db.Snapshot()
	defer snapshot.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := newReadTx(s.db, snapshot)
		tx.writable = true
		if err := tx.drop(prefix); err != nil {
			b.Fatal(err)
		}
		if _, _, err := s.usageAfter(tx); err != nil {
			b.Fatal(err)
		}
	}
}
