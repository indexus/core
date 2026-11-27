package wire

import (
	"fmt"
	"testing"
)

const benchmarkSetsRows = 10_000

func benchmarkSetsOf(rows int) map[string]*Set {
	set := NewSet()
	for i := 0; i < rows; i++ {
		set.Put(fmt.Sprintf("%06x", i), NewAbelian(i+1, []float64{0, 0, float64(i * 7), float64(i) / 8, float64(i) / 16}))
	}
	return map[string]*Set{"@": set}
}

// Six-character keys under the root take two key words; under a parent of
// length 2 the four-character suffixes take one, the common case.
func BenchmarkEncodeSetsBlock(b *testing.B) {
	for _, rows := range []int{benchmarkSetsRows, 10 * benchmarkSetsRows} {
		for _, parent := range []string{"@", "00"} {
			b.Run(fmt.Sprintf("rows-%d/parent-%s", rows, parent), func(b *testing.B) {
				sets := map[string]*Set{parent: benchmarkSetsOf(rows)["@"]}
				locations := []string{parent}
				var size int
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					body, err := EncodeSetsBlock(sets, locations, SetsPrecision, nil, SetsBlockF32)
					if err != nil || len(body) == 0 {
						b.Fatal("invalid encoded block", err)
					}
					size = len(body)
				}
				b.ReportMetric(float64(size), "bytes")
			})
		}
	}
}
