package wire

import (
	"strconv"
	"testing"
)

var benchmarkSet *Set

func BenchmarkSetConstruction(b *testing.B) {
	b.Run("empty", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkSet = NewSet()
		}
	})
}

func BenchmarkCloneReadSet(b *testing.B) {
	s := NewSet()
	for i := 0; i < 128; i++ {
		s.Put("aa:"+strconv.Itoa(i), NewAbelian(1, []float64{1, 2, 3}))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkSet, _ = s.Clone(16 << 20)
	}
}
