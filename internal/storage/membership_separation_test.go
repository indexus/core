package storage

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestLocalControlReusesImmutableMembership(t *testing.T) {
	store, _ := routingStore(t)
	if _, err := store.CausalState(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := store.update(func(tx *Tx) error {
			tx.reader.Reader = keysOnlyReader{tx.reader.Reader, "", map[string]bool{meta(membershipKey): true}}
			state, err := readCausal(tx)
			if err != nil {
				return err
			}
			state.Permissions.EndpointEpoch++
			if err = writeCausal(tx, state); err != nil {
				return err
			}
			if _, rewritten := tx.pending[meta(membershipKey)]; rewritten {
				t.Fatal("local authority control rewrote unchanged network membership")
			}
			return nil
		}); err != nil {
			t.Fatal("local control loaded whole membership", err)
		}
	}
}

func TestMembershipExportsAreDetachedFromInternalSnapshots(t *testing.T) {
	store, _ := routingStore(t)
	original, err := store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	want := original.Membership.Members[0]
	original.Membership.Members[0].Endpoint.Host = "caller-mutated"
	_, _, first, err := store.Membership()
	if err != nil || first.Members[0] != want {
		t.Fatalf("CausalState caller changed cached membership: %v %v", first, err)
	}
	first.Members[0].Retired = true
	_, _, second, err := store.Membership()
	if err != nil || second.Members[0] != want {
		t.Fatalf("Membership caller changed cached membership: %v %v", second, err)
	}
}

func TestMembershipReadsKeepTheirMVCCVersionAcrossMerge(t *testing.T) {
	store, _ := routingStore(t)
	before, err := store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := store.db.Snapshot()
	defer snapshot.Close()
	member := before.Membership.Members[0]
	member.Endpoint.Epoch++
	member.Endpoint.Host += "-next"
	if err := store.MergeMembership(causal.MembershipDelta{Members: []causal.Member{member}}); err != nil {
		t.Fatal(err)
	}
	// Force both reads through their own snapshot's physical membership row.
	for i := range store.membershipCache {
		store.membershipCache[i].Store(nil)
	}
	old, err := readCausal(store.transaction(snapshot, false))
	if err != nil || old.MembershipDigest != before.MembershipDigest || old.Membership.Members[0] != before.Membership.Members[0] {
		t.Fatalf("old committed read saw later membership: %v %v", old.Membership, err)
	}
	current, err := store.CausalState()
	if err != nil || current.MembershipDigest == old.MembershipDigest || current.Membership.Members[0] != member {
		t.Fatalf("new committed read missed merged membership: %v %v", current.Membership, err)
	}
}

func TestAbortedMembershipDoesNotReplaceCommittedReference(t *testing.T) {
	store, _ := routingStore(t)
	before, err := store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort membership transaction")
	err = store.update(func(tx *Tx) error {
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		delta := causal.MembershipDelta{Members: append([]causal.Member(nil), state.Membership.Members...)}
		delta.Members[0].Endpoint.Host += "-uncommitted"
		if err = writeMembership(tx, &state, delta); err != nil {
			return err
		}
		if err = writeCausal(tx, state); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	after, err := store.CausalState()
	if err != nil || after.MembershipDigest != before.MembershipDigest || after.Membership.Members[0] != before.Membership.Members[0] {
		t.Fatalf("aborted cached membership became current: %v %v", after.Membership, err)
	}
}

func TestMembershipBlobDigestIsCheckedAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "membership")
	store, err := OpenWithOptions(path, "root", "routing", 1<<24, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := store.Endpoint("root")
	if err = store.InitCausal(tree.VNodeDescriptor{Name: "root", ID: tree.ID{1}}, causal.Genesis{Cluster: "routing", Holder: endpoint}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.update(func(tx *Tx) error { return tx.put(meta(membershipKey), []byte(`{"Members":[]}`)) }); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenWithOptions(path, "root", "routing", 1<<24, Options{NoSync: true})
	if err != nil {
		if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		return
	}
	defer store.Close()
	if _, err = store.CausalState(); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed physical membership accepted under old digest: %v", err)
	}
}
