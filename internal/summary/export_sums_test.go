package summary

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// siblingBoundaryTree holds records in n sibling zones below one parent
// another vnode owns: every sibling is a boundary this node exports.
func siblingBoundaryTree(t testing.TB, n int) (*Tree, string) {
	s := describedService(t, "export-sums", peerTransport{})
	initCluster(t, s)
	const collection = "Demo000000000000"
	mine, _ := tree.NewZoneKey(collection, "A"+routingAlphabet[:1])
	parent, _ := tree.NewZoneKey(collection, "A")
	other, _ := tree.NewZoneKey(collection, "B")
	for c := range n {
		at := "A" + routingAlphabet[c:c+1]
		if _, err := s.Store.Write(fmt.Sprint("w", c), storage.Record{Collection: collection, Location: at, ID: fmt.Sprint("r", c), Metrics: []float64{float64(c)}}); err != nil {
			t.Fatal(err)
		}
	}
	view, err := tree.NewViewFrom(1, []tree.VNodeDescriptor{{Name: s.Self.VNode, ID: mine.ID}, {Name: "parent", ID: parent.ID}, {Name: "other", ID: other.ID}})
	if err != nil {
		t.Fatal(err)
	}
	tr := New(s, 1, Hooks{})
	tr.SetView(view, true, false)
	tr.Summaries = &foldMark{digest: view.Digest}
	return tr, collection
}

// A boundary poll values every exported key from the store. Sibling keys
// are slots of one read of their parent's children: an export of siblings
// reads that parent once, not once per key, and every value is still the
// store's.
func TestExportPageReadsSiblingParentsOnce(t *testing.T) {
	tr, collection := siblingBoundaryTree(t, 64)
	before := childrenReads.Load()
	out, more := tr.ExportPage("", nil, "", nil)
	reads := childrenReads.Load() - before
	parents := map[string]bool{}
	for _, c := range out {
		parents[tree.BASE64.Parent(c.Location)] = true
		want, err := tr.service.Store.ZoneTotal(collection, c.Location)
		if err != nil || int64(c.Count) != want.Count {
			t.Fatalf("exported %q count %d, store %d (%v)", c.Location, c.Count, want.Count, err)
		}
	}
	if more || len(out) < 2 || len(out) == len(parents) {
		t.Fatalf("vacuous export: %d contributions under %d parents, more=%v", len(out), len(parents), more)
	}
	if reads != int64(len(parents)) {
		t.Fatalf("an export of %d keys under %d parents read children %d times", len(out), len(parents), reads)
	}
}

// BenchmarkExportSiblings is one boundary poll of 32 sibling boundaries over
// 20,000 records in flushed segments.
func BenchmarkExportSiblings(b *testing.B) {
	tr, collection := siblingBoundaryTree(b, 64)
	for i := range 20000 {
		at := "A" + string(routingAlphabet[i%64]) + string(routingAlphabet[i/64%64])
		r := storage.Record{Collection: collection, Location: at, ID: fmt.Sprint("b", i)}
		if _, _, _, err := tr.service.Store.ApplyRecord(r.ID, r); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if out, _ := tr.ExportPage("", nil, "", nil); len(out) < 2 {
			b.Fatal("vacuous export")
		}
	}
}
