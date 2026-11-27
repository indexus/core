package tree

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
)

func FuzzXORCompareOwner(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, []byte{12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, []byte{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1})
	f.Fuzz(func(t *testing.T, a, b, key []byte) {
		ka, kb, kz := FromSlice(a), FromSlice(b), FromSlice(key)
		if XORCompare(kz, ka, kb) != testoracle.XORCompare(kz, ka, kb) {
			t.Fatal("XORCompare diverged from the slow oracle")
		}
	})
}
