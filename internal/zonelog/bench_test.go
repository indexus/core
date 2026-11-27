package zonelog

import (
	"fmt"
	"math/rand"
	"testing"
)

// benchRecord is shaped like the vegetation collection: one record per
// depth-5 zone, 13 metrics, 200 bytes of payload.
func benchRecord(rng *rand.Rand, i int) Record {
	loc := make([]byte, 5)
	for j := range loc {
		loc[j] = alphabet[rng.Intn(64)]
	}
	metrics := make([]float64, 13)
	for j := range metrics {
		metrics[j] = rng.Float64()
	}
	return Record{Collection: testCollection, Location: string(loc), ID: fmt.Sprint("v", i), Metrics: metrics, Payload: make([]byte, 200)}
}

func benchStore(b *testing.B, records int) (*Store, []Record) {
	s, err := Open(b.TempDir(), Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	rng := rand.New(rand.NewSource(1))
	all := make([]Record, records)
	for i := 0; i < records; i += 256 {
		batch := make([]Record, 0, 256)
		for j := i; j < min(i+256, records); j++ {
			all[j] = benchRecord(rng, j)
			batch = append(batch, all[j])
		}
		if err := s.Apply(batch); err != nil {
			b.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}
	return s, all
}

// One fsync per batch of 256, and one fsync per record.
func BenchmarkApply(b *testing.B) {
	for _, size := range []int{256, 1} {
		b.Run(fmt.Sprint("batch-", size), func(b *testing.B) {
			s, err := Open(b.TempDir(), Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			rng := rand.New(rand.NewSource(1))
			b.ResetTimer()
			for i := 0; i < b.N; i += size {
				batch := make([]Record, size)
				for j := range batch {
					batch[j] = benchRecord(rng, i+j)
				}
				if err := s.Apply(batch); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "inserts/s")
		})
	}
}

// Sums of the 64 children of a zone, by the depth of the zone.
func BenchmarkChildren(b *testing.B) {
	s, all := benchStore(b, 300_000)
	for depth := range 5 {
		b.Run(fmt.Sprint("zone-depth-", depth), func(b *testing.B) {
			for i := range b.N {
				if _, err := s.Children(testCollection, all[i%64].Location[:depth]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// The records of one zone, at most 64.
func BenchmarkScanZone(b *testing.B) {
	s, all := benchStore(b, 300_000)
	b.ResetTimer()
	for i := range b.N {
		n := 0
		err := s.Scan(testCollection, all[i%1024].Location[:3], nil, func(Record) bool {
			n++
			return n < 64
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
