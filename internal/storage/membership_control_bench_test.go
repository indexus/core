package storage

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// These are committed local-control transactions with unchanged membership.
// NoSync isolates membership-dependent preparation from physical fsync latency;
// the result is neither client p99 nor network/cluster throughput.
func BenchmarkLocalControlMembershipSize(b *testing.B) {
	for _, count := range []int{16, 64, 256, 1024} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			store, err := OpenWithOptions(filepath.Join(b.TempDir(), "control"), "root", "bench", 1<<30, Options{NoSync: true})
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			endpoint, err := store.Endpoint("root")
			if err != nil {
				b.Fatal(err)
			}
			var rootID tree.ID
			binary.BigEndian.PutUint64(rootID[4:], 1)
			if err = store.InitCausal(tree.VNodeDescriptor{Name: "root", ID: rootID}, causal.Genesis{Cluster: "bench", Holder: endpoint}); err != nil {
				b.Fatal(err)
			}
			members := make([]causal.Member, 0, count-1)
			for i := 1; i < count; i++ {
				name := fmt.Sprint("peer", i)
				var id tree.ID
				binary.BigEndian.PutUint64(id[4:], uint64(i+1))
				members = append(members, causal.Member{VNode: tree.VNodeDescriptor{Name: name, ID: id}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name}, Host: name}})
			}
			if err = store.MergeMembership(causal.MembershipDelta{Members: members}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := store.updateCausal(func(tx *Tx, state CausalState) error {
					state.Permissions.EndpointEpoch++
					return writeCausal(tx, state)
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
