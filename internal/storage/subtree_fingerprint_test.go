package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

func TestSubtreeFingerprintMutationAggregationAndCache(t *testing.T) {
	s, _, _ := ticketStores(t)
	const c = "0000000000000001"
	put := func(id, loc, body string, deleted bool) {
		t.Helper()
		r := Record{Collection: c, Location: loc, ID: id, Deleted: deleted, Metrics: []float64{1, 2}, Payload: json.RawMessage(body)}
		if _, _, _, err := s.ApplyRecord("fp-"+id+"-"+body+loc, r); err != nil {
			t.Fatal(err)
		}
	}
	get := func(loc string) SubtreeFingerprint {
		t.Helper()
		v, e := s.LocalSubtreeFingerprint(context.Background(), c, loc)
		if e != nil || v.Fingerprint == "" || v.Guard == "" {
			t.Fatalf("%s: %+v %v", loc, v, e)
		}
		return v
	}
	put("self", "a", `"a"`, false)
	put("child", "a0", `"x"`, false)
	put("other", "a9b", `"y"`, false)
	if err := s.db.Flush(); err != nil {
		t.Fatal(err)
	}
	before, _ := zonelog.ScanWork()
	first := get("a")
	cold, _ := zonelog.ScanWork()
	hot := get("a")
	after, _ := zonelog.ScanWork()
	if first != hot || first.Count != 3 || cold-before < 3 || after != cold {
		t.Fatal(first, hot, before, cold, after)
	}
	t.Logf("3-record prefix, cold keys=%d hot keys=%d", cold-before, after-cold)
	direct := SubtreeFingerprint{}
	direct.addRow(Record{Collection: c, Location: "a", ID: "self", Metrics: []float64{1, 2}, Payload: json.RawMessage(`"a"`)})
	sum := new(big.Int).SetBytes(direct.Sum[:])
	count := direct.Count
	for _, loc := range []string{"a0", "a9", "a1"} {
		v := get(loc)
		sum.Add(sum, new(big.Int).SetBytes(v.Sum[:]))
		count += v.Count
	}
	sum.Mod(sum, new(big.Int).Lsh(big.NewInt(1), 256))
	if sum.Cmp(new(big.Int).SetBytes(first.Sum[:])) != 0 || first.Count != count {
		t.Fatal("noncomposable subtree")
	}
	put("unrelated", "z", `"z"`, false)
	outside := get("a")
	if outside.Fingerprint != first.Fingerprint || outside.Guard == first.Guard {
		t.Fatal("unrelated write must only invalidate global guard")
	}
	put("child", "a0", `"new"`, false)
	changed := get("a")
	if changed.Fingerprint == outside.Fingerprint || changed.Count != 3 {
		t.Fatal("payload-only update missed")
	}
	put("child", "a0", "", true)
	deleted := get("a")
	if deleted.Count != 2 || deleted.Fingerprint == changed.Fingerprint {
		t.Fatal("delete missed")
	}
	put("transient", "a1", `"temporary"`, false)
	put("transient", "a1", "", true)
	aba := get("a")
	if aba.Fingerprint != deleted.Fingerprint || aba.Guard == deleted.Guard {
		t.Fatal("ABA guard failed")
	}
}

