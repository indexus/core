package indexus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// Keep the same populated facade for both modes. Setup excludes fsync, while
// the measured read paths keep production cache and authority checks enabled.
func BenchmarkSteadyRead(b *testing.B) {
	n := newMemoryBenchNode(b)
	const collection = "MemoryBench00001"
	payload := json.RawMessage(`{"text":"` + strings.Repeat("x", 8192) + `"}`)
	for i := range 64 {
		row := storage.Record{Collection: collection, Location: "aa", ID: fmt.Sprint(i), Metrics: []float64{1, 2}, Payload: payload}
		if _, _, _, err := n.durableStore().ApplyRecord(row.ID, row); err != nil {
			b.Fatal(err)
		}
	}
	if err := n.Update(); err != nil {
		b.Fatal(err)
	}
	for _, fresh := range []bool{false, true} {
		name := "ordinary"
		if fresh {
			name = "fresh"
		}
		b.Run(name, func(b *testing.B) {
			read := func() error {
				_, set, err := n.GetSetContext(context.Background(), collection, "@", true, nil, fresh)
				if err != nil {
					return err
				}
				if set == nil || set.Abelian().Count() != 64 {
					return fmt.Errorf("read did not preserve the 64 ACKs")
				}
				return nil
			}
			if err := read(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
