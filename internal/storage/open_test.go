package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A new store is its records directory: no file at its path, before or
// after a write and a reopen.
func TestNewStoreIsItsRecordsDirectoryAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.causal.db")
	s, _ := bulkStore(t, path)
	if _, err := s.Write("w", Record{Collection: bulkCollection, Location: "B", ID: "x", Metrics: []float64{1}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a file exists at the store's path: %v", err)
	}
	s, err := OpenWithOptions(path, "D", "test", 1<<30, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := inventoryOf(t, s); len(got) != 1 {
		t.Fatalf("after reopen: %v", got)
	}
}

// A file at the store's path (a store of the former bbolt engine) is not
// read: the open fails, says so, and leaves the file and creates nothing.
func TestFileAtStorePathIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.causal.db")
	if err := os.WriteFile(path, []byte("an old store"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenWithOptions(path, "D", "test", 1<<30, Options{NoSync: true})
	if err == nil || !strings.Contains(err.Error(), "not a zonelog store") {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(RecordsDir(path)); !os.IsNotExist(err) {
		t.Fatalf("a records directory was created: %v", err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "an old store" {
		t.Fatalf("the file changed: %q, %v", raw, err)
	}
}
