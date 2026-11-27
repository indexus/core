package tree_test

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestGuard_SingleCompetitor(t *testing.T) {
	d := tree.SetBit(tree.ID{}, 95, 1)
	u := tree.SetBit(tree.ID{}, 0, 1)
	if got, want := tree.GuardBit(d, []tree.ID{d, u}), testoracle.GuardSlow(d, []tree.ID{d, u}); got != want || got != 1 {
		t.Fatalf("g(D)=%d slow=%d", got, want)
	}
}

func TestGuard_ClosestPrefixCompetitorWins(t *testing.T) {
	d := tree.SetBit(tree.ID{}, 0, 1)
	near := tree.SetBit(d, 40, 1)
	far := tree.SetBit(tree.ID{}, 1, 1)
	g := tree.GuardBit(d, []tree.ID{d, near, far})
	if g != 41 {
		t.Fatalf("g(D)=%d want 41 (1+LCP with nearest)", g)
	}
}

func TestGuard_DuplicateRejected(t *testing.T) {
	d := tree.SetBit(tree.ID{}, 3, 1)
	if tree.GuardBit(d, []tree.ID{d, d}) != 0 {
		t.Fatal("duplicate of D is not a competitor")
	}
}

func TestGuard_AtBit95(t *testing.T) {
	d := tree.ID{}
	u := tree.SetBit(d, 95, 1)
	g := tree.GuardBit(d, []tree.ID{d, u})
	if g != 96 {
		t.Fatalf("max LCP=95 ⇒ g(D)=%d want 96", g)
	}
	if _, err := tree.ForkID(d, g, 1, u); err == nil {
		t.Fatal("g(D)=96 must not yield a legal fork bit")
	}
}

func TestGuard_NoSafeBit(t *testing.T) {
	d := packedAllButLast()
	u := tree.SetBit(d, 95, 1-tree.Bit(d, 95))
	g := tree.GuardBit(d, []tree.ID{d, u})
	if g != 96 {
		t.Fatalf("g=%d", g)
	}
	_, ok := tree.FirstDiffFrom(d, packedAllButLast(), g)
	if ok {
		t.Fatal("no bit ≥96 exists")
	}
}

func packedAllButLast() tree.ID {
	var id tree.ID
	for i := uint8(0); i < 95; i++ {
		id = tree.SetBit(id, i, 1)
	}
	return id
}

func TestGuard_Bit95ExactlyOneSplit(t *testing.T) {
	d := tree.SetBit(tree.ID{}, 94, 1)
	u := tree.SetBit(d, 94, 1-tree.Bit(d, 94))
	g := tree.GuardBit(d, []tree.ID{d, u})
	if g != 95 {
		t.Fatalf("LCP=94 ⇒ g(D)=%d want 95", g)
	}
	z1 := tree.SetBit(d, 95, 1-tree.Bit(d, 95))
	b, ok := tree.FirstDiffFrom(d, z1, g)
	if !ok || b != 95 {
		t.Fatalf("zone diverging at 95 must fork at 95, got bit=%d ok=%v", b, ok)
	}
	if _, err := tree.ForkID(d, b, tree.Bit(z1, b), z1); err != nil {
		t.Fatal(err)
	}
	z2 := d
	if _, ok := tree.FirstDiffFrom(d, z2, g); ok {
		t.Fatal("zone matching D from bit 95 is a safe-atom")
	}
}

func TestHotSplitAddressFlipsExactlyOneBit(t *testing.T) {
	d := tree.ID{0x5a, 0xa5, 0xff, 0x01}
	for bit := uint8(0); bit < 96; bit++ {
		j, err := tree.HotSplitAddress(d, bit)
		if err != nil {
			t.Fatalf("bit %d: %v", bit, err)
		}
		got, ok := tree.SingleDiffBit(d, j)
		if !ok || got != bit {
			t.Fatalf("bit %d produced sole difference %d ok=%v", bit, got, ok)
		}
		if tree.Bit(j, bit) == tree.Bit(d, bit) {
			t.Fatalf("bit %d was not flipped", bit)
		}
	}
}

func TestHotSplitAddressRejectsReservedZero(t *testing.T) {
	d := tree.SetBit(tree.ID{}, 95, 1)
	if _, err := tree.HotSplitAddress(d, 95); err == nil {
		t.Fatal("hot split produced the reserved zero ID")
	}
}

func FuzzGuardAgainstSlowLCP(f *testing.F) {
	f.Add([]byte{1}, []byte{2}, []byte{3})
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		d, u, w := tree.FromSlice(a), tree.FromSlice(b), tree.FromSlice(c)
		if d.IsZero() {
			return
		}
		others := []tree.ID{d, u, w}
		if tree.GuardBit(d, others) != testoracle.GuardSlow(d, others) {
			t.Fatalf("g(D) diverged from slow LCP")
		}
	})
}
