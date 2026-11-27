package storage

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

func testFallbackWinner(a, b FallbackEntry) bool {
	if a.Ver != b.Ver {
		return a.Ver > b.Ver
	}
	if a.Record.Deleted != b.Record.Deleted {
		return a.Record.Deleted
	}
	return bytes.Compare(a.Digest[:], b.Digest[:]) < 0
}

func TestFallbackApplyMergeAndAck(t *testing.T) {
	_, owner, replica, _ := openShadowTestStores(t)
	z, err := tree.NewZoneKey("collection-00000", "ef")
	if err != nil {
		t.Fatal(err)
	}

	// Owner writes k1 at seq 1.
	r1 := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{10}}
	if _, _, _, err := owner.ApplyRecord("req-owner-1", r1); err != nil {
		t.Fatal(err)
	}

	// Owner refuses fallback for its own zone.
	if _, err := owner.ApplyFallback("req-fb-own", r1, 1, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner fallback: got %v, want ErrConflict", err)
	}

	// Replica accepts fallback writes for k1 (higher Ver=2) and k2 (new key).
	r1New := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{42}}
	e1, err := replica.ApplyFallback("req-fb-1", r1New, 1, 1<<20)
	if err != nil || e1.Ver != 2 || e1.DotCounter != 1 {
		t.Fatalf("ApplyFallback e1: entry=%+v err=%v", e1, err)
	}
	// Idempotent on requestID replay.
	e1Replay, err := replica.ApplyFallback("req-fb-1", r1New, 1, 1<<20)
	if err != nil || e1Replay.DotCounter != e1.DotCounter || e1Replay.Ver != e1.Ver {
		t.Fatalf("ApplyFallback replay: %+v vs %+v, err=%v", e1Replay, e1, err)
	}

	r2 := Record{Collection: "collection-00000", Location: "ef", ID: "k2", Metrics: []float64{7}}
	if _, err := replica.ApplyFallback("req-fb-2", r2, 1, 1<<20); err != nil {
		t.Fatal(err)
	}

	// Fallback rows stay invisible to live reads/inventory on the replica.
	if snap, err := replica.Snapshot(z); err != nil || len(snap.Records) != 0 {
		t.Fatalf("fallback leaked to Snapshot: %+v err=%v", snap, err)
	}

	// Capacity cap is enforced.
	if _, err := replica.ApplyFallback("req-fb-over", r2, 1, 1); !errors.Is(err, ErrDataCapacity) {
		t.Fatalf("fallback cap: got %v, want ErrDataCapacity", err)
	}

	// Repair: merge fallback entries onto owner, idempotent on retry.
	entries, err := replica.FallbackEntries(z)
	if err != nil || len(entries) != 2 {
		t.Fatalf("FallbackEntries: len=%d err=%v", len(entries), err)
	}
	applied, err := owner.MergeFallback(z, entries, testFallbackWinner)
	if err != nil || applied != 2 {
		t.Fatalf("MergeFallback: applied=%d err=%v", applied, err)
	}
	appliedAgain, err := owner.MergeFallback(z, entries, testFallbackWinner)
	if err != nil || appliedAgain != 0 {
		t.Fatalf("MergeFallback retry: applied=%d err=%v", appliedAgain, err)
	}

	snap, err := owner.Snapshot(z)
	if err != nil || len(snap.Records) != 2 {
		t.Fatalf("owner Snapshot after repair: %+v err=%v", snap, err)
	}

	// FallbackZones lists z while pending and clears after AckFallback.
	zones, err := replica.FallbackZones()
	if err != nil || len(zones) != 1 || zones[0] != z {
		t.Fatalf("FallbackZones before ack = (%v, %v), want [%v]", zones, err, z)
	}

	// AckFallback clears confirmed entries on the replica.
	if err := replica.AckFallback(z, []string{"req-fb-1", "req-fb-2"}); err != nil {
		t.Fatal(err)
	}
	entries, err = replica.FallbackEntries(z)
	if err != nil || len(entries) != 0 {
		t.Fatalf("FallbackEntries after ack: len=%d err=%v", len(entries), err)
	}
	zones, err = replica.FallbackZones()
	if err != nil || len(zones) != 0 {
		t.Fatalf("FallbackZones after ack = (%v, %v), want empty", zones, err)
	}
	assertStagingIndex(t, replica)
	assertStagingIndex(t, owner)
}

