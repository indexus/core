package tree

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func cellScaleView(tb testing.TB, n int) *View {
	tb.Helper()
	active := make([]VNodeDescriptor, n)
	for i := range active {
		name := fmt.Sprint(i)
		hash := sha256.Sum256([]byte(name))
		active[i] = VNodeDescriptor{Name: name, ID: ID(hash[:Size])}
	}
	v, err := NewViewFrom(0, active)
	if err != nil {
		tb.Fatal(err)
	}
	return v
}

// Every competitor imposes the first differing bit. This deliberately simple
// oracle does not use the runtime's compressed XOR index.
func pairwiseCell(v *View, owner VNodeDescriptor) Region {
	var out Region
	for _, other := range v.Active {
		if other.ID == owner.ID {
			continue
		}
		bit := CommonPrefixLen(owner.ID, other.ID)
		out.Mask[bit/8] |= 128 >> (bit % 8)
		out.Value[bit/8] |= owner.ID[bit/8] & out.Mask[bit/8]
	}
	return out
}

func TestCellMatchesPairwiseOracleAcrossIndexRebuilds(t *testing.T) {
	for _, n := range []int{1, 15, 16, 64, 256} {
		v := cellScaleView(t, n)
		for pass := range 2 {
			for _, owner := range v.Active {
				got, err := Cell(v, owner.Name)
				if want := pairwiseCell(v, owner); err != nil || got != want {
					t.Fatalf("n=%d pass=%d owner=%s got=%v want=%v err=%v", n, pass, owner.Name, got, want, err)
				}
			}
			// Public Add invalidates the index until a completed view rebuild.
			if err := v.Add("later", ID{255, 254, 253}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCellMatchesPairwiseOracleOnDeepAndLiteralViews(t *testing.T) {
	active := make([]VNodeDescriptor, Size*8)
	for bit := range active {
		id := ID{}
		id[bit/8] = 128 >> (bit % 8)
		active[bit] = VNodeDescriptor{Name: fmt.Sprint(bit), ID: id}
	}
	indexed, err := NewViewFrom(0, active)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []*View{indexed, {Active: active}} {
		for _, owner := range active {
			got, err := Cell(v, owner.Name)
			if want := pairwiseCell(v, owner); err != nil || got != want {
				t.Fatalf("owner=%s got=%v want=%v err=%v", owner.Name, got, want, err)
			}
		}
		if _, err := Cell(v, "missing"); err == nil {
			t.Fatal("missing owner accepted")
		}
	}
}

func BenchmarkCellAllMembers(b *testing.B) {
	for _, n := range []int{64, 256, 1024, 4096, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			v := cellScaleView(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				for _, member := range v.Active {
					if _, err := Cell(v, member.Name); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
