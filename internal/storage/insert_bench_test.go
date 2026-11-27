package storage

import (
	"encoding/binary"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// Compare the same authority, receipts and history with and without fsync,
// and individual versus grouped commits. NoSync is a measurement control only.
func BenchmarkCausalInsert(b *testing.B) {
	for _, diffuse := range []bool{false, true} {
		zones := "same-zone"
		if diffuse {
			zones = "distinct-zones"
		}
		for _, mode := range []struct {
			name    string
			options Options
		}{
			{"durable-update", Options{}},
			{"durable-batch", Options{BatchWrites: true, BatchSize: 256, BatchDelay: 2 * time.Millisecond}},
			{"logical-update", Options{NoSync: true}},
			{"logical-batch", Options{NoSync: true, BatchWrites: true, BatchSize: 256, BatchDelay: 2 * time.Millisecond}},
		} {
			b.Run(zones+"/"+mode.name, func(b *testing.B) {
				store, err := OpenWithOptions(filepath.Join(b.TempDir(), "insert.db"), "D", "test", 1<<30, mode.options)
				if err != nil {
					b.Fatal(err)
				}
				defer store.Close()
				holder, err := store.Endpoint("D")
				if err != nil {
					b.Fatal(err)
				}
				if err := store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
					b.Fatal(err)
				}
				var next atomic.Uint64
				var firstTx, lastTx int
				if err := store.view(func(tx *Tx) error { firstTx = tx.ID(); return nil }); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.SetParallelism(4)
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						i := next.Add(1)
						id := strconv.FormatUint(i, 10)
						location := "W000000000000000"
						if diffuse {
							var key tree.ID
							binary.BigEndian.PutUint64(key[4:], i)
							location = tree.EncodeName(key)
						}
						_, _, err := store.Apply(id, Record{Collection: "0AABCDEFGH123456", Location: location, ID: id, Metrics: []float64{1, 2}})
						if err != nil {
							b.Error(err)
							return
						}
					}
				})
				b.StopTimer()
				if err := store.view(func(tx *Tx) error { lastTx = tx.ID(); return nil }); err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(b.N)/float64(lastTx-firstTx), "inserts/commit")
				b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "inserts/s")
			})
		}
	}
}
