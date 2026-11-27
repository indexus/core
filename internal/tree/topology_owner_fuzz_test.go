package tree

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
)

func FuzzViewOwner(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, []byte{12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, []byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2})
	f.Fuzz(func(t *testing.T, a, b, zone []byte) {
		va := NewView(1)
		ia, ib := FromSlice(a), FromSlice(b)
		if ia == ib || ia.IsZero() || ib.IsZero() {
			return
		}
		if err := va.Add("A", ia); err != nil {
			t.Fatal(err)
		}
		if err := va.Add("B", ib); err != nil {
			t.Fatal(err)
		}
		z := FromSlice(zone)
		if va.Owner(z) != testoracle.OwnerSlow(z, []ID{ia, ib}) {
			t.Fatal("View.Owner diverged from the slow oracle")
		}
	})
}

func FuzzDeltaApply(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, []byte{12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		ia, ib := FromSlice(a), FromSlice(b)
		if ia == ib || ia.IsZero() || ib.IsZero() {
			return
		}
		old := NewView(2)
		if err := old.Add("A", ia); err != nil {
			t.Fatal(err)
		}
		next, err := Apply(old, Delta{BaseDigest: old.Digest, Add: []VNodeDescriptor{{ID: ib, Name: "B"}}})
		if err != nil {
			t.Fatal(err)
		}
		if next.Len() != 2 {
			t.Fatalf("len=%d", next.Len())
		}
		if old.Len() != 1 {
			t.Fatal("Apply mutated old view")
		}
	})
}
