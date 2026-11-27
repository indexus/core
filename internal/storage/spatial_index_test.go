package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestSpatialDiscoveryDoesNotRescanDatasetForEachGrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spatial")
	s, e := OpenWithOptions(path, "D", "test", 1<<26, Options{NoSync: true})
	if e != nil {
		t.Fatal(e)
	}
	initTestCausal(t, s)
	state, e := s.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	g := state.Genesis
	defer func() { s.Close() }()
	const count = 4096
	records := make([]Record, count)
	for i := range records {
		records[i] = Record{Collection: fmt.Sprintf("collection-%05d", i), Location: "aa", ID: "seed"}
	}
	if e := s.SeedLocalRecords(records); e != nil {
		t.Fatal(e)
	}
	// Reopen the real disk: derived roots are not persisted. The initial
	// O(records) metadata construction is measured separately from queries.
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = OpenWithOptions(path, "D", "test", 1<<26, Options{NoSync: true})
	if e != nil {
		t.Fatal(e)
	}
	if e := s.ensureSpatial(context.Background()); e != nil {
		t.Fatal(e)
	}
	if got := s.spatial.buildRows.Load(); got != count {
		t.Fatalf("cold construction examined %d headers, want %d", got, count)
	}
	target := g.Holder
	target.Owner.Storage = "other-disk"
	target.Owner.VNode = "other"
	target.Host = "other"
	before := s.spatial.visits.Load()
	const grains = 64
	for i := range grains {
		z, _ := recordZone(records[(i*61)%count])
		op, e := s.BeginCausal(context.Background(), g.Right().ID, target, tree.Point(z.ID))
		if e != nil {
			t.Fatal(e)
		}
		n := 0
		_, e = s.eachZoneStatePaged(op.Cession.ID, func(_ *Tx, _ CausalState, st ZoneState) (bool, error) { n++; return true, nil })
		if e != nil || n != 1 {
			t.Fatalf("discovery: %d %v", n, e)
		}
		if e = s.AbortCausal(op.Cession.ID); e != nil {
			t.Fatal(e)
		}
		if e = s.FinishAbortCausal(op.Cession.ID); e != nil {
			t.Fatal(e)
		}
	}
	visits := s.spatial.visits.Load() - before
	if visits > grains*128 {
		t.Fatalf("point grains examined %d index nodes: dataset rescanned", visits)
	}
	if got := s.spatial.buildRows.Load(); got != count {
		t.Fatalf("repeated initial construction: %d", got)
	}
	t.Logf("cold headers=%d, %d real BeginCausal discoveries examined %d index nodes (dataset=%d)", count, grains, visits, count)
}

