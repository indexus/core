package tree

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
)

func TestID1_ZoneIDIs96Bits(t *testing.T) {
	id, err := ZoneID("colAAAAAAAAAAAAA", "ab")
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != Size {
		t.Fatalf("width %d", len(id))
	}
}

func TestID1_VNodeEncodeDecodeRoundTrip(t *testing.T) {
	id := bitID(3)
	back, err := DecodeName(EncodeName(id))
	if err != nil {
		t.Fatal(err)
	}
	if back != id {
		t.Fatalf("round trip %v want %v", back, id)
	}
}

func TestID1_EmptyLocationIsRoot(t *testing.T) {
	root, err := ZoneID("colAAAAAAAAAAAAA", BASE64.Root())
	if err != nil {
		t.Fatal(err)
	}
	empty, err := ZoneID("colAAAAAAAAAAAAA", "")
	if err != nil {
		t.Fatal(err)
	}
	if root != empty {
		t.Fatal("root location and empty location must encode the same id")
	}
}

// Location bits come first: a child shares its parent's prefix, and the
// collection only separates zones at the same location.
func TestID1_ChildSharesParentPrefix(t *testing.T) {
	parent, err := NewZoneKey("Demo000000000000", "0z")
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewZoneKey("Demo000000000000", "0za")
	if err != nil {
		t.Fatal(err)
	}
	if lcp := CommonPrefixLen(parent.ID, child.ID); lcp < 12 {
		t.Fatalf("LCP(parent,child)=%d want >= 12", lcp)
	}
	if !parent.IsAncestorOf(child) {
		t.Fatal("0z must cover 0za")
	}
	a, _ := ZoneID("colAAAAAAAAAAAAA", "ab")
	b, _ := ZoneID("colBBBBBBBBBBBBB", "ab")
	if a == b {
		t.Fatal("the collection must distinguish two zones at the same location")
	}
	if CommonPrefixLen(a, b) < 12 {
		t.Fatal("two collections at the same location must share the location prefix")
	}
}

func TestOWN1_OwnerMatchesOracleExhaustive8Bit(t *testing.T) {
	const bits = 8
	v := NewView(1)
	var active []ID
	for n := uint64(1); n <= 5; n++ {
		id := ID(testoracle.Packed(n*17+3, bits))
		if err := v.Add(string(rune('A'+n)), id); err != nil {
			t.Fatal(err)
		}
		active = append(active, id)
	}
	for k := uint64(0); k < 1<<bits; k++ {
		z := ID(testoracle.Packed(k, bits))
		if got, want := v.Owner(z), testoracle.OwnerSlow(z, active); got != want {
			t.Fatalf("key %d owner %v want %v", k, got, want)
		}
	}
}

func TestTOPO1_SameActiveSetSameOwner(t *testing.T) {
	d, j := bitID(95), bitID(0)
	a := mustView(t, VNodeDescriptor{ID: d, Name: "D"}, VNodeDescriptor{ID: j, Name: "J"})
	b := mustView(t, VNodeDescriptor{ID: j, Name: "J"}, VNodeDescriptor{ID: d, Name: "D"})
	z := bitID(94)
	if a.Owner(z) != b.Owner(z) || a.Owner(z) != testoracle.OwnerSlow(z, []ID{d, j}) {
		t.Fatal("same V_active produced different owners")
	}
}

func bitID(bit uint8) ID {
	return SetBit(ID{}, bit, 1)
}

func mustView(t *testing.T, pairs ...VNodeDescriptor) *View {
	t.Helper()
	v := NewView(1)
	for _, d := range pairs {
		if err := v.Add(d.Name, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	return v
}
