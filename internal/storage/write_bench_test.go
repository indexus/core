package storage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// BenchmarkApplyRecord is one acknowledged write of a record with 8 metrics
// and a 1 KiB payload, without fsync.
func BenchmarkApplyRecord(b *testing.B) {
	store, err := OpenWithOptions(filepath.Join(b.TempDir(), "write.db"), "D", "test", 1<<34, Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	holder, _ := store.Endpoint("D")
	if err := store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		b.Fatal(err)
	}
	payload, _ := json.Marshal(fmt.Sprintf("%01000d", 7))
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		r := Record{Collection: "writebench000000", Location: fmt.Sprintf("A%02d", i%50), ID: fmt.Sprint("r", i), Metrics: []float64{1, 2, 3, 4, 5, 6, 7, 8}, Payload: payload}
		if _, err := store.Write(fmt.Sprint("w", i), r); err != nil {
			b.Fatal(err)
		}
	}
}
