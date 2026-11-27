package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestCommittedCausalNeverRebuildsOnMiss(t *testing.T) {
	t.Parallel()
	s, _ := routingStore(t)
	r, err := s.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.committedCausal(); got == nil || got.state.MembershipDigest != r.snapshot.state.MembershipDigest {
		t.Fatal("cached hint differs from committed facts")
	}
	s.causalCache.Store(nil)
	s.causalPrevious.Store(nil)
	s.causalCacheMu.Lock()
	defer s.causalCacheMu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- s.committedCausal() != nil }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("missing snapshot returned as current")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MembershipDigest hint waited for control reconstruction")
	}
}

// Routing resolves a holder per zone: while nothing was committed, asking
// for the committed snapshot again reads nothing and allocates nothing, and
// the first commit that changes the control state is seen at once.
func TestCommittedCausalIsFreeBetweenCommits(t *testing.T) {
	s, _ := routingStore(t)
	if _, err := s.CausalRouting(); err != nil {
		t.Fatal(err)
	}
	before := s.committedCausal()
	if before == nil {
		t.Fatal("no committed snapshot after a routing read")
	}
	if n := testing.AllocsPerRun(100, func() {
		if s.committedCausal() != before {
			t.Fatal("snapshot changed without a commit")
		}
	}); n != 0 {
		t.Fatalf("committedCausal allocates %v objects between commits", n)
	}
	if err := s.update(func(tx *Tx) error { return tx.put(meta(controlVersionKey), []byte("another-version")) }); err != nil {
		t.Fatal(err)
	}
	if got := s.committedCausal(); got == before {
		t.Fatal("stale snapshot served after the control version changed")
	}
}

// Every acknowledged write and every summary contribution asks for this
// disk's endpoint. Between commits of the control state the answer comes from
// the committed snapshot: no transaction, no row read, no allocation. A
// commit that changes the endpoint epoch is seen at once.
func TestEndpointIsFreeBetweenCommits(t *testing.T) {
	s, _ := routingStore(t)
	if _, err := s.CausalRouting(); err != nil {
		t.Fatal(err)
	}
	before, err := s.Endpoint("D")
	if err != nil || before.Storage == "" || before.VNode != "D" || before.Host != "D" {
		t.Fatalf("endpoint %+v, %v", before, err)
	}
	if n := testing.AllocsPerRun(100, func() {
		if got, err := s.Endpoint("D"); err != nil || got != before {
			t.Fatalf("endpoint %+v, %v; want %+v", got, err, before)
		}
	}); n != 0 {
		t.Fatalf("Endpoint allocates %v objects between commits", n)
	}
	if err := s.updateCausal(func(tx *Tx, state CausalState) error {
		state.Permissions.EndpointEpoch++
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Endpoint("D")
	if err != nil || after.Epoch != before.Epoch+1 || after.Owner != before.Owner {
		t.Fatalf("endpoint after an epoch commit %+v, %v; before %+v", after, err, before)
	}
}

// BenchmarkEndpointAfterWrite is the endpoint read of one acknowledged write:
// each write commits, so the first read after it revalidates the snapshot.
func BenchmarkEndpointAfterWrite(b *testing.B) {
	s, err := OpenWithOptions(filepath.Join(b.TempDir(), "endpoint.db"), "D", "test", 1<<30, Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	h, _ := s.Endpoint("D")
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: h}); err != nil {
		b.Fatal(err)
	}
	for _, reads := range []int{1, 8} {
		b.Run(fmt.Sprint("reads=", reads), func(b *testing.B) {
			b.ReportAllocs()
			for i := range b.N {
				b.StopTimer()
				if err := s.update(func(tx *Tx) error { return tx.put(meta([]byte("unrelated")), number(uint64(i))) }); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				for range reads {
					if _, err := s.Endpoint("D"); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
