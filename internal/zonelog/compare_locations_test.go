package zonelog

import (
	"bytes"
	"math/rand"
	"testing"
)

// CompareLocations is the key order: for any two locations it agrees with
// bytes.Compare of their keys (ids empty).
func TestCompareLocationsIsKeyOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	random := func() string {
		if rng.Intn(10) == 0 {
			return "@"
		}
		n := 1 + rng.Intn(16)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	cases := [][2]string{{"v4HdAwn7VUKej0rf", "-Ptg6-LmpAwp_SJI"}, {"ab", "ab0"}, {"ab", "ab1"}, {"@", "0"}, {"z", "-"}, {"_", "-"}}
	for range 20000 {
		a, b := random(), random()
		if rng.Intn(4) == 0 && a != "@" && len(a) < 16 {
			b = a + string(alphabet[rng.Intn(len(alphabet))])
		}
		cases = append(cases, [2]string{a, b})
	}
	for _, c := range cases {
		ka, err := makeKey(1, c[0], "")
		if err != nil {
			t.Fatal(err)
		}
		kb, err := makeKey(1, c[1], "")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := CompareLocations(c[0], c[1]), bytes.Compare(ka, kb); got != want {
			t.Fatalf("CompareLocations(%q, %q) = %d, keys say %d", c[0], c[1], got, want)
		}
	}
}
