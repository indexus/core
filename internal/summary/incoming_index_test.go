package summary

import (
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// incomingBelowBrute is incomingBelowLocked as it was: a walk of every
// contribution (2026-11-20: called for each edge valued under t.Mu, it made a
// node with thousands of contributions hold every write for seconds).
func incomingBelowBrute(incoming map[tree.Key]summaryContribution, collection, location string) []tree.Key {
	path := summaryPath(location)
	var keys []tree.Key
	for key := range incoming {
		if key.Collection == collection && len(key.Location) > len(path) && strings.HasPrefix(key.Location, path) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Location < keys[j].Location })
	out := keys[:0]
	for _, key := range keys {
		if len(out) > 0 && strings.HasPrefix(key.Location, out[len(out)-1].Location) {
			continue
		}
		out = append(out, key)
	}
	return out
}

// The sorted index answers what the walk answered, through additions and
// removals made the way acceptChild and refreshLocked make them, and after
// incoming is replaced by other means.
func TestIncomingBelowMatchesTheWalk(t *testing.T) {
	const alphabet = "ABC"
	rng := rand.New(rand.NewSource(7))
	location := func() string {
		n := 1 + rng.Intn(4)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	collections := []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"}
	tr := &Tree{incoming: map[tree.Key]summaryContribution{}}
	check := func(step int) {
		t.Helper()
		for _, c := range collections {
			for _, at := range []string{tree.BASE64.Root(), "A", "B", "AB", "CA", "ABC"} {
				got, want := tr.incomingBelowLocked(c, at), incomingBelowBrute(tr.incoming, c, at)
				if !slices.Equal(got, want) {
					t.Fatalf("step %d, %s below %q: got %v, want %v", step, c, at, got, want)
				}
			}
		}
	}
	for step := range 3000 {
		key := tree.Key{Collection: collections[rng.Intn(len(collections))], Location: location()}
		switch r := rng.Intn(10); {
		case r < 6: // acceptChild
			if _, ok := tr.incoming[key]; !ok {
				tr.indexIncomingLocked(key, true)
			}
			tr.incoming[key] = summaryContribution{}
		case r < 9: // refreshLocked
			if _, ok := tr.incoming[key]; ok {
				tr.indexIncomingLocked(key, false)
				delete(tr.incoming, key)
			}
		default: // set by other means: the index is rebuilt
			tr.incoming[tree.Key{Collection: key.Collection, Location: location() + "C"}] = summaryContribution{}
		}
		if step%7 == 0 {
			check(step)
		}
	}
	check(-1)
}
