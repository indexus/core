package tree

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
)

func TestViewOwnerIgnoresCapacity(t *testing.T) {
	v := NewView(1)
	d, err := DecodeName("0123456789ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	j, err := DecodeName("FEDCBA9876543210")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Add("D", d); err != nil {
		t.Fatal(err)
	}
	if err := v.Add("J", j); err != nil {
		t.Fatal(err)
	}
	z, err := ZoneID("colAAAAAAAAAAAAA", "ab")
	if err != nil {
		t.Fatal(err)
	}
	a := v.Owner(z)
	b := v.Owner(z)
	if a != b {
		t.Fatal("Owner is not deterministic")
	}
	if a != testoracle.OwnerSlow(z, []ID{d, j}) {
		t.Fatal("View.Owner diverged from the slow oracle")
	}
}

func TestViewRejectsDuplicateID(t *testing.T) {
	v := NewView(0)
	id, err := DecodeName("0123456789ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Add("A", id); err != nil {
		t.Fatal(err)
	}
	if err := v.Add("B", id); err == nil {
		t.Fatal("expected duplicate id")
	}
}
