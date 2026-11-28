package tree

import (
	"math/rand"
	"strconv"
	"testing"
)

func foldView(t testing.TB, size int, seed int64) *View {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	ds := make([]VNodeDescriptor, size)
	for i := range ds {
		r.Read(ds[i].ID[:])
		if seed%2 == 0 {
			clear(ds[i].ID[:8])
		}
		ds[i].Name = strconv.Itoa(i)
	}
	v, err := NewViewFrom(1, ds)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The previous geometric definition checks coverage after removing the donor.
func coveringPeer(v *View, d VNodeDescriptor) (VNodeDescriptor, bool) {
	cell, _ := Cell(v, d.Name)
	post := v.Clone()
	post.Remove(d.Name)
	for _, p := range post.Active {
		next, _ := Cell(post, p.Name)
		if both, ok := cell.Intersect(next); ok && both == cell {
			return p, true
		}
	}
	return VNodeDescriptor{}, false
}

func TestFoldSiblingMatchesWholeCellCoverage(t *testing.T) {
	for _, n := range []int{1, 2, 7, 15, 16, 17, 64} {
		for seed := int64(0); seed < 12; seed++ {
			v := foldView(t, n, seed)
			scan := v.Clone()
			scan.xorIndex = nil
			for _, d := range v.Active {
				want, ok := coveringPeer(v, d)
				for _, view := range []*View{v, scan} {
					got, found := view.FoldSibling(d.ID)
					if found != ok || got != want {
						t.Fatalf("n=%d seed=%d donor=%s got %v/%v want %v/%v", n, seed, d.Name, got, found, want, ok)
					}
				}
			}
		}
	}
}

func BenchmarkFoldSibling(b *testing.B) {
	for _, n := range []int{64, 256, 1024} {
		v := foldView(b, n, 73)
		b.Run(strconv.Itoa(n)+"/coverage", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				coveringPeer(v, v.Active[i%n])
			}
		})
		b.Run(strconv.Itoa(n)+"/index", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				v.FoldSibling(v.Active[i%n].ID)
			}
		})
	}
}