func TestFallbackMergeCommutativityAndDeleteWins(t *testing.T) {
	z, err := tree.NewZoneKey("collection-00000", "ef")
	if err != nil {
		t.Fatal(err)
	}
	initRec := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{1}}

	// Build two competing fallback batches from two independent replicas:
	// batchB writes k1 (Ver=2) and k2 (Ver=3); batchC writes k1 (Ver=2).
	_, _, repB, _ := openShadowTestStores(t)
	_, _, repC, _ := openShadowTestStores(t)
	rB1 := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{100}}
	rB2 := Record{Collection: "collection-00000", Location: "ef", ID: "k2", Metrics: []float64{200}}
	rC1 := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{300}}
	if _, err := repB.ApplyFallback("b-1", rB1, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repB.ApplyFallback("b-2", rB2, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repC.ApplyFallback("c-1", rC1, 1, 0); err != nil {
		t.Fatal(err)
	}
	batchB, err := repB.FallbackEntries(z)
	if err != nil {
		t.Fatal(err)
	}
	batchC, err := repC.FallbackEntries(z)
	if err != nil {
		t.Fatal(err)
	}

	runOrder := func(first, second []FallbackEntry) map[string]Record {
		_, owner, _, _ := openShadowTestStores(t)
		if _, _, _, err := owner.ApplyRecord("init-1", initRec); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.MergeFallback(z, first, testFallbackWinner); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.MergeFallback(z, second, testFallbackWinner); err != nil {
			t.Fatal(err)
		}
		snap, err := owner.Snapshot(z)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]Record, len(snap.Records))
		for _, r := range snap.Records {
			out[r.ID] = r
		}
		return out
	}

	gotBC := runOrder(batchB, batchC)
	gotCB := runOrder(batchC, batchB)
	if len(gotBC) != len(gotCB) || gotBC["k1"].Metrics[0] != gotCB["k1"].Metrics[0] || gotBC["k2"].Metrics[0] != gotCB["k2"].Metrics[0] {
		t.Fatalf("non-commutative merge: B->C=%+v vs C->B=%+v", gotBC, gotCB)
	}

	// Delete wins on equal Ver, and a later local write supersedes it.
	_, owner, repD, _ := openShadowTestStores(t)
	if _, _, _, err := owner.ApplyRecord("o-1", initRec); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := owner.ApplyRecord("o-2", Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{2}}); err != nil {
		t.Fatal(err)
	}
	delRec := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Deleted: true}
	delEntry, err := repD.ApplyFallback("d-1", delRec, 1, 0) // Ver=2 == owner's k1 seq=2
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := owner.MergeFallback(z, []FallbackEntry{delEntry}, testFallbackWinner); err != nil || applied != 1 {
		t.Fatalf("delete-wins merge: applied=%d err=%v", applied, err)
	}
	snap, err := owner.Snapshot(z)
	if err != nil || len(snap.Records) != 1 || !snap.Records[0].Deleted {
		t.Fatalf("expected k1 deleted after equal-Ver merge, got %+v err=%v", snap.Records, err)
	}
}

func TestFallbackPersistenceAcrossRestart(t *testing.T) {
	dir, _, replica, _ := openShadowTestStores(t)
	z, err := tree.NewZoneKey("collection-00000", "ef")
	if err != nil {
		t.Fatal(err)
	}
	r1 := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{5}}
	e1, err := replica.ApplyFallback("req-1", r1, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestData(t, filepath.Join(dir, "replica"), "W000000000000000")
	defer reopened.Close()
	entries, err := reopened.FallbackEntries(z)
	if err != nil || len(entries) != 1 || entries[0].Ver != e1.Ver || entries[0].DotCounter != 1 {
		t.Fatalf("reopened FallbackEntries = %+v err=%v", entries, err)
	}
	e2, err := reopened.ApplyFallback("req-2", r1, 3, 0)
	if err != nil || e2.DotCounter != 2 || e2.Ver != e1.Ver+1 {
		t.Fatalf("post-restart ApplyFallback = %+v err=%v", e2, err)
	}
	if sessions := assertStagingIndex(t, reopened); !slices.Equal(sessions, []string{fallbackSession(z)}) {
		t.Fatalf("indexed staging sessions %q", sessions)
	}
}

