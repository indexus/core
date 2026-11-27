package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
)

func newPlanningStore(t testing.TB, path, name string) (*Store, causal.Endpoint) {
	t.Helper()
	s, err := OpenWithOptions(path, name, "planning", 1<<28, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	holder, err := s.Endpoint(name)
	if err != nil {
		t.Fatal(err)
	}
	return s, holder
}

func assertPlanningParity(t testing.TB, s *Store) []ZonePlanningStats {
	t.Helper()
	want := map[tree.ZoneKey]ZonePlanningStats{}
	// Planning counts the zones this vnode may write; client reads also serve
	// zones still moving (readableIn), so the reference scan uses writableIn.
	rows, err := read(s, func(tx *Tx) ([]Snapshot, error) {
		var out []Snapshot
		err := s.eachInventoryZone(context.Background(), tx, nil, 0, nil, rowsWithPayloads, func(z tree.ZoneKey, rows []*row) error {
			snap, err := s.snapshotOfRows(context.Background(), tx, z, rows, true)
			out = append(out, snap)
			return err
		})
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, snap := range rows {
		seal, err := snap.Seal()
		if err != nil {
			t.Fatal(err)
		}
		// The bytes are those the rows take in zonelog, not their JSON:
		// the seal's arrays with each record's row size in place of its JSON.
		arrays := func(n, size int) int {
			if n == 0 {
				return 4
			}
			return size + n - 1 + 2
		}
		recordBytes := 0
		for _, r := range snap.Records {
			size, err := recordSize(r)
			if err != nil {
				t.Fatal(err)
			}
			recordBytes += size
		}
		requestBytes := 0
		for _, op := range snap.Requests {
			raw, _ := json.Marshal(op)
			requestBytes += len(raw)
		}
		size := len(`{"Records":,"Requests":}`) + arrays(len(snap.Records), recordBytes) + arrays(len(snap.Requests), requestBytes)
		want[snap.Zone] = ZonePlanningStats{Zone: snap.Zone, Items: seal.Items, Bytes: int64(size)}
	}
	got, err := s.PlanningStats()
	if err != nil {
		t.Fatal(err)
	}
	actual := map[tree.ZoneKey]ZonePlanningStats{}
	for _, row := range got {
		if _, duplicate := actual[row.Zone]; duplicate {
			t.Fatalf("duplicate planning zone %+v", row.Zone)
		}
		actual[row.Zone] = row
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("planning counters differ from independent snapshot seals:\ngot=%+v\nwant=%+v", actual, want)
	}
	totals, err := s.PlanningTotals()
	if err != nil {
		t.Fatal(err)
	}
	var items int64
	collections := map[string]bool{}
	for _, row := range got {
		items += row.Items
		collections[row.Zone.Collection] = true
	}
	if totals.Items != items || totals.Zones != len(got) || totals.Collections != len(collections) {
		t.Fatalf("streamed monitoring totals disagree: %+v", totals)
	}
	return got
}

func TestPlanningStatsTrackMutationsReceiptsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	s, holder := newPlanningStore(t, path, "D")
	defer func() { s.Close() }()
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "planning", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	// These canonical paths deliberately have the same geometric ID.
	parent, err := tree.NewZoneKey("0000000000000001", "a")
	if err != nil {
		t.Fatal(err)
	}
	child, err := tree.NewZoneKey(parent.Collection, "a0")
	if err != nil || parent.ID != child.ID {
		t.Fatalf("coincident-zone fixture: %+v %+v %v", parent, child, err)
	}
	operations := []Record{
		{Collection: parent.Collection, Location: parent.Location, ID: "same-id", Metrics: nil},
		{Collection: child.Collection, Location: child.Location, ID: "same-id", Metrics: []float64{}},
		{Collection: "Other00000000001", Location: parent.Location, ID: "same-id", Metrics: []float64{math.Copysign(0, -1), 0.125, 1e20}},
		{Collection: parent.Collection, Location: parent.Location, ID: "same-id", Metrics: []float64{7, 9}},
		{Collection: parent.Collection, Location: parent.Location, ID: "same-id", Deleted: true},
		{Collection: parent.Collection, Location: parent.Location, ID: "already-deleted", Deleted: true},
		{Collection: parent.Collection, Location: parent.Location, ID: "same-id", Metrics: []float64{3}},
	}
	for i, record := range operations {
		request := fmt.Sprintf("request-%d", i)
		if _, _, applied, err := s.ApplyRecord(request, record); err != nil || !applied {
			t.Fatalf("apply %d: applied=%v err=%v", i, applied, err)
		}
		before := assertPlanningParity(t, s)
		if _, _, applied, err := s.ApplyRecord(request, record); err != nil || applied {
			t.Fatalf("receipt retry %d: applied=%v err=%v", i, applied, err)
		}
		if after := assertPlanningParity(t, s); !reflect.DeepEqual(before, after) {
			t.Fatal("receipt retry changed planning counters")
		}
		assertStoredUsage(t, s)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "D", "planning", 1<<28)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanningParity(t, s)

}

func TestPlanningStatsRollbackDoesNotPublishDerivedChanges(t *testing.T) {
	s, holder := newPlanningStore(t, filepath.Join(t.TempDir(), "rollback.db"), "D")
	defer s.Close()
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "planning", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "row", Metrics: []float64{1}}
	if _, _, _, err := s.ApplyRecord("seed", r); err != nil {
		t.Fatal(err)
	}
	before := assertPlanningParity(t, s)
	_, used, err := s.CapacityUsage()
	if err != nil {
		t.Fatal(err)
	}
	capacity := s.capacity
	s.capacity = used.PhysicalLoad() + 128
	r.Payload = bigPayload(8000)
	if _, _, applied, err := s.ApplyRecord("refused", r); !errors.Is(err, ErrDataCapacity) || applied {
		t.Fatalf("capacity refusal: applied=%v err=%v", applied, err)
	}
	s.capacity = capacity
	if after := assertPlanningParity(t, s); !reflect.DeepEqual(before, after) {
		t.Fatal("capacity rollback changed planning counters")
	}
	assertStoredUsage(t, s)
}

func TestPlanningStatsBatchFailureKeepsSuccessfulSiblings(t *testing.T) {
	s, err := OpenWithOptions(filepath.Join(t.TempDir(), "batch.db"), "D", "planning", 1<<17, Options{NoSync: true, BatchWrites: true, BatchSize: 16, BatchDelay: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	holder, err := s.Endpoint("D")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "planning", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	const good = 8
	start := make(chan struct{})
	results := make(chan error, good+1)
	var workers sync.WaitGroup
	for i := range good {
		workers.Go(func() {
			<-start
			_, _, applied, err := s.ApplyRecord(fmt.Sprintf("good-%d", i), Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprint(i), Metrics: []float64{1}})
			if err == nil && !applied {
				err = fmt.Errorf("new request %d was not applied", i)
			}
			results <- err
		})
	}
	workers.Go(func() {
		<-start
		_, _, applied, err := s.ApplyRecord("bad", Record{Collection: "Demo000000000000", Location: "ab", ID: "too-large", Payload: bigPayload(800_000)})
		if !errors.Is(err, ErrDataCapacity) || applied {
			results <- fmt.Errorf("expected capacity refusal, applied=%v err=%v", applied, err)
			return
		}
		results <- nil
	})
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := assertPlanningParity(t, s)
	if len(got) != 1 || got[0].Items != good {
		t.Fatalf("failed callback leaked counters: %+v", got)
	}
	assertStoredUsage(t, s)
}

func TestPlanningStatsFollowCausalTransferAndReturn(t *testing.T) {
	sp, tp := filepath.Join(t.TempDir(), "source.db"), filepath.Join(t.TempDir(), "target.db")
	source, sh := newPlanningStore(t, sp, "D")
	target, th := newPlanningStore(t, tp, "J")
	defer func() { source.Close(); target.Close() }()
	root := tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}
	g := causal.Genesis{Cluster: "planning", Holder: sh}
	for _, s := range []*Store{source, target} {
		if err := s.InitCausal(root, g); err != nil {
			t.Fatal(err)
		}
	}
	for i, location := range []string{"aa", "aa0"} {
		for j := range 3 {
			r := Record{Collection: "Demo000000000000", Location: location, ID: fmt.Sprintf("row-%d", j), Metrics: []float64{float64(i + j)}}
			if _, _, _, err := source.ApplyRecord(fmt.Sprintf("put-%d-%d", i, j), r); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertPlanningParity(t, source)
	move := func(from, to *Store) {
		t.Helper()
		state, err := from.CausalState()
		if err != nil || len(state.Permissions.Native) == 0 {
			t.Fatalf("missing native permission: %v", err)
		}
		op := ticketAdmission(t, from, to, state.Permissions.Native[0].ID, tree.Region{})
		if _, err := from.ForceFreezeRegion(op.Cession.ID); err != nil {
			t.Fatal(err)
		}
		if got := assertPlanningParity(t, from); len(got) != 0 {
			t.Fatal("frozen source exposed writable planning zones")
		}
		ticketFinish(t, from, to, op.Cession.ID)
	}
	move(source, target)
	if got := assertPlanningParity(t, target); len(got) == 0 {
		t.Fatal("installed rights must be writable before membership activation")
	}
	if _, err := target.PublishActivation(tree.VNodeDescriptor{Name: "J", ID: tree.ID{128}}, th); err != nil {
		t.Fatal(err)
	}
	assertPlanningParity(t, target)
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	target, err = Open(tp, "J", "planning", 1<<28)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanningParity(t, target)
	if _, _, _, err := target.ApplyRecord("replace-on-target", Record{Collection: "Demo000000000000", Location: "aa", ID: "row-0", Deleted: true}); err != nil {
		t.Fatal(err)
	}
	assertPlanningParity(t, target)
	move(target, source)
	assertPlanningParity(t, source)
	assertPlanningParity(t, target)
	assertStoredUsage(t, source)
	assertStoredUsage(t, target)
}

func BenchmarkPlanningStats(b *testing.B) {
	for _, rows := range []int{1024, 8192} {
		b.Run(fmt.Sprint(rows), func(b *testing.B) {
			s, holder := newPlanningStore(b, filepath.Join(b.TempDir(), "stats.db"), "D")
			defer s.Close()
			if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "planning", Holder: holder}); err != nil {
				b.Fatal(err)
			}
			for i := range rows {
				if _, _, _, err := s.ApplyRecord(fmt.Sprint(i), Record{Collection: "Demo000000000000", Location: fmt.Sprintf("a%d", i%8), ID: fmt.Sprint(i), Metrics: []float64{1, 2, 3}}); err != nil {
					b.Fatal(err)
				}
			}
			assertPlanningParity(b, s)
			b.Run("counters", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := s.PlanningStats(); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("inventory-and-seal", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					inventory, err := s.Inventory(true)
					if err != nil {
						b.Fatal(err)
					}
					for _, snap := range inventory {
						if _, err := snap.Seal(); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		})
	}
}

// bigPayload is a JSON string of n bytes in all: a large record.
func bigPayload(n int) json.RawMessage {
	return json.RawMessage(`"` + strings.Repeat("x", n-2) + `"`)
}
