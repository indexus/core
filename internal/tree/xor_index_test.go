package tree

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
)

func xorMembers(n int, clustered bool) []VNodeDescriptor {
	r := rand.New(rand.NewSource(817))
	out := make([]VNodeDescriptor, n)
	for i := range out {
		var id ID
		if !clustered {
			r.Read(id[:])
		}
		id[10], id[11] = byte((i+1)>>8), byte(i+1)
		out[i] = VNodeDescriptor{Name: fmt.Sprintf("node-%04d", i), ID: id}
	}
	return out
}
func assertXOROracle(t *testing.T, v *View, r *rand.Rand) {
	t.Helper()
	ids := make([]ID, len(v.Active))
	for i, d := range v.Active {
		ids[i] = d.ID
	}
	for i := 0; i < 300; i++ {
		var zone ID
		r.Read(zone[:])
		if i < len(ids) {
			zone = ids[i]
		}
		expected := testoracle.OwnerSlow(zone, ids)
		if got := v.Owner(zone); got != expected {
			t.Fatalf("n=%d zone=%s got=%s want=%s", len(ids), zone, got, expected)
		}
	}
}
func TestXORIndexMatchesLinearAcrossAllBitsAndMutations(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 17, 64, 256, 1024} {
		for _, clustered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/clustered=%t", n, clustered), func(t *testing.T) {
				active := xorMembers(n, clustered)
				v, err := NewViewFrom(7, active)
				if err != nil {
					t.Fatal(err)
				}
				r := rand.New(rand.NewSource(91))
				assertXOROracle(t, v, r)
				if n >= 16 && len(v.xorIndex) != 2*n-1 {
					t.Fatal("index is not compressed")
				}
				copy := v.Clone()
				if n > 0 {
					copy.Remove(active[n/2].Name)
					active[0].Name = "changed-input"
				}
				added := VNodeDescriptor{Name: "new", ID: ID{7, 9, 1}}
				if err := copy.Add(added.Name, added.ID); err != nil {
					t.Fatal(err)
				}
				assertXOROracle(t, copy, r)
				assertXOROracle(t, v, r)
				next, err := Apply(v, Delta{BaseDigest: v.Digest, Add: []VNodeDescriptor{added}})
				if err != nil {
					t.Fatal(err)
				}
				assertXOROracle(t, next, r)
				assertXOROracle(t, v, r)
				if n > 0 && v.Active[0].Name == "changed-input" {
					t.Fatal("input slice leaked")
				}
			})
		}
	}
	// The last bit is a real branch, not a truncated address or location alias.
	members := xorMembers(16, true)
	members[0].ID = ID{11}
	members[1].ID = ID{11, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	v, err := NewViewFrom(0, members)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range members {
		if v.Owner(d.ID) != d.ID {
			t.Fatal("lost exact 96-bit owner")
		}
	}
}
func TestBulkViewValidationAndDetachedConcurrentClones(t *testing.T) {
	members := xorMembers(64, false)
	v, err := NewViewFrom(4, members)
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewView(4)
	for _, d := range members {
		if err := legacy.Add(d.Name, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	if legacy.Digest != v.Digest {
		t.Fatal("bulk digest differs from incremental")
	}
	cases := [][]VNodeDescriptor{{{Name: "bad"}}, {{ID: ID{1}}}, {members[0], {Name: members[0].Name, ID: ID{1}}}, {members[0], {Name: "other", ID: members[0].ID}}}
	for _, bad := range cases {
		if _, err := NewViewFrom(4, bad); err == nil {
			t.Fatal("accepted invalid membership")
		}
	}
	copy := v.Clone()
	copy.ReserveRetired(ID{9})
	copy.Active[0].Name = "external"
	copy.Generation = 99
	copy.Digest = [32]byte{}
	if v.IsRetiredID(ID{9}) || v.Active[0].Name == "external" || v.Generation != 4 || v.Digest != legacy.Digest {
		t.Fatal("clone mutated original")
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c := v.Clone()
				c.Remove(members[i%len(members)].Name)
				if v.Owner(members[0].ID) != members[0].ID {
					t.Error("original changed")
				}
			}
		}()
	}
	wg.Wait()
}

var ownerBenchmarkSink ID

func BenchmarkViewBulkAndRouting(b *testing.B) {
	for _, n := range []int{16, 64, 256, 1024} {
		members := xorMembers(n, false)
		b.Run(fmt.Sprintf("build-add/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				v := NewView(0)
				for _, d := range members {
					if err := v.Add(d.Name, d.ID); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
		b.Run(fmt.Sprintf("build-bulk/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := NewViewFrom(0, members); err != nil {
					b.Fatal(err)
				}
			}
		})
		v, _ := NewViewFrom(0, members)
		ids := make([]ID, n)
		for i, d := range members {
			ids[i] = d.ID
		}
		zone := ID{17, 91, 11}
		b.Run(fmt.Sprintf("owner-index/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ownerBenchmarkSink = v.Owner(zone)
			}
		})
		b.Run(fmt.Sprintf("owner-linear/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ownerBenchmarkSink = testoracle.OwnerSlow(zone, ids)
			}
		})
	}
}

// Pinned independently from the pre-bulk encoding: generation big endian,
// then descriptors ordered by their complete ID, each name terminated by NUL.
func TestBulkViewPreservesCanonicalDigestEncoding(t *testing.T) {
	v, err := NewViewFrom(7, []VNodeDescriptor{{Name: "alpha", ID: ID{1}}, {Name: "beta", ID: ID{0, 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", v.Digest); got != "aca92e5a6cadff4c53dae063c87958da88d23ab53699136ded9d2e0f518ccb7f" {
		t.Fatalf("canonical digest changed: %s", got)
	}
}
