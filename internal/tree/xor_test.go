package tree

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
)

func TestXORCompareAgreesWithByteDistances(t *testing.T) {
	key, err := ZoneID("colAAAAAAAAAAAAA", "ab")
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecodeName("0123456789ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeName("FEDCBA9876543210")
	if err != nil {
		t.Fatal(err)
	}
	if XORCompare(key, a, b) != testoracle.XORCompare(key, a, b) {
		t.Fatal("XORCompare diverged from the slow oracle")
	}
}

func TestZoneKeyParentAndAncestor(t *testing.T) {
	child, err := NewZoneKey("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := NewZoneKey("Demo000000000000", "a")
	if err != nil {
		t.Fatal(err)
	}
	if !parent.IsAncestorOf(child) {
		t.Fatal("parent must cover child")
	}
	if child.IsAncestorOf(parent) {
		t.Fatal("child must not cover parent")
	}
	sib, err := NewZoneKey("Demo000000000000", "ab")
	if err != nil {
		t.Fatal(err)
	}
	if child.IsAncestorOf(sib) {
		t.Fatal("siblings are independent")
	}
}
