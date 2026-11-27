package storage

import (
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"
)

// Measures cached ACK contact lookup, not fsync throughput or network latency.
func BenchmarkLocalEndpoint(b *testing.B) {
	for _, count := range []int{1, 65} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			store, e := OpenWithOptions(filepath.Join(b.TempDir(), "endpoint"), "root", "bench", 1<<24, Options{NoSync: true})
			if e != nil {
				b.Fatal(e)
			}
			defer store.Close()
			endpoint, e := store.Endpoint("root")
			if e != nil {
				b.Fatal(e)
			}
			g := causal.Genesis{Cluster: "bench", Holder: endpoint}
			if e = store.InitCausal(tree.VNodeDescriptor{Name: "root", ID: tree.ID{1}}, g); e != nil {
				b.Fatal(e)
			}
			var members []causal.Member
			for i := 1; i < count; i++ {
				name := fmt.Sprint("peer", i)
				members = append(members, causal.Member{VNode: tree.VNodeDescriptor{Name: name, ID: tree.ID{byte(i + 1)}}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name}, Host: name}})
			}
			if e = store.MergeMembership(causal.MembershipDelta{Members: members}); e != nil {
				b.Fatal(e)
			}
			if _, e = store.Endpoint("root"); e != nil {
				b.Fatal(e)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, e = store.Endpoint("root"); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}
