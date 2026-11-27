package storage

import (
	"errors"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

func openShadowTestStores(t *testing.T) (dir string, owner, replica *Store, ownerHolder causal.Endpoint) {
	t.Helper()
	dir = t.TempDir()
	owner = openTestData(t, filepath.Join(dir, "owner"), "0000000000000000")
	t.Cleanup(func() { _ = owner.Close() })
	replica = openTestData(t, filepath.Join(dir, "replica"), "W000000000000000")
	t.Cleanup(func() { _ = replica.Close() })

	ownerID, err := owner.Identity()
	if err != nil {
		t.Fatal(err)
	}
	desc := tree.VNodeDescriptor{Name: "0000000000000000", ID: tree.ID{1}}
	ownerHolder = causal.Endpoint{Host: "http://owner", Owner: causal.Owner{VNode: "0000000000000000", Storage: ownerID}}
	g := causal.Genesis{
		Cluster: "test",
		Holder:  ownerHolder,
	}
	if err := owner.InitCausal(desc, g); err != nil {
		t.Fatal(err)
	}
	if err := replica.InitCausal(desc, g); err != nil {
		t.Fatal(err)
	}
	return dir, owner, replica, ownerHolder
}

// W2 + SHADOW-1: shadow rows are invisible to reads, inventory, summaries and
// writableIn, and a disk refuses to shadow a zone it owns.
func TestShadowIsolationAndOwnershipGuard(t *testing.T) {
	_, owner, replica, ownerHolder := openShadowTestStores(t)

	r1 := Record{Collection: "collection-00000", Location: "ab", ID: "k1", Metrics: []float64{10}}
	if _, _, _, err := owner.ApplyRecord("req-1", r1); err != nil {
		t.Fatal(err)
	}
	z, err := tree.NewZoneKey("collection-00000", "ab")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := owner.Snapshot(z)
	if err != nil {
		t.Fatal(err)
	}

	// Owner refuses to shadow its own zone or its own storage ID.
	if _, err := owner.PreloadShadow(ownerHolder, []Snapshot{snap}); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner self-shadow: got %v, want ErrConflict", err)
	}
	other := causal.Endpoint{Host: "http://other", Owner: causal.Owner{VNode: "W000000000000000", Storage: "other-storage"}}
	if _, err := owner.PreloadShadow(other, []Snapshot{snap}); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner shadowing owned zone: got %v, want ErrConflict", err)
	}

	// Replica admits shadow preload.
	prog, err := replica.PreloadShadow(ownerHolder, []Snapshot{snap})
	if err != nil {
		t.Fatal(err)
	}
	if prog[sealKey(z)] != 1 {
		t.Fatalf("preload progress = %d, want 1", prog[sealKey(z)])
	}

	// W2: invisible to zone reads, inventory, summaries and writableIn.
	if gotSnap, err := replica.Snapshot(z); err != nil || len(gotSnap.Records) != 0 {
		t.Fatalf("shadow row leaked to Snapshot: len=%d err=%v", len(gotSnap.Records), err)
	}
	inv, err := replica.Inventory(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 0 {
		t.Fatalf("shadow row leaked to Inventory: %d snapshots", len(inv))
	}
	total, err := replica.ZoneTotal("collection-00000", "ab")
	if err != nil {
		t.Fatal(err)
	}
	if total.Count != 0 {
		t.Fatalf("shadow row leaked to ZoneTotal: %+v", total)
	}
	if _, _, _, err := replica.ApplyRecord("req-local", r1); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("shadow made zone writable: got %v, want ErrNotOwner", err)
	}
}

// W4 + SHADOW-2: mirror enforces contiguous sequence numbers, is idempotent on
// replay, and persists staged progress across store reopen.
func TestShadowMirrorSequenceAndRestartProgress(t *testing.T) {
	dir, owner, replica, ownerHolder := openShadowTestStores(t)

	z, err := tree.NewZoneKey("collection-00000", "cd")
	if err != nil {
		t.Fatal(err)
	}
	r1 := Record{Collection: "collection-00000", Location: "cd", ID: "k1", Metrics: []float64{5}}
	if _, _, _, err := owner.ApplyRecord("req-1", r1); err != nil {
		t.Fatal(err)
	}
	snap, err := owner.Snapshot(z)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.PreloadShadow(ownerHolder, []Snapshot{snap}); err != nil {
		t.Fatal(err)
	}

	r2 := Record{Collection: "collection-00000", Location: "cd", ID: "k2", Metrics: []float64{7}}
	r3 := Record{Collection: "collection-00000", Location: "cd", ID: "k3", Metrics: []float64{9}}
	// SHADOW-2: gap (seq 3 before seq 2) is refused with ErrSequence.
	if err := replica.MirrorShadow(ownerHolder, []ZoneMutations{{
		Zone:      z,
		Mutations: []Mutation{{Seq: 3, RequestID: "req-3", Record: r3}},
	}}); !errors.Is(err, ErrSequence) {
		t.Fatalf("gap mirror: got %v, want ErrSequence", err)
	}

	// Contiguous seq 2 succeeds and is idempotent on replay.
	m2 := ZoneMutations{Zone: z, Mutations: []Mutation{{Seq: 2, RequestID: "req-2", Record: r2}}}
	if err := replica.MirrorShadow(ownerHolder, []ZoneMutations{m2}); err != nil {
		t.Fatalf("mirror seq 2: %v", err)
	}
	if err := replica.MirrorShadow(ownerHolder, []ZoneMutations{m2}); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if seq, ok, err := replica.ShadowSeq(ownerHolder, z); err != nil || !ok || seq != 2 {
		t.Fatalf("ShadowSeq = (%d, %v, %v), want (2, true, nil)", seq, ok, err)
	}

	// W4: reopen replica and verify shadow sequences survive restart.
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestData(t, filepath.Join(dir, "replica"), "W000000000000000")
	defer reopened.Close()

	got, err := reopened.ShadowSeqs(ownerHolder, []tree.ZoneKey{z})
	if err != nil {
		t.Fatal(err)
	}
	if got[sealKey(z)] != 2 {
		t.Fatalf("reopened ShadowSeqs = %v, want seq 2", got)
	}

}
