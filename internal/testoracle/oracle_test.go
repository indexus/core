package testoracle

import (
	"testing"
)

func TestOwnerSlow_LinearArgmin(t *testing.T) {
	key := Packed(7, 8)
	a := Packed(1, 8)
	b := Packed(2, 8)
	got := OwnerSlow(key, []ID{a, b})
	if XORCompare(key, a, b) < 0 && got != a {
		t.Fatal("slow oracle did not pick the nearer ID")
	}
	if XORCompare(key, a, b) > 0 && got != b {
		t.Fatal("slow oracle did not pick the nearer ID")
	}
}

func TestOwnershipDiff_OnlyRealChanges(t *testing.T) {
	old := []ID{Packed(1, 8)}
	neu := []ID{Packed(1, 8), Packed(200, 8)}
	z := Packed(200, 8)
	diff := OwnershipDiff(old, neu, []ID{z, Packed(1, 8)})
	for _, ch := range diff {
		if ch.Old == ch.New {
			t.Fatal("phantom change")
		}
		if OwnerSlow(ch.Zone, old) != ch.Old || OwnerSlow(ch.Zone, neu) != ch.New {
			t.Fatal("diff disagrees with OwnerSlow")
		}
	}
}
