package mesh

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Measure preparation separately from RPC and fsync. A constant-size wire
// reference must not be mistaken for constant CPU/allocations at its producer.
func BenchmarkMembershipReferencePreparation(b *testing.B) {
	for _, n := range []int{64, 256, 1024, 4096, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			root := tree.VNodeDescriptor{Name: "root", ID: tree.ID{1}}
			genesis := causal.Genesis{Cluster: "scale", Holder: causal.Endpoint{
				Owner: causal.Owner{VNode: root.Name, Storage: "root-disk"}, Host: "root:1",
			}}
			m, err := causal.NewMembership(root, genesis)
			if err != nil {
				b.Fatal(err)
			}
			members := make([]causal.Member, n-1)
			for i := range members {
				name := fmt.Sprintf("node-%08d", i)
				hash := sha256.Sum256([]byte(name))
				members[i] = causal.Member{
					VNode:    tree.VNodeDescriptor{Name: name, ID: tree.ID(hash[:tree.Size])},
					Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name + "-disk"}, Host: name + ":1"},
				}
			}
			if err := m.Merge(causal.MembershipDelta{Members: members}); err != nil {
				b.Fatal(err)
			}
			w := newMembershipWire(func() (*MembershipEnvelope, error) {
				return &MembershipEnvelope{Root: root, Genesis: genesis, Delta: m.Export()}, nil
			}, nil)
			defer w.close()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				p, err := w.packet(causal.Digest{}, false)
				if err != nil || p.Body != nil {
					b.Fatalf("reference packet: %v", err)
				}
			}
		})
	}
}