func TestDegradedReadOverlaysFallbackOnShadow(t *testing.T) {
	_, owner, replica, ownerHolder := openShadowTestStores(t)
	z, err := tree.NewZoneKey("collection-00000", "ef")
	if err != nil {
		t.Fatal(err)
	}
	r1 := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{10}}
	r2 := Record{Collection: "collection-00000", Location: "ef", ID: "k2", Metrics: []float64{20}}
	if _, _, _, err := owner.ApplyRecord("o-1", r1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := owner.ApplyRecord("o-2", r2); err != nil {
		t.Fatal(err)
	}
	snap, err := owner.Snapshot(z)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.PreloadShadow(ownerHolder, []Snapshot{snap}); err != nil {
		t.Fatal(err)
	}

	// Reads from shadow when no fallback write exists.
	got, ok, err := replica.DegradedRead(ownerHolder, z, "k1")
	if err != nil || !ok || got.Metrics[0] != 10 {
		t.Fatalf("DegradedRead shadow k1 = (%+v, %v, %v), want 10", got, ok, err)
	}
	// Fallback update overlays shadow for k1, while k2 still reads shadow.
	r1New := Record{Collection: "collection-00000", Location: "ef", ID: "k1", Metrics: []float64{99}}
	if _, err := replica.ApplyFallback("fb-1", r1New, snap.Seq, 0); err != nil {
		t.Fatal(err)
	}
	got, ok, err = replica.DegradedRead(ownerHolder, z, "k1")
	if err != nil || !ok || got.Metrics[0] != 99 {
		t.Fatalf("DegradedRead fallback k1 = (%+v, %v, %v), want 99", got, ok, err)
	}
	got2, ok, err := replica.DegradedRead(ownerHolder, z, "k2")
	if err != nil || !ok || got2.Metrics[0] != 20 {
		t.Fatalf("DegradedRead shadow k2 = (%+v, %v, %v), want 20", got2, ok, err)
	}
	// Fallback delete hides the shadowed k2.
	r2Del := Record{Collection: "collection-00000", Location: "ef", ID: "k2", Deleted: true}
	if _, err := replica.ApplyFallback("fb-2", r2Del, snap.Seq, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := replica.DegradedRead(ownerHolder, z, "k2"); err != nil || ok {
		t.Fatalf("DegradedRead deleted k2 = (ok=%v, err=%v), want false", ok, err)
	}
}

func openShadowBenchStores(b *testing.B) (owner, replica *Store, ownerHolder causal.Endpoint) {
	b.Helper()
	dir := b.TempDir()
	var err error
	owner, err = Open(filepath.Join(dir, "owner"), "0000000000000000", "bench", 1<<30)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = owner.Close() })
	replica, err = Open(filepath.Join(dir, "replica"), "W000000000000000", "bench", 1<<30)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = replica.Close() })
	ownerID, err := owner.Identity()
	if err != nil {
		b.Fatal(err)
	}
	desc := tree.VNodeDescriptor{Name: "0000000000000000", ID: tree.ID{1}}
	ownerHolder = causal.Endpoint{Host: "http://owner", Owner: causal.Owner{VNode: "0000000000000000", Storage: ownerID}}
	g := causal.Genesis{Cluster: "bench", Holder: ownerHolder}
	if err := owner.InitCausal(desc, g); err != nil {
		b.Fatal(err)
	}
	if err := replica.InitCausal(desc, g); err != nil {
		b.Fatal(err)
	}
	return owner, replica, ownerHolder
}

func BenchmarkPreloadAndMirrorShadow(b *testing.B) {
	owner, replica, ownerHolder := openShadowBenchStores(b)
	z, _ := tree.NewZoneKey("collection-00000", "ef")
	r0 := Record{Collection: "collection-00000", Location: "ef", ID: "k0", Metrics: []float64{1}}
	if _, _, _, err := owner.ApplyRecord("seed", r0); err != nil {
		b.Fatal(err)
	}
	snap, err := owner.Snapshot(z)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := replica.PreloadShadow(ownerHolder, []Snapshot{snap}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := Record{Collection: "collection-00000", Location: "ef", ID: fmt.Sprintf("k-%d", i), Metrics: []float64{float64(i)}}
		m, _, _, err := owner.ApplyRecord(fmt.Sprintf("w-%d", i), r)
		if err != nil {
			b.Fatal(err)
		}
		if err := replica.MirrorShadow(ownerHolder, []ZoneMutations{{Zone: z, Mutations: []Mutation{m}}}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplyAndMergeFallback(b *testing.B) {
	owner, replica, _ := openShadowBenchStores(b)
	z, _ := tree.NewZoneKey("collection-00000", "ef")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := Record{Collection: "collection-00000", Location: "ef", ID: fmt.Sprintf("k-%d", i), Metrics: []float64{float64(i)}}
		e, err := replica.ApplyFallback(fmt.Sprintf("fb-%d", i), r, 0, 0)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := owner.MergeFallback(z, []FallbackEntry{e}, testFallbackWinner); err != nil {
			b.Fatal(err)
		}
	}
}
