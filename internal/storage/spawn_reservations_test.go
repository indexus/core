package storage

import (
	"reflect"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestSpawnReservationFollowsPlacementNotExactName(t *testing.T) {
	for _, tc := range []struct {
		name          string
		id            tree.ID
		retired, done bool
	}{
		{name: "exact", id: tree.ID{128}, done: true},
		{name: "fresh nearby identity", id: tree.ID{128, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7}, done: true},
		{name: "unrelated announcement", id: tree.ID{3}},
		{name: "retired exact identity", id: tree.ID{128}, retired: true},
		{name: "retired nearby identity", id: tree.ID{128, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7}, retired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, g := routingStore(t)
			target := tree.VNodeDescriptor{ID: tree.ID{128}}
			target.Name = tree.EncodeName(target.ID)
			if err := source.ReserveSpawns([]tree.VNodeDescriptor{target}); err != nil {
				t.Fatal(err)
			}
			before, err := source.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			if names, err := source.PendingSpawnNames(); err != nil || len(names) != 1 {
				t.Fatalf("reservation lost before activation: %v %v", names, err)
			}
			actual := tree.VNodeDescriptor{ID: tc.id, Name: tree.EncodeName(tc.id)}
			member := causal.Member{VNode: actual, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: actual.Name, Storage: "receiver"}, Host: "https://receiver:21100"}, Retired: tc.retired}
			if tc.retired {
				member.Redirect = g.Holder
			}
			delta := causal.MembershipDelta{Members: []causal.Member{member}}
			for range 2 { // Duplicate announcements must not reopen a consumed request.
				if err := source.MergeMembership(delta); err != nil {
					t.Fatal(err)
				}
				names, err := source.PendingSpawnNames()
				if err != nil {
					t.Fatal(err)
				}
				if (len(names) == 0) != tc.done {
					t.Fatalf("pending=%v want done=%t", names, tc.done)
				}
			}
			if tc.retired {
				member.Retired = false
				member.Redirect = causal.Endpoint{}
				if err := source.MergeMembership(causal.MembershipDelta{Members: []causal.Member{member}}); err != nil {
					t.Fatal(err)
				}
				if names, err := source.PendingSpawnNames(); err != nil || len(names) != 1 {
					t.Fatalf("old active announcement satisfied retired target: %v %v", names, err)
				}
			}
			after, err := source.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Permissions, after.Permissions) {
				t.Fatal("advisory reservation changed write authority")
			}
			absent, err := read(source, func(tx *Tx) (bool, error) { return tx.value(meta(pendingSpawnsKey)) == nil, nil })
			if err != nil || absent != tc.done {
				t.Fatalf("durable GC: absent=%t done=%t err=%v", absent, tc.done, err)
			}
		})
	}
}

func TestSpawnReservationEndpointMoveDoesNotSatisfyCapacity(t *testing.T) {
	source, g := routingStore(t)
	target := tree.VNodeDescriptor{ID: tree.ID{128}}
	target.Name = tree.EncodeName(target.ID)
	if err := source.ReserveSpawns([]tree.VNodeDescriptor{target}); err != nil {
		t.Fatal(err)
	}
	moved := g.Holder
	moved.Host = "https://other-machine:21100"
	moved.Epoch++
	member := causal.Member{VNode: tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, Endpoint: moved}
	if err := source.MergeMembership(causal.MembershipDelta{Members: []causal.Member{member}}); err != nil {
		t.Fatal(err)
	}
	names, err := source.PendingSpawnNames()
	if err != nil || len(names) != 1 {
		t.Fatalf("physical move consumed capacity reservation: %v %v", names, err)
	}
}

func TestSatisfiedSpawnReservationStaysConsumedAfterReopen(t *testing.T) {
	source, _ := routingStore(t)
	target := tree.VNodeDescriptor{ID: tree.ID{128}}
	target.Name = tree.EncodeName(target.ID)
	if err := source.ReserveSpawns([]tree.VNodeDescriptor{target}); err != nil {
		t.Fatal(err)
	}
	actual := tree.VNodeDescriptor{ID: tree.ID{128, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7}}
	actual.Name = tree.EncodeName(actual.ID)
	delta := causal.MembershipDelta{Members: []causal.Member{{VNode: actual, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: actual.Name, Storage: "receiver"}, Host: "https://receiver:21100"}}}}
	if err := source.MergeMembership(delta); err != nil {
		t.Fatal(err)
	}
	path := source.path
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithOptions(path, "D", "test", 1<<26, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.MergeMembership(delta); err != nil {
		t.Fatal(err)
	}
	names, err := reopened.PendingSpawnNames()
	if err != nil || len(names) != 0 {
		t.Fatalf("completed request reopened after restart/replay: %v %v", names, err)
	}
}