func TestSpatialViewRetainsExactMVCCAndUpdatesZoneMembership(t *testing.T) {
	s, _ := routingStore(t)
	first := Record{Collection: "Demo000000000000", Location: "aa", ID: "one"}
	second := Record{Collection: "Demo000000000000", Location: "ab", ID: "two"}
	if _, e := s.Write("one", first); e != nil {
		t.Fatal(e)
	}
	z, _ := recordZone(first)
	z2, _ := recordZone(second)
	if e := s.spatialView(context.Background(), func(tx *Tx, root *spatialNode) error {
		// Immutable readers hold no index/writer lock while disk commits proceed.
		if _, e := s.Write("two", second); e != nil {
			return e
		}
		if spatialHas(root, z2) || !spatialHas(root, z) {
			t.Fatal("reader root changed after concurrent commit")
		}
		rows, e := rowsOf(tx, z2, false)
		if e != nil {
			return e
		}
		if len(rows) != 0 {
			t.Fatal("data snapshot advanced without its index root")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if !spatialHas(s.spatial.state.Load().root, z2) {
		t.Fatal("committed new zone missing")
	}
	sentinel := errors.New("rollback")
	if e := s.update(func(tx *Tx) error {
		if e := tx.clearZone(z); e != nil {
			return e
		}
		return sentinel
	}); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	if !spatialHas(s.spatial.state.Load().root, z) {
		t.Fatal("rollback removed zone")
	}
	// Deleted records remain canonical records. Only clearZone removes an entire
	// zone; this is the invariant used by spatialChanges' all-nil removal case.
	first.Deleted = true
	if _, e := s.Write("delete", first); e != nil {
		t.Fatal(e)
	}
	if !spatialHas(s.spatial.state.Load().root, z) {
		t.Fatal("record tombstone removed canonical zone")
	}
	if e := s.update(func(tx *Tx) error { return tx.clearZone(z) }); e != nil {
		t.Fatal(e)
	}
	if spatialHas(s.spatial.state.Load().root, z) || !spatialHas(s.spatial.state.Load().root, z2) {
		t.Fatal("whole-zone removal lost another zone or retained removed zone")
	}
}

func TestSpatialNonPrefixMasksAreExactAndNewGrainsUseLeadingBits(t *testing.T) {
	s, _ := routingStore(t)
	var records []Record
	for i := 0; i < 1024; i++ {
		var id tree.ID
		id[0] = byte(i >> 2)
		id[1] = byte(i << 6)
		id[len(id)-1] = byte(i & 1)
		records = append(records, Record{Collection: "Demo000000000000", Location: tree.BASE64.Encode(id[:]), ID: "seed"})
	}
	if e := s.SeedLocalRecords(records); e != nil {
		t.Fatal(e)
	}
	// A fixed low bit produces a non-contiguous masked cube. Search is exact;
	// no universal sublinear bound is claimed for such arbitrary original masks.
	var stripe tree.Region
	stripe.Mask[len(stripe.Mask)-1] = 1
	got := map[tree.ZoneKey]bool{}
	if e := s.scanSpatialRegion(context.Background(), stripe, rowsNone, func(_ *Tx, z tree.ZoneKey, _ []*row) error { got[z] = true; return nil }); e != nil {
		t.Fatal(e)
	}
	if len(got) != 512 {
		t.Fatalf("low-bit mask returned %d zones", len(got))
	}
	grain, e := s.HandoffGrain(context.Background(), stripe, 32, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	for z := range got {
		if grain.Contains(z.ID) {
			n++
		}
	}
	if n == 0 || n > 32 {
		t.Fatalf("grain zones=%d", n)
	}
	if grain.Mask[0]&128 == 0 {
		t.Fatalf("refinement did not constrain leading spatial bits: %+v", grain)
	}
}

func TestSpatialDiscoveryCannotRecreateRowsAfterConcurrentAbort(t *testing.T) {
	source, target, g := ticketStores(t)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "before"}
	if _, e := source.Write("before", record); e != nil {
		t.Fatal(e)
	}
	endpoint, e := target.Endpoint("B")
	if e != nil {
		t.Fatal(e)
	}
	// Hold only the derived-index lock: the initial metadata intent can commit,
	// but its first inventory snapshot cannot yet be captured.
	source.spatial.mu.Lock()
	locked := true
	defer func() {
		if locked {
			source.spatial.mu.Unlock()
		}
	}()
	result := make(chan error, 1)
	go func() {
		_, err := source.BeginCausal(context.Background(), g.Right().ID, endpoint, tree.Region{})
		result <- err
	}()
	var op CausalTransfer
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := source.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range state.Transfers {
			op = candidate
		}
		if op.Cession.ID.Valid() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source intent never committed")
		}
		runtime.Gosched()
	}
	reserved, e := target.ReserveCausal(op.Cession)
	if e != nil {
		t.Fatal(e)
	}
	if e = source.BindCausal(op.Cession.ID, reserved.Ticket); e != nil {
		t.Fatal(e)
	}
	op.Cession = reserved
	if _, e = target.PreloadCausal(op); e != nil {
		t.Fatal(e)
	}
	if e = source.ConfirmCausal(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = source.ForceFreezeRegion(op.Cession.ID); !errors.Is(e, ErrNotReady) {
		t.Fatalf("fenced incomplete discovery: %v", e)
	}
	if e = source.AbortCausal(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = target.CancelCausal(op.Cession); e != nil {
		t.Fatal(e)
	}
	if e = source.FinishAbortCausal(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	source.spatial.mu.Unlock()
	locked = false
	select {
	case e = <-result:
		if !errors.Is(e, ErrNotReady) {
			t.Fatalf("cancelled discovery returned %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not stop")
	}
	count := 0
	_, e = source.eachZoneStatePaged(op.Cession.ID, func(_ *Tx, _ CausalState, _ ZoneState) (bool, error) { count++; return true, nil })
	if e != nil || count != 0 {
		t.Fatalf("orphan discovery rows: %d %v", count, e)
	}
	if _, e = source.Write("after-abort", record); e != nil {
		t.Fatal(e)
	}
}
