package tree

import (
	"math/rand/v2"
	"testing"
)

const testAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

// PREFIX-1: every descendant of a location prefix, in the same collection,
// has a zone id inside PrefixRegion(prefix); a full-length location's region
// is the single point of its own id.
func TestPREFIX1_DescendantsStayInPrefixRegion(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	word := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = testAlphabet[rng.IntN(len(testAlphabet))]
		}
		return string(b)
	}
	for trial := 0; trial < 20000; trial++ {
		collection := word(16)
		depth := 1 + rng.IntN(16)
		prefix := word(depth)
		parent, err := NewZoneKey(collection, prefix)
		if err != nil {
			t.Fatal(err)
		}
		region := PrefixRegion(parent)
		if !region.Valid() || !region.Contains(parent.ID) {
			t.Fatalf("%q/%q: region excludes the prefix itself", collection, prefix)
		}
		child, err := NewZoneKey(collection, prefix+word(rng.IntN(16-depth+1)))
		if err != nil {
			t.Fatal(err)
		}
		if !region.Contains(child.ID) {
			t.Fatalf("%q: descendant %q of %q outside its region", collection, child.Location, prefix)
		}
		if depth == 16 {
			for i := range region.Mask {
				if region.Mask[i] != 0xff {
					t.Fatalf("full-length location %q is not a point", prefix)
				}
			}
		}
	}
	if root := PrefixRegion(ZoneKey{Location: BASE64.Root()}); root != (Region{}) {
		t.Fatal("root region must be everything")
	}
}
