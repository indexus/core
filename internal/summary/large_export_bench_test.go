package summary

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Each generated location has its own depth-16 boundary, with the other
// vnode owning its parent. Read live values from the actual store.
func deepBoundaryTree(t testing.TB, n int) *Tree {
	t.Helper()
	service := describedService(t, "packed-cache", peerTransport{})
	initCluster(t, service)
	tr := New(service, 1, Hooks{})
	view, err := tree.NewViewFrom(2, []tree.VNodeDescriptor{{Name: tr.name, ID: tree.ID{0: 7, 11: 1}}, {Name: "parent", ID: tree.ID{0: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	tr.SetView(view, true, false)
	tr.Summaries = &foldMark{digest: view.Digest}
	records := make([]storage.Record, n)
	for i := range records {
		records[i] = storage.Record{Collection: "Many000000000000", Location: fmt.Sprintf("%015x1", i), ID: fmt.Sprint(i)}
	}
	if err := tr.service.Store.SeedLocalRecords(records); err != nil {
		t.Fatal(err)
	}
	return tr
}

func BenchmarkLargeBoundaryContinuation(b *testing.B) {
	const n = 16384
	tr := deepBoundaryTree(b, n)
	const collection = "Many000000000000"
	keys, _ := tr.exportKeysFor(collection, false)
	if len(keys) != n {
		b.Fatalf("got %d boundaries, want %d", len(keys), n)
	}
	for _, tc := range []struct {
		name   string
		cursor string
		want   tree.Key
	}{
		{"first", "", keys[0]},
		{"late", keys[n-3].Location, keys[n-2]},
	} {
		b.Run(tc.name, func(b *testing.B) {
			page := &mesh.SummaryPage{Limit: 1}
			if tc.cursor != "" {
				page.Collection, page.Location = collection, tc.cursor
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				got, more := tr.ExportPage(collection, nil, "parent", page)
				if !more || len(got) != 1 || got[0].Location != tc.want.Location || got[0].Count != 1 {
					b.Fatalf("page %+v, more=%v", got, more)
				}
			}
		})
	}
}
