package storage

import (
	"bytes"
	"encoding/binary"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// diskEntries lists every control row with its value, and each bucket of
// its path as a key that ends with "/".
func diskEntries(tb testing.TB, s *Store) map[string][]byte {
	tb.Helper()
	out := map[string][]byte{}
	err := s.view(func(tx *Tx) error {
		return tx.each("", "", func(path string, v []byte) bool {
			name, elems := "", split(path)
			for _, e := range elems[:len(elems)-1] {
				name += string(e) + "/"
				out[name] = nil
			}
			out[name+string(elems[len(elems)-1])] = bytes.Clone(v)
			return true
		})
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// BenchmarkInsertCost reports what one insert leaves on disk next to its
// time: values put, buckets created, bytes of keys and values, and how many
// of those bytes are the record itself. One insert per transaction, no fsync.
func BenchmarkInsertCost(b *testing.B) {
	for _, diffuse := range []bool{false, true} {
		name := "same-zone"
		if diffuse {
			name = "distinct-zones"
		}
		b.Run(name, func(b *testing.B) {
			store, err := OpenWithOptions(filepath.Join(b.TempDir(), "insert.db"), "D", "test", 1<<30, Options{NoSync: true})
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
			insert := func(i uint64) {
				id := strconv.FormatUint(i, 10)
				location := "W000000000000000"
				if diffuse {
					var key tree.ID
					binary.BigEndian.PutUint64(key[4:], i)
					location = tree.EncodeName(key)
				}
				record := Record{Collection: "0AABCDEFGH123456", Location: location, ID: id, Metrics: []float64{1, 2, 3, 4, 5}}
				if _, _, err := store.Apply("request-"+id, record); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for i := range b.N {
				insert(uint64(i) + 1)
			}
			b.StopTimer()

			before := diskEntries(b, store)
			insert(uint64(b.N) + 1)
			var puts, buckets, size, record int
			for key, value := range diskEntries(b, store) {
				old, known := before[key]
				if known && bytes.Equal(old, value) {
					continue
				}
				if value == nil {
					buckets++
					continue
				}
				puts++
				size += len(key) + len(value)
				if strings.HasPrefix(key, "live/") && strings.Contains(key, "/\x01") {
					record = len(value)
				}
			}
			b.ReportMetric(float64(puts), "puts/insert")
			b.ReportMetric(float64(buckets), "buckets/insert")
			b.ReportMetric(float64(size), "disk-B/insert")
			b.ReportMetric(float64(record), "record-B/insert")
		})
	}
}
