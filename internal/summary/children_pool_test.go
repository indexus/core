package summary

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestSumVersionChangeReleasesCachedChildren(t *testing.T) {
	tr := &Tree{storeGen: 2}
	sums := tr.openZoneSums()
	children := childrenPool.Get().(*[65]storage.ZoneSum)
	children[4] = storage.ZoneSum{Count: 3, Metrics: []float64{4}, Only: "retained-location"}
	sums.children[tree.Key{Collection: "collection", Location: "parent"}] = children
	version := uint64(1)
	tr.currentLocked(sums, &version)
	if version != 2 || len(sums.children) != 0 {
		t.Fatalf("stale cache survived version change: version=%d entries=%d", version, len(sums.children))
	}
	// Ownership of children returned to the pool: do not inspect that pointer.
	sums.close()
	sums.close() // no second return of the buffers released by currentLocked
}