func TestSubtreeFingerprintRestartAndTransfer(t *testing.T) {
	s, g := routingStore(t)
	const c = "Collection000000"
	ctx := context.Background()
	before, err := s.LocalSubtreeFingerprint(ctx, c, "@")
	if err != nil || before.Guard == "" {
		t.Fatal(before, err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(path, s.node, s.cluster, 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	again, err := fresh.LocalSubtreeFingerprint(ctx, c, "@")
	if err != nil || again.Fingerprint == "" || again.Sum != before.Sum || again.Guard == before.Guard || again.Fingerprint == before.Fingerprint {
		t.Fatal(before, again, err)
	}
	target, err := Open(filepath.Join(t.TempDir(), "target"), "J", "test", 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	state, _ := fresh.CausalState()
	if err := target.InitCausalReceiver(state.Root, g); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, fresh, target, g.Right().ID, tree.Region{})
	z, _ := tree.NewZoneKey(c, "a")
	if _, err = fresh.FreezeZones(op.Cession.ID, []tree.ZoneKey{z}); err != nil {
		t.Fatal(err)
	}
	got, err := fresh.LocalSubtreeFingerprint(ctx, c, "@")
	if err != nil || got != (SubtreeFingerprint{}) {
		t.Fatal("transferring content must be unknown", got, err)
	}
}

func TestFingerprintEmptyBinaryAndJSONDifferentAcrossFlush(t *testing.T) {
	s, _, _ := ticketStores(t)
	const c = "0000000000000001"
	fingerprint := func() SubtreeFingerprint {
		t.Helper()
		v, e := s.LocalSubtreeFingerprint(context.Background(), c, "a")
		if e != nil || v.Fingerprint == "" || v.Count != 1 {
			t.Fatal(v, e)
		}
		return v
	}
	data := Record{Collection: c, Location: "a", ID: "x", Binary: true, Payload: json.RawMessage(`""`)}
	if _, _, _, e := s.ApplyRecord("binary", data); e != nil {
		t.Fatal(e)
	}
	memory := fingerprint()
	if e := s.db.Flush(); e != nil {
		t.Fatal(e)
	}
	// Force a cache miss with the same corpus, including a practically
	// zero-length value freshly read from a disk segment.
	if e := s.update(func(tx *Tx) error { return tx.put(meta([]byte("fp-test")), []byte("updated")) }); e != nil {
		t.Fatal(e)
	}
	disk := fingerprint()
	if disk.Sum != memory.Sum || disk.Fingerprint != memory.Fingerprint || disk.Guard == memory.Guard {
		t.Fatal(memory, disk)
	}
	data.Binary = false
	if _, _, _, e := s.ApplyRecord("json", data); e != nil {
		t.Fatal(e)
	}
	if fingerprint().Sum == memory.Sum {
		t.Fatal("binary flag ignored")
	}
}

func BenchmarkLocalSubtreeFingerprint(b *testing.B) {
	s, _, _ := ticketStores(b)
	const c = "0000000000000001"
	rows := make([]Record, 512)
	for i := range rows {
		rows[i] = Record{Collection: c, Location: "a0", ID: fmt.Sprint(i), Metrics: []float64{float64(i)}, Payload: json.RawMessage(`{"hello":"world"}`)}
	}
	if err := s.SeedLocalRecords(rows); err != nil {
		b.Fatal(err)
	}
	if err := s.db.Flush(); err != nil {
		b.Fatal(err)
	}
	zone, _ := tree.NewZoneKey(c, "a")
	for _, mode := range []string{"cold-result-cache", "hot-result-cache"} {
		b.Run(mode, func(b *testing.B) {
			ctx := context.Background()
			if _, e := s.LocalSubtreeFingerprint(ctx, c, "a"); e != nil {
				b.Fatal(e)
			}
			start, _ := zonelog.ScanWork()
			b.ReportAllocs()
			for b.Loop() {
				if mode == "cold-result-cache" {
					s.fingerprints.mu.Lock()
					delete(s.fingerprints.entries, zone)
					s.fingerprints.mu.Unlock()
				}
				fp, e := s.LocalSubtreeFingerprint(ctx, c, "a")
				if e != nil || fp.Count != 512 {
					b.Fatal(fp, e)
				}
			}
			end, _ := zonelog.ScanWork()
			b.ReportMetric(float64(end-start)/float64(b.N), "keys/op")
		})
	}
}

func TestSubtreeFingerprintDeclinesColdMigration(t *testing.T) {
	s, _, _ := ticketStores(t)
	ctx := context.Background()
	const c = "0000000000000001"
	before, e := s.LocalSubtreeFingerprint(ctx, c, "@")
	if e != nil || before.Guard == "" {
		t.Fatal(before, e)
	}
	if e := s.update(func(tx *Tx) error { return tx.put(meta(coldDrainKey), []byte("pending")) }); e != nil {
		t.Fatal(e)
	}
	now, e := s.LocalSubtreeFingerprint(ctx, c, "@")
	if e != nil || now != (SubtreeFingerprint{}) {
		t.Fatal(now, e)
	}
}

func TestSubtreeFingerprintDuringDisjointHandoff(t *testing.T) {
	source, target, genesis := ticketStores(t)
	ctx := context.Background()
	const collection = "0000000000000001"
	write := func(id, location, body string, deleted bool) {
		t.Helper()
		record := Record{Collection: collection, Location: location, ID: id, Payload: json.RawMessage(body), Deleted: deleted, Metrics: []float64{1}}
		if _, _, _, err := source.ApplyRecord(id+location+body, record); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint := func(s *Store, location string) SubtreeFingerprint {
		t.Helper()
		fp, err := s.LocalSubtreeFingerprint(ctx, collection, location)
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	zone, err := tree.NewZoneKey(collection, "aa")
	if err != nil {
		t.Fatal(err)
	}
	write("moving", "aa0", `"moving"`, false)
	write("stable", "b0", `"stable"`, false)
	stable := fingerprint(source, "b")

	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.PrefixRegion(zone))
	for _, store := range []*Store{source, target} {
		for _, location := range []string{"@", "a", "aa", "aa0"} {
			if got := fingerprint(store, location); got != (SubtreeFingerprint{}) {
				t.Fatalf("overlapping prefix %q on %s has proof: %+v", location, store.node, got)
			}
		}
	}
	got := fingerprint(source, "b")
	if got.Fingerprint == "" || got.Guard == "" || got.Guard == stable.Guard || got.Count != 1 || got.Sum != stable.Sum {
		t.Fatalf("disjoint stable rows unavailable or stale: %+v (before %+v)", got, stable)
	}
	empty := fingerprint(target, "b")
	if empty.Fingerprint == "" || empty.Guard == "" || empty.Count != 0 {
		t.Fatalf("disjoint empty local scope not observed: %+v", empty)
	}
	write("temporary", "b1", `"new"`, false)
	write("temporary", "b1", "", true)
	restored := fingerprint(source, "b")
	if restored.Fingerprint != got.Fingerprint || restored.Guard == got.Guard {
		t.Fatalf("disjoint ABA guard failed: %+v (before %+v)", restored, got)
	}
	ticketFinish(t, source, target, op.Cession.ID)
	if got := fingerprint(target, "aa"); got.Fingerprint == "" || got.Count != 1 {
		t.Fatalf("installed content unavailable: %+v", got)
	}
}
